package relay

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// instanceTTL is how long a relay_instances row stays live after its last
// heartbeat. Liveness is a read-side predicate (last_seen within the TTL), so
// a crashed relay disappears from routing without anyone deleting anything.
const instanceTTL = 15 * time.Second

// NOTIFY channels. Fired inside the store method that changes the rows;
// payload is the key that changed.
const (
	chanInstances = "piper_instances" // instance id
	chanOwners    = "piper_owners"    // agent base domain
	chanHostnames = "piper_hostnames" // hostname or custom domain
	chanEvents    = "piper_events"    // agent name (= base domain for account enrollments)
)

// liveWhere is the liveness predicate, evaluated against the server clock.
var liveWhere = fmt.Sprintf(`last_seen > now() - interval '%d seconds'`, int(instanceTTL/time.Second))

// InstanceRow is one relay process as the pool sees it.
type InstanceRow struct {
	ID         string
	StartedAt  time.Time
	Sessions   int
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	APIAddr    string
}

// execer is the slice of *sql.DB and *sql.Tx that notify needs.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// notify fires a Postgres NOTIFY. Inside a transaction it is delivered on
// commit, which is exactly when the row it announces becomes visible.
func notify(ex execer, channel, payload string) error {
	_, err := ex.Exec(`SELECT pg_notify($1, $2)`, channel, payload)
	return err
}

// UpsertInstance inserts or refreshes an instance row — the heartbeat — and
// announces it on piper_instances.
func (s *Store) UpsertInstance(r InstanceRow) error {
	if _, err := s.db.Exec(
		`INSERT INTO relay_instances(id, started_at, last_seen, sessions, tls_addr, http_addr, tunnel_addr, api_addr)
		 VALUES($1, $2, now(), $3, $4, $5, $6, $7)
		 ON CONFLICT(id) DO UPDATE SET last_seen = now(), sessions = excluded.sessions`,
		r.ID, r.StartedAt, r.Sessions, r.TLSAddr, r.HTTPAddr, r.TunnelAddr, r.APIAddr); err != nil {
		return err
	}
	return notify(s.db, chanInstances, r.ID)
}

// DeleteInstance removes an instance row; the cascade takes its agent_owners
// rows. Clean shutdown calls it, and so does whoever finds the instance dead.
func (s *Store) DeleteInstance(id string) error {
	if _, err := s.db.Exec(`DELETE FROM relay_instances WHERE id=$1`, id); err != nil {
		return err
	}
	return notify(s.db, chanInstances, id)
}

// PurgeDeadInstances deletes every row past instanceTTL. Rows it removes were
// already invisible to LiveInstances/OwnerOf, so it does not notify.
func (s *Store) PurgeDeadInstances() error {
	_, err := s.db.Exec(`DELETE FROM relay_instances WHERE NOT (` + liveWhere + `)`)
	return err
}

const instanceCols = `id, started_at, sessions, tls_addr, http_addr, tunnel_addr, api_addr`

func scanInstance(sc interface{ Scan(...any) error }) (InstanceRow, error) {
	var r InstanceRow
	err := sc.Scan(&r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr)
	return r, err
}

// LiveInstances lists the instances heard from within instanceTTL, earliest
// started first (ties by id, so the order is total).
func (s *Store) LiveInstances() ([]InstanceRow, error) {
	rows, err := s.db.Query(`SELECT ` + instanceCols + ` FROM relay_instances WHERE ` + liveWhere + ` ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceRow
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetOwner records that instanceID now terminates baseDomain's tunnel. An
// upsert: an agent that reconnected elsewhere is the truth, so the new owner
// overwrites. Unknown agents are ErrBadToken.
func (s *Store) SetOwner(baseDomain, instanceID string) error {
	res, err := s.db.Exec(
		`INSERT INTO agent_owners(agent_name, instance_id, since)
		 SELECT name, $2, now() FROM agents WHERE base_domain=$1
		 ON CONFLICT(agent_name) DO UPDATE SET instance_id = excluded.instance_id, since = excluded.since`,
		baseDomain, instanceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadToken
	}
	return notify(s.db, chanOwners, baseDomain)
}

// ClearOwner drops baseDomain's owner row only while instanceID still holds
// it, so a relay whose half-open session dies late never removes the new
// owner's row. Clearing a row someone else holds is a silent no-op.
func (s *Store) ClearOwner(baseDomain, instanceID string) error {
	res, err := s.db.Exec(
		`DELETE FROM agent_owners WHERE instance_id=$2
		    AND agent_name = (SELECT name FROM agents WHERE base_domain=$1)`,
		baseDomain, instanceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	return notify(s.db, chanOwners, baseDomain)
}

// OwnerOf returns the live instance holding baseDomain's tunnel. ok is false
// when nobody does, or when the recorded owner has stopped heartbeating.
func (s *Store) OwnerOf(baseDomain string) (InstanceRow, bool, error) {
	r, err := scanInstance(s.db.QueryRow(
		`SELECT i.id, i.started_at, i.sessions, i.tls_addr, i.http_addr, i.tunnel_addr, i.api_addr
		   FROM agent_owners o
		   JOIN agents a ON a.name = o.agent_name
		   JOIN relay_instances i ON i.id = o.instance_id
		  WHERE a.base_domain=$1 AND i.`+liveWhere, baseDomain))
	if errors.Is(err, sql.ErrNoRows) {
		return InstanceRow{}, false, nil
	}
	if err != nil {
		return InstanceRow{}, false, err
	}
	return r, true, nil
}

// Owners maps every agent base domain to the id of its live owner.
func (s *Store) Owners() (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT a.base_domain, o.instance_id
		   FROM agent_owners o
		   JOIN agents a ON a.name = o.agent_name
		   JOIN relay_instances i ON i.id = o.instance_id
		  WHERE i.` + liveWhere)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var base, id string
		if err := rows.Scan(&base, &id); err != nil {
			return nil, err
		}
		out[base] = id
	}
	return out, rows.Err()
}

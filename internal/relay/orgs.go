package relay

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Org is one org membership from the caller's point of view.
type Org struct {
	ID   string
	Slug string
	Role string // the caller's role: "owner" | "member"
}

// ErrNoOrg is returned when an org doesn't exist or the caller is not a
// member — deliberately indistinguishable, so org existence never leaks.
var ErrNoOrg = errors.New("no such org")

// CreateOrg creates an org account (type='org', no GitHub identity, no
// credentials) with a slug derived from name — unique among orgs only, since
// users hold their own namespace (#411) — and makes the creator its sole owner.
func (s *Store) CreateOrg(creatorID, name string) (Org, error) {
	var ctype string
	err := s.db.QueryRow(`SELECT type FROM accounts WHERE id=?`, creatorID).Scan(&ctype)
	if errors.Is(err, sql.ErrNoRows) {
		return Org{}, ErrBadCredential
	}
	if err != nil {
		return Org{}, err
	}
	if ctype != "user" {
		return Org{}, errors.New("only user accounts create orgs")
	}

	base := deriveUsername(name)
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return Org{}, err
	}
	defer tx.Rollback()
	for i := 1; ; i++ {
		slug := base
		if i > 1 {
			slug = base + "-" + strconv.Itoa(i)
		}
		_, err := tx.Exec(
			`INSERT INTO accounts(id, github_id, github_login, username, type, disabled, created_at)
			 VALUES(?,NULL,NULL,?,'org',0,?)`, id, slug, now)
		if err == nil {
			if _, err := tx.Exec(
				`INSERT INTO org_members(org_id, account_id, role, created_at) VALUES(?,?,'owner',?)`,
				id, creatorID, now); err != nil {
				return Org{}, err
			}
			if err := tx.Commit(); err != nil {
				return Org{}, err
			}
			return Org{ID: id, Slug: slug, Role: "owner"}, nil
		}
		if isUniqueViolation(err) {
			continue // another org holds this slug; try the next suffix
		}
		return Org{}, err
	}
}

// OrgsForAccount lists the orgs accountID belongs to, oldest membership first.
func (s *Store) OrgsForAccount(accountID string) ([]Org, error) {
	rows, err := s.db.Query(
		`SELECT o.id, o.username, m.role
		   FROM org_members m JOIN accounts o ON o.id = m.org_id
		  WHERE m.account_id = ? ORDER BY m.rowid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orgs []Org
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Role); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

// OrgRole resolves slug to the org's account id and accountID's role in it.
// ErrNoOrg both when no such org exists and when the caller is not a member,
// so a non-member can't probe org existence.
func (s *Store) OrgRole(slug, accountID string) (orgID, role string, err error) {
	err = s.db.QueryRow(
		`SELECT o.id, m.role
		   FROM accounts o JOIN org_members m ON m.org_id = o.id AND m.account_id = ?
		  WHERE o.username = ? AND o.type = 'org'`, accountID, slug).
		Scan(&orgID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNoOrg
	}
	if err != nil {
		return "", "", err
	}
	return orgID, role, nil
}

// Member is one row of an org's member list.
type Member struct {
	Username string
	Role     string
}

// ErrNotMember is returned when the target username has no membership row.
var ErrNotMember = errors.New("not a member")

// ErrLastOwner is returned when a role change or removal would leave the org
// with no owner.
var ErrLastOwner = errors.New("an org must keep at least one owner")

// Members lists an org's members, oldest first.
func (s *Store) Members(orgID string) ([]Member, error) {
	rows, err := s.db.Query(
		`SELECT a.username, m.role
		   FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? ORDER BY m.rowid`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Username, &m.Role); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// memberForUpdate resolves username's membership row inside tx and, when the
// change would drop an owner, enforces the last-owner rule.
func memberForUpdate(tx *sql.Tx, orgID, username string, dropsOwner func(cur string) bool) (targetID string, err error) {
	var cur string
	err = tx.QueryRow(
		`SELECT a.id, m.role
		   FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? AND a.username = ?`, orgID, username).
		Scan(&targetID, &cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotMember
	}
	if err != nil {
		return "", err
	}
	if dropsOwner(cur) {
		var owners int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM org_members WHERE org_id = ? AND role = 'owner'`, orgID).
			Scan(&owners); err != nil {
			return "", err
		}
		if owners <= 1 {
			return "", ErrLastOwner
		}
	}
	return targetID, nil
}

// SetMemberRole changes username's role in the org. Demoting the last owner is
// ErrLastOwner; an unknown target is ErrNotMember.
func (s *Store) SetMemberRole(orgID, username, role string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	targetID, err := memberForUpdate(tx, orgID, username,
		func(cur string) bool { return cur == "owner" && role != "owner" })
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE org_members SET role=? WHERE org_id=? AND account_id=?`, role, orgID, targetID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveMember deletes username's membership. Removing the last owner is
// ErrLastOwner; an unknown target is ErrNotMember. The member's personal
// account and boxes are untouched.
func (s *Store) RemoveMember(orgID, username string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	targetID, err := memberForUpdate(tx, orgID, username,
		func(cur string) bool { return cur == "owner" })
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM org_members WHERE org_id=? AND account_id=?`, orgID, targetID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrAlreadyMember is returned when inviting someone who is already a member.
var ErrAlreadyMember = errors.New("already a member")

// ErrNoInvite is returned when no matching pending invite exists — including
// for a nonexistent org, so accept/decline don't leak org existence.
var ErrNoInvite = errors.New("no such invite")

// CreateInvite records a pending invite for a GitHub username (stored
// lowercased; matching is case-insensitive). Inviting an existing member is
// ErrAlreadyMember; re-inviting the same login is idempotent. The username is
// not validated against GitHub — a typo'd invite sits pending until revoked.
func (s *Store) CreateInvite(orgID, githubLogin, inviterID string) error {
	login := strings.ToLower(githubLogin)
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? AND lower(a.github_login) = ?`, orgID, login).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrAlreadyMember
	}
	_, err := s.db.Exec(
		`INSERT INTO org_invites(org_id, github_login, invited_by, created_at) VALUES(?,?,?,?)`,
		orgID, login, inviterID, time.Now().UTC().Format(time.RFC3339Nano))
	if isUniqueViolation(err) {
		return nil // an identical pending invite already exists
	}
	return err
}

// OrgInvites lists an org's pending invite logins, oldest first.
func (s *Store) OrgInvites(orgID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT github_login FROM org_invites WHERE org_id = ? ORDER BY rowid`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logins []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		logins = append(logins, l)
	}
	return logins, rows.Err()
}

// RevokeInvite withdraws a pending invite. ErrNoInvite if none matches.
func (s *Store) RevokeInvite(orgID, githubLogin string) error {
	res, err := s.db.Exec(
		`DELETE FROM org_invites WHERE org_id = ? AND github_login = ?`,
		orgID, strings.ToLower(githubLogin))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoInvite
	}
	return nil
}

// InvitesForAccount lists the org slugs holding a pending invite for the
// account's current GitHub login. An account with no stored login has no
// matchable invites.
func (s *Store) InvitesForAccount(accountID string) ([]string, error) {
	var login sql.NullString
	if err := s.db.QueryRow(
		`SELECT github_login FROM accounts WHERE id = ?`, accountID).Scan(&login); err != nil {
		return nil, err
	}
	if !login.Valid || login.String == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT o.username FROM org_invites i JOIN accounts o ON o.id = i.org_id
		  WHERE i.github_login = ? ORDER BY i.rowid`, strings.ToLower(login.String))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// takeInvite consumes (deletes) the pending invite matching accountID's
// current GitHub login in the org named orgSlug, returning the org id.
// Any miss — unknown org, no stored login, no invite — is ErrNoInvite.
func takeInvite(tx *sql.Tx, accountID, orgSlug string) (orgID string, err error) {
	err = tx.QueryRow(
		`SELECT id FROM accounts WHERE username = ? AND type = 'org'`, orgSlug).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInvite
	}
	if err != nil {
		return "", err
	}
	var login sql.NullString
	if err := tx.QueryRow(
		`SELECT github_login FROM accounts WHERE id = ?`, accountID).Scan(&login); err != nil {
		return "", err
	}
	if !login.Valid || login.String == "" {
		return "", ErrNoInvite
	}
	res, err := tx.Exec(
		`DELETE FROM org_invites WHERE org_id = ? AND github_login = ?`,
		orgID, strings.ToLower(login.String))
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNoInvite
	}
	return orgID, nil
}

// AcceptInvite consumes accountID's pending invite to orgSlug and adds the
// membership as "member" (owners promote afterwards).
func (s *Store) AcceptInvite(accountID, orgSlug string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	orgID, err := takeInvite(tx, accountID, orgSlug)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO org_members(org_id, account_id, role, created_at) VALUES(?,?,'member',?)`,
		orgID, accountID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeclineInvite consumes the invite without creating a membership.
func (s *Store) DeclineInvite(accountID, orgSlug string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := takeInvite(tx, accountID, orgSlug); err != nil {
		return err
	}
	return tx.Commit()
}

// CanControl reports whether caller may drive agents owned by owner: the
// owner itself, or any member of the owning org. Role does not matter here —
// owners and members both drive; role only gates org management.
func (s *Store) CanControl(callerID, ownerID string) (bool, error) {
	if callerID == ownerID {
		return true, nil
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM org_members WHERE org_id = ? AND account_id = ?`,
		ownerID, callerID).Scan(&n)
	return n > 0, err
}

// CanManage reports whether callerID may change ownerID's inventory: the
// account itself, or an "owner" member of the org. Distinct from CanControl,
// which grants drive rights to every member — enrolling a box is owner-only
// (see api.go), so removing one must be too, or a member could permanently
// destroy a box nobody can re-create without shell access to the hardware.
func (s *Store) CanManage(callerID, ownerID string) (bool, error) {
	if callerID == ownerID {
		return true, nil
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM org_members WHERE org_id = ? AND account_id = ? AND role = 'owner'`,
		ownerID, callerID).Scan(&n)
	return n > 0, err
}

// OwnedAgent is one row of an account's visible-agents list.
type OwnedAgent struct {
	BaseDomain string
	Name       string // operator-chosen box name (piperd token create --name)
	Owner      string // owning account/org slug
}

// AgentsVisibleTo returns the agents accountID may drive — its own plus those
// of every org it belongs to — in enrollment order, tagged with the owner slug.
func (s *Store) AgentsVisibleTo(accountID string) ([]OwnedAgent, error) {
	rows, err := s.db.Query(
		`SELECT ag.base_domain, ag.name, acc.username
		   FROM agents ag JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.account_id = ?
		     OR ag.account_id IN (SELECT org_id FROM org_members WHERE account_id = ?)
		  ORDER BY ag.rowid`, accountID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []OwnedAgent
	for rows.Next() {
		var a OwnedAgent
		if err := rows.Scan(&a.BaseDomain, &a.Name, &a.Owner); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// ErrOrgHasAgents is returned when deleting an org that still owns agents —
// boxes must be re-homed or retired first, never orphaned.
var ErrOrgHasAgents = errors.New("org still owns agents")

// DeleteOrg removes an empty org: its memberships, pending invites, hostname
// rows, and the account row itself. Refused while the org owns agents.
func (s *Store) DeleteOrg(orgID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var otype string
	err = tx.QueryRow(`SELECT type FROM accounts WHERE id = ?`, orgID).Scan(&otype)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoOrg
	}
	if err != nil {
		return err
	}
	if otype != "org" {
		return ErrNoOrg
	}
	var agents int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id = ?`, orgID).Scan(&agents); err != nil {
		return err
	}
	if agents > 0 {
		return ErrOrgHasAgents
	}
	for _, stmt := range []string{
		`DELETE FROM org_invites WHERE org_id = ?`,
		`DELETE FROM org_members WHERE org_id = ?`,
		`DELETE FROM hostnames WHERE account_id = ?`,
		`DELETE FROM accounts WHERE id = ? AND type = 'org'`,
	} {
		if _, err := tx.Exec(stmt, orgID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

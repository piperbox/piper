package relay

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"
)

// stampInstance inserts a live relay_instances row for a fake relay whose
// four listener addrs are all addr, and returns the matching Instance. Tests
// that never dial pass "127.0.0.1:1".
func stampInstance(t *testing.T, st *Store, id, addr string, started time.Time) *Instance {
	t.Helper()
	inst := &Instance{ID: id, StartedAt: started.UTC(), TLSAddr: addr, HTTPAddr: addr, TunnelAddr: addr, APIAddr: addr}
	if err := st.UpsertInstance(inst.row(0)); err != nil {
		t.Fatal(err)
	}
	return inst
}

// testInstance stamps a uniquely named placeholder instance for tests that
// only need serveTunnel's ownership writes to have a parent row.
func testInstance(t *testing.T, st *Store) *Instance {
	t.Helper()
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	return stampInstance(t, st, "test-"+hex.EncodeToString(raw[:]), "127.0.0.1:1", time.Now())
}

// ageInstance pushes an instance's last_seen back so it reads as dead.
func ageInstance(t *testing.T, st *Store, id string) {
	t.Helper()
	if _, err := st.db.Exec(`UPDATE relay_instances SET last_seen = now() - interval '20 seconds' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func enrollTestAgent(t *testing.T, st *Store) Enrollment {
	t.Helper()
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return en
}

func TestLiveInstancesOrdersByStartAndSkipsStale(t *testing.T) {
	st := openTestStore(t)
	stampInstance(t, st, "b", "127.0.0.1:1", time.Now())
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now().Add(-time.Minute))
	stampInstance(t, st, "stale", "127.0.0.1:1", time.Now().Add(-time.Hour))
	ageInstance(t, st, "stale")

	rows, err := st.LiveInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != "a" || rows[1].ID != "b" {
		t.Fatalf("live = %+v, want [a b]", rows)
	}
}

func TestUpsertInstanceRefreshesSessionsAndLastSeen(t *testing.T) {
	st := openTestStore(t)
	inst := stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	ageInstance(t, st, "a")
	if rows, _ := st.LiveInstances(); len(rows) != 0 {
		t.Fatalf("aged row still live: %+v", rows)
	}
	if err := st.UpsertInstance(inst.row(3)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.LiveInstances()
	if len(rows) != 1 || rows[0].Sessions != 3 {
		t.Fatalf("after heartbeat: %+v, want one live row with 3 sessions", rows)
	}
}

func TestPurgeDeadInstancesDeletesOnlyStaleRows(t *testing.T) {
	st := openTestStore(t)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	stampInstance(t, st, "stale", "127.0.0.1:1", time.Now())
	ageInstance(t, st, "stale")
	if err := st.PurgeDeadInstances(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM relay_instances`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after purge = %d (%v), want 1", n, err)
	}
}

func TestSetOwnerOverwritesAndClearOwnerIsConditional(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	stampInstance(t, st, "b", "127.0.0.1:1", time.Now())

	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwner(en.BaseDomain, "b"); err != nil {
		t.Fatal(err)
	}
	// a's late unregister must not remove b's row.
	if err := st.ClearOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if r, ok, err := st.OwnerOf(en.BaseDomain); err != nil || !ok || r.ID != "b" {
		t.Fatalf("owner after conditional clear = %+v ok=%v err=%v, want b", r, ok, err)
	}
	if err := st.ClearOwner(en.BaseDomain, "b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.OwnerOf(en.BaseDomain); ok {
		t.Fatal("owner still set after the holder cleared it")
	}
}

func TestSetOwnerUnknownAgentIsBadToken(t *testing.T) {
	st := openTestStore(t)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner("nobody.public.getpiper.co", "a"); err != ErrBadToken {
		t.Fatalf("SetOwner unknown agent = %v, want ErrBadToken", err)
	}
}

func TestDeleteInstanceCascadesOwnership(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteInstance("a"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agent_owners`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("agent_owners after cascade = %d (%v), want 0", n, err)
	}
}

func TestOwnerOfIgnoresDeadOwner(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	ageInstance(t, st, "a")
	if _, ok, err := st.OwnerOf(en.BaseDomain); err != nil || ok {
		t.Fatalf("dead owner reported live (ok=%v err=%v)", ok, err)
	}
	owners, err := st.Owners()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := owners[en.BaseDomain]; ok {
		t.Fatalf("Owners() lists a dead owner: %v", owners)
	}
}

func TestOwnersMapsBaseDomainToLiveInstance(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	owners, err := st.Owners()
	if err != nil {
		t.Fatal(err)
	}
	if owners[en.BaseDomain] != "a" {
		t.Fatalf("Owners() = %v, want %s→a", owners, en.BaseDomain)
	}
}

func TestDeleteAgentDropsItsOwnerRow(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(en.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent with an owner row: %v", err)
	}
}

func TestAgentForHostPrecedence(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	base := en.BaseDomain
	host, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		host string
		want string
		ok   bool
	}{
		{"terminated hostname", host, base, true},
		{"custom exact", "shop.example.com", base, true},
		{"custom subdomain", "www.shop.example.com", base, true},
		{"base exact", base, base, true},
		{"base subdomain", "app." + base, base, true},
		{"unknown", "nobody.example.net", "", false},
		{"suffix without dot", "x" + base, "", false},
	}
	for _, c := range cases {
		got, ok, err := st.AgentForHost(c.host)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want || ok != c.ok {
			t.Errorf("%s: AgentForHost(%q) = %q,%v want %q,%v", c.name, c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestAgentForCustomHostMatchesCustomDomainsOnly(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	base := en.BaseDomain
	if err := st.AddCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := st.AgentForCustomHost("www.shop.example.com"); !ok || got != base {
		t.Fatalf("custom subdomain = %q,%v want %q,true", got, ok, base)
	}
	if _, ok, _ := st.AgentForCustomHost("app." + base); ok {
		t.Fatal("shared-domain host matched the :80 rule")
	}
}

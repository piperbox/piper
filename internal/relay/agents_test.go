package relay

import (
	"errors"
	"testing"
	"time"
)

// bindAndPark gives an agent one repo_bindings row and one pending_events row,
// so a deletion has child rows to cascade through.
func bindAndPark(t *testing.T, st *Store, agentName string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.db.Exec(
		`INSERT INTO repo_bindings(agent_name, app, repo, branch, created_at) VALUES(?,?,?,?,?)`,
		agentName, "blog", "alice/blog", "main", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at, attempts, next_try_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		agentName, "blog", "main", "push", []byte("{}"), now, 1, now); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteAgentClearsAgentAndItsChildRows(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := st.db.QueryRow(`SELECT name FROM agents WHERE base_domain=?`, en.BaseDomain).Scan(&name); err != nil {
		t.Fatal(err)
	}
	bindAndPark(t, st, name)

	if err := st.DeleteAgent(en.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM agents WHERE base_domain=?`, en.BaseDomain); n != 0 {
		t.Errorf("agents rows = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM repo_bindings WHERE agent_name=?`, name); n != 0 {
		t.Errorf("repo_bindings rows = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM pending_events WHERE agent_name=?`, name); n != 0 {
		t.Errorf("pending_events rows = %d, want 0", n)
	}
}

func TestDeleteAgentUnknownBaseDomain(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	if err := st.DeleteAgent("nope.public.getpiper.co"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("DeleteAgent(unknown) = %v, want ErrUnknownAgent", err)
	}
}

func TestDeleteAgentLeavesTheAccountsOtherAgentAlone(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var keeperName string
	if err := st.db.QueryRow(`SELECT name FROM agents WHERE base_domain=?`, keeper.BaseDomain).Scan(&keeperName); err != nil {
		t.Fatal(err)
	}
	bindAndPark(t, st, keeperName)

	if err := st.DeleteAgent(doomed.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM agents WHERE base_domain=?`, keeper.BaseDomain); n != 1 {
		t.Errorf("keeper agent rows = %d, want 1", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM repo_bindings WHERE agent_name=?`, keeperName); n != 1 {
		t.Errorf("keeper repo_bindings = %d, want 1", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM pending_events WHERE agent_name=?`, keeperName); n != 1 {
		t.Errorf("keeper pending_events = %d, want 1", n)
	}
}

// hostnames key on account_id with no agent column, so a removal cannot know
// which rows were this box's. Deleting the account's hostnames would destroy
// the URLs of its *other* boxes, so they are deliberately left behind: removal
// frees an agent slot, never an app slot. Pinned here so a future change cannot
// quietly turn this into cross-box data loss.
// hostnames names the agent directly since #405, so a removed box's rows go
// with it — releasing the app slots they held on the account.
func TestDeleteAgentReclaimsItsHostnames(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(en.BaseDomain, "blog", 0); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteAgent(en.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM hostnames WHERE account_id=?`, acc.ID); n != 0 {
		t.Errorf("hostnames rows = %d, want 0 (removal must reclaim app slots)", n)
	}
}

// custom_domains names the agent directly via agent_base, so unlike hostnames
// it must go with the box. ClaimDomain evicts only expired *pending* claims, so
// an active row left behind would answer ErrDomainTaken forever — to the domain's
// own owner, with no API able to clear it.
func TestDeleteAgentClearsItsCustomDomains(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain(doomed.BaseDomain, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain(keeper.BaseDomain, "blog.example.com"); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteAgent(doomed.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM custom_domains WHERE agent_base=?`, doomed.BaseDomain); n != 0 {
		t.Errorf("removed box left %d custom_domains rows, want 0", n)
	}
	// The surviving box keeps its domain.
	if n := countRows(t, st, `SELECT COUNT(*) FROM custom_domains WHERE agent_base=?`, keeper.BaseDomain); n != 1 {
		t.Errorf("keeper custom_domains = %d, want 1", n)
	}
	// And the freed domain can be claimed again — the squat is gone.
	if err := st.AddCustomDomain(keeper.BaseDomain, "shop.example.com"); err != nil {
		t.Errorf("re-claiming the removed box's domain: %v, want nil", err)
	}
}

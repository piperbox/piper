package relay

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/tunnel"
)

func newAccountAgent(t *testing.T) (*Store, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("gh-1", "alice")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatalf("EnrollForAccount: %v", err)
	}
	return st, en.BaseDomain
}

func TestRegisterHostnameIdempotentAndDerived(t *testing.T) {
	st, base := newAccountAgent(t)
	h1, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatalf("RegisterHostname: %v", err)
	}
	if !strings.HasSuffix(h1, "-alice.public.getpiper.co") {
		t.Fatalf("hostname = %q, want -alice.public.getpiper.co suffix", h1)
	}
	if strings.Count(strings.TrimSuffix(h1, ".public.getpiper.co"), ".") != 0 {
		t.Fatalf("hostname %q is not a single label under the apex", h1)
	}
	h2, err := st.RegisterHostname(base, "blog", 0)
	if err != nil || h2 != h1 {
		t.Fatalf("re-register = %q,%v want %q (idempotent)", h2, err, h1)
	}
	h3, err := st.RegisterHostname(base, "api", 0)
	if err != nil || h3 == h1 {
		t.Fatalf("distinct app got %q,%v (want a different hostname)", h3, err)
	}
}

// TestRegisterPreviewHostname covers PR previews on a relay-terminated box
// (#302): a preview needs its own hostname per (account, app, pr), and it has
// to stay a single label under the apex — the relay's wildcard matches exactly
// one label, so the agent's old "pr-<N>-<app>.<base-domain>" construction was
// both untrusted by TLS and unknown to the router.
func TestRegisterPreviewHostname(t *testing.T) {
	st, base := newAccountAgent(t)
	prod, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatalf("RegisterHostname: %v", err)
	}
	prev, err := st.RegisterHostname(base, "blog", 7)
	if err != nil {
		t.Fatalf("RegisterHostname preview: %v", err)
	}
	if prev == prod {
		t.Fatalf("preview reused the production hostname %q", prev)
	}
	if strings.Count(strings.TrimSuffix(prev, ".public.getpiper.co"), ".") != 0 {
		t.Fatalf("preview hostname %q is not a single label under the apex", prev)
	}
	if again, err := st.RegisterHostname(base, "blog", 7); err != nil || again != prev {
		t.Fatalf("re-register = %q,%v want %q (idempotent)", again, err, prev)
	}
	other, err := st.RegisterHostname(base, "blog", 8)
	if err != nil || other == prev {
		t.Fatalf("PR 8 got %q,%v want a hostname of its own", other, err)
	}
	// Production must survive a preview's deregistration, and vice versa.
	if err := st.DeregisterHostname(base, prev); err != nil {
		t.Fatalf("DeregisterHostname: %v", err)
	}
	if got, err := st.RegisterHostname(base, "blog", 0); err != nil || got != prod {
		t.Fatalf("production hostname after preview teardown = %q,%v want %q", got, err, prod)
	}
}

// TestPreviewHostnamesDoNotConsumeAppCap keeps open PRs from locking an account
// out of creating apps: the cap counts apps, and a preview is not one.
func TestPreviewHostnamesDoNotConsumeAppCap(t *testing.T) {
	st, base := newAccountAgent(t)
	st.Configure("public.getpiper.co", 3, 2, 5) // cap 2 apps
	if _, err := st.RegisterHostname(base, "a", 0); err != nil {
		t.Fatal(err)
	}
	for pr := 1; pr <= 4; pr++ {
		if _, err := st.RegisterHostname(base, "a", pr); err != nil {
			t.Fatalf("preview PR %d: %v", pr, err)
		}
	}
	if _, err := st.RegisterHostname(base, "b", 0); err != nil {
		t.Fatalf("second app after four previews: %v, want it to fit under the cap", err)
	}
}

func TestRegisterHostnameAppCap(t *testing.T) {
	st, base := newAccountAgent(t)
	st.Configure("public.getpiper.co", 3, 2, 5) // cap 2 apps
	if _, err := st.RegisterHostname(base, "a", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "b", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "c", 0); err != ErrQuotaExceeded {
		t.Fatalf("third app err = %v, want ErrQuotaExceeded", err)
	}
	// A re-register of an existing app must not be blocked by the cap.
	if _, err := st.RegisterHostname(base, "a", 0); err != nil {
		t.Fatalf("re-register under cap: %v", err)
	}
}

func TestRegisterHostnameDisabledAccount(t *testing.T) {
	st, base := newAccountAgent(t)
	if err := st.DisableAccount("alice", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "blog", 0); err != ErrBadCredential {
		t.Fatalf("disabled-account register err = %v, want ErrBadCredential", err)
	}
}

// TestAgentDisabledOutcomes pins the three-way read the watchdog's transient-vs-
// permanent split depends on, plus the LEFT-JOIN contract for account-less agents:
//   - known + enabled       -> (false, nil)
//   - known + disabled      -> (true, nil)
//   - unknown base           -> (false, ErrUnknownAccount)
//   - account-less           -> (false, nil)  (agent row exists, acc.disabled NULL)
func TestAgentDisabledOutcomes(t *testing.T) {
	st, base := newAccountAgent(t) // account-owned agent "alice"

	// known + enabled
	if off, err := st.AgentDisabled(base); off || err != nil {
		t.Fatalf("enabled agent: got (%v, %v), want (false, nil)", off, err)
	}

	// known + disabled
	if err := st.DisableAccount("alice", "user"); err != nil {
		t.Fatal(err)
	}
	if off, err := st.AgentDisabled(base); !off || err != nil {
		t.Fatalf("disabled agent: got (%v, %v), want (true, nil)", off, err)
	}

	// unknown base
	if off, err := st.AgentDisabled("nope.example.com"); off || !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("unknown base: got (%v, %v), want (false, ErrUnknownAccount)", off, err)
	}

	// account-less operator-enrolled agent (no account_id): the LEFT JOIN yields NULL
	// acc.disabled, which must read as not-disabled, not as unknown.
	if _, err := st.Enroll("legacy", "legacy.example.com"); err != nil {
		t.Fatal(err)
	}
	if off, err := st.AgentDisabled("legacy.example.com"); off || err != nil {
		t.Fatalf("legacy account-less agent: got (%v, %v), want (false, nil)", off, err)
	}
}

func TestDeregisterHostname(t *testing.T) {
	st, base := newAccountAgent(t)
	h, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeregisterHostname(base, h); err != nil {
		t.Fatalf("DeregisterHostname: %v", err)
	}
	// Now under cap again and re-registerable.
	if err := st.DeregisterHostname(base, h); err != nil {
		t.Fatalf("deregister missing row should be no-op: %v", err)
	}
}

// #405: two boxes on one account deploying the same app name derive the same
// hostname (appHostname hashes the account, not the agent), so the second box
// silently takes over the first's URL.
func TestRegisterHostnameIsPerAgent(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	boxA, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	boxB, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	hostA, err := st.RegisterHostname(boxA.BaseDomain, "blog", 0)
	if err != nil {
		t.Fatalf("box A register: %v", err)
	}
	hostB, err := st.RegisterHostname(boxB.BaseDomain, "blog", 0)
	if err != nil {
		t.Fatalf("box B register: %v", err)
	}
	if hostA == hostB {
		t.Fatalf("both boxes serve %q; box B took over box A's URL", hostA)
	}
}

// #405: removing a box must return its app slots to the account, or an account
// stays blocked from deploying by boxes that no longer exist.
func TestDeleteAgentReclaimsItsAppSlots(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 1, 5) // one app per account
	acc, _ := st.UpsertAccount("sub-1", "alice")
	boxA, _ := st.EnrollForAccount(acc.ID, "")
	boxB, _ := st.EnrollForAccount(acc.ID, "")
	if _, err := st.RegisterHostname(boxA.BaseDomain, "blog", 0); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteAgent(boxA.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	// A *different* app name: registering "blog" again would be answered by the
	// orphaned row's idempotency lookup, which hides the exhausted quota.
	if _, err := st.RegisterHostname(boxB.BaseDomain, "shop", 0); err != nil {
		t.Fatalf("register after removal: %v, want the freed slot to be reusable", err)
	}
}

// #418: the box is the authority on which apps exist, so a connect-time sync
// prunes hostname rows for apps it no longer has. Those rows are otherwise
// orphaned forever — deleting an app while the relay is unreachable skips
// deregistration entirely — and they keep consuming the account's app quota.
func TestReconcileHostnamesPrunesAppsTheBoxNoLongerHas(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	box, _ := st.EnrollForAccount(acc.ID, "")
	kept, err := st.RegisterHostname(box.BaseDomain, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(box.BaseDomain, "shop", 0); err != nil {
		t.Fatal(err)
	}

	live, _, err := st.ReconcileHostnames(box.BaseDomain, []tunnel.AppRef{{App: "blog"}})
	if err != nil {
		t.Fatalf("ReconcileHostnames: %v", err)
	}

	if len(live) != 1 || live[0].Hostname != kept {
		t.Fatalf("live = %v, want [%s]", live, kept)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM hostnames WHERE account_id=?`, acc.ID); n != 1 {
		t.Fatalf("rows = %d, want 1 (shop's row must be pruned)", n)
	}
}

// A relay DB wipe (routine under the no-migrations policy) leaves the box
// holding hostnames the relay has no rows for. Sync must recreate them, which
// is what the per-app re-push did implicitly via idempotent Register.
func TestReconcileHostnamesRecreatesMissingRows(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	box, _ := st.EnrollForAccount(acc.ID, "")

	live, _, err := st.ReconcileHostnames(box.BaseDomain, []tunnel.AppRef{{App: "blog"}})
	if err != nil {
		t.Fatalf("ReconcileHostnames: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live = %v, want one hostname created from nothing", live)
	}
}

// Pruning is safe to get wrong because appHostname is deterministic in
// (agent, app, pr): a row dropped in error comes back identical.
func TestReconcileHostnamesIsHostnameStableAcrossAPruneCycle(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	box, _ := st.EnrollForAccount(acc.ID, "")
	before, _ := st.RegisterHostname(box.BaseDomain, "blog", 0)

	if _, _, err := st.ReconcileHostnames(box.BaseDomain, nil); err != nil {
		t.Fatal(err)
	}
	after, _, err := st.ReconcileHostnames(box.BaseDomain, []tunnel.AppRef{{App: "blog"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Hostname != before {
		t.Fatalf("after = %v, want the original %q back", after, before)
	}
}

// Previews hold their own (app, pr) rows and must survive a sync that lists
// them; production and preview for one app are different slots.
func TestReconcileHostnamesKeepsPreviews(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	box, _ := st.EnrollForAccount(acc.ID, "")
	prod, _ := st.RegisterHostname(box.BaseDomain, "blog", 0)
	preview, _ := st.RegisterHostname(box.BaseDomain, "blog", 7)

	live, _, err := st.ReconcileHostnames(box.BaseDomain,
		[]tunnel.AppRef{{App: "blog"}, {App: "blog", PR: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("live = %v, want both %q and %q", live, prod, preview)
	}
}

// One box's sync must not touch another box's rows, even on the same account.
func TestReconcileHostnamesLeavesOtherBoxesAlone(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	boxA, _ := st.EnrollForAccount(acc.ID, "")
	boxB, _ := st.EnrollForAccount(acc.ID, "")
	if _, err := st.RegisterHostname(boxB.BaseDomain, "blog", 0); err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.ReconcileHostnames(boxA.BaseDomain, nil); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM hostnames WHERE account_id=?`, acc.ID); n != 1 {
		t.Fatalf("rows = %d, want box B's row untouched", n)
	}
}

// A slot the account cannot fit under its app cap is skipped, not fatal: one
// app must not strand the rest of the box's URLs, which is the resilience the
// per-app re-push used to provide by looping.
func TestReconcileHostnamesSkipsASlotItCannotFit(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 1, 5) // one app per account
	acc, _ := st.UpsertAccount("sub-1", "alice")
	box, _ := st.EnrollForAccount(acc.ID, "")

	live, _, err := st.ReconcileHostnames(box.BaseDomain,
		[]tunnel.AppRef{{App: "blog"}, {App: "shop"}})
	if err != nil {
		t.Fatalf("ReconcileHostnames: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live = %v, want the one slot that fits under the cap", live)
	}
}

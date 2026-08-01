package relay

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestEnrollAndAuthenticate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tok, err := st.Enroll("alice", "alice.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	ag, err := st.Authenticate(tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ag.Name != "alice" || ag.BaseDomain != "alice.example.com" {
		t.Fatalf("agent = %+v", ag)
	}
	if _, err := st.Authenticate("bogus"); err != ErrBadToken {
		t.Fatalf("bogus token err = %v; want ErrBadToken", err)
	}
}

func TestOpenSetsBusyTimeout(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var timeout int
	if err := st.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestEnrollRejectsDuplicateBaseDomain(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Enroll("alice", "shared.example.com"); err != nil {
		t.Fatalf("first Enroll: %v", err)
	}
	if _, err := st.Enroll("bob", "shared.example.com"); err == nil {
		t.Fatal("second Enroll succeeded for duplicate base domain")
	}
}

func TestControlTokenRoundTrip(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-ct", "ct")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	// Never provisioned: empty token, no error.
	if tok, err := st.ControlToken(en.BaseDomain); err != nil || tok != "" {
		t.Fatalf("fresh ControlToken = %q, %v (want \"\", nil)", tok, err)
	}
	if err := st.SetControlToken(en.BaseDomain, "tok-1"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := st.ControlToken(en.BaseDomain); tok != "tok-1" {
		t.Fatalf("ControlToken = %q, want tok-1", tok)
	}
	// A re-push overwrites (re-claim provisions a fresh token).
	if err := st.SetControlToken(en.BaseDomain, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := st.ControlToken(en.BaseDomain); tok != "tok-2" {
		t.Fatalf("ControlToken = %q, want tok-2", tok)
	}
	// Unknown agents fail closed in both directions.
	if err := st.SetControlToken("nope.example.com", "t"); err == nil {
		t.Fatal("SetControlToken(unknown agent) = nil, want error")
	}
	if _, err := st.ControlToken("nope.example.com"); err == nil {
		t.Fatal("ControlToken(unknown agent) = nil error, want error")
	}
}

// A modified agent must not be able to claim relay-managed names as its
// "custom domain": another agent's base domain (SNI hijack), the apex, a
// DNS-label parent/child of either, or its own base domain.
func TestAddCustomDomainRejectsRelayNamespace(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Configure("public.getpiper.co", 3, 10, 5)
	if _, err := st.Enroll("alice", "alice.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll("bob", "bob.example.com"); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{
		"alice.example.com",      // another agent's base domain
		"shop.alice.example.com", // subdomain of it
		"example.com",            // DNS-label parent of enrolled bases
		"bob.example.com",        // the requester's own base domain
		"public.getpiper.co",     // the relay apex
		"x.public.getpiper.co",   // subdomain of the apex (incl. api.<apex>)
		"getpiper.co",            // parent of the apex
	} {
		if err := st.AddCustomDomain("bob.example.com", d); !errors.Is(err, ErrDomainReserved) {
			t.Errorf("AddCustomDomain(%q) err = %v, want ErrDomainReserved", d, err)
		}
	}
	for _, d := range []string{
		"Not.A.Domain", // uppercase
		"no-dots",
		"-bad.example.dev",
		"shop..dev",
	} {
		if err := st.AddCustomDomain("bob.example.com", d); !errors.Is(err, ErrInvalidDomain) {
			t.Errorf("AddCustomDomain(%q) err = %v, want ErrInvalidDomain", d, err)
		}
	}
	// Nothing above may have stuck.
	if got, _ := st.CustomDomains("bob.example.com"); len(got) != 0 {
		t.Fatalf("custom domains = %v after rejected claims, want none", got)
	}
	// A legitimate unrelated domain still works.
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("legit domain rejected: %v", err)
	}
	// "Suffix" means DNS labels, not raw strings: xalice.example.comx shares a
	// raw suffix with alice.example.com but is a different DNS name.
	if err := st.AddCustomDomain("bob.example.com", "xalice.example.comx"); err != nil {
		t.Fatalf("raw-suffix lookalike rejected: %v", err)
	}
}

func TestOpenCreatesOrgTables(t *testing.T) {
	st := openTestStore(t)
	for _, table := range []string{"org_members", "org_invites"} {
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

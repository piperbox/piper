package relay

import (
	"errors"
	"testing"
	"time"
)

func openDomainsStore(t *testing.T) *Store {
	t.Helper()
	st := openTestStore(t)
	if _, err := st.Enroll("alice", "alice.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll("bob", "bob.example.com"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAddCustomDomainClaimAndList(t *testing.T) {
	st := openDomainsStore(t)
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddCustomDomain("alice.example.com", "blog.dev"); err != nil {
		t.Fatalf("second add: %v", err)
	}
	got, err := st.CustomDomains("alice.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "blog.dev" || got[1] != "shop.dev" {
		t.Fatalf("CustomDomains = %v, want [blog.dev shop.dev]", got)
	}
	// FCFS: a live pending claim blocks a rival.
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival claim: err = %v, want ErrDomainTaken", err)
	}
	// Validation and identity errors are enforced on every claim.
	if err := st.AddCustomDomain("alice.example.com", "Bad_Domain"); !errors.Is(err, ErrInvalidDomain) {
		t.Fatalf("malformed: %v", err)
	}
	if err := st.AddCustomDomain("alice.example.com", "bob.example.com"); !errors.Is(err, ErrDomainReserved) {
		t.Fatalf("relay namespace: %v", err)
	}
	if err := st.AddCustomDomain("nobody.example.com", "x.dev"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("unknown agent: %v", err)
	}
}

func TestAddCustomDomainRejectsDisabledAccount(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain(en.BaseDomain, "shop.dev"); err != nil {
		t.Fatalf("healthy account rejected: %v", err)
	}
	if err := st.DisableAccount(acc.Username, "user"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain(en.BaseDomain, "other.dev"); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("disabled account: err = %v, want ErrBadCredential", err)
	}
	if got, err := st.CustomDomains(en.BaseDomain); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 || got[0] != "shop.dev" {
		t.Fatalf("domains after rejected claim = %v, want [shop.dev]", got)
	}
}

// An expired pending claim is squat, not ownership: a rival claim evicts it.
// Re-adding your own pending domain refreshes the window.
func TestAddCustomDomainExpiryAndEviction(t *testing.T) {
	st := openDomainsStore(t)
	now := time.Now()
	st.nowFunc = func() time.Time { return now }

	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatal(err)
	}
	// Not yet expired: rival still blocked at TTL-1s.
	now = now.Add(pendingTTL - time.Second)
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival before expiry: %v", err)
	}
	// Refresh resets alice's window.
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	now = now.Add(pendingTTL - time.Second)
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival after refresh: %v", err)
	}
	// Past the (refreshed) TTL the claim is evictable.
	now = now.Add(2 * time.Second)
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("evicting expired squat: %v", err)
	}
	if got, _ := st.CustomDomains("bob.example.com"); len(got) != 1 || got[0] != "shop.dev" {
		t.Fatalf("bob's domains = %v", got)
	}
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 0 {
		t.Fatalf("alice still lists %v after eviction", got)
	}
	// Expired pending rows are filtered from the list (reconnect re-derive).
	now = now.Add(pendingTTL + time.Second)
	if got, _ := st.CustomDomains("bob.example.com"); len(got) != 0 {
		t.Fatalf("expired pending still listed: %v", got)
	}
}

func TestAddCustomDomainCap(t *testing.T) {
	st := openDomainsStore(t)
	st.Configure("public.getpiper.co", 3, 10, 2)
	for i, d := range []string{"one.dev", "two.dev"} {
		if err := st.AddCustomDomain("alice.example.com", d); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := st.AddCustomDomain("alice.example.com", "three.dev"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over cap: err = %v, want ErrQuotaExceeded", err)
	}
	// The cap is per agent, not global.
	if err := st.AddCustomDomain("bob.example.com", "three.dev"); err != nil {
		t.Fatalf("bob under cap: %v", err)
	}
	// Re-adding an existing domain is not a new claim and must not hit the cap.
	if err := st.AddCustomDomain("alice.example.com", "one.dev"); err != nil {
		t.Fatalf("re-add at cap: %v", err)
	}
}

func TestConfirmCustomDomain(t *testing.T) {
	st := openDomainsStore(t)
	now := time.Now()
	st.nowFunc = func() time.Time { return now }
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatal(err)
	}
	// Only the holder may confirm.
	if err := st.ConfirmCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("rival confirm: err = %v, want ErrDomainNotFound", err)
	}
	if err := st.ConfirmCustomDomain("alice.example.com", "nope.dev"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("missing row: err = %v, want ErrDomainNotFound", err)
	}
	// Confirm ignores pending age: eviction is the only claim-killer, so a
	// slow issuance (>TTL) still confirms if nobody contested the name.
	now = now.Add(pendingTTL + time.Minute)
	if err := st.ConfirmCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("confirm after TTL: %v", err)
	}
	// Active rows never expire from the list.
	now = now.Add(24 * time.Hour)
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 1 || got[0] != "shop.dev" {
		t.Fatalf("active domain listed = %v", got)
	}
	// Idempotent on active.
	if err := st.ConfirmCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	// An active row blocks rivals forever.
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival vs active: %v", err)
	}
}

func TestRemoveCustomDomain(t *testing.T) {
	st := openDomainsStore(t)
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatal(err)
	}
	// Another agent's remove must not touch the row.
	if err := st.RemoveCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("rival remove errored: %v", err)
	}
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 1 {
		t.Fatalf("rival remove deleted the row: %v", got)
	}
	if err := st.RemoveCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 0 {
		t.Fatalf("still listed after remove: %v", got)
	}
	// Idempotent: removing again is a no-op, and the name is free for others.
	if err := st.RemoveCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("re-remove: %v", err)
	}
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("claim after remove: %v", err)
	}
}

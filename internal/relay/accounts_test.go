package relay

import (
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUpsertAccountIsIdempotentByGitHubID(t *testing.T) {
	st := openTestStore(t)

	a1, err := st.UpsertAccount("583231", "Alice-Smith")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if a1.Username != "alice-smith" {
		t.Fatalf("username = %q, want alice-smith", a1.Username)
	}
	if a1.ID == "" {
		t.Fatal("empty account id")
	}

	a2, err := st.UpsertAccount("583231", "Alice-Smith")
	if err != nil {
		t.Fatalf("second UpsertAccount: %v", err)
	}
	if a2.ID != a1.ID {
		t.Fatalf("re-upsert made a new account: %s != %s", a2.ID, a1.ID)
	}
}

func TestUpsertAccountDisambiguatesUsername(t *testing.T) {
	st := openTestStore(t)
	// Two different GitHub accounts can collide on the derived username
	// (e.g. after a rename freed the login for someone else).
	a1, _ := st.UpsertAccount("gh-a", "bob")
	a2, _ := st.UpsertAccount("gh-b", "bob")
	if a1.Username != "bob" {
		t.Fatalf("first username = %q, want bob", a1.Username)
	}
	if a2.Username != "bob-2" {
		t.Fatalf("second username = %q, want bob-2", a2.Username)
	}
}

func TestUpsertAccountCapsLongLogin(t *testing.T) {
	st := openTestStore(t)
	// GitHub logins go up to 39 chars; usernames cap at 30 to keep the
	// eventual "<hash>-<username>.<apex>" DNS label under 63 chars.
	long := "a-very-long-github-login-name-indeed-x" // 39 chars
	acc, err := st.UpsertAccount("gh-long", long)
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if len(acc.Username) > 30 {
		t.Fatalf("username %q is %d chars, want <= 30", acc.Username, len(acc.Username))
	}
}

func TestMintAndAuthenticateCredential(t *testing.T) {
	st := openTestStore(t)
	acc, _ := st.UpsertAccount("sub-1", "carol")

	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatalf("MintAccountCredential: %v", err)
	}
	if cred == "" {
		t.Fatal("empty credential")
	}

	got, err := st.AuthenticateAccount(cred)
	if err != nil {
		t.Fatalf("AuthenticateAccount: %v", err)
	}
	if got.ID != acc.ID || got.Username != acc.Username {
		t.Fatalf("account = %+v, want %+v", got, acc)
	}

	if _, err := st.AuthenticateAccount("nope"); err != ErrBadCredential {
		t.Fatalf("bad cred err = %v, want ErrBadCredential", err)
	}
}

func TestDisabledAccountCredentialRejected(t *testing.T) {
	st := openTestStore(t)
	acc, _ := st.UpsertAccount("sub-1", "dave")
	cred, _ := st.MintAccountCredential(acc.ID)

	if err := st.DisableAccount(acc.Username, "user"); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if _, err := st.AuthenticateAccount(cred); err != ErrBadCredential {
		t.Fatalf("disabled cred err = %v, want ErrBadCredential", err)
	}
}

// A user and an org may now hold the same slug (#411), so the kill-switch has
// to name which one it means — disabling "acme" the user must not sever "acme"
// the org, nor the reverse.
func TestDisableAccountLeavesTheSameNamedOrgAlone(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	user, _ := st.UpsertAccount("gh-acme", "acme")
	org, _ := st.CreateOrg(alice.ID, "acme")
	orgAgent, err := st.EnrollForAccount(org.ID)
	if err != nil {
		t.Fatalf("org enroll: %v", err)
	}

	if err := st.DisableAccount(user.Username, "user"); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}

	disabled, err := st.AgentDisabled(orgAgent.BaseDomain)
	if err != nil {
		t.Fatalf("AgentDisabled: %v", err)
	}
	if disabled {
		t.Fatal("disabling user acme also disabled org acme")
	}
}

func TestDisableAccountLeavesTheSameNamedUserAlone(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	user, _ := st.UpsertAccount("gh-acme", "acme")
	cred, _ := st.MintAccountCredential(user.ID)
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if err := st.DisableAccount("acme", "org"); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}

	if _, err := st.AuthenticateAccount(cred); err != nil {
		t.Fatalf("user acme rejected after disabling org acme: %v", err)
	}
}

func TestEnrollForAccountRejectsUnknownAccount(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)

	if _, err := st.EnrollForAccount("no-such-account"); err != ErrUnknownAccount {
		t.Fatalf("unknown account err = %v, want ErrUnknownAccount", err)
	}
}

func TestEnrollForAccountAssignsLabelAndBindsAccount(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "erin")

	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatalf("EnrollForAccount: %v", err)
	}
	if en.Token == "" {
		t.Fatal("empty enrollment token")
	}
	if !strings.HasSuffix(en.BaseDomain, "-erin.public.getpiper.co") {
		t.Fatalf("base domain = %q, want <hash>-erin.public.getpiper.co", en.BaseDomain)
	}
	// The enrollment token authenticates as an agent bound to this base domain.
	ag, err := st.Authenticate(en.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ag.BaseDomain != en.BaseDomain {
		t.Fatalf("agent base = %q, want %q", ag.BaseDomain, en.BaseDomain)
	}
}

func TestEnrollForAccountEnforcesCap(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 2, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "frank")

	for i := 0; i < 2; i++ {
		if _, err := st.EnrollForAccount(acc.ID); err != nil {
			t.Fatalf("enroll %d: %v", i, err)
		}
	}
	if _, err := st.EnrollForAccount(acc.ID); err != ErrQuotaExceeded {
		t.Fatalf("over-cap err = %v, want ErrQuotaExceeded", err)
	}
}

func TestAuthenticateRejectsDisabledAccountAgent(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "grace")
	en, _ := st.EnrollForAccount(acc.ID)

	if err := st.DisableAccount(acc.Username, "user"); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if _, err := st.Authenticate(en.Token); err != ErrBadToken {
		t.Fatalf("disabled agent auth err = %v, want ErrBadToken", err)
	}
}

func TestUpsertAccountStoresAndRefreshesGithubLogin(t *testing.T) {
	st := openTestStore(t)

	a1, err := st.UpsertAccount("gh-1", "Alice-Smith")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if a1.GithubLogin != "Alice-Smith" {
		t.Fatalf("GithubLogin = %q, want Alice-Smith", a1.GithubLogin)
	}

	// GitHub login renamed: re-login refreshes the stored login.
	a2, err := st.UpsertAccount("gh-1", "alice-renamed")
	if err != nil {
		t.Fatalf("re-login UpsertAccount: %v", err)
	}
	if a2.GithubLogin != "alice-renamed" {
		t.Fatalf("refreshed GithubLogin = %q, want alice-renamed", a2.GithubLogin)
	}
	var stored string
	if err := st.db.QueryRow(`SELECT github_login FROM accounts WHERE github_id='gh-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "alice-renamed" {
		t.Fatalf("stored github_login = %q, want alice-renamed", stored)
	}

	// AuthenticateAccount surfaces the login too (needed for invite matching).
	cred, _ := st.MintAccountCredential(a1.ID)
	got, err := st.AuthenticateAccount(cred)
	if err != nil {
		t.Fatal(err)
	}
	if got.GithubLogin != "alice-renamed" {
		t.Fatalf("authenticated GithubLogin = %q, want alice-renamed", got.GithubLogin)
	}
}

package relay

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreateOrgMakesCreatorSoleOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")

	org, err := st.CreateOrg(alice.ID, "Acme Robotics")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Slug != "acme-robotics" {
		t.Fatalf("slug = %q, want acme-robotics", org.Slug)
	}
	if org.ID == "" || org.ID == alice.ID {
		t.Fatalf("org id = %q, want a fresh account id", org.ID)
	}

	orgs, err := st.OrgsForAccount(alice.ID)
	if err != nil {
		t.Fatalf("OrgsForAccount: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Slug != "acme-robotics" || orgs[0].Role != "owner" {
		t.Fatalf("orgs = %+v, want [acme-robotics owner]", orgs)
	}
}

func TestCreateOrgDoesNotTakeAUserLogin(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	// A user already holds "bob". Orgs live in their own namespace, so the
	// org gets "bob" too — neither displaces the other (#411).
	st.UpsertAccount("gh-bob", "bob")

	org, err := st.CreateOrg(alice.ID, "Bob")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Slug != "bob" {
		t.Fatalf("slug = %q, want bob", org.Slug)
	}
}

func TestCreateOrgRejectsATakenName(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	if _, err := st.CreateOrg(alice.ID, "Acme"); err != nil {
		t.Fatalf("first CreateOrg: %v", err)
	}

	// A name someone typed fails visibly rather than becoming "acme-2" (#412).
	org, err := st.CreateOrg(alice.ID, "Acme")
	if !errors.Is(err, ErrOrgNameTaken) {
		t.Fatalf("second CreateOrg = (%q, %v), want ErrOrgNameTaken", org.Slug, err)
	}
}

func TestUpsertAccountKeepsLoginHeldByAnOrg(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	// An org squats "bob" before the GitHub user of that name ever signs in.
	if _, err := st.CreateOrg(alice.ID, "bob"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	bob, err := st.UpsertAccount("gh-bob", "bob")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if bob.Username != "bob" {
		t.Fatalf("username = %q, want bob — an org must not displace a GitHub login", bob.Username)
	}
}

func TestSameNamedUserAndOrgGetDistinctHostnames(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 5, 10, 5)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	acme, _ := st.UpsertAccount("gh-acme", "acme")
	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Slug != acme.Username {
		t.Fatalf("org slug %q != user slug %q; this test needs them equal", org.Slug, acme.Username)
	}

	userAgent, err := st.EnrollForAccount(acme.ID, "")
	if err != nil {
		t.Fatalf("user enroll: %v", err)
	}
	orgAgent, err := st.EnrollForAccount(org.ID, "")
	if err != nil {
		t.Fatalf("org enroll: %v", err)
	}
	if userAgent.BaseDomain == orgAgent.BaseDomain {
		t.Fatalf("user and org share base domain %q", userAgent.BaseDomain)
	}

	// Both hold the slug "acme"; the per-account hash keeps their app
	// hostnames apart, which is why the slug need not be globally unique.
	userHost, err := st.RegisterHostname(userAgent.BaseDomain, "blog", 0)
	if err != nil {
		t.Fatalf("user RegisterHostname: %v", err)
	}
	orgHost, err := st.RegisterHostname(orgAgent.BaseDomain, "blog", 0)
	if err != nil {
		t.Fatalf("org RegisterHostname: %v", err)
	}
	if userHost == orgHost {
		t.Fatalf("user and org share hostname %q", userHost)
	}
}

func TestOrgRoleHidesOrgFromNonMembers(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	mallory, _ := st.UpsertAccount("gh-mallory", "mallory")
	org, _ := st.CreateOrg(alice.ID, "acme")

	orgID, role, err := st.OrgRole(org.Slug, alice.ID)
	if err != nil || orgID != org.ID || role != "owner" {
		t.Fatalf("member OrgRole = (%q,%q,%v), want (%q, owner, nil)", orgID, role, err, org.ID)
	}
	// Non-member, nonexistent org, and a *user* slug are indistinguishable.
	if _, _, err := st.OrgRole(org.Slug, mallory.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("non-member err = %v, want ErrNoOrg", err)
	}
	if _, _, err := st.OrgRole("nope", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("nonexistent err = %v, want ErrNoOrg", err)
	}
	if _, _, err := st.OrgRole("mallory", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("user-slug err = %v, want ErrNoOrg", err)
	}
}

func TestOrgStaysInertAsPrincipal(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// An org can never hold a credential.
	if _, err := st.MintAccountCredential(org.ID); err == nil {
		t.Fatal("MintAccountCredential(org) succeeded, want error")
	}
	// An org cannot create an org.
	if _, err := st.CreateOrg(org.ID, "suborg"); err == nil {
		t.Fatal("CreateOrg(by org) succeeded, want error")
	}
}

func TestOrgAgentQuotaIsIndependent(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 2, 10, 5)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Alice fills her personal cap.
	for i := 0; i < 2; i++ {
		if _, err := st.EnrollForAccount(alice.ID, ""); err != nil {
			t.Fatalf("personal enroll %d: %v", i, err)
		}
	}
	if _, err := st.EnrollForAccount(alice.ID, ""); err != ErrQuotaExceeded {
		t.Fatalf("over personal cap err = %v, want ErrQuotaExceeded", err)
	}
	// The org's cap is its own; its base domain carries the org slug.
	en, err := st.EnrollForAccount(org.ID, "")
	if err != nil {
		t.Fatalf("org enroll: %v", err)
	}
	if want := "-acme.public.getpiper.co"; !strings.HasSuffix(en.BaseDomain, want) {
		t.Fatalf("org base domain = %q, want suffix %q", en.BaseDomain, want)
	}
}

// addMember inserts a membership row directly; org/membership tests must not
// depend on the invite flow.
func addMember(t *testing.T, st *Store, orgID, accountID, role string) {
	t.Helper()
	if _, err := st.db.Exec(
		`INSERT INTO org_members(org_id, account_id, role, created_at)
		 VALUES($1,$2,$3,'2026-01-01T00:00:00Z')`, orgID, accountID, role); err != nil {
		t.Fatal(err)
	}
}

func TestMembersListsUsernamesAndRoles(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	members, err := st.Members(org.ID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 ||
		members[0] != (Member{Username: "alice", Role: "owner"}) ||
		members[1] != (Member{Username: "bob", Role: "member"}) {
		t.Fatalf("members = %+v, want [alice/owner bob/member]", members)
	}
}

func TestSetMemberRolePromotesAndGuardsLastOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	// Sole owner cannot demote themselves.
	if err := st.SetMemberRole(org.ID, "alice", "member"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote sole owner err = %v, want ErrLastOwner", err)
	}
	// Promote bob, then alice may step down.
	if err := st.SetMemberRole(org.ID, "bob", "owner"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := st.SetMemberRole(org.ID, "alice", "member"); err != nil {
		t.Fatalf("demote after promote: %v", err)
	}
	// Unknown target.
	if err := st.SetMemberRole(org.ID, "nobody", "member"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("unknown member err = %v, want ErrNotMember", err)
	}
}

func TestRemoveMemberGuardsLastOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	if err := st.RemoveMember(org.ID, "alice"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("remove sole owner err = %v, want ErrLastOwner", err)
	}
	if err := st.RemoveMember(org.ID, "bob"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if err := st.RemoveMember(org.ID, "bob"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("re-remove err = %v, want ErrNotMember", err)
	}
	// Removal is real: bob no longer lists the org.
	orgs, _ := st.OrgsForAccount(bob.ID)
	if len(orgs) != 0 {
		t.Fatalf("bob still in orgs: %+v", orgs)
	}
}

func TestInviteLifecycle(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "Bob-Builder")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Invite by GitHub username, any case; duplicate is idempotent.
	if err := st.CreateInvite(org.ID, "BOB-builder", alice.ID); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := st.CreateInvite(org.ID, "bob-builder", alice.ID); err != nil {
		t.Fatalf("duplicate invite: %v, want nil (idempotent)", err)
	}
	pending, err := st.OrgInvites(org.ID)
	if err != nil || len(pending) != 1 || pending[0] != "bob-builder" {
		t.Fatalf("OrgInvites = %v (%v), want [bob-builder]", pending, err)
	}
	mine, err := st.InvitesForAccount(bob.ID)
	if err != nil || len(mine) != 1 || mine[0] != "acme" {
		t.Fatalf("InvitesForAccount = %v (%v), want [acme]", mine, err)
	}

	// Accept: membership as member, invite consumed.
	if err := st.AcceptInvite(bob.ID, "acme"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	orgs, _ := st.OrgsForAccount(bob.ID)
	if len(orgs) != 1 || orgs[0].Role != "member" {
		t.Fatalf("bob's orgs = %+v, want [acme member]", orgs)
	}
	if pending, _ := st.OrgInvites(org.ID); len(pending) != 0 {
		t.Fatalf("invite not consumed: %v", pending)
	}
	if err := st.AcceptInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-accept err = %v, want ErrNoInvite", err)
	}

	// Inviting an existing member is refused.
	if err := st.CreateInvite(org.ID, "Bob-Builder", alice.ID); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("invite member err = %v, want ErrAlreadyMember", err)
	}
}

func TestInviteBeforeFirstLogin(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Invited before ever logging into the relay.
	if err := st.CreateInvite(org.ID, "Newbie", alice.ID); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	newbie, _ := st.UpsertAccount("gh-newbie", "Newbie")
	mine, err := st.InvitesForAccount(newbie.ID)
	if err != nil || len(mine) != 1 || mine[0] != "acme" {
		t.Fatalf("InvitesForAccount = %v (%v), want [acme]", mine, err)
	}
	if err := st.AcceptInvite(newbie.ID, "acme"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
}

func TestDeclineAndRevokeInvite(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")

	st.CreateInvite(org.ID, "bob", alice.ID)
	if err := st.DeclineInvite(bob.ID, "acme"); err != nil {
		t.Fatalf("DeclineInvite: %v", err)
	}
	if orgs, _ := st.OrgsForAccount(bob.ID); len(orgs) != 0 {
		t.Fatalf("decline created membership: %+v", orgs)
	}
	if err := st.DeclineInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-decline err = %v, want ErrNoInvite", err)
	}

	st.CreateInvite(org.ID, "bob", alice.ID)
	if err := st.RevokeInvite(org.ID, "BOB"); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if err := st.RevokeInvite(org.ID, "bob"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-revoke err = %v, want ErrNoInvite", err)
	}
	// A consumed/revoked invite no longer accepts.
	if err := st.AcceptInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("accept revoked err = %v, want ErrNoInvite", err)
	}
	// Accepting a nonexistent org is the same error (no existence leak).
	if err := st.AcceptInvite(bob.ID, "nope"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("accept unknown org err = %v, want ErrNoInvite", err)
	}
}

func TestCanControlOwnerAndOrgMember(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	mallory, _ := st.UpsertAccount("gh-mallory", "mallory")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	cases := []struct {
		name          string
		caller, owner string
		want          bool
	}{
		{"self", alice.ID, alice.ID, true},
		{"org owner", alice.ID, org.ID, true},
		{"org member", bob.ID, org.ID, true},
		{"non-member", mallory.ID, org.ID, false},
		{"other user's box", mallory.ID, alice.ID, false},
	}
	for _, c := range cases {
		got, err := st.CanControl(c.caller, c.owner)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("CanControl(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAgentsVisibleToMergesPersonalAndOrg(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	personal, _ := st.EnrollForAccount(bob.ID, "")
	orgEn, _ := st.EnrollForAccount(org.ID, "")
	st.EnrollForAccount(alice.ID, "") // alice's personal box: invisible to bob

	got, err := st.AgentsVisibleTo(bob.ID)
	if err != nil {
		t.Fatalf("AgentsVisibleTo: %v", err)
	}
	want := []OwnedAgent{
		{BaseDomain: personal.BaseDomain, Name: personal.BaseDomain, Owner: "bob"},
		{BaseDomain: orgEn.BaseDomain, Name: orgEn.BaseDomain, Owner: "acme"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("AgentsVisibleTo = %+v, want %+v", got, want)
	}

	// An account with nothing visible lists empty, not an error.
	carol, _ := st.UpsertAccount("gh-carol", "carol")
	if got, err := st.AgentsVisibleTo(carol.ID); err != nil || len(got) != 0 {
		t.Fatalf("empty AgentsVisibleTo = %+v (%v), want none", got, err)
	}
}

func TestDeleteOrgRefusedWhileAgentsExist(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")
	st.CreateInvite(org.ID, "someone", alice.ID)

	if _, err := st.EnrollForAccount(org.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOrg(org.ID); !errors.Is(err, ErrOrgHasAgents) {
		t.Fatalf("delete with agents err = %v, want ErrOrgHasAgents", err)
	}

	// Clear the agent, then deletion sweeps members, invites, and the slug.
	if _, err := st.db.Exec(`DELETE FROM agents WHERE account_id=$1`, org.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOrg(org.ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if orgs, _ := st.OrgsForAccount(alice.ID); len(orgs) != 0 {
		t.Fatalf("membership survived delete: %+v", orgs)
	}
	if _, _, err := st.OrgRole("acme", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("org survived delete: %v", err)
	}
	// The slug is free again.
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatalf("slug not freed: %v", err)
	}
}

func TestDeleteOrgRefusesNonOrgAccounts(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	if _, err := st.Enroll("alice-box", "alice-box.example.com"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.db.Exec(
		`INSERT INTO hostnames(hostname, agent_name, account_id, app, created_at) VALUES($1,$2,$3,$4,$5)`,
		"alice-app.piper.localhost", "alice-box", alice.ID, "app",
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteOrg(alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("DeleteOrg(user account id) err = %v, want ErrNoOrg", err)
	}
	if err := st.DeleteOrg("nope"); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("DeleteOrg(bogus id) err = %v, want ErrNoOrg", err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM hostnames WHERE account_id=$1`, alice.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("hostnames row survived refused delete = %d, want 1", n)
	}
}

// An org-target App installation is linked to the org account
// (ingress routes it through OrgForGitHubInstall). Postgres enforces the
// github_installations.account_id foreign key, so DeleteOrg must remove
// those rows or the account delete fails.
func TestDeleteOrgRemovesItsInstallations(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallationForAccount("inst-9", org.ID, "org", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOrg(org.ID); err != nil {
		t.Fatalf("DeleteOrg with an installation: %v", err)
	}
	if _, err := st.AccountForInstallation("inst-9"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("installation survived org delete: %v", err)
	}
}

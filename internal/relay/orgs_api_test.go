package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// orgAPIFixture: an API over a store with two logged-in users.
func orgAPIFixture(t *testing.T) (api http.Handler, st *Store, aliceCred, bobCred string) {
	t.Helper()
	st = openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	alice, err := st.UpsertAccount("sub-alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, _ = st.MintAccountCredential(alice.ID)
	bob, err := st.UpsertAccount("sub-bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	bobCred, _ = st.MintAccountCredential(bob.ID)
	api = NewAPI(st, NewFakeVerifier())
	return
}

// apiReq performs one JSON request against the API.
func apiReq(t *testing.T, api http.Handler, method, path, cred, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestOrgCreateConflictsOnATakenName(t *testing.T) {
	api, _, aliceCred, bobCred := orgAPIFixture(t)
	if rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{"name":"acme"}`); rr.Code != http.StatusOK {
		t.Fatalf("create org: %d", rr.Code)
	}

	// Bob asks for the same name: a visible conflict, not a silent "acme-2".
	rr := apiReq(t, api, "POST", "/v1/orgs", bobCred, `{"name":"Acme"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate create: %d, want 409", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "acme") {
		t.Fatalf("body %q does not name the taken slug", rr.Body.String())
	}
}

func TestOrgCreateAllowsAUsersName(t *testing.T) {
	api, _, aliceCred, _ := orgAPIFixture(t)
	// "bob" is a user in the fixture; orgs hold their own namespace (#411), so
	// the 409 path must not fire here.
	rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{"name":"bob"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create org named after a user: %d, want 200", rr.Code)
	}
	var got struct{ Org string }
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Org != "bob" {
		t.Fatalf("org = %q, want bob", got.Org)
	}
}

func TestOrgSetGitHub(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	if rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{"name":"acme"}`); rr.Code != http.StatusOK {
		t.Fatalf("create org: %d", rr.Code)
	}

	// A non-member cannot see the org, let alone set its GitHub link.
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/github", bobCred, `{"github_login":"acme"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member: %d, want 404", rr.Code)
	}
	// Missing bearer is unauthorized.
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/github", "", `{"github_login":"acme"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}

	// The owner links the GitHub org.
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/github", aliceCred, `{"github_login":"Acme-Inc"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("owner set: %d (body %s)", rr.Code, rr.Body.String())
	}

	// The link now resolves an install for the GitHub org, verified through
	// alice's membership.
	if _, err := st.OrgForGitHubInstall("5000", "acme-inc", "sub-alice"); err != nil {
		t.Fatalf("OrgForGitHubInstall after set: %v", err)
	}
}

func TestOrgCreateAndList(t *testing.T) {
	api, _, aliceCred, bobCred := orgAPIFixture(t)

	// Auth gates.
	if rr := apiReq(t, api, "POST", "/v1/orgs", "", `{"name":"acme"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty name: %d, want 400", rr.Code)
	}

	rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{"name":"Acme Robotics"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d (body %s)", rr.Code, rr.Body.String())
	}
	var created struct {
		Org  string `json:"org"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Org != "acme-robotics" || created.Role != "owner" {
		t.Fatalf("created = %+v, want acme-robotics/owner", created)
	}

	// Creator lists it; a stranger's list stays empty (and non-null).
	rr = apiReq(t, api, "GET", "/v1/orgs", aliceCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var list struct {
		Orgs []struct {
			Org  string `json:"org"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Orgs) != 1 || list.Orgs[0].Org != "acme-robotics" || list.Orgs[0].Role != "owner" {
		t.Fatalf("list = %+v, want [acme-robotics owner]", list.Orgs)
	}

	rr = apiReq(t, api, "GET", "/v1/orgs", bobCred, "")
	list.Orgs = nil
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Orgs == nil || len(list.Orgs) != 0 {
		t.Fatalf("bob's list = %+v, want empty non-null array", list.Orgs)
	}
}

// orgWithMember: alice owns "acme", bob is a plain member.
func orgWithMember(t *testing.T, st *Store, aliceCred, bobCred string) (orgID string) {
	t.Helper()
	alice, err := st.AuthenticateAccount(aliceCred)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.AuthenticateAccount(bobCred)
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	addMember(t, st, org.ID, bob.ID, "member")
	return org.ID
}

func TestOrgMembersEndpointRoleMatrix(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)
	mallory, _ := st.UpsertAccount("sub-mallory", "mallory")
	malloryCred, _ := st.MintAccountCredential(mallory.ID)

	// Any member reads the list; a non-member gets 404, not 403 (no leak).
	rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("member list: %d", rr.Code)
	}
	var list struct {
		Members []struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"members"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Members) != 2 || list.Members[0].Username != "alice" || list.Members[0].Role != "owner" ||
		list.Members[1].Username != "bob" || list.Members[1].Role != "member" {
		t.Fatalf("members = %+v", list.Members)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", malloryCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member list: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/ghost/members", aliceCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown org list: %d, want 404", rr.Code)
	}

	// Promote: owner-only; members get 403; bad role 400; unknown target 404.
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", bobCred, `{"role":"owner"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("member promote: %d, want 403", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"admin"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad role: %d, want 400", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/nobody", aliceCred, `{"role":"member"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown target: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"owner"}`); rr.Code != http.StatusOK {
		t.Fatalf("promote: %d", rr.Code)
	}

	// Last-owner guard surfaces as 409 (bob demoted back first).
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"member"}`); rr.Code != http.StatusOK {
		t.Fatalf("demote bob: %d", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/alice", aliceCred, `{"role":"member"}`); rr.Code != http.StatusConflict {
		t.Fatalf("demote last owner: %d, want 409", rr.Code)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/alice", aliceCred, ""); rr.Code != http.StatusConflict {
		t.Fatalf("remove last owner: %d, want 409", rr.Code)
	}
}

func TestOrgMemberRemovalAndSelfLeave(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)

	// A member cannot remove someone else...
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/alice", bobCred, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("member removes other: %d, want 403", rr.Code)
	}
	// ...but may leave.
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/bob", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("self-leave: %d", rr.Code)
	}
	// Gone: the org now 404s for bob.
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("after leave: %d, want 404", rr.Code)
	}
}

func TestInviteEndpointsFullFlow(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	alice, _ := st.AuthenticateAccount(aliceCred)
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatal(err)
	}

	// Owner-only invite creation; members/strangers can't see the surface.
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", bobCred, `{"github_username":"x"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member invites: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty username: %d, want 400", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"Bob"}`); rr.Code != http.StatusOK {
		t.Fatalf("invite: %d (body %s)", rr.Code, rr.Body.String())
	}

	// Owner sees it pending; the invitee sees it under /v1/invites.
	rr := apiReq(t, api, "GET", "/v1/orgs/acme/invites", aliceCred, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"bob"`) {
		t.Fatalf("pending list: %d %s", rr.Code, rr.Body.String())
	}
	rr = apiReq(t, api, "GET", "/v1/invites", bobCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("my invites: %d", rr.Code)
	}
	var mine struct {
		Invites []struct {
			Org string `json:"org"`
		} `json:"invites"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&mine); err != nil {
		t.Fatal(err)
	}
	if len(mine.Invites) != 1 || mine.Invites[0].Org != "acme" {
		t.Fatalf("my invites = %+v, want [acme]", mine.Invites)
	}

	// Accept → member; invite consumed; re-accept 404.
	if rr := apiReq(t, api, "POST", "/v1/invites/acme/accept", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("accept: %d", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("bob not a member after accept: %d", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/invites/acme/accept", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("re-accept: %d, want 404", rr.Code)
	}
	// Inviting an existing member → 409.
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"bob"}`); rr.Code != http.StatusConflict {
		t.Fatalf("invite member: %d, want 409", rr.Code)
	}
}

func TestInviteDeclineAndRevokeEndpoints(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	alice, _ := st.AuthenticateAccount(aliceCred)
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatal(err)
	}

	apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"bob"}`)
	if rr := apiReq(t, api, "POST", "/v1/invites/acme/decline", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("decline: %d", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("decline must not add membership: %d, want 404", rr.Code)
	}

	apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"bob"}`)
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/invites/bob", aliceCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rr.Code)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/invites/bob", aliceCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("re-revoke: %d, want 404", rr.Code)
	}
	// Accepting a nonexistent org's invite → 404 (no existence probe).
	if rr := apiReq(t, api, "POST", "/v1/invites/ghost/accept", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("accept unknown org: %d, want 404", rr.Code)
	}
}

func TestEnrollIntoOrg(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred) // alice owner, bob member

	// Owner enrolls into the org: base domain carries the org slug.
	rr := apiReq(t, api, "POST", "/v1/enroll", aliceCred, `{"org":"acme"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("org enroll: %d (body %s)", rr.Code, rr.Body.String())
	}
	var en struct {
		BaseDomain string `json:"base_domain"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&en); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(en.BaseDomain, "-acme.") {
		t.Fatalf("base domain = %q, want the acme org slug", en.BaseDomain)
	}

	// A plain member may not enroll (owners manage the footprint).
	if rr := apiReq(t, api, "POST", "/v1/enroll", bobCred, `{"org":"acme"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("member org enroll: %d, want 403", rr.Code)
	}
	// Unknown org and non-member are indistinguishable 404s.
	if rr := apiReq(t, api, "POST", "/v1/enroll", aliceCred, `{"org":"ghost"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown org enroll: %d, want 404", rr.Code)
	}

	// No body: personal enrollment still works exactly as before.
	rr = apiReq(t, api, "POST", "/v1/enroll", aliceCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("personal enroll: %d", rr.Code)
	}
	en.BaseDomain = ""
	if err := json.NewDecoder(rr.Body).Decode(&en); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(en.BaseDomain, "-alice.") {
		t.Fatalf("personal base domain = %q, want the alice slug", en.BaseDomain)
	}
}

func TestOrgDeleteEndpoint(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)

	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme", bobCred, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("member delete: %d, want 403", rr.Code)
	}
	alice, _ := st.AuthenticateAccount(aliceCred)
	org, _, err := st.OrgRole("acme", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnrollForAccount(org); err != nil {
		t.Fatal(err)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme", aliceCred, ""); rr.Code != http.StatusConflict {
		t.Fatalf("delete with agents: %d, want 409", rr.Code)
	}
	if _, err := st.db.Exec(`DELETE FROM agents WHERE account_id=?`, org); err != nil {
		t.Fatal(err)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme", aliceCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", aliceCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("org survived: %d, want 404", rr.Code)
	}
}

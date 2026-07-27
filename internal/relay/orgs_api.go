package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// registerOrgRoutes wires the org-management surface (#104). All handlers
// authenticate the account credential; org-scoped ones resolve the {slug}
// through OrgRole, whose ErrNoOrg (unknown org OR non-member) maps to 404 so
// org existence never leaks.
func (a *api) registerOrgRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/orgs", a.orgCreate)
	mux.HandleFunc("GET /v1/orgs", a.orgList)
	mux.HandleFunc("GET /v1/orgs/{slug}/members", a.orgMembers)
	mux.HandleFunc("PUT /v1/orgs/{slug}/members/{username}", a.orgSetRole)
	mux.HandleFunc("DELETE /v1/orgs/{slug}/members/{username}", a.orgRemoveMember)
	mux.HandleFunc("POST /v1/orgs/{slug}/invites", a.orgInvite)
	mux.HandleFunc("GET /v1/orgs/{slug}/invites", a.orgInvitesList)
	mux.HandleFunc("DELETE /v1/orgs/{slug}/invites/{login}", a.orgRevokeInvite)
	mux.HandleFunc("GET /v1/invites", a.myInvites)
	mux.HandleFunc("POST /v1/invites/{slug}/accept", a.inviteAccept)
	mux.HandleFunc("POST /v1/invites/{slug}/decline", a.inviteDecline)
	mux.HandleFunc("PUT /v1/orgs/{slug}/github", a.orgSetGitHub)
	mux.HandleFunc("DELETE /v1/orgs/{slug}", a.orgDelete)
}

func (a *api) orgCreate(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	org, err := a.st.CreateOrg(acc.ID, req.Name)
	if errors.Is(err, ErrOrgNameTaken) {
		// Name the slug that collided: the creator typed a name, and what is
		// taken is what it derived to.
		http.Error(w, "org name taken: "+deriveUsername(req.Name), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "org create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"org": org.Slug, "role": "owner"})
}

func (a *api) orgList(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgs, err := a.st.OrgsForAccount(acc.ID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, map[string]string{"org": o.Slug, "role": o.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orgs": out})
}

// orgForMember resolves {slug} for the authenticated caller. Non-members and
// unknown orgs both 404 (ErrNoOrg is deliberately ambiguous).
func (a *api) orgForMember(w http.ResponseWriter, r *http.Request, accID string) (orgID, role string, ok bool) {
	orgID, role, err := a.st.OrgRole(r.PathValue("slug"), accID)
	if errors.Is(err, ErrNoOrg) {
		http.NotFound(w, r)
		return "", "", false
	}
	if err != nil {
		http.Error(w, "org error", http.StatusInternalServerError)
		return "", "", false
	}
	return orgID, role, true
}

func (a *api) orgMembers(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, _, ok := a.orgForMember(w, r, acc.ID)
	if !ok {
		return
	}
	members, err := a.st.Members(orgID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(members))
	for _, m := range members {
		out = append(out, map[string]string{"username": m.Username, "role": m.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

// writeMembershipErr maps the shared membership store errors onto HTTP.
func writeMembershipErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotMember):
		http.NotFound(w, r)
	case errors.Is(err, ErrLastOwner):
		http.Error(w, "an org must keep at least one owner", http.StatusConflict)
	default:
		http.Error(w, "membership error", http.StatusInternalServerError)
	}
}

func (a *api) orgSetRole(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, role, ok := a.orgForMember(w, r, acc.ID)
	if !ok {
		return
	}
	if role != "owner" {
		http.Error(w, "owner role required", http.StatusForbidden)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Role != "owner" && req.Role != "member") {
		http.Error(w, `role must be "owner" or "member"`, http.StatusBadRequest)
		return
	}
	if err := a.st.SetMemberRole(orgID, r.PathValue("username"), req.Role); err != nil {
		writeMembershipErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": r.PathValue("username"), "role": req.Role})
}

// orgSetGitHub links the org to the GitHub organization whose installations
// should route to it. Owner-only, mirroring the other org-management writes.
func (a *api) orgSetGitHub(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, role, ok := a.orgForMember(w, r, acc.ID)
	if !ok {
		return
	}
	if role != "owner" {
		http.Error(w, "owner role required", http.StatusForbidden)
		return
	}
	var req struct {
		GitHubLogin string `json:"github_login"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.GitHubLogin) == "" {
		http.Error(w, "github_login required", http.StatusBadRequest)
		return
	}
	if err := a.st.SetOrgGitHub(orgID, req.GitHubLogin); err != nil {
		http.Error(w, "org error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) orgRemoveMember(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, role, ok := a.orgForMember(w, r, acc.ID)
	if !ok {
		return
	}
	target := r.PathValue("username")
	// Owners remove anyone; a plain member may only remove themselves (leave).
	if role != "owner" && target != acc.Username {
		http.Error(w, "owner role required", http.StatusForbidden)
		return
	}
	if err := a.st.RemoveMember(orgID, target); err != nil {
		writeMembershipErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": target})
}

// requireOwner is orgForMember plus the owner gate shared by the owner-only
// management endpoints.
func (a *api) requireOwner(w http.ResponseWriter, r *http.Request, accID string) (orgID string, ok bool) {
	orgID, role, ok := a.orgForMember(w, r, accID)
	if !ok {
		return "", false
	}
	if role != "owner" {
		http.Error(w, "owner role required", http.StatusForbidden)
		return "", false
	}
	return orgID, true
}

func (a *api) orgInvite(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	var req struct {
		GithubUsername string `json:"github_username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.GithubUsername) == "" {
		http.Error(w, "github_username required", http.StatusBadRequest)
		return
	}
	err := a.st.CreateInvite(orgID, req.GithubUsername, acc.ID)
	if errors.Is(err, ErrAlreadyMember) {
		http.Error(w, "already a member", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "invite failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"invited": strings.ToLower(req.GithubUsername)})
}

func (a *api) orgInvitesList(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	logins, err := a.st.OrgInvites(orgID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if logins == nil {
		logins = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": logins})
}

func (a *api) orgRevokeInvite(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	err := a.st.RevokeInvite(orgID, r.PathValue("login"))
	if errors.Is(err, ErrNoInvite) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"revoked": strings.ToLower(r.PathValue("login"))})
}

func (a *api) myInvites(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	slugs, err := a.st.InvitesForAccount(acc.ID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, map[string]string{"org": s})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

func (a *api) inviteAccept(w http.ResponseWriter, r *http.Request) {
	a.consumeInvite(w, r, a.st.AcceptInvite, "accepted")
}

func (a *api) inviteDecline(w http.ResponseWriter, r *http.Request) {
	a.consumeInvite(w, r, a.st.DeclineInvite, "declined")
}

func (a *api) consumeInvite(w http.ResponseWriter, r *http.Request, act func(accountID, orgSlug string) error, verb string) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	slug := r.PathValue("slug")
	err := act(acc.ID, slug)
	if errors.Is(err, ErrNoInvite) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "invite error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{verb: slug})
}

func (a *api) orgDelete(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	err := a.st.DeleteOrg(orgID)
	if errors.Is(err, ErrOrgHasAgents) {
		http.Error(w, "org still owns agents", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("slug")})
}

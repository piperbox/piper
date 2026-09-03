package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/tunnel"
)

// pipeSession builds an in-memory relay↔agent tunnel pair whose relay-side
// session carries base as its BaseDomain.
func pipeSession(t *testing.T, base string) (relaySide, agentSide *tunnel.Session) {
	t.Helper()
	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	srvCh := make(chan *tunnel.Session, 1)
	go func() {
		s, err := tunnel.Serve(sc, func(_, _ string) error { return nil })
		if err == nil {
			srvCh <- s
		}
	}()
	agentSess, err := tunnel.Dial(cc, "tok", base)
	if err != nil {
		t.Fatal(err)
	}
	return <-srvCh, agentSess
}

// fakeBox answers KindControlAPI streams: one HTTP request per stream, echoing
// method, path and Authorization so tests see exactly what the proxy forwarded.
func fakeBox(sess *tunnel.Session) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		if kind != tunnel.KindControlAPI {
			stream.Close()
			continue
		}
		go func() {
			defer stream.Close()
			req, err := http.ReadRequest(bufio.NewReader(stream))
			if err != nil {
				return
			}
			body := req.Method + " " + req.URL.RequestURI() + " auth=" + req.Header.Get("Authorization")
			fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		}()
	}
}

// proxyFixture: alice owns an enrolled agent; mallory is another tenant.
func proxyFixture(t *testing.T) (api http.Handler, st *Store, router *Router, aliceCred, malloryCred, base string) {
	t.Helper()
	st = openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	alice, err := st.UpsertAccount("sub-alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, _ = st.MintAccountCredential(alice.ID)
	mallory, err := st.UpsertAccount("sub-mallory", "mallory")
	if err != nil {
		t.Fatal(err)
	}
	malloryCred, _ = st.MintAccountCredential(mallory.ID)
	en, err := st.EnrollForAccount(alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	base = en.BaseDomain
	router = NewRouter()
	api = NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil)
	return
}

func proxyGet(t *testing.T, api http.Handler, path, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestControlProxyAuthz(t *testing.T) {
	api, _, router, aliceCred, malloryCred, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	// No credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	// Unknown credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", "bogus"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad cred: %d, want 401", rr.Code)
	}
	// Another tenant's credential → 404, indistinguishable from unknown agent.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant: %d, want 404", rr.Code)
	}
	// Unknown agent → 404.
	if rr := proxyGet(t, api, "/agents/nope.public.getpiper.co/v1/apps", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d, want 404", rr.Code)
	}
	// Path without /v1/ → 404.
	if rr := proxyGet(t, api, "/agents/"+base+"/secrets", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("non-v1 path: %d, want 404", rr.Code)
	}
}

func TestControlProxyOfflineAgent(t *testing.T) {
	api, _, _, aliceCred, _, base := proxyFixture(t)
	// Agent enrolled but no live session registered.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline agent: %d, want 503", rr.Code)
	}
}

func TestControlProxyForwardsWithTokenB(t *testing.T) {
	api, st, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)
	if err := st.SetControlToken(base, "boxtok"); err != nil {
		t.Fatal(err)
	}

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps?limit=2", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxied: %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "GET /v1/apps?limit=2 ") {
		t.Fatalf("prefix not stripped / query lost: %q", body)
	}
	if !strings.Contains(body, "auth=Bearer boxtok") {
		t.Fatalf("Token B not injected: %q", body)
	}
	if strings.Contains(body, aliceCred) {
		t.Fatalf("account credential leaked to the box: %q", body)
	}
}

func TestControlProxyNoTokenBForwardsBare(t *testing.T) {
	api, _, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxied: %d", rr.Code)
	}
	// Never provisioned: forwarded with NO Authorization (a real box would 401).
	if !strings.Contains(rr.Body.String(), "auth= ") && !strings.HasSuffix(strings.TrimSpace(rr.Body.String()), "auth=") {
		t.Fatalf("expected empty forwarded auth, got %q", rr.Body.String())
	}
}

func TestControlProxyLiveness(t *testing.T) {
	api, _, router, aliceCred, malloryCred, base := proxyFixture(t)

	// Same gates as the proxy: no/bad credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	// Cross-tenant and unknown agents → 404, indistinguishable.
	if rr := proxyGet(t, api, "/agents/"+base, malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant: %d, want 404", rr.Code)
	}
	if rr := proxyGet(t, api, "/agents/nope.public.getpiper.co", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d, want 404", rr.Code)
	}

	// Owned but no live session: offline is an answer, not an error.
	rr := proxyGet(t, api, "/agents/"+base, aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("offline liveness: %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var live struct {
		Agent     string `json:"agent"`
		Connected bool   `json:"connected"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&live); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if live.Agent != base || live.Connected {
		t.Errorf("offline liveness = %+v, want agent=%s connected=false", live, base)
	}

	// Connected session ⇒ box up. No fakeBox is serving streams: if the
	// handler opened a tunnel stream, this request would hang — liveness
	// must be answered from the router's in-memory map alone.
	relaySess, _ := pipeSession(t, base)
	router.Register(relaySess)
	rr = proxyGet(t, api, "/agents/"+base, aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("connected liveness: %d, want 200", rr.Code)
	}
	if err := json.NewDecoder(rr.Body).Decode(&live); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !live.Connected {
		t.Errorf("connected liveness = %+v, want connected=true", live)
	}

	// Bare agent path is a GET-only resource.
	req := httptest.NewRequest(http.MethodPost, "/agents/"+base, nil)
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	pr := httptest.NewRecorder()
	api.ServeHTTP(pr, req)
	if pr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST liveness: %d, want 405", pr.Code)
	}
}

func TestControlProxyListAgents(t *testing.T) {
	api, st, router, aliceCred, malloryCred, base := proxyFixture(t)

	// Same gates as the per-agent endpoints: no/bad credential → 401.
	if rr := proxyGet(t, api, "/agents", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	if rr := proxyGet(t, api, "/agents", "bogus"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad cred: %d, want 401", rr.Code)
	}

	// Alice has two agents: `base` connected, `base2` offline.
	acc, err := st.AuthenticateAccount(aliceCred)
	if err != nil {
		t.Fatal(err)
	}
	en2, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	relaySess, _ := pipeSession(t, base)
	router.Register(relaySess)

	rr := proxyGet(t, api, "/agents", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var list struct {
		Agents []struct {
			Agent     string `json:"agent"`
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Agents) != 2 {
		t.Fatalf("listed %d agents, want 2: %+v", len(list.Agents), list.Agents)
	}
	if list.Agents[0].Agent != base || !list.Agents[0].Connected {
		t.Errorf("agent[0] = %+v, want %s connected", list.Agents[0], base)
	}
	// The box name is surfaced so the dashboard can head each section with it
	// instead of the base-domain hash (#143).
	if list.Agents[0].Name != base {
		t.Errorf("agent[0].name = %q, want %q", list.Agents[0].Name, base)
	}
	if list.Agents[1].Agent != en2.BaseDomain || list.Agents[1].Connected {
		t.Errorf("agent[1] = %+v, want %s offline", list.Agents[1], en2.BaseDomain)
	}

	// Another tenant sees only its own (here: empty) list — never alice's.
	rr = proxyGet(t, api, "/agents", malloryCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("mallory list: %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); strings.Contains(body, base) {
		t.Fatalf("mallory sees alice's agents: %s", body)
	}
	list.Agents = nil
	if err := json.NewDecoder(strings.NewReader(rr.Body.String())).Decode(&list); err != nil {
		t.Fatalf("decode mallory: %v", err)
	}
	if list.Agents == nil || len(list.Agents) != 0 {
		t.Fatalf("mallory list = %+v, want empty (non-null) agents array", list.Agents)
	}

	// Trailing-slash form answers the same; the list is a GET-only resource.
	if rr := proxyGet(t, api, "/agents/", aliceCred); rr.Code != http.StatusOK {
		t.Fatalf("trailing slash: %d, want 200", rr.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/", nil)
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	pr := httptest.NewRecorder()
	api.ServeHTTP(pr, req)
	if pr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST list: %d, want 405", pr.Code)
	}
}

// orgProxyFixture: alice owns org "acme" with an enrolled agent; bob is a
// member, mallory a stranger.
func orgProxyFixture(t *testing.T) (api http.Handler, st *Store, router *Router, aliceCred, bobCred, malloryCred, base string) {
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
	mallory, err := st.UpsertAccount("sub-mallory", "mallory")
	if err != nil {
		t.Fatal(err)
	}
	malloryCred, _ = st.MintAccountCredential(mallory.ID)

	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	addMember(t, st, org.ID, bob.ID, "member")
	en, err := st.EnrollForAccount(org.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	base = en.BaseDomain
	router = NewRouter()
	api = NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil)
	return
}

func TestControlProxyOrgMemberDrivesBox(t *testing.T) {
	api, st, router, _, bobCred, malloryCred, base := orgProxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)
	if err := st.SetControlToken(base, "boxtok"); err != nil {
		t.Fatal(err)
	}

	// A plain member drives the org's box end-to-end, Token B injected.
	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", bobCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("member request: %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "auth=Bearer boxtok") {
		t.Fatalf("Token B not injected for member: %q", rr.Body.String())
	}

	// A non-member gets 404 — indistinguishable from an unknown agent.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member: %d, want 404", rr.Code)
	}
	// Liveness is equally gated.
	if rr := proxyGet(t, api, "/agents/"+base, malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member liveness: %d, want 404", rr.Code)
	}
}

func TestControlProxyDisabledOrgSeversMembers(t *testing.T) {
	api, st, router, _, bobCred, _, base := orgProxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	if err := st.DisableAccount("acme", "org"); err != nil {
		t.Fatal(err)
	}
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", bobCred); rr.Code != http.StatusNotFound {
		t.Fatalf("disabled org: %d, want 404", rr.Code)
	}
}

func TestControlProxyListIncludesOrgAgentsWithOwner(t *testing.T) {
	api, st, router, _, bobCred, _, base := orgProxyFixture(t)
	relaySess, _ := pipeSession(t, base)
	router.Register(relaySess)

	// Bob also has a personal box.
	acc, err := st.AuthenticateAccount(bobCred)
	if err != nil {
		t.Fatal(err)
	}
	personal, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	rr := proxyGet(t, api, "/agents", bobCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d (body %s)", rr.Code, rr.Body.String())
	}
	var list struct {
		Agents []struct {
			Agent     string `json:"agent"`
			Owner     string `json:"owner"`
			Connected bool   `json:"connected"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Agents) != 2 {
		t.Fatalf("listed %d agents, want 2: %+v", len(list.Agents), list.Agents)
	}
	if list.Agents[0].Agent != base || list.Agents[0].Owner != "acme" || !list.Agents[0].Connected {
		t.Errorf("agent[0] = %+v, want %s owner=acme connected", list.Agents[0], base)
	}
	if list.Agents[1].Agent != personal.BaseDomain || list.Agents[1].Owner != "bob" || list.Agents[1].Connected {
		t.Errorf("agent[1] = %+v, want %s owner=bob offline", list.Agents[1], personal.BaseDomain)
	}
}

// proxyDelete issues a DELETE with the given credential, mirroring proxyGet.
func proxyDelete(t *testing.T, api http.Handler, path, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestControlProxyRemoveAgent(t *testing.T) {
	api, st, _, aliceCred, _, base := proxyFixture(t)

	if rr := proxyDelete(t, api, "/agents/"+base, aliceCred); rr.Code != http.StatusNoContent {
		t.Fatalf("remove: %d, want 204 (body %q)", rr.Code, rr.Body.String())
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=$1`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("agent row still present after 204")
	}
	// Now genuinely unknown: a second removal is a 404, like any other
	// unknown agent, with no hint that it ever existed.
	if rr := proxyDelete(t, api, "/agents/"+base, aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("second remove: %d, want 404", rr.Code)
	}
}

// A box holding a live tunnel session is refused rather than evicted: removal
// is irreversible, so mistyping a base domain must not take down a running box.
func TestControlProxyRemoveConnectedAgentIsRefused(t *testing.T) {
	api, st, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	rr := proxyDelete(t, api, "/agents/"+base, aliceCred)
	if rr.Code != http.StatusConflict {
		t.Fatalf("remove connected: %d, want 409", rr.Code)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=$1`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("connected agent was removed despite the 409")
	}
}

// Cross-tenant removal is indistinguishable from an unknown agent, and must not
// delete anything.
func TestControlProxyRemoveForeignAgentIs404(t *testing.T) {
	api, st, _, _, malloryCred, base := proxyFixture(t)

	if rr := proxyDelete(t, api, "/agents/"+base, malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant remove: %d, want 404", rr.Code)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=$1`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("another tenant removed alice's agent")
	}
}

// Enrolling an org's box is owner-only (api.go), so removing one must be too:
// a plain member gets 403 and the agent survives.
func TestControlProxyRemoveOrgBoxMemberForbidden(t *testing.T) {
	api, st, _, _, bobCred, _, base := orgProxyFixture(t)

	rr := proxyDelete(t, api, "/agents/"+base, bobCred)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member remove: %d, want 403 (body %q)", rr.Code, rr.Body.String())
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=$1`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a member's forbidden DELETE removed the org's agent")
	}
}

// The org owner can remove the org's box.
func TestControlProxyRemoveOrgBoxOwnerSucceeds(t *testing.T) {
	api, st, _, aliceCred, _, _, base := orgProxyFixture(t)

	rr := proxyDelete(t, api, "/agents/"+base, aliceCred)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("owner remove: %d, want 204 (body %q)", rr.Code, rr.Body.String())
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=$1`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("agent row still present after owner's 204")
	}
}

// Adding DELETE must not widen the path to other verbs.
func TestControlProxyAgentPathStillRejectsOtherMethods(t *testing.T) {
	api, _, _, aliceCred, _, base := proxyFixture(t)
	req := httptest.NewRequest(http.MethodPut, "/agents/"+base, nil)
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d, want 405", rr.Code)
	}
}

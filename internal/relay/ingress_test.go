package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturingDeliverer struct {
	mu         sync.Mutex
	calls      []Binding
	done       chan struct{}
	fail       bool // when set, Deliver reports failure instead of succeeding
	drains     []string
	drained    chan string
	dispatched int
}

func (c *capturingDeliverer) Deliver(_ context.Context, b Binding, _ string, _ []byte) error {
	c.mu.Lock()
	c.calls = append(c.calls, b)
	fail := c.fail
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
	if fail {
		return fmt.Errorf("simulated delivery failure")
	}
	return nil
}

// Dispatch runs fn on its own goroutine, as the real pool does, so the
// ingress's asynchrony is still exercised.
func (c *capturingDeliverer) Dispatch(fn func(ctx context.Context)) {
	c.mu.Lock()
	c.dispatched++
	c.mu.Unlock()
	go fn(context.Background())
}

func (c *capturingDeliverer) dispatchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dispatched
}

func (c *capturingDeliverer) DrainFor(_ context.Context, agentName string) {
	c.mu.Lock()
	c.drains = append(c.drains, agentName)
	c.mu.Unlock()
	select {
	case c.drained <- agentName:
	default:
	}
}

func (c *capturingDeliverer) seen() []Binding {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Binding(nil), c.calls...)
}

func (c *capturingDeliverer) drainCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.drains...)
}

func signed(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func postEvent(t *testing.T, h http.Handler, event, sig, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/gh", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newTestIngress(t *testing.T, st *Store, d Deliverer) http.Handler {
	t.Helper()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewGitHubIngress(st, app, d)
}

func TestIngressRejectsBadSignature(t *testing.T) {
	st := openTestStore(t)
	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	rec := postEvent(t, h, "push", "sha256=deadbeef", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(d.seen()) != 0 {
		t.Fatal("delivered despite bad signature")
	}
}

func TestIngressRoutesPushToBinding(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","after":"abc",` +
		`"repository":{"full_name":"alice/blog"},"installation":{"id":55}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within 2s")
	}
	seen := d.seen()
	if len(seen) != 1 || seen[0].AgentName != agent || seen[0].App != "blog" {
		t.Fatalf("delivered = %+v", seen)
	}
}

func TestIngressDropsUnlinkedInstallation(t *testing.T) {
	st := openTestStore(t)
	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","repository":{"full_name":"alice/blog"},"installation":{"id":999}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if len(d.seen()) != 0 {
		t.Fatal("routed an event for an unlinked installation")
	}
}

func TestIngressStoreErrorOnLinkedInstallationReturns500(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}

	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	// Force a transient store error (distinct from ErrNoInstallation) on an
	// otherwise-linked installation by closing the store's DB out from under
	// the handler.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	body := `{"ref":"refs/heads/main","after":"abc",` +
		`"repository":{"full_name":"alice/blog"},"installation":{"id":55}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(d.seen()) != 0 {
		t.Fatal("delivered despite store error")
	}
}

func TestIngressLinksAndUnlinksInstallation(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	h := newTestIngress(t, st, &capturingDeliverer{done: make(chan struct{}, 8)})

	created := `{"action":"created","installation":{"id":55,` +
		`"account":{"type":"User","login":"alice"}},"sender":{"id":1001,"login":"alice"}}`
	if rec := postEvent(t, h, "installation", signed("s3cret", []byte(created)), created); rec.Code != http.StatusAccepted {
		t.Fatalf("created status = %d", rec.Code)
	}
	if _, err := st.AccountForInstallation("55"); err != nil {
		t.Fatalf("installation not linked: %v", err)
	}

	deleted := fmt.Sprintf(`{"action":"deleted","installation":{"id":55,`+
		`"account":{"type":"User","login":"alice"}},"sender":{"id":%d,"login":"alice"}}`, 1001)
	if rec := postEvent(t, h, "installation", signed("s3cret", []byte(deleted)), deleted); rec.Code != http.StatusAccepted {
		t.Fatalf("deleted status = %d", rec.Code)
	}
	if _, err := st.AccountForInstallation("55"); err == nil {
		t.Fatal("installation survived deletion")
	}
}

// TestIngressInstallationEventLinkingAndLogging covers #471: the linking
// triggers the relay honours, and the two silent paths a failing install-link
// used to take. installation_repositories is the event GitHub fires when a
// user re-saves the repository selection of an installation that already
// exists — the only linking action its UI offers at that point — so it must
// link like installation.created. Anything the handler chooses not to act on,
// and any link that fails, has to say so in the log: a relay that drops these
// in silence is what made #471 expensive to diagnose.
func TestIngressInstallationEventLinkingAndLogging(t *testing.T) {
	const alice = `"installation":{"id":55,"account":{"type":"User","login":"alice"}},` +
		`"sender":{"id":1001,"login":"alice"}}`

	for _, tc := range []struct {
		name    string
		account bool // create the piper account for github id 1001
		// prelink links installation 55 to github id 1001 before the event,
		// and also creates the sender's account (github id 2002) so a
		// sender-resolving relink would actually succeed — unless
		// unknownSender skips that account, leaving the sender with no
		// piper account at all.
		prelink       bool
		unknownSender bool
		event         string
		body          string
		wantLinked    bool
		wantLogged    []string
		notLogged     []string
	}{
		{
			name:       "repositories added links",
			account:    true,
			event:      "installation_repositories",
			body:       `{"action":"added",` + alice,
			wantLinked: true,
		},
		{
			name:    "repositories event preserves the current owner",
			account: true,
			prelink: true,
			event:   "installation_repositories",
			body: `{"action":"added","installation":{"id":55,` +
				`"account":{"type":"User","login":"alice"}},` +
				`"sender":{"id":2002,"login":"mallory"}}`,
			wantLinked: true,
			wantLogged: []string{"already-linked", "55"},
		},
		{
			// #489: an already-linked installation must log the
			// preserving-owner diagnostic even when the admin re-saving the
			// repository selection has no piper account — no recovery link
			// was needed, so there is nothing to fail.
			name:          "repositories event preserves the owner for an unknown sender",
			account:       true,
			prelink:       true,
			unknownSender: true,
			event:         "installation_repositories",
			body: `{"action":"added","installation":{"id":55,` +
				`"account":{"type":"User","login":"alice"}},` +
				`"sender":{"id":2002,"login":"mallory"}}`,
			wantLinked: true,
			wantLogged: []string{"already-linked", "55"},
			notLogged:  []string{"no piper account"},
		},
		{
			name:       "repositories removed links",
			account:    true,
			event:      "installation_repositories",
			body:       `{"action":"removed",` + alice,
			wantLinked: true,
		},
		{
			name:       "unhandled action is logged",
			account:    true,
			event:      "installation",
			body:       `{"action":"edited",` + alice,
			wantLinked: false,
			wantLogged: []string{"installation", "edited", "55"},
		},
		{
			name:       "failed link names the installer",
			account:    false, // nobody has run piper login as alice
			event:      "installation",
			body:       `{"action":"created",` + alice,
			wantLinked: false,
			wantLogged: []string{"55", "alice", "1001"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			if tc.account {
				if _, err := st.UpsertAccount("1001", "alice"); err != nil {
					t.Fatal(err)
				}
			}
			prelinked := ""
			if tc.prelink {
				if !tc.unknownSender {
					if _, err := st.UpsertAccount("2002", "mallory"); err != nil {
						t.Fatal(err)
					}
				}
				if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
					t.Fatal(err)
				}
				owner, err := st.AccountForInstallation("55")
				if err != nil {
					t.Fatal(err)
				}
				prelinked = owner
			}
			h := newTestIngress(t, st, &capturingDeliverer{done: make(chan struct{}, 8)})

			var logged syncLogBuffer
			prevOut, prevFlags := log.Writer(), log.Flags()
			log.SetOutput(&logged)
			log.SetFlags(0)
			t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

			if rec := postEvent(t, h, tc.event, signed("s3cret", []byte(tc.body)), tc.body); rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}

			acct, err := st.AccountForInstallation("55")
			if tc.wantLinked && err != nil {
				t.Fatalf("%s/%s did not link the installation: %v", tc.event, tc.body, err)
			}
			if !tc.wantLinked && err == nil {
				t.Fatalf("%s/%s linked the installation, want no link", tc.event, tc.body)
			}
			if tc.prelink && acct != prelinked {
				t.Fatalf("%s/%s reassigned the installation owner: was %q, now %q",
					tc.event, tc.body, prelinked, acct)
			}
			for _, want := range tc.wantLogged {
				if !strings.Contains(logged.String(), want) {
					t.Fatalf("log = %q, want it to mention %q", logged.String(), want)
				}
			}
			for _, unwanted := range tc.notLogged {
				if strings.Contains(logged.String(), unwanted) {
					t.Fatalf("log = %q, want it not to mention %q", logged.String(), unwanted)
				}
			}
		})
	}
}

// TestIngressRoutesOrgInstallToOrgOwnedBox is #290's acceptance criterion: an
// org-target installation, linked to the Piper org via the sender's membership,
// routes a push to the org-owned box and never to a non-member's box bound to
// the same repository.
func TestIngressRoutesOrgInstallToOrgOwnedBox(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("1001", "alice") // owner ⇒ member of the org
	org, _ := st.CreateOrg(alice.ID, "acme")
	if err := st.SetOrgGitHub(org.ID, "acme"); err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(org.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	orgAgent := en.BaseDomain
	if err := st.BindRepo(orgAgent, "blog", "acme/blog", "main"); err != nil {
		t.Fatal(err)
	}
	// A non-member's personal box bound to the very same repo must stay dark.
	_, malloryAgent := enrolledAgent(t, st, "2002", "mallory")
	if err := st.BindRepo(malloryAgent, "blog", "acme/blog", "main"); err != nil {
		t.Fatal(err)
	}

	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	// Alice installs the App on GitHub org "acme" (github id 5000).
	created := `{"action":"created","installation":{"id":77,` +
		`"account":{"id":5000,"type":"Organization","login":"acme"}},` +
		`"sender":{"id":1001,"login":"alice"}}`
	if rec := postEvent(t, h, "installation", signed("s3cret", []byte(created)), created); rec.Code != http.StatusAccepted {
		t.Fatalf("install status = %d", rec.Code)
	}
	acct, err := st.AccountForInstallation("77")
	if err != nil || acct != org.ID {
		t.Fatalf("installation account = (%q,%v), want org %q", acct, err, org.ID)
	}

	body := `{"ref":"refs/heads/main","after":"abc",` +
		`"repository":{"full_name":"acme/blog"},"installation":{"id":77}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push status = %d", rec.Code)
	}
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within 2s")
	}
	time.Sleep(50 * time.Millisecond) // let any stray goroutine land
	seen := d.seen()
	if len(seen) != 1 || seen[0].AgentName != orgAgent {
		t.Fatalf("delivered = %+v, want only org box %q", seen, orgAgent)
	}
}

func TestIngressPongsPing(t *testing.T) {
	st := openTestStore(t)
	h := newTestIngress(t, st, &capturingDeliverer{done: make(chan struct{}, 8)})
	body := `{"zen":"hi"}`
	rec := postEvent(t, h, "ping", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestIngressParksOnFailedDelivery pins the ingress's park-on-failure path:
// GitHub already got its 202 for this event, so a failed Deliver must leave
// it recoverable in the store rather than dropping it on the floor.
func TestIngressParksOnFailedDelivery(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	d := &capturingDeliverer{done: make(chan struct{}, 8), drained: make(chan string, 8), fail: true}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","after":"abc",` +
		`"repository":{"full_name":"alice/blog"},"installation":{"id":55}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery attempt within 2s")
	}

	// Give the parking goroutine a moment to finish its store write.
	deadline := time.After(2 * time.Second)
	for {
		got, err := st.DrainEvents(agent)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 1 {
			if got[0].App != "blog" || got[0].Ref != "main" || got[0].Event != "push" {
				t.Fatalf("parked event = %+v", got[0])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("got %d parked events after failed delivery, want 1", len(got))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestIngressReDrainsAfterParkingFailedDelivery pins the ingress's post-park
// call to DrainFor — the fix that closes the race where the box reconnects
// between a delivery failing and the park landing, so the reconnect's own
// drain misses the event.
func TestIngressReDrainsAfterParkingFailedDelivery(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	d := &capturingDeliverer{done: make(chan struct{}, 8), drained: make(chan string, 8), fail: true}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","after":"abc",` +
		`"repository":{"full_name":"alice/blog"},"installation":{"id":55}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	select {
	case got := <-d.drained:
		if got != agent {
			t.Fatalf("DrainFor called for %q, want %q", got, agent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ingress never called DrainFor after parking a failed delivery")
	}
}

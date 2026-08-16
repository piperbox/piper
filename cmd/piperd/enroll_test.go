package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/enrollapi"
	"github.com/piperbox/piper/internal/relayclient"
)

func TestLoadOrMintBoxIDIsDurable(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrMintBoxID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("box id = %q, want 32 hex chars", first)
	}
	second, err := loadOrMintBoxID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("box id not durable: %q -> %q", first, second)
	}
	fi, err := os.Stat(filepath.Join(dir, "box-id"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("box-id mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestValidateEnrollmentFailsOnUnreachableRelay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := validateEnrollment(ctx, addr, "tok", "base.example"); err == nil {
		t.Fatal("validate against a dead relay must fail")
	}
}

func TestValidateEnrollmentFailsOnSilentRelay(t *testing.T) {
	// A listener that accepts and says nothing: tunnel.Dial must give up on
	// the ack wait, not report success.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	ctx := context.Background()
	err = validateEnrollment(ctx, ln.Addr().String(), "tok", "base.example")
	if err == nil {
		t.Fatal("validate against a silent relay must fail")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("err = %v, want a handshake failure", err)
	}
}

// validateEnrollment runs while the enroll handler holds s.mu, so a client that
// walks away must release it promptly rather than pin it for tunnel.Dial's full
// ack deadline (#481).
func TestValidateEnrollmentAbortsOnContextCancel(t *testing.T) {
	// A relay that accepts and reads the handshake frame but never acks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	handshaking := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return
		}
		close(handshaking)
		io.Copy(io.Discard, conn)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- validateEnrollment(ctx, ln.Addr().String(), "tok", "base.example") }()
	select {
	case <-handshaking:
	case <-time.After(2 * time.Second):
		t.Fatal("relay never saw the enrollment handshake")
	}
	cancel()

	var got error
	select {
	case got = <-res:
	case <-time.After(2 * time.Second):
		t.Fatal("validateEnrollment did not return after cancel; it sat out tunnel.Dial's ack deadline")
	}
	if got == nil {
		t.Fatal("validateEnrollment = nil, want a cancellation error")
	}
	// Both halves matter: the text must still name the stage that failed, and
	// the error must still unwrap — a text-only check passes a %w -> %v
	// downgrade that silently costs callers errors.Is.
	if !strings.Contains(got.Error(), "relay handshake") {
		t.Fatalf("err = %q, want it to name the relay handshake stage", got)
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("err = %q, want it to unwrap to context.Canceled", got)
	}
}

// newTestEnrollServer returns a server whose seams are all benign fakes plus
// recorders for what got called.
type enrollRec struct {
	enrolled  []string // "api|cred|boxID|org" per relayEnroll call
	validated []string // "addr|token|base" per validate call
	applied   atomic.Int32
}

func newTestEnrollServer(t *testing.T, dataDir string) (*enrollServer, *enrollRec) {
	t.Helper()
	rec := &enrollRec{}
	s := &enrollServer{
		dataDir:      dataDir,
		version:      "test",
		envManaged:   func() bool { return false },
		relayStatus:  func() (string, string) { return "", "" },
		tunnelStatus: func() (string, string) { return "off", "" },
		relayEnroll: func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
			rec.enrolled = append(rec.enrolled, api+"|"+cred+"|"+boxID+"|"+org)
			return relayclient.Enrollment{
				EnrollmentToken: "enr-1", BaseDomain: "ab12-erin.public.getpiper.co",
				TunnelEndpoint: "relay.getpiper.co:7000", WebhookSecret: "whsec-1", GitHubApp: true,
			}, nil
		},
		validate: func(ctx context.Context, addr, token, base string) error {
			rec.validated = append(rec.validated, addr+"|"+token+"|"+base)
			return nil
		},
		countBuilding: func() (int64, error) { return 0, nil },
		apply:         func() { rec.applied.Add(1) },
	}
	return s, rec
}

func postEnroll(t *testing.T, h http.Handler, req enrollapi.EnrollRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, enrollapi.PathEnroll, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e enrollapi.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	return e.Error
}

func TestEnrollHappyPathPersistsThenApplies(t *testing.T) {
	dir := t.TempDir()
	s, rec := newTestEnrollServer(t, dir)
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "https://api.relay", AccountCredential: "cred-xyz"})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	var out enrollapi.EnrollResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.BaseDomain != "ab12-erin.public.getpiper.co" {
		t.Fatalf("resp = %+v", out)
	}
	if res.Header().Get("Content-Length") == "" {
		t.Fatal("response must carry Content-Length (flushed before the apply)")
	}
	rf, found, err := config.LoadRelayFile(dir)
	if err != nil || !found {
		t.Fatalf("relay.json: found=%v err=%v", found, err)
	}
	want := config.RelayFile{RelayAddr: "relay.getpiper.co:7000", RelayToken: "enr-1",
		BaseDomain: "ab12-erin.public.getpiper.co", Terminated: true,
		WebhookSecret: "whsec-1", GitHubBrokered: true}
	if rf != want {
		t.Fatalf("relay.json = %+v, want %+v", rf, want)
	}
	// The relay was called with the durable box id; validate saw the enrollment.
	boxID, _ := loadOrMintBoxID(dir)
	if len(rec.enrolled) != 1 || rec.enrolled[0] != "https://api.relay|cred-xyz|"+boxID+"|" {
		t.Fatalf("relayEnroll calls = %v", rec.enrolled)
	}
	if len(rec.validated) != 1 || rec.validated[0] != "relay.getpiper.co:7000|enr-1|ab12-erin.public.getpiper.co" {
		t.Fatalf("validate calls = %v", rec.validated)
	}
	waitFor(t, func() bool { return rec.applied.Load() == 1 }) // apply is async
}

func TestEnrollValidateFailurePersistsNothing(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	s.validate = func(ctx context.Context, addr, token, base string) error {
		return errors.New("relay handshake: rejected")
	}
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "https://api.relay", AccountCredential: "cred-xyz"})
	if res.Code != http.StatusBadGateway || errCode(t, res) != "validate-failed" {
		t.Fatalf("status = %d code = %s", res.Code, res.Body.String())
	}
	if _, found, _ := config.LoadRelayFile(dir); found {
		t.Fatal("relay.json written despite failed validation")
	}
}

func TestEnrollGuardsInSpecOrder(t *testing.T) {
	dir := t.TempDir()

	// bad request
	s, rec := newTestEnrollServer(t, dir)
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{})
	if res.Code != http.StatusBadRequest || errCode(t, res) != "bad-request" {
		t.Fatalf("empty request: %d %s", res.Code, res.Body.String())
	}

	// env-managed wins before anything else
	s.envManaged = func() bool { return true }
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusConflict || errCode(t, res) != "env-managed" {
		t.Fatalf("env-managed: %d %s", res.Code, res.Body.String())
	}
	s.envManaged = func() bool { return false }

	// already-enrolled unless Replace
	if err := config.SaveRelayFile(dir, config.RelayFile{RelayAddr: "r:1", RelayToken: "old",
		BaseDomain: "old-base.public.getpiper.co", Terminated: true}); err != nil {
		t.Fatal(err)
	}
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusConflict || errCode(t, res) != "already-enrolled" {
		t.Fatalf("already-enrolled: %d %s", res.Code, res.Body.String())
	}
	var e enrollapi.ErrorResponse
	res2 := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	_ = json.NewDecoder(res2.Body).Decode(&e)
	if e.BaseDomain != "old-base.public.getpiper.co" {
		t.Fatalf("already-enrolled must report the current base domain, got %+v", e)
	}
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c", Replace: true})
	if res.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", res.Code, res.Body.String())
	}
	if rec.enrolled == nil {
		t.Fatal("replace did not reach the relay")
	}
}

func TestEnrollBusyWhileBuilding(t *testing.T) {
	dir := t.TempDir()
	s, rec := newTestEnrollServer(t, dir)
	s.countBuilding = func() (int64, error) { return 1, nil }
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusConflict || errCode(t, res) != "busy" {
		t.Fatalf("building guard: %d %s", res.Code, res.Body.String())
	}
	if len(rec.enrolled) != 0 {
		t.Fatal("busy guard must fire before the relay call")
	}
}

func TestEnrollSerializesConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	inFirst := make(chan struct{})
	release := make(chan struct{})
	s.relayEnroll = func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
		close(inFirst)
		<-release
		return relayclient.Enrollment{EnrollmentToken: "enr-1", BaseDomain: "b.example",
			TunnelEndpoint: "r:1"}, nil
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	}()
	<-inFirst
	second := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if second.Code != http.StatusConflict || errCode(t, second) != "busy" {
		t.Fatalf("concurrent enroll: %d %s", second.Code, second.Body.String())
	}
	close(release)
	if res := <-firstDone; res.Code != http.StatusOK {
		t.Fatalf("first enroll: %d %s", res.Code, res.Body.String())
	}
}

func TestEnrollMapsRelayErrors(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	s.relayEnroll = func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
		return relayclient.Enrollment{}, relayclient.ErrBadCredential
	}
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusUnauthorized || errCode(t, res) != "bad-credential" {
		t.Fatalf("bad credential: %d %s", res.Code, res.Body.String())
	}
	s.relayEnroll = func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
		return relayclient.Enrollment{}, relayclient.ErrQuotaExceeded
	}
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusTooManyRequests || errCode(t, res) != "quota" {
		t.Fatalf("quota: %d %s", res.Code, res.Body.String())
	}
}

func TestStatusNeverLeaksSecrets(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	if err := config.SaveRelayFile(dir, config.RelayFile{RelayAddr: "r:1", RelayToken: "SECRET-TOKEN",
		BaseDomain: "b.example", Terminated: true, WebhookSecret: "SECRET-WHSEC"}); err != nil {
		t.Fatal(err)
	}
	s.relayStatus = func() (string, string) { return "r:1", "b.example" }
	r := httptest.NewRequest(http.MethodGet, enrollapi.PathStatus, nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, banned := range []string{"SECRET-TOKEN", "SECRET-WHSEC"} {
		if strings.Contains(body, banned) {
			t.Fatalf("status leaked %q: %s", banned, body)
		}
	}
	var st enrollapi.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enrolled || st.BaseDomain != "b.example" || st.Tunnel != "off" {
		t.Fatalf("status = %+v", st)
	}
}

func getStatus(t *testing.T, s *enrollServer) enrollapi.Status {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, enrollapi.PathStatus, nil))
	var st enrollapi.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
	}
	return st
}

// A re-enroll rewrites relay.json and then re-execs; between those two the old
// process still answers this socket. Enrolled has always been read fresh from
// the file, so serving the identity from the old in-memory config produced a
// snapshot that claimed the new enrollment under the old box's name.
func TestStatusServesIdentityFromTheSameSnapshotAsEnrolled(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	// The running config is the pre-re-enroll identity...
	s.relayStatus = func() (string, string) { return "old-relay:7000", "old.example" }
	s.tunnelStatus = func() (string, string) { return "connected", "" }
	// ...while relay.json already carries the enrollment just applied.
	if err := config.SaveRelayFile(dir, config.RelayFile{RelayAddr: "new-relay:7000",
		RelayToken: "enr-2", BaseDomain: "new.example", Terminated: true}); err != nil {
		t.Fatal(err)
	}
	st := getStatus(t, s)
	if !st.Enrolled {
		t.Fatalf("status = %+v, want enrolled from the saved relay file", st)
	}
	if st.BaseDomain != "new.example" || st.RelayAddr != "new-relay:7000" {
		t.Fatalf("status = %+v, want the identity of the relay file that made Enrolled true", st)
	}
	// The tunnel fields come from the live controller and must survive.
	if st.Tunnel != "connected" {
		t.Fatalf("status = %+v, want the running tunnel controller's state", st)
	}
}

// An operator-pinned enrollment lives in the environment, which config.Load
// prefers over relay.json — so there the running config stays the authority.
func TestStatusServesEnvManagedIdentityFromTheRunningConfig(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	s.envManaged = func() bool { return true }
	s.relayStatus = func() (string, string) { return "env-relay:7000", "env.example" }
	st := getStatus(t, s)
	if !st.Enrolled || !st.EnvManaged {
		t.Fatalf("status = %+v, want an env-managed enrollment", st)
	}
	if st.BaseDomain != "env.example" || st.RelayAddr != "env-relay:7000" {
		t.Fatalf("status = %+v, want the environment's identity even with no relay file", st)
	}
}

func TestEnrollSocketPathPrecedence(t *testing.T) {
	t.Setenv("RUNTIME_DIRECTORY", "/run/piper")
	if got := enrollSocketPath("/dd"); got != "/run/piper/piperd.sock" {
		t.Fatalf("systemd path = %q", got)
	}
	t.Setenv("RUNTIME_DIRECTORY", "")
	if got := enrollSocketPath("/dd"); got != filepath.Join("/dd", "piperd.sock") {
		// On a non-root test run the darwin-root branch cannot fire, so the
		// data-dir fallback is the expected answer on both platforms.
		t.Fatalf("fallback path = %q", got)
	}
}

func TestListenEnrollSocketReplacesStaleSocket(t *testing.T) {
	// os.MkdirTemp, not t.TempDir(): t.TempDir() embeds the (long) test name in
	// the path, which blows darwin's ~104-byte sockaddr_un sun_path limit under
	// a normal $TMPDIR. MkdirTemp("", "p") drops that segment and stays well
	// under the limit on both darwin and linux.
	dir, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "piperd.sock")
	ln1, err := listenEnrollSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	ln1.Close() // leaves the socket file behind, as a crash would
	ln2, err := listenEnrollSocket(path)
	if err != nil {
		t.Fatalf("rebind over stale socket: %v", err)
	}
	defer ln2.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode = %v, want 0666 (dir perms are the auth)", fi.Mode().Perm())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition never became true")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

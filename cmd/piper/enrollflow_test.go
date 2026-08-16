package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
)

// stubNoLocalPiperd makes every install-detection signal negative and returns
// a data dir that does not exist. Without this, a dev machine's real piperd on
// PATH (or a real /run/piper socket) would leak into the tests.
func stubNoLocalPiperd(t *testing.T) string {
	t.Helper()
	oldPath := piperdPath
	piperdPath = func() (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() { piperdPath = oldPath })
	oldEnv := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { config.SystemEnvDir = oldEnv })
	oldRun, oldDarwin := config.SystemRuntimeSocket, config.DarwinRootSocket
	scratch := t.TempDir()
	config.SystemRuntimeSocket = filepath.Join(scratch, "absent-run.sock")
	config.DarwinRootSocket = filepath.Join(scratch, "absent-root.sock")
	t.Cleanup(func() { config.SystemRuntimeSocket, config.DarwinRootSocket = oldRun, oldDarwin })
	return filepath.Join(t.TempDir(), "absent-datadir")
}

// startFakeEnrollSocket serves handler at <dir>/piperd.sock and returns the
// data dir whose candidate list finds it.
func startFakeEnrollSocket(t *testing.T, mux http.Handler) string {
	t.Helper()
	// os.MkdirTemp, not t.TempDir(): t.TempDir() embeds the (long) test name in
	// the path, which blows darwin's ~104-byte sockaddr_un sun_path limit under
	// a normal $TMPDIR. MkdirTemp("", "p") drops that segment and stays well
	// under the limit on both darwin and linux.
	dir, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "piperd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return dir
}

// fakePiperd is a scriptable enrollment-socket daemon.
type fakePiperd struct {
	status atomic.Value // enrollapi.Status
	enroll func(enrollapi.EnrollRequest) (int, any)
	got    atomic.Value // last enrollapi.EnrollRequest
}

func (f *fakePiperd) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "test"})
	})
	m.HandleFunc("GET "+enrollapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.status.Load())
	})
	m.HandleFunc("POST "+enrollapi.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
		var req enrollapi.EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.got.Store(req)
		code, body := f.enroll(req)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
	return m
}

func fastPoll(t *testing.T) {
	t.Helper()
	oldT, oldI, oldS := enrollApplyTimeout, enrollPollInterval, pollSleep
	enrollApplyTimeout = 300 * time.Millisecond
	enrollPollInterval = 10 * time.Millisecond
	pollSleep = func(d time.Duration) { time.Sleep(d) }
	t.Cleanup(func() { enrollApplyTimeout, enrollPollInterval, pollSleep = oldT, oldI, oldS })
}

func TestEnrollFlowIdentityOnlyOffBox(t *testing.T) {
	dataDir := stubNoLocalPiperd(t)
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "https://api.relay", dataDir: dataDir}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "identity only") {
		t.Fatalf("stdout = %q, want the explicit identity-only note", out.String())
	}
}

func TestEnrollFlowInstalledButStoppedExitsOne(t *testing.T) {
	dataDir := stubNoLocalPiperd(t)
	// Installed signal: the data dir exists (an enrolled-before box).
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "https://api.relay", dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errb.String(), "piper agent up") || !strings.Contains(errb.String(), "NOT connected") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestEnrollFlowClaimsAndWaitsForConnect(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(req enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "ab12-erin.public.getpiper.co",
			RelayAddr: "relay:7000", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "ab12-erin.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(),
		enrollFlowOpts{relayAPI: "https://api.relay", dataDir: dataDir, org: "acme"}, "cred-xyz", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	got := f.got.Load().(enrollapi.EnrollRequest)
	if got.RelayAPI != "https://api.relay" || got.AccountCredential != "cred-xyz" || got.Org != "acme" {
		t.Fatalf("enroll body = %+v", got)
	}
	if !strings.Contains(out.String(), "ab12-erin.public.getpiper.co") || !strings.Contains(out.String(), "live") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestEnrollFlowPersistsIdentityOnLocalRowDespiteRemoteClientAddr(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "http://192.168.1.6:8088")
	t.Setenv("PIPER_API_ADDR", "")
	stubNoLocalPiperd(t)
	fastPoll(t)
	if err := config.SaveClientFile(config.ClientFile{
		Boxes: []config.Box{
			{Name: "remote", Addr: "http://192.168.1.6:8088", BaseDomain: "remote.example"},
			{Name: "local", Addr: "http://127.0.0.1:8088", BaseDomain: "old-local.example"},
		},
		Current: "remote",
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "new-local.example", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "new-local.example", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	if code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir, reEnroll: true}, "cred", &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}

	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if got := cf.Boxes[1].BaseDomain; got != "new-local.example" {
		t.Fatalf("enrolled daemon identity landed on the wrong row: local=%q remote=%q; config=%+v", got, cf.Boxes[0].BaseDomain, cf)
	}
	if got := cf.Boxes[0].BaseDomain; got != "remote.example" {
		t.Fatalf("unrelated remote identity changed: %q", got)
	}
}

func TestEnrollFlowReportsSkippedIdentityWhenNoLocalRowMatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_API_ADDR", "")
	stubNoLocalPiperd(t)
	fastPoll(t)
	if err := config.SaveClientFile(config.ClientFile{
		Boxes:   []config.Box{{Name: "remote", Addr: "192.168.1.6:8088"}},
		Current: "remote",
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "new-local.example", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "new-local.example", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	if code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir, reEnroll: true}, "cred", &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "skipping relay identity") {
		t.Fatalf("stderr = %q, want a visible no-match skip", errb.String())
	}
}

// scriptedStatusSocket answers the first status poll with first and every later
// one with rest — the shape of an apply, where the pre-restart process keeps
// answering until the new one owns the socket. It returns the data dir that
// finds the socket, plus the poll counter.
func scriptedStatusSocket(t *testing.T, first, rest enrollapi.Status) (string, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "test"})
	})
	mux.HandleFunc("GET "+enrollapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		st := rest
		if calls.Add(1) == 1 {
			st = first
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	return startFakeEnrollSocket(t, mux), calls
}

// saveLocalBox points the client config at a fresh HOME holding one row for the
// local daemon — the row waitConnected persists the relay identity onto.
func saveLocalBox(t *testing.T, baseDomain string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_API_ADDR", "")
	if err := config.SaveClientFile(config.ClientFile{
		Boxes:   []config.Box{{Name: "local", Addr: "http://127.0.0.1:8088", BaseDomain: baseDomain}},
		Current: "local",
	}); err != nil {
		t.Fatal(err)
	}
}

func localBoxBaseDomain(t *testing.T) string {
	t.Helper()
	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	return cf.Boxes[0].BaseDomain
}

// The lost-response path has no expected identity, so the daemon's own answer
// is the only source there and every change to it must land — a one-shot latch
// would freeze the first (pre-restart) reading forever.
func TestWaitConnectedPersistsChangedIdentityWithNoExpectedDomain(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	saveLocalBox(t, "old.example")
	dataDir, _ := scriptedStatusSocket(t,
		enrollapi.Status{Enrolled: true, BaseDomain: "old.example", Tunnel: "retrying"},
		enrollapi.Status{Enrolled: true, BaseDomain: "new.example", Tunnel: "connected"})
	var out, errb bytes.Buffer
	if code := waitConnected(context.Background(), dataDir, "", &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if got := localBoxBaseDomain(t); got != "new.example" {
		t.Fatalf("latest observed daemon identity was not persisted: got %q", got)
	}
}

// During a --re-enroll apply the first poll can still reach the pre-restart
// process, which answers with the OLD identity and an old tunnel that reports
// "connected" until its session tears down. Taking that answer overwrites the
// just-persisted new identity and exits 0 on a box that is about to disappear.
func TestWaitConnectedRejectsPreRestartIdentitySnapshot(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	// The enroll response already persisted the new identity on this row.
	saveLocalBox(t, "new.example")
	dataDir, calls := scriptedStatusSocket(t,
		enrollapi.Status{Enrolled: true, BaseDomain: "old.example", RelayAddr: "old-relay:7000", Tunnel: "connected"},
		enrollapi.Status{Enrolled: true, BaseDomain: "new.example", RelayAddr: "new-relay:7000", Tunnel: "connected"})
	var out, errb bytes.Buffer
	if code := waitConnected(context.Background(), dataDir, "new.example", &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if got := localBoxBaseDomain(t); got != "new.example" {
		t.Fatalf("pre-restart identity overwrote the enrolled one: got %q", got)
	}
	if !strings.Contains(out.String(), "new.example") || strings.Contains(out.String(), "old.example") {
		t.Fatalf("stdout = %q, want the enrolled identity reported and the pre-restart one absent", out.String())
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("status polls = %d, want the pre-restart answer to be polled past", n)
	}
}

// Same snapshot, the other verdict it carries: a handshake rejection recorded
// by the process being replaced says nothing about the enrollment replacing it.
func TestWaitConnectedRejectsPreRestartTunnelError(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	saveLocalBox(t, "new.example")
	dataDir, _ := scriptedStatusSocket(t,
		enrollapi.Status{Enrolled: true, BaseDomain: "old.example", Tunnel: "retrying",
			LastTunnelError: "relay rejected this box's enrollment"},
		enrollapi.Status{Enrolled: true, BaseDomain: "new.example", Tunnel: "connected"})
	var out, errb bytes.Buffer
	if code := waitConnected(context.Background(), dataDir, "new.example", &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %q: a rejection recorded by the pre-restart process is not this enrollment's verdict", code, errb.String())
	}
	if !strings.Contains(out.String(), "new.example") {
		t.Fatalf("stdout = %q, want the enrolled identity reported live", out.String())
	}
}

// The pre-restart process can outlive the whole apply window. Nothing it says
// settles this enrollment — but the enrollment IS persisted (its own Enrolled
// reads the new relay.json), so the wait must still end advisory, not exit 1.
func TestWaitConnectedNeverSettlesOnAPersistentlyMismatchedDaemon(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	saveLocalBox(t, "new.example")
	stale := enrollapi.Status{Enrolled: true, BaseDomain: "old.example", RelayAddr: "old-relay:7000",
		Tunnel: "connected", LastTunnelError: "relay rejected this box's enrollment"}
	dataDir, calls := scriptedStatusSocket(t, stale, stale)
	var out, errb bytes.Buffer
	if code := waitConnected(context.Background(), dataDir, "new.example", &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %q: the enrollment persisted, so the wait stays advisory", code, errb.String())
	}
	if got := localBoxBaseDomain(t); got != "new.example" {
		t.Fatalf("pre-restart identity was adopted: got %q", got)
	}
	if strings.Contains(out.String(), "old.example") || strings.Contains(errb.String(), "rejected") {
		t.Fatalf("stdout = %q stderr = %q, want no verdict taken from the pre-restart process", out.String(), errb.String())
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("status polls = %d, want the loop to keep polling past the mismatch", n)
	}
}

func TestEnrollFlowQuotaMessage(t *testing.T) {
	stubNoLocalPiperd(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		return http.StatusTooManyRequests, enrollapi.ErrorResponse{Error: "quota"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	msg := errb.String()
	for _, want := range []string{"quota", "piper box ls", "piper box rm"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "upgrade") {
		t.Errorf("quota message must not say upgrade: %s", msg)
	}
}

func TestEnrollFlowEnvManagedIsInformational(t *testing.T) {
	stubNoLocalPiperd(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: true, EnvManaged: true,
		BaseDomain: "byo.example.com", Tunnel: "connected"})
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "operator-managed") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestEnrollFlowStaleEnrollmentSuggestsReEnroll(t *testing.T) {
	stubNoLocalPiperd(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "gone.public.getpiper.co", Tunnel: "retrying"})
	dataDir := startFakeEnrollSocket(t, f.mux())
	// The account's relay box list does NOT contain this base domain.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents" {
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer relay.Close()
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: relay.URL, dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errb.String(), "--re-enroll") {
		t.Fatalf("stderr = %q, want a --re-enroll hint", errb.String())
	}
}

func TestEnrollFlowReEnrollSendsReplace(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "old.public.getpiper.co", Tunnel: "connected"})
	f.enroll = func(req enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "new.public.getpiper.co", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "new.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir, reEnroll: true}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if got := f.got.Load().(enrollapi.EnrollRequest); !got.Replace {
		t.Fatalf("re-enroll must send replace=true, got %+v", got)
	}
}

func TestEnrollFlowAdvisoryWhenTunnelPending(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "b.public.getpiper.co",
			Tunnel: "retrying", LastTunnelError: "dial tcp: connection refused"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "b.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("pending tunnel must stay advisory (enrollment is durable); code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "retrying") && !strings.Contains(out.String(), "not connected yet") {
		t.Fatalf("stdout = %q, want the advisory note", out.String())
	}
}

// TestEnrollFlowInterruptStopsApplyWait pins #297's precedent onto the
// apply-wait loop: Ctrl-C during a retrying tunnel must stop the wait
// promptly, not sit out the full enrollApplyTimeout. enrollApplyTimeout here
// is set much longer than the cancellation delay (and than fastPoll's usual
// 300ms) specifically so that, pre-fix (ctx not threaded into waitConnected),
// the call visibly hangs for the whole deadline instead of returning quickly
// for an unrelated reason — that is what makes the elapsed-time assertion
// genuinely RED against the old code.
func TestEnrollFlowInterruptStopsApplyWait(t *testing.T) {
	stubNoLocalPiperd(t)
	oldTimeout, oldInterval, oldSleep := enrollApplyTimeout, enrollPollInterval, pollSleep
	enrollApplyTimeout = 5 * time.Second
	enrollPollInterval = 10 * time.Millisecond
	pollSleep = func(d time.Duration) { time.Sleep(d) }
	t.Cleanup(func() { enrollApplyTimeout, enrollPollInterval, pollSleep = oldTimeout, oldInterval, oldSleep })

	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		// The enroll POST succeeds and persists — the tunnel just stays
		// "retrying" forever, so the apply-wait loop keeps polling.
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "b.public.getpiper.co", Tunnel: "retrying"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "b.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var out, errb bytes.Buffer
	start := time.Now()
	code := enrollAfterLogin(ctx, enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("waitConnected took %v after cancellation, want a prompt return well under the %v deadline", elapsed, enrollApplyTimeout)
	}
	if !strings.Contains(out.String(), "still retrying in the background") {
		t.Fatalf("stdout = %q, want the advisory note", out.String())
	}
}

// A daemon that answers throughout but never reports the enrollment is a
// different failure from a daemon that vanished, and deserves a different
// diagnosis. This is the lost-response path's bad ending: the enroll POST's
// reply never arrived, the status poll settles it, and the claim simply did not
// persist. Telling the user "piperd did not come back" — and pointing them at
// service logs for a process that is plainly up — sends them the wrong way (#467).
func TestEnrollFlowLiveDaemonThatNeverEnrolledSaysSo(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	// Answers every status poll, always unenrolled.
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "b.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())

	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d, want 1; err = %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "did not come back") {
		t.Errorf("diagnosed a live daemon as vanished:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "did not persist") {
		t.Errorf("stderr = %q, want the claim-never-persisted diagnosis", errb.String())
	}
}

func TestEnrollFlowRejectedTokenIsDefinitive(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "b.public.getpiper.co",
			Tunnel: "retrying", LastTunnelError: "relay handshake: relay rejected b.public.getpiper.co: unknown or revoked enrollment"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "b.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("a recorded rejection is definitive; code = %d", code)
	}
	if !strings.Contains(errb.String(), "rejected") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

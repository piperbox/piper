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
	dir := t.TempDir()
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

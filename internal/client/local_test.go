package client

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/enrollapi"
)

// serveUnix runs handler on a unix socket in a temp dir and returns its path.
func serveUnix(t *testing.T, handler http.Handler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "piperd.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func TestUnixClientStatusAndEnroll(t *testing.T) {
	var gotEnroll enrollapi.EnrollRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "test"})
	})
	mux.HandleFunc("GET "+enrollapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	})
	mux.HandleFunc("POST "+enrollapi.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotEnroll)
		_ = json.NewEncoder(w).Encode(enrollapi.EnrollResponse{
			BaseDomain: "ab12-erin.public.getpiper.co", RelayAddr: "relay:7000"})
	})
	c := NewUnix(serveUnix(t, mux))

	if v, err := c.AgentVersion(); err != nil || v != "test" {
		t.Fatalf("AgentVersion over unix = %q, %v", v, err)
	}
	st, err := c.RelayStatus()
	if err != nil || st.Tunnel != "off" {
		t.Fatalf("RelayStatus = %+v, %v", st, err)
	}
	resp, err := c.EnrollRelay(enrollapi.EnrollRequest{RelayAPI: "https://api.relay", AccountCredential: "cred-xyz"})
	if err != nil || resp.BaseDomain != "ab12-erin.public.getpiper.co" {
		t.Fatalf("EnrollRelay = %+v, %v", resp, err)
	}
	if gotEnroll.RelayAPI != "https://api.relay" || gotEnroll.AccountCredential != "cred-xyz" {
		t.Fatalf("wire body = %+v", gotEnroll)
	}
}

func TestEnrollRelayMapsErrorCodes(t *testing.T) {
	cases := []struct {
		status int
		body   enrollapi.ErrorResponse
		check  func(error) bool
	}{
		{409, enrollapi.ErrorResponse{Error: "env-managed"}, func(e error) bool { return errors.Is(e, ErrEnvManaged) }},
		{409, enrollapi.ErrorResponse{Error: "busy"}, func(e error) bool { return errors.Is(e, ErrEnrollBusy) }},
		{401, enrollapi.ErrorResponse{Error: "bad-credential"}, func(e error) bool { return errors.Is(e, ErrEnrollCredential) }},
		{429, enrollapi.ErrorResponse{Error: "quota"}, func(e error) bool { return errors.Is(e, ErrEnrollQuota) }},
		{409, enrollapi.ErrorResponse{Error: "already-enrolled", BaseDomain: "b.example"}, func(e error) bool {
			var ae *AlreadyEnrolledError
			return errors.As(e, &ae) && ae.BaseDomain == "b.example"
		}},
	}
	for _, tc := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("POST "+enrollapi.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(tc.body)
		})
		c := NewUnix(serveUnix(t, mux))
		_, err := c.EnrollRelay(enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
		if err == nil || !tc.check(err) {
			t.Errorf("code %s: err = %v", tc.body.Error, err)
		}
	}
}

func TestRelayStatusUnsupportedOn404(t *testing.T) {
	c := NewUnix(serveUnix(t, http.NewServeMux())) // no routes at all
	if _, err := c.RelayStatus(); !errors.Is(err, ErrRelayStatusUnsupported) {
		t.Fatalf("err = %v, want ErrRelayStatusUnsupported", err)
	}
}

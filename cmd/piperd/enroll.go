package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/enrollapi"
	"github.com/piperbox/piper/internal/relayclient"
	"github.com/piperbox/piper/internal/tunnel"
)

// loadOrMintBoxID returns this box's durable identity, minting and persisting
// it on first use. It exists BEFORE the first relay call so a crashed enroll
// retries under the same identity — the relay upserts its agent row on
// (account, box_id), which is what makes `piper login` idempotent. It never
// leaves the machine except inside the enroll request.
func loadOrMintBoxID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "box-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// enrollDialTimeout bounds the validation dial, mirroring the tunnel client's
// own relayDialTimeout.
const enrollDialTimeout = 10 * time.Second

// validateEnrollment proves an enrollment works before anything is persisted:
// it dials the relay and completes a real tunnel handshake with the token,
// then closes the throwaway session. A bad endpoint or rejected token fails
// here — so a poisoned enrollment can never reach relay.json and crash-loop
// the next boot (validate-before-persist, one-command login design). The
// validation session is closed immediately; a transient second session for an
// already-connected box (the --re-enroll path) is accepted.
func validateEnrollment(ctx context.Context, relayAddr, token, baseDomain string) error {
	conn, err := (&net.Dialer{Timeout: enrollDialTimeout}).DialContext(ctx, "tcp", relayAddr)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	sess, err := tunnel.Dial(conn, token, baseDomain)
	if err != nil {
		conn.Close() // tunnel.Dial does not close conn on failure
		return fmt.Errorf("relay handshake: %w", err)
	}
	return sess.Close()
}

// enrollServer is the local enrollment surface: version + status + enroll,
// served ONLY on the piperd-owned unix socket — never on the TCP control API
// and never on the relay-facing authenticated listener (a route there would
// let a hostile relay re-point the box; see the one-command login design).
// Every side effect goes through a seam so the guards unit-test with fakes.
type enrollServer struct {
	dataDir string
	version string

	envManaged    func() bool
	relayStatus   func() (relayAddr, baseDomain string)
	tunnelStatus  func() (state, lastErr string)
	relayEnroll   func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error)
	validate      func(ctx context.Context, relayAddr, token, baseDomain string) error
	countBuilding func() (int64, error)
	apply         func() // async: hands off to main's drain-then-exec

	mu sync.Mutex // serializes enrolls; a held lock answers 409 busy
}

func (s *enrollServer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeEnrollJSON(w, http.StatusOK, map[string]string{"version": s.version})
	})
	m.HandleFunc("GET "+enrollapi.PathStatus, s.status)
	m.HandleFunc("POST "+enrollapi.PathEnroll, s.enroll)
	return m
}

// status is a fixed, secrets-free shape built field-by-field — never a marshal
// of config.RelayFile.
func (s *enrollServer) status(w http.ResponseWriter, r *http.Request) {
	_, found, _ := config.LoadRelayFile(s.dataDir)
	env := s.envManaged()
	st := enrollapi.Status{Enrolled: found || env, EnvManaged: env}
	if st.Enrolled {
		st.RelayAddr, st.BaseDomain = s.relayStatus()
	}
	st.Tunnel, st.LastTunnelError = s.tunnelStatus()
	writeEnrollJSON(w, http.StatusOK, st)
}

func (s *enrollServer) enroll(w http.ResponseWriter, r *http.Request) {
	var req enrollapi.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.RelayAPI == "" || req.AccountCredential == "" {
		writeEnrollError(w, http.StatusBadRequest, "bad-request",
			"relay_api and account_credential are required", "")
		return
	}
	if s.envManaged() {
		writeEnrollError(w, http.StatusConflict, "env-managed",
			"enrollment is pinned by PIPER_RELAY_* environment (see "+config.SystemEnvFile()+")", "")
		return
	}
	if !s.mu.TryLock() {
		writeEnrollError(w, http.StatusConflict, "busy", "another enrollment is in flight", "")
		return
	}
	defer s.mu.Unlock()
	if rf, found, err := config.LoadRelayFile(s.dataDir); err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	} else if found && !req.Replace {
		writeEnrollError(w, http.StatusConflict, "already-enrolled", "", rf.BaseDomain)
		return
	}
	if n, err := s.countBuilding(); err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	} else if n > 0 {
		writeEnrollError(w, http.StatusConflict, "busy",
			"a deployment is building; retry when it finishes", "")
		return
	}
	boxID, err := loadOrMintBoxID(s.dataDir)
	if err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	}
	en, err := s.relayEnroll(r.Context(), req.RelayAPI, req.AccountCredential, boxID, req.Org)
	switch {
	case errors.Is(err, relayclient.ErrBadCredential):
		writeEnrollError(w, http.StatusUnauthorized, "bad-credential",
			"relay rejected the account credential", "")
		return
	case errors.Is(err, relayclient.ErrQuotaExceeded):
		writeEnrollError(w, http.StatusTooManyRequests, "quota",
			"account agent quota exceeded", "")
		return
	case err != nil:
		writeEnrollError(w, http.StatusBadGateway, "enroll-failed", err.Error(), "")
		return
	}
	if err := s.validate(r.Context(), en.TunnelEndpoint, en.EnrollmentToken, en.BaseDomain); err != nil {
		// Nothing persisted: a poisoned enrollment must never reach relay.json.
		writeEnrollError(w, http.StatusBadGateway, "validate-failed", err.Error(), "")
		return
	}
	if err := config.SaveRelayFile(s.dataDir, config.RelayFile{
		RelayAddr: en.TunnelEndpoint, RelayToken: en.EnrollmentToken,
		BaseDomain: en.BaseDomain, Terminated: true,
		WebhookSecret: en.WebhookSecret, GitHubBrokered: en.GitHubApp,
	}); err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	}
	// Deliver the response completely before the apply tears this listener
	// down: explicit Content-Length + Flush, then hand off asynchronously (the
	// drain waits for this handler to return, so apply cannot run inline).
	body, _ := json.Marshal(enrollapi.EnrollResponse{BaseDomain: en.BaseDomain, RelayAddr: en.TunnelEndpoint})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Printf("enrolled as %s via %s (box %s); applying", en.BaseDomain, en.TunnelEndpoint, boxID)
	go s.apply()
}

func writeEnrollJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeEnrollError(w http.ResponseWriter, code int, errCode, detail, baseDomain string) {
	writeEnrollJSON(w, code, enrollapi.ErrorResponse{Error: errCode, Detail: detail, BaseDomain: baseDomain})
}

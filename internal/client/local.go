package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/piperbox/piper/internal/enrollapi"
)

// NewUnix returns a Client whose transport dials piperd's enrollment socket.
// The base host is a placeholder — unix sockets have no authority — and no
// bearer is attached: the socket's directory permissions are the trust
// boundary (one-command login design).
func NewUnix(socketPath string) *Client {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}
	return &Client{base: "http://piperd", http: &http.Client{Transport: t, Timeout: 10 * time.Second}, pollInterval: time.Second}
}

// ErrRelayStatusUnsupported means the daemon answered 404 for the relay-status
// route: a piperd from before the one-command login — stale process or old
// install (#375-style trap).
var ErrRelayStatusUnsupported = errors.New("piperd does not support relay status (too old?)")

// Sentinels for the enroll endpoint's machine error codes.
var (
	ErrEnvManaged       = errors.New("enrollment is env-managed on this box")
	ErrEnrollBusy       = errors.New("piperd is busy (deployment building or another enrollment in flight)")
	ErrEnrollCredential = errors.New("relay rejected the account credential")
	ErrEnrollQuota      = errors.New("account agent quota exceeded")
)

// AlreadyEnrolledError reports the box already holds an enrollment; BaseDomain
// is its current identity.
type AlreadyEnrolledError struct{ BaseDomain string }

func (e *AlreadyEnrolledError) Error() string {
	return "box already enrolled as " + e.BaseDomain
}

// RelayStatus reads the fixed, secrets-free relay state from the enrollment
// socket.
func (c *Client) RelayStatus() (enrollapi.Status, error) {
	resp, err := c.do(http.MethodGet, enrollapi.PathStatus, "", nil)
	if err != nil {
		return enrollapi.Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return enrollapi.Status{}, ErrRelayStatusUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return enrollapi.Status{}, responseError("relay status", resp)
	}
	var st enrollapi.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return enrollapi.Status{}, err
	}
	return st, nil
}

// EnrollRelay asks the local piperd to claim this box on the relay and apply
// the enrollment. piperd does the relay round-trip itself; the credential is
// used once and never persisted on the box.
func (c *Client) EnrollRelay(req enrollapi.EnrollRequest) (enrollapi.EnrollResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return enrollapi.EnrollResponse{}, err
	}
	// forAction, not the socket's ordinary timeout: this POST spans piperd's
	// relay round-trip, tunnel validation and persistence, which a Pi on a slow
	// uplink can carry past 10s. Losing the response there does not lose the
	// enrollment — the caller's status poll settles it — but it reports a
	// success as "enrollment response lost", which reads like a failure (#467).
	resp, err := c.doWith(c.forAction(), http.MethodPost, enrollapi.PathEnroll, "application/json", bytes.NewReader(body))
	if err != nil {
		return enrollapi.EnrollResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var out enrollapi.EnrollResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return enrollapi.EnrollResponse{}, err
		}
		return out, nil
	}
	var e enrollapi.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	switch e.Error {
	case "env-managed":
		return enrollapi.EnrollResponse{}, ErrEnvManaged
	case "already-enrolled":
		return enrollapi.EnrollResponse{}, &AlreadyEnrolledError{BaseDomain: e.BaseDomain}
	case "busy":
		return enrollapi.EnrollResponse{}, ErrEnrollBusy
	case "bad-credential":
		return enrollapi.EnrollResponse{}, ErrEnrollCredential
	case "quota":
		return enrollapi.EnrollResponse{}, ErrEnrollQuota
	default:
		if e.Detail != "" {
			return enrollapi.EnrollResponse{}, fmt.Errorf("enroll: %s: %s", resp.Status, e.Detail)
		}
		return enrollapi.EnrollResponse{}, fmt.Errorf("enroll: %s", resp.Status)
	}
}

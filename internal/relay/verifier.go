package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
)

// DeviceAuth is what a caller shows the user to complete an OAuth device flow.
type DeviceAuth struct {
	UserCode        string
	VerificationURI string
	Interval        int // seconds between polls
	ExpiresIn       int // seconds until the device code expires
}

// Identity is the verified subject of a completed login.
type Identity struct {
	Subject string // stable IdP user id (GitHub numeric id, as a decimal string)
	Login   string // GitHub login; source of the derived username
}

// ErrAuthPending means the user has not yet completed the device flow.
var ErrAuthPending = errors.New("authorization_pending")

// Verifier brokers an OAuth device flow with an identity provider. Start begins a
// flow and returns an opaque handle; Poll reports ErrAuthPending until the user
// finishes, then the verified Identity.
type Verifier interface {
	Start(ctx context.Context) (handle string, d DeviceAuth, err error)
	Poll(ctx context.Context, handle string) (Identity, error)
}

// WebVerifier brokers the browser authorization-code flow with the identity
// provider: AuthCodeURL is where /v1/login/web redirects the browser, and
// Exchange resolves the code GitHub posts back to /v1/login/callback.
type WebVerifier interface {
	AuthCodeURL(state string) string
	Exchange(ctx context.Context, code string) (Identity, error)
}

// FakeVerifier is an in-memory Verifier for tests. Approve completes a flow.
type FakeVerifier struct {
	mu       sync.Mutex
	approved map[string]Identity
	started  map[string]bool
	codes    map[string]Identity // web-flow codes granted via GrantCode
	auto     *Identity           // when set, Poll auto-approves any started handle (test-only)
}

func NewFakeVerifier() *FakeVerifier {
	return &FakeVerifier{
		approved: map[string]Identity{},
		started:  map[string]bool{},
		codes:    map[string]Identity{},
	}
}

// NewAutoApproveVerifier is a FakeVerifier whose device-flow poll completes
// immediately with a canned identity. It exists so the loopback e2e can drive
// `piper login` (login + claim) end-to-end without a real GitHub IdP. NEVER
// selected in production: main.go uses it only under
// PIPER_RELAY_FAKE_APPROVE=1 and only when no real GitHub client ID is
// configured.
func NewAutoApproveVerifier(sub, login string) *FakeVerifier {
	f := NewFakeVerifier()
	f.auto = &Identity{Subject: sub, Login: login}
	return f
}

func (f *FakeVerifier) Start(context.Context) (string, DeviceAuth, error) {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	handle := hex.EncodeToString(raw)
	f.mu.Lock()
	f.started[handle] = true
	f.mu.Unlock()
	return handle, DeviceAuth{
		UserCode:        "FAKE-CODE",
		VerificationURI: "https://example.test/device",
		Interval:        1,
		ExpiresIn:       300,
	}, nil
}

func (f *FakeVerifier) Poll(_ context.Context, handle string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started[handle] {
		return Identity{}, errors.New("unknown handle")
	}
	if id, ok := f.approved[handle]; ok {
		return id, nil
	}
	if f.auto != nil {
		return *f.auto, nil
	}
	return Identity{}, ErrAuthPending
}

// Approve marks a started handle complete with the given identity (test helper).
func (f *FakeVerifier) Approve(handle string, id Identity) {
	f.mu.Lock()
	f.approved[handle] = id
	f.mu.Unlock()
}

func (f *FakeVerifier) AuthCodeURL(state string) string {
	return "https://github.example.test/login/oauth/authorize?state=" + url.QueryEscape(state)
}

func (f *FakeVerifier) Exchange(_ context.Context, code string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.codes[code]; ok {
		return id, nil
	}
	return Identity{}, errors.New("bad code")
}

// GrantCode makes a web-flow code exchangeable for id (test helper).
func (f *FakeVerifier) GrantCode(code string, id Identity) {
	f.mu.Lock()
	f.codes[code] = id
	f.mu.Unlock()
}

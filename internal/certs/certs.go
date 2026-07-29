package certs

import (
	"crypto"
	"crypto/ecdsa"
	"fmt"
	"reflect"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

// Config configures ACME issuance. Exactly one challenge mode must be set:
// DNSProvider (DNS-01, any lego challenge provider — needed for wildcard
// certs, box-wide BYO) or ALPNSolver (TLS-ALPN-01 — tokenless exact-host
// certs, per-app BYO). AccountKey is the persisted ACME account key.
type Config struct {
	Email       string
	CADirURL    string
	DNSProvider challenge.Provider
	ALPNSolver  challenge.Provider
	AccountKey  *ecdsa.PrivateKey
}

// Manager obtains certificates via ACME (DNS-01 or TLS-ALPN-01).
type Manager struct {
	client *lego.Client
}

// user implements lego's registration.User.
type user struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (u *user) GetEmail() string                        { return u.email }
func (u *user) GetRegistration() *registration.Resource { return u.reg }
func (u *user) GetPrivateKey() crypto.PrivateKey        { return u.key }

// challengeSet reports whether p holds a usable provider. A typed nil — a
// nil pointer boxed into the interface — compares non-nil with == but would
// only blow up later inside lego, so it counts as unset (#242).
func challengeSet(p challenge.Provider) bool {
	if p == nil {
		return false
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return !v.IsNil()
	default:
		return true
	}
}

// New builds a Manager and registers the ACME account.
func New(cfg Config) (*Manager, error) {
	if challengeSet(cfg.DNSProvider) == challengeSet(cfg.ALPNSolver) {
		return nil, fmt.Errorf("certs: exactly one of DNSProvider or ALPNSolver must be set")
	}
	u := &user{email: cfg.Email, key: cfg.AccountKey}
	lc := lego.NewConfig(u)
	if cfg.CADirURL != "" {
		lc.CADirURL = cfg.CADirURL
	}
	client, err := lego.NewClient(lc)
	if err != nil {
		return nil, err
	}
	if challengeSet(cfg.DNSProvider) {
		if err := client.Challenge.SetDNS01Provider(cfg.DNSProvider); err != nil {
			return nil, err
		}
	} else {
		if err := client.Challenge.SetTLSALPN01Provider(cfg.ALPNSolver); err != nil {
			return nil, err
		}
	}
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, err
	}
	u.reg = reg
	return &Manager{client: client}, nil
}

// Obtain returns a PEM-encoded certificate chain and private key covering the
// given domains (e.g. []string{"*.alice.example.com", "alice.example.com"}).
func (m *Manager) Obtain(domains []string) (certPEM, keyPEM []byte, err error) {
	res, err := m.client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: domains,
		Bundle:  true,
	})
	if err != nil {
		return nil, nil, err
	}
	return res.Certificate, res.PrivateKey, nil
}

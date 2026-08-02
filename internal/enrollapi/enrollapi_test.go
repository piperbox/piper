package enrollapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// The status shape is a fixed, secrets-free contract: it must marshal exactly
// these keys and nothing else (never a RelayFile), so a token can never leak
// through it by accident.
func TestStatusMarshalsFixedShape(t *testing.T) {
	b, err := json.Marshal(Status{Enrolled: true, EnvManaged: false,
		RelayAddr: "relay:7000", BaseDomain: "ab12-erin.public.getpiper.co",
		Tunnel: "retrying", LastTunnelError: "dial tcp: refused"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"enrolled":true`, `"relay_addr"`, `"base_domain"`, `"tunnel":"retrying"`, `"last_tunnel_error"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshal missing %s in %s", want, b)
		}
	}
	for _, banned := range []string{"token", "secret", "credential"} {
		if strings.Contains(string(b), banned) {
			t.Errorf("status shape must never carry %q: %s", banned, b)
		}
	}
}

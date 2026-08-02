// Package enrollapi defines the wire contract of piperd's local enrollment
// socket: the surface `piper login` uses to hand a claimed enrollment to the
// daemon (one-command login design). It is a types-only leaf shared by
// cmd/piperd (server) and internal/client (CLI); the routes are deliberately
// NOT part of internal/api — they must never be reachable through the relay
// tunnel or the TCP control listeners.
package enrollapi

// Route paths on the enrollment socket. GET /v1/version also answers there,
// mirroring the control API's shape, so the CLI can positively identify piperd
// before sending anything.
const (
	PathStatus = "/v1/relay/status"
	PathEnroll = "/v1/relay/enroll"
)

// EnrollRequest asks piperd to claim this box on a relay. The account
// credential is used for the single relay call and never persisted on the box.
type EnrollRequest struct {
	RelayAPI          string `json:"relay_api"`
	AccountCredential string `json:"account_credential"`
	Org               string `json:"org,omitempty"`
	Replace           bool   `json:"replace,omitempty"`
}

// EnrollResponse reports a persisted (validated) enrollment.
type EnrollResponse struct {
	BaseDomain string `json:"base_domain"`
	RelayAddr  string `json:"relay_addr"`
}

// ErrorResponse is the JSON body of every non-2xx enrollment answer. Error is
// a machine code: env-managed, already-enrolled, busy, bad-credential, quota,
// validate-failed, enroll-failed, bad-request. BaseDomain rides along on
// already-enrolled so the CLI can report the current identity.
type ErrorResponse struct {
	Error      string `json:"error"`
	Detail     string `json:"detail,omitempty"`
	BaseDomain string `json:"base_domain,omitempty"`
}

// Status is the fixed, secrets-free view of the box's relay state. It is built
// field-by-field — never a marshal of config.RelayFile — so the enrollment
// token and webhook secret cannot leak through it. Tunnel is one of
// "connected", "retrying" (relay mode up, not currently connected), "off"
// (no relay mode).
type Status struct {
	Enrolled        bool   `json:"enrolled"`
	EnvManaged      bool   `json:"env_managed"`
	RelayAddr       string `json:"relay_addr,omitempty"`
	BaseDomain      string `json:"base_domain,omitempty"`
	Tunnel          string `json:"tunnel"`
	LastTunnelError string `json:"last_tunnel_error,omitempty"`
}

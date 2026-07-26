// Package tunnel multiplexes streams over a single connection between the agent
// and the relay. The agent dials out (beating NAT/CGNAT), presents a token and
// its base domain, and both ends open/accept yamux streams over that link.
package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// preAuthReadTimeout bounds the unauthenticated handshake read on the relay's
// tunnel listener. Without it a client that connects and sends nothing pins a
// goroutine + fd forever (slowloris / fd-exhaustion). Tests override it to a
// tiny value. Cleared once the handshake is in hand. The trusted agent Dial
// path is intentionally not deadlined.
var preAuthReadTimeout = 10 * time.Second

// ackReadTimeout bounds the agent's wait for the relay's handshake ack. Tests
// override it to a tiny value.
var ackReadTimeout = 10 * time.Second

// Auth validates a client's presented token and claimed base domain. A non-nil
// return rejects the connection.
type Auth func(token, baseDomain string) error

type handshake struct {
	Token      string `json:"token"`
	BaseDomain string `json:"base_domain"`
}

// Session is a live multiplexed link. Open (server→agent) and Accept (agent
// side) yield net.Conn streams.
type Session struct {
	BaseDomain string
	mux        *yamux.Session
}

func (s *Session) Open() (net.Conn, error)    { return s.mux.Open() }
func (s *Session) Accept() (net.Conn, error)  { return s.mux.Accept() }
func (s *Session) CloseChan() <-chan struct{} { return s.mux.CloseChan() }
func (s *Session) Close() error               { return s.mux.Close() }

// Closed non-blockingly reports whether the session has been torn down. A
// zero-value Session (no mux) reports false, so callers can probe a session
// without racing its construction.
func (s *Session) Closed() bool {
	if s.mux == nil {
		return false
	}
	select {
	case <-s.mux.CloseChan():
		return true
	default:
		return false
	}
}

// writeFrame writes a uint16-length-prefixed payload. Length-prefixing (rather
// than a json.Decoder) guarantees we consume exactly the handshake bytes and
// leave the rest of the stream untouched for yamux.
func writeFrame(w io.Writer, b []byte) error {
	if len(b) > 0xffff {
		return fmt.Errorf("handshake too large")
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// rejectedReason is the single reason the relay reports for any failed
// handshake. It is deliberately undifferentiated: the peer is unauthenticated
// at this point, so telling it whether the token was wrong or the base domain
// unknown would confirm which enrollments exist. The relay logs the specific
// cause locally instead.
const rejectedReason = "unknown or revoked enrollment"

// handshakeAck is the relay's verdict on a handshake, sent before yamux starts.
// An empty Error means accepted.
type handshakeAck struct {
	Error string `json:"error,omitempty"`
}

// Dial performs the client handshake over conn, waits for the relay's verdict,
// then starts a yamux client.
//
// The ack is what makes a rejection visible. Without it Dial returned a
// live-looking Session as soon as the handshake frame was written, so an agent
// whose enrollment the relay had dropped reported "connected" on every retry
// forever while the relay silently closed each connection — the failure was
// invisible on both ends (#400). Note the ack proves the handshake was
// accepted, not that the session will survive: the relay can still evict it
// afterwards (a disabled account), which it logs itself.
func Dial(conn net.Conn, token, baseDomain string) (*Session, error) {
	payload, _ := json.Marshal(handshake{Token: token, BaseDomain: baseDomain})
	if err := writeFrame(conn, payload); err != nil {
		return nil, err
	}
	// Bound the wait: a relay that accepts the connection and then goes quiet
	// must not pin the agent, or the reconnect loop can never retry.
	_ = conn.SetReadDeadline(time.Now().Add(ackReadTimeout))
	ackPayload, err := readFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("awaiting relay handshake ack: %w", err)
	}
	var ack handshakeAck
	if err := json.Unmarshal(ackPayload, &ack); err != nil {
		return nil, fmt.Errorf("malformed relay handshake ack: %w", err)
	}
	if ack.Error != "" {
		return nil, fmt.Errorf("relay rejected %s: %s", baseDomain, ack.Error)
	}
	mux, err := yamux.Client(conn, nil)
	if err != nil {
		return nil, err
	}
	return &Session{BaseDomain: baseDomain, mux: mux}, nil
}

// Stream kinds: every stream opens with a single kind byte so each end can
// dispatch by purpose. The agent opens only Control streams; the relay opens
// Passthrough, HTTP, and ControlAPI streams.
const (
	KindPassthrough byte = 'T' // relay→agent: replayed ClientHello follows; agent pipes to :443
	KindHTTP        byte = 'H' // relay→agent: relay-terminated plaintext HTTP; agent pipes to :80
	KindControl     byte = 'C' // agent→relay: a length-prefixed ControlRequest/ControlResponse
	KindControlAPI  byte = 'A' // relay→agent: a forwarded control-plane HTTP request; agent pipes to the control API
)

// OpenKind opens a new stream and writes its kind byte.
func (s *Session) OpenKind(kind byte) (net.Conn, error) {
	c, err := s.mux.Open()
	if err != nil {
		return nil, err
	}
	if _, err := c.Write([]byte{kind}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// AcceptKind accepts a stream and reads its leading kind byte.
func (s *Session) AcceptKind() (byte, net.Conn, error) {
	c, err := s.mux.Accept()
	if err != nil {
		return 0, nil, err
	}
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		c.Close()
		return 0, nil, err
	}
	return b[0], c, nil
}

// ControlRequest is an agent→relay control message on a KindControl stream.
type ControlRequest struct {
	Op       string `json:"op"` // "register" | "deregister" | "provision" | "add-domain" | "remove-domain" | "domain-active" | "bind-repo" | "unbind-repo" | "gh-token"
	App      string `json:"app,omitempty"`
	PR       int    `json:"pr,omitempty"` // "register": PR number for a preview host; 0 is production
	Hostname string `json:"hostname,omitempty"`
	Token    string `json:"token,omitempty"`  // "provision": the box's control-API bearer for the relay to inject
	Domain   string `json:"domain,omitempty"` // custom domain for add/remove/active operations
	Repo     string `json:"repo,omitempty"`   // "owner/name" for bind-repo and gh-token
	Branch   string `json:"branch,omitempty"` // tracked branch for bind-repo
}

// ControlResponse is the relay's reply. Error is non-empty on failure.
type ControlResponse struct {
	Hostname string `json:"hostname,omitempty"`
	Error    string `json:"error,omitempty"`
	Token    string `json:"token,omitempty"`   // "gh-token": repo-scoped installation token
	Expires  string `json:"expires,omitempty"` // "gh-token": RFC3339 expiry
}

// WriteMsg writes v as a single length-prefixed JSON frame.
func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, b)
}

// ReadMsg reads one length-prefixed JSON frame into v.
func ReadMsg(r io.Reader, v any) error {
	b, err := readFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Serve reads the client handshake over conn, authorizes it, then starts a
// yamux server. On auth failure it returns the auth error (caller closes conn).
func Serve(conn net.Conn, auth Auth) (*Session, error) {
	// Deadline the unauthenticated handshake read; clear it once the frame is in
	// hand so the established yamux session isn't killed mid-traffic.
	_ = conn.SetReadDeadline(time.Now().Add(preAuthReadTimeout))

	payload, err := readFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	var hs handshake
	if err := json.Unmarshal(payload, &hs); err != nil {
		return nil, err
	}
	if err := auth(hs.Token, hs.BaseDomain); err != nil {
		// Best-effort: tell the agent *why* before dropping it, so a stranded
		// enrollment is self-diagnosing instead of an invisible reconnect loop
		// (#400). A failed write changes nothing — the connection is going away.
		_ = conn.SetWriteDeadline(time.Now().Add(ackReadTimeout))
		ackPayload, _ := json.Marshal(handshakeAck{Error: rejectedReason})
		_ = writeFrame(conn, ackPayload)
		_ = conn.SetWriteDeadline(time.Time{})
		return nil, err
	}
	ackPayload, _ := json.Marshal(handshakeAck{})
	if err := writeFrame(conn, ackPayload); err != nil {
		return nil, err
	}
	mux, err := yamux.Server(conn, nil)
	if err != nil {
		return nil, err
	}
	return &Session{BaseDomain: hs.BaseDomain, mux: mux}, nil
}

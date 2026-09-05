// Package tunnel multiplexes streams over a single connection between the agent
// and the relay. The agent dials out (beating NAT/CGNAT), presents a token and
// its base domain, and both ends open/accept yamux streams over that link.
package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
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

// handshakeWriteTimeout bounds a handshake-frame write: the agent's handshake
// in Dial and the relay's success ack in Serve. Tests override it to a tiny
// value.
var handshakeWriteTimeout = 10 * time.Second

// Auth validates a client's presented token and claimed base domain. A non-nil
// return rejects the connection.
type Auth func(token, baseDomain string) error

// The handshake is two frames, written in one flush by Dial. The preface is
// the routing key: it carries only the base domain, so the edge can peek it
// (ReadPreface) and place the dial without ever reading a credential — the
// same shape as SNI on :443 and the Host line on :80. The credential frame
// carries the token and nothing else, so the two frames cannot disagree
// about the base; the relay's auth check that the token's enrolled base
// equals the claimed one is what makes the unauthenticated preface
// trustworthy once auth passes.
type preface struct {
	BaseDomain string `json:"base_domain"`
}

type credential struct {
	Token string `json:"token"`
}

// ErrDuplicateSession is what a relay's Auth returns when it already holds a
// session for the claimed base. It is the one rejection Serve names to the
// peer (duplicateReason): it can only be raised after the token was
// validated, so naming it confirms nothing to an unauthenticated peer. Dial
// maps the reason back to this sentinel so the agent can errors.Is it.
var ErrDuplicateSession = errors.New("relay already holds a session for this agent")

// duplicateReason is the ack text for ErrDuplicateSession.
const duplicateReason = "duplicate session"

// Session is a live multiplexed link. Open (server→agent) and Accept (agent
// side) yield net.Conn streams.
type Session struct {
	BaseDomain   string
	ObservedAddr string // host the relay saw this connection from; "" if unknown
	mux          *yamux.Session
}

func (s *Session) Open() (net.Conn, error)    { return s.mux.Open() }
func (s *Session) Accept() (net.Conn, error)  { return s.mux.Accept() }
func (s *Session) CloseChan() <-chan struct{} { return s.mux.CloseChan() }
func (s *Session) Close() error               { return s.mux.Close() }

// NumStreams is the number of streams open on the link, whichever end
// opened them. A draining relay closes a session only when this reads zero,
// so every relay-side opener must close its stream when done or the wait
// runs to its deadline.
func (s *Session) NumStreams() int { return s.mux.NumStreams() }

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

// readFrameRaw reads one frame and returns both its payload and the raw
// bytes (header + payload) that carried it.
func readFrameRaw(r io.Reader) (payload, raw []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, nil, err
	}
	return buf, append(hdr[:], buf...), nil
}

func readFrame(r io.Reader) ([]byte, error) {
	payload, _, err := readFrameRaw(r)
	return payload, err
}

// ReadPreface reads exactly the preface frame from r and returns the base
// domain it names plus the frame's raw bytes (length header included), so a
// peeking proxy can replay them verbatim. Nothing past the preface is
// consumed. An empty base is an error: the frame is the routing key and a
// blank one routes nowhere.
func ReadPreface(r io.Reader) (string, []byte, error) {
	payload, raw, err := readFrameRaw(r)
	if err != nil {
		return "", nil, err
	}
	var p preface
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", nil, fmt.Errorf("malformed preface: %w", err)
	}
	if p.BaseDomain == "" {
		return "", nil, errors.New("preface names no base domain")
	}
	return p.BaseDomain, raw, nil
}

// rejectedReason is the single reason the relay reports for any failed
// handshake. It is deliberately undifferentiated: the peer is unauthenticated
// at this point, so telling it whether the token was wrong or the base domain
// unknown would confirm which enrollments exist. The relay logs the specific
// cause locally instead.
const rejectedReason = "unknown or revoked enrollment"

// handshakeAck is the relay's verdict on a handshake, sent before yamux starts.
// An empty Error means accepted. ObservedAddr is the source host (no port) the
// relay accepted the connection from — the agent's best guess at its own
// public IP for direct serve mode; advisory, since it can be a NAT egress.
type handshakeAck struct {
	Error        string `json:"error,omitempty"`
	ObservedAddr string `json:"observed_addr,omitempty"`
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
	prefacePayload, _ := json.Marshal(preface{BaseDomain: baseDomain})
	credPayload, _ := json.Marshal(credential{Token: token})
	// Bound the write like the ack wait below: a relay that accepts the
	// connection and then stops reading lets the send buffer and receive
	// window fill, and an undeadlined write pins the reconnect loop the
	// same way an undeadlined read does. Both frames go under one deadline
	// and, on a TCP conn, out in one flush.
	_ = conn.SetWriteDeadline(time.Now().Add(handshakeWriteTimeout))
	writeErr := writeFrame(conn, prefacePayload)
	if writeErr == nil {
		writeErr = writeFrame(conn, credPayload)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if writeErr != nil {
		return nil, fmt.Errorf("writing handshake: %w", writeErr)
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
	if ack.Error == duplicateReason {
		return nil, fmt.Errorf("relay rejected %s: %w", baseDomain, ErrDuplicateSession)
	}
	if ack.Error != "" {
		return nil, fmt.Errorf("relay rejected %s: %s", baseDomain, ack.Error)
	}
	mux, err := yamux.Client(conn, nil)
	if err != nil {
		return nil, err
	}
	return &Session{BaseDomain: baseDomain, ObservedAddr: ack.ObservedAddr, mux: mux}, nil
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

// AppRef names one hostname-holding slot on a box: an app, plus the PR number
// for a preview (0 is production). "sync-apps" carries the box's whole set.
type AppRef struct {
	App string `json:"app"`
	PR  int    `json:"pr,omitempty"`
}

// AppHost is one slot's assigned hostname, as "sync-apps" answers it. The relay
// is the authority on what each slot is called, and the name can change under a
// box (the hash keys on the agent since #405), so the box persists what comes
// back rather than trusting the copy it stored at deploy time.
type AppHost struct {
	App      string `json:"app"`
	PR       int    `json:"pr,omitempty"`
	Hostname string `json:"hostname"`
}

// ControlRequest is an agent→relay control message on a KindControl stream.
type ControlRequest struct {
	Op       string   `json:"op"` // "register" | "deregister" | "sync-apps" | "provision" | "add-domain" | "remove-domain" | "domain-active" | "bind-repo" | "unbind-repo" | "gh-token"
	App      string   `json:"app,omitempty"`
	PR       int      `json:"pr,omitempty"` // "register": PR number for a preview host; 0 is production
	Hostname string   `json:"hostname,omitempty"`
	Token    string   `json:"token,omitempty"`  // "provision": the box's control-API bearer for the relay to inject
	Domain   string   `json:"domain,omitempty"` // custom domain for add/remove/active operations
	Repo     string   `json:"repo,omitempty"`   // "owner/name" for bind-repo and gh-token
	Branch   string   `json:"branch,omitempty"` // tracked branch for bind-repo
	Apps     []AppRef `json:"apps,omitempty"`   // "sync-apps": every slot the box holds
}

// ControlResponse is the relay's reply. Error is non-empty on failure.
type ControlResponse struct {
	Hostname string    `json:"hostname,omitempty"`
	Apps     []AppHost `json:"apps,omitempty"` // "sync-apps": the assigned hostname per surviving slot
	Error    string    `json:"error,omitempty"`
	Token    string    `json:"token,omitempty"`   // "gh-token": repo-scoped installation token
	Expires  string    `json:"expires,omitempty"` // "gh-token": RFC3339 expiry
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
	base, _, err := ReadPreface(conn)
	if err != nil {
		_ = conn.SetReadDeadline(time.Time{})
		return nil, err
	}
	credPayload, err := readFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	var cred credential
	if err := json.Unmarshal(credPayload, &cred); err != nil {
		return nil, err
	}
	if err := auth(cred.Token, base); err != nil {
		// Best-effort: tell the agent *why* before dropping it, so a stranded
		// enrollment is self-diagnosing instead of an invisible reconnect loop
		// (#400). A failed write changes nothing — the connection is going away.
		// Only a duplicate is named: it is raised after the token was
		// validated, so it confirms nothing to an unauthenticated peer.
		reason := rejectedReason
		if errors.Is(err, ErrDuplicateSession) {
			reason = duplicateReason
		}
		_ = conn.SetWriteDeadline(time.Now().Add(ackReadTimeout))
		ackPayload, _ := json.Marshal(handshakeAck{Error: reason})
		_ = writeFrame(conn, ackPayload)
		_ = conn.SetWriteDeadline(time.Time{})
		return nil, err
	}
	// Bound the ack write the way the rejection path above already is: an agent
	// that completes the handshake and then stops reading lets the send buffer
	// and receive window fill, and an undeadlined write pins this accept before
	// yamux.Server is ever reached. Cleared once the frame is out, or it would
	// later expire mid-session and kill a healthy connection.
	_ = conn.SetWriteDeadline(time.Now().Add(handshakeWriteTimeout))
	observed := ""
	if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
		observed = host
	}
	ackPayload, _ := json.Marshal(handshakeAck{ObservedAddr: observed})
	writeErr := writeFrame(conn, ackPayload)
	_ = conn.SetWriteDeadline(time.Time{})
	if writeErr != nil {
		return nil, fmt.Errorf("writing handshake ack: %w", writeErr)
	}
	mux, err := yamux.Server(conn, nil)
	if err != nil {
		return nil, err
	}
	return &Session{BaseDomain: base, ObservedAddr: observed, mux: mux}, nil
}

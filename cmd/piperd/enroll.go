package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

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

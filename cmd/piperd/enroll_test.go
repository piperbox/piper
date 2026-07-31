package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOrMintBoxIDIsDurable(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrMintBoxID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("box id = %q, want 32 hex chars", first)
	}
	second, err := loadOrMintBoxID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("box id not durable: %q -> %q", first, second)
	}
	fi, err := os.Stat(filepath.Join(dir, "box-id"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("box-id mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestValidateEnrollmentFailsOnUnreachableRelay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := validateEnrollment(ctx, addr, "tok", "base.example"); err == nil {
		t.Fatal("validate against a dead relay must fail")
	}
}

func TestValidateEnrollmentFailsOnSilentRelay(t *testing.T) {
	// A listener that accepts and says nothing: tunnel.Dial must give up on
	// the ack wait, not report success.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	ctx := context.Background()
	err = validateEnrollment(ctx, ln.Addr().String(), "tok", "base.example")
	if err == nil {
		t.Fatal("validate against a silent relay must fail")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("err = %v, want a handshake failure", err)
	}
}

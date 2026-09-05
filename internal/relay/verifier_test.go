package relay

import (
	"context"
	"strings"
	"testing"
)

func TestFakeVerifierStartPollApprove(t *testing.T) {
	f := NewFakeVerifier()
	ctx := context.Background()

	handle, d, err := f.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle == "" || d.UserCode == "" || d.VerificationURI == "" {
		t.Fatalf("empty device auth: %q %+v", handle, d)
	}

	if _, err := f.Poll(ctx, handle); err != ErrAuthPending {
		t.Fatalf("pre-approval Poll err = %v, want ErrAuthPending", err)
	}

	f.Approve(handle, Identity{Subject: "sub-1", Login: "heidi"})
	id, err := f.Poll(ctx, handle)
	if err != nil {
		t.Fatalf("post-approval Poll: %v", err)
	}
	if id.Subject != "sub-1" || id.Login != "heidi" {
		t.Fatalf("identity = %+v", id)
	}

	if _, err := f.Poll(ctx, "unknown-handle"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}

func TestFakeVerifierWebFlow(t *testing.T) {
	f := NewFakeVerifier()

	if got := f.AuthCodeURL("st-1"); !strings.Contains(got, "state=st-1") {
		t.Fatalf("AuthCodeURL = %q, want state embedded", got)
	}

	if _, err := f.Exchange(context.Background(), "unknown-code"); err == nil {
		t.Fatal("Exchange(unknown) succeeded, want error")
	}

	f.GrantCode("code-1", Identity{Subject: "sub-1", Login: "heidi"})
	id, err := f.Exchange(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Login != "heidi" {
		t.Fatalf("identity = %+v", id)
	}

	// Both verifiers satisfy WebVerifier.
	var _ WebVerifier = f
	var _ WebVerifier = NewGitHubVerifier("id", "secret", nil)
}

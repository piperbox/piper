package relay

import (
	"testing"
	"time"
)

func TestWebStateTakeIsSingleUse(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutWebState("s1", "https://dash.getpiper.co/auth", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	ru, ok, err := st.TakeWebState("s1")
	if err != nil || !ok || ru != "https://dash.getpiper.co/auth" {
		t.Fatalf("first take = (%q, %v, %v)", ru, ok, err)
	}
	if _, ok, err := st.TakeWebState("s1"); err != nil || ok {
		t.Fatalf("second take ok=%v err=%v, want not found", ok, err)
	}
}

func TestWebStateExpiredIsInvisibleAndSwept(t *testing.T) {
	st := openTestStore(t)
	// A negative TTL is already expired the moment it lands.
	if err := st.PutWebState("stale", "https://dash.getpiper.co/x", -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.TakeWebState("stale"); ok {
		t.Fatal("expired state was redeemable")
	}
	if err := st.PutWebState("fresh", "https://dash.getpiper.co/y", time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM login_web_states`); n != 1 {
		t.Fatalf("rows after sweep = %d, want 1 (only the fresh state)", n)
	}
}

func TestCLIHandleStateMachine(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("42", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutCLIHandle("h1", "ABCD-1234", 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	// Pending until confirmed and finished.
	if _, _, state, err := st.TakeFinishedCLIHandle("h1"); err != nil || state != cliHandlePending {
		t.Fatalf("take before confirm = state %v err %v, want pending", state, err)
	}
	// Finishing an unconfirmed handle is refused.
	if err := st.FinishCLIHandle("h1", acc.ID); err == nil {
		t.Fatal("FinishCLIHandle succeeded on an unconfirmed handle")
	}

	// Code entry is forgiving (case, dashes) and confirms exactly once.
	h, ok, err := st.ConfirmCLIHandle("abcd1234")
	if err != nil || !ok || h != "h1" {
		t.Fatalf("confirm = (%q, %v, %v)", h, ok, err)
	}
	if _, ok, _ := st.ConfirmCLIHandle("ABCD-1234"); ok {
		t.Fatal("second confirm of the same code succeeded")
	}
	row, ok, err := st.CLIHandle("h1")
	if err != nil || !ok || !row.Confirmed || row.AccountID != "" {
		t.Fatalf("CLIHandle after confirm = %+v ok=%v err=%v", row, ok, err)
	}

	if err := st.FinishCLIHandle("h1", acc.ID); err != nil {
		t.Fatalf("FinishCLIHandle: %v", err)
	}
	id, username, state, err := st.TakeFinishedCLIHandle("h1")
	if err != nil || state != cliHandleDone || id != acc.ID || username != "alice" {
		t.Fatalf("take after finish = (%q, %q, %v, %v)", id, username, state, err)
	}
	// Single use.
	if _, _, state, _ := st.TakeFinishedCLIHandle("h1"); state != cliHandleUnknown {
		t.Fatalf("second take state = %v, want unknown", state)
	}
}

func TestCLIHandleWrongOrEmptyCodeDoesNotConfirm(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutCLIHandle("h1", "ABCD-1234", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"", "WXYZ-9999", "ABCD-123"} {
		if _, ok, err := st.ConfirmCLIHandle(code); err != nil || ok {
			t.Fatalf("ConfirmCLIHandle(%q) = ok %v err %v, want no match", code, ok, err)
		}
	}
}

func TestCLIHandleExpiredIsInvisibleAndSwept(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutCLIHandle("stale", "ABCD-1234", -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.ConfirmCLIHandle("ABCD-1234"); ok {
		t.Fatal("expired handle confirmed")
	}
	if _, ok, _ := st.CLIHandle("stale"); ok {
		t.Fatal("expired handle readable")
	}
	if _, _, state, _ := st.TakeFinishedCLIHandle("stale"); state != cliHandleUnknown {
		t.Fatalf("expired take state = %v, want unknown", state)
	}
	if err := st.PutCLIHandle("fresh", "EFGH-5678", time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM login_cli_handles`); n != 1 {
		t.Fatalf("rows after sweep = %d, want 1", n)
	}
}

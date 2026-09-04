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

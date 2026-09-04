package relay

import (
	"context"
	"testing"
	"time"
)

func TestNewInstanceAdvertisesHostWithListenerPorts(t *testing.T) {
	inst, err := NewInstance("10.0.0.7", ":443", "0.0.0.0:80", "127.0.0.1:7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID == "" || inst.StartedAt.IsZero() {
		t.Fatalf("identity not minted: %+v", inst)
	}
	want := map[string]string{"tls": "10.0.0.7:443", "http": "10.0.0.7:80", "tunnel": "10.0.0.7:7000", "api": "10.0.0.7:8080"}
	got := map[string]string{"tls": inst.TLSAddr, "http": inst.HTTPAddr, "tunnel": inst.TunnelAddr, "api": inst.APIAddr}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s addr = %q, want %q", k, got[k], w)
		}
	}
	other, _ := NewInstance("10.0.0.7", ":443", ":80", ":7000", ":8080")
	if other.ID == inst.ID {
		t.Fatal("two instances share an id")
	}
}

func TestNewInstanceRejectsAddrWithoutPort(t *testing.T) {
	if _, err := NewInstance("10.0.0.7", "443", ":80", ":7000", ":8080"); err == nil {
		t.Fatal("bare port accepted")
	}
}

func TestDefaultAdvertiseHostIsNonLoopbackIPv4(t *testing.T) {
	host, err := defaultAdvertiseHost()
	if err != nil {
		t.Skip("no non-loopback IPv4 on this machine:", err)
	}
	if host == "" || host == "127.0.0.1" {
		t.Fatalf("advertise host = %q", host)
	}
}

func TestHeartbeatPublishesSessionsAndLeavesOnStop(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter()
	relaySess, _ := pipeSession(t, "box.public.getpiper.co")
	router.Register(relaySess)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inst.heartbeat(ctx, st, router); close(done) }()

	waitCond(t, 3*time.Second, "heartbeat row with one session", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == inst.ID && rows[0].Sessions == 1 && rows[0].TLSAddr == "127.0.0.1:443"
	})
	cancel()
	<-done
	if rows, _ := st.LiveInstances(); len(rows) != 0 {
		t.Fatalf("row survived a clean stop: %+v", rows)
	}
}

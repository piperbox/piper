package relay

import (
	"strings"
	"testing"
	"time"
)

func instRow(id string, started time.Time, sessions int) InstanceRow {
	return InstanceRow{ID: id, StartedAt: started, Sessions: sessions,
		TLSAddr: id + ":443", HTTPAddr: id + ":80", TunnelAddr: id + ":7000", APIAddr: id + ":8080"}
}

func TestEdgeStatePickAPIStartsWithEarliest(t *testing.T) {
	t0 := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	if _, ok := s.pickAPI(); ok {
		t.Fatal("empty pool picked something")
	}
	s.setInstances([]InstanceRow{instRow("b", t0.Add(time.Minute), 0), instRow("a", t0, 9), instRow("c", t0.Add(time.Hour), 0)})
	if got, ok := s.pickAPI(); !ok || got.ID != "a" {
		t.Fatalf("pickAPI = %+v ok=%v, want a (earliest, load ignored)", got, ok)
	}
}

func TestPickTunnelFewestSessionsThenEarliest(t *testing.T) {
	t0 := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	s.setInstances([]InstanceRow{
		instRow("busy", t0, 5),
		instRow("late-idle", t0.Add(time.Minute), 1),
		instRow("early-idle", t0.Add(time.Second), 1),
	})
	cases := []struct {
		name    string
		exclude map[string]bool
		want    string
	}{
		{"fewest then earliest", nil, "early-idle"},
		{"excluded first choice", map[string]bool{"early-idle": true}, "late-idle"},
		{"only the busy one left", map[string]bool{"early-idle": true, "late-idle": true}, "busy"},
	}
	for _, c := range cases {
		got, ok := s.pickTunnel(c.exclude)
		if !ok || got.ID != c.want {
			t.Errorf("%s: pickTunnel = %+v ok=%v, want %s", c.name, got, ok, c.want)
		}
	}
	if _, ok := s.pickTunnel(map[string]bool{"early-idle": true, "late-idle": true, "busy": true}); ok {
		t.Fatal("every candidate excluded still picked one")
	}
}

func TestOwnerOfRequiresALiveInstanceAndEvictCascades(t *testing.T) {
	t0 := time.Now()
	s := newEdgeState()
	s.setInstances([]InstanceRow{instRow("a", t0, 0), instRow("b", t0, 0)})
	s.setOwners(map[string]string{"x.example": "a", "y.example": "b"})
	if got, ok := s.ownerOf("x.example"); !ok || got.TLSAddr != "a:443" {
		t.Fatalf("ownerOf x = %+v ok=%v", got, ok)
	}
	if _, ok := s.ownerOf("nobody.example"); ok {
		t.Fatal("unowned agent resolved")
	}
	s.setOwner("x.example", "")
	if _, ok := s.ownerOf("x.example"); ok {
		t.Fatal("cleared owner still resolved")
	}
	s.setOwner("x.example", "a")
	s.evict("a")
	if _, ok := s.ownerOf("x.example"); ok {
		t.Fatal("owner survived its instance's eviction")
	}
	if got, ok := s.pickTunnel(nil); !ok || got.ID != "b" {
		t.Fatalf("after evict pickTunnel = %+v ok=%v, want b", got, ok)
	}
	// An owner row that points at an unknown (dead) instance is unroutable.
	s.setOwner("y.example", "ghost")
	if _, ok := s.ownerOf("y.example"); ok {
		t.Fatal("owner naming an unknown instance resolved")
	}
}

func TestNameCacheExpiresAndClears(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	s.now = func() time.Time { return now }

	if _, cached, _ := s.cachedName("app.example"); cached {
		t.Fatal("empty cache reported an entry")
	}
	s.cacheName("app.example", "box.example")
	s.cacheName("nothing.example", "")
	if agent, cached, fresh := s.cachedName("app.example"); !cached || !fresh || agent != "box.example" {
		t.Fatalf("fresh positive = %q %v %v", agent, cached, fresh)
	}
	if agent, cached, fresh := s.cachedName("nothing.example"); !cached || !fresh || agent != "" {
		t.Fatalf("fresh negative = %q %v %v", agent, cached, fresh)
	}
	now = now.Add(nameCacheTTL + time.Second)
	if agent, cached, fresh := s.cachedName("app.example"); !cached || fresh || agent != "box.example" {
		t.Fatalf("expired entry = %q cached=%v fresh=%v, want stale-but-present", agent, cached, fresh)
	}
	s.clearNames()
	if _, cached, _ := s.cachedName("app.example"); cached {
		t.Fatal("clearNames left an entry")
	}
}

// api.<apex> is round-robined: with login state in Postgres (#522) any relay
// serves any request, so the edge spreads them instead of pinning one.
func TestEdgeStatePickAPIRoundRobins(t *testing.T) {
	s := newEdgeState()
	t0 := time.Now()
	s.setInstances([]InstanceRow{
		{ID: "b", StartedAt: t0.Add(time.Second)},
		{ID: "a", StartedAt: t0},
	})
	var got []string
	for i := 0; i < 4; i++ {
		r, ok := s.pickAPI()
		if !ok {
			t.Fatal("pickAPI found nothing")
		}
		got = append(got, r.ID)
	}
	if want := []string{"a", "b", "a", "b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pickAPI order = %v, want %v", got, want)
	}
	s.evict("a")
	if r, ok := s.pickAPI(); !ok || r.ID != "b" {
		t.Fatalf("pickAPI after evict = (%v, %v), want b", r.ID, ok)
	}
}

// A draining relay must get nothing new — no tunnel placement however idle
// it looks (its session count is falling), no api.<apex> connection — but it
// still owns the agents it holds, so :443/:80 keep routing to it until each
// session closes.
func TestPlacementSkipsDrainingInstances(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	a := instRow("a", t0, 0)
	a.Draining = true
	s.setInstances([]InstanceRow{a, instRow("b", t0.Add(time.Minute), 7)})
	s.setOwners(map[string]string{"x.example": "a"})

	if got, ok := s.pickTunnel(nil); !ok || got.ID != "b" {
		t.Fatalf("pickTunnel = %+v ok=%v, want b (a is draining, however idle and early)", got, ok)
	}
	if got, ok := s.pickAPI(); !ok || got.ID != "b" {
		t.Fatalf("pickAPI = %+v ok=%v, want b (a is draining)", got, ok)
	}
	if got, ok := s.ownerOf("x.example"); !ok || got.ID != "a" {
		t.Fatalf("ownerOf x = %+v ok=%v, want the draining owner a", got, ok)
	}

	b := instRow("b", t0.Add(time.Minute), 7)
	b.Draining = true
	s.setInstances([]InstanceRow{a, b})
	if got, ok := s.pickTunnel(nil); ok {
		t.Fatalf("pickTunnel = %+v on a pool that is all draining, want no candidate", got)
	}
	if got, ok := s.pickAPI(); ok {
		t.Fatalf("pickAPI = %+v on a pool that is all draining, want no candidate", got)
	}
}

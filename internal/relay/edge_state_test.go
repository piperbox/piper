package relay

import (
	"testing"
	"time"
)

func instRow(id string, started time.Time, sessions int) InstanceRow {
	return InstanceRow{ID: id, StartedAt: started, Sessions: sessions,
		TLSAddr: id + ":443", HTTPAddr: id + ":80", TunnelAddr: id + ":7000", APIAddr: id + ":8080"}
}

func TestPickAPIPinsEarliestStarted(t *testing.T) {
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

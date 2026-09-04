package relay

import (
	"sync"
	"time"
)

// nameCacheTTL bounds how long the edge trusts a hostname → agent answer,
// positive or negative, before asking Postgres again. piper_hostnames clears
// the cache the moment a name changes, so this is only the backstop.
const nameCacheTTL = 30 * time.Second

// edgeState is the edge's in-memory picture of the cluster: the live
// instance pool, who owns which agent, and a hostname → agent cache. Every
// routing decision reads it; the listener and poll goroutines write it.
type edgeState struct {
	mu        sync.RWMutex
	instances map[string]InstanceRow
	owners    map[string]string // agent base domain → instance id
	names     map[string]nameEntry
	now       func() time.Time
}

// nameEntry caches one lookup. agent == "" is a negative entry: nothing
// serves this name (yet).
type nameEntry struct {
	agent   string
	expires time.Time
}

func newEdgeState() *edgeState {
	return &edgeState{
		instances: map[string]InstanceRow{},
		owners:    map[string]string{},
		names:     map[string]nameEntry{},
		now:       time.Now,
	}
}

func (s *edgeState) setInstances(rows []InstanceRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances = make(map[string]InstanceRow, len(rows))
	for _, r := range rows {
		s.instances[r.ID] = r
	}
}

func (s *edgeState) setOwners(m map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = make(map[string]string, len(m))
	for agent, id := range m {
		s.owners[agent] = id
	}
}

// setOwner records one ownership change; id == "" clears it.
func (s *edgeState) setOwner(agent, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		delete(s.owners, agent)
		return
	}
	s.owners[agent] = id
}

// evict drops an instance the edge found dead on dial, with every agent it
// owned — the in-memory twin of the agent_owners cascade.
func (s *edgeState) evict(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, id)
	for agent, owner := range s.owners {
		if owner == id {
			delete(s.owners, agent)
		}
	}
}

// ownerOf returns the live instance owning agent.
func (s *edgeState) ownerOf(agent string) (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.owners[agent]
	if !ok {
		return InstanceRow{}, false
	}
	r, ok := s.instances[id]
	return r, ok
}

// pickAPI is the api.<apex> pin: the live instance that started first, so
// every login-flow poll lands on one process until that state moves to
// Postgres.
func (s *edgeState) pickAPI() (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pickLocked(earlier, nil)
}

// pickTunnel is :7000 placement: fewest sessions, ties to the earliest
// started. exclude names instances a failed dial has just ruled out.
func (s *edgeState) pickTunnel(exclude map[string]bool) (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pickLocked(func(a, b InstanceRow) bool {
		if a.Sessions != b.Sessions {
			return a.Sessions < b.Sessions
		}
		return earlier(a, b)
	}, exclude)
}

// earlier is a total order on instances: start time, then id.
func earlier(a, b InstanceRow) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.Before(b.StartedAt)
	}
	return a.ID < b.ID
}

func (s *edgeState) pickLocked(less func(a, b InstanceRow) bool, exclude map[string]bool) (InstanceRow, bool) {
	var best InstanceRow
	found := false
	for _, r := range s.instances {
		if exclude[r.ID] {
			continue
		}
		if !found || less(r, best) {
			best, found = r, true
		}
	}
	return best, found
}

// cachedName reports the cached agent for key: cached says an entry exists
// at all (stale entries are kept so an unreachable Postgres serves old
// answers rather than none), fresh says it is within nameCacheTTL.
func (s *edgeState) cachedName(key string) (agent string, cached, fresh bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.names[key]
	if !ok {
		return "", false, false
	}
	return e.agent, true, s.now().Before(e.expires)
}

func (s *edgeState) cacheName(key, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names[key] = nameEntry{agent: agent, expires: s.now().Add(nameCacheTTL)}
}

func (s *edgeState) clearNames() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = map[string]nameEntry{}
}

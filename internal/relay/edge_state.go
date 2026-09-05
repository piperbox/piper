package relay

import (
	"sort"
	"sync"
	"sync/atomic"
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
	owners    map[string][]string // agent base domain → live owner ids
	names     map[string]nameEntry
	now       func() time.Time
	apiNext   atomic.Uint64 // round-robin cursor for api.<apex>
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
		owners:    map[string][]string{},
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

func (s *edgeState) setOwners(m map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = make(map[string][]string, len(m))
	for agent, ids := range m {
		s.owners[agent] = append([]string(nil), ids...)
	}
}

// setOwner replaces one agent's owner set; an empty set clears it.
func (s *edgeState) setOwner(agent string, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		delete(s.owners, agent)
		return
	}
	s.owners[agent] = append([]string(nil), ids...)
}

// evict drops an instance the edge found dead on dial, out of every owner
// set it was in — the in-memory twin of the agent_owners cascade.
func (s *edgeState) evict(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, id)
	for agent, ids := range s.owners {
		kept := make([]string, 0, len(ids))
		for _, o := range ids {
			if o != id {
				kept = append(kept, o)
			}
		}
		if len(kept) == 0 {
			delete(s.owners, agent)
		} else {
			s.owners[agent] = kept
		}
	}
}

// ownersOf lists the live instances owning agent in routing preference:
// non-draining first, then fewest sessions, then earliest started. A
// draining relay still holds its session and keeps serving, so it stays a
// candidate; it only loses ties, which is what shifts new connections to
// the survivor the moment a rolling restart begins (#530).
func (s *edgeState) ownersOf(agent string) []InstanceRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []InstanceRow
	for _, id := range s.owners[agent] {
		if r, ok := s.instances[id]; ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Draining != b.Draining {
			return !a.Draining
		}
		if a.Sessions != b.Sessions {
			return a.Sessions < b.Sessions
		}
		return earlier(a, b)
	})
	return out
}

// ownerOf is the first choice among ownersOf.
func (s *edgeState) ownerOf(agent string) (InstanceRow, bool) {
	owners := s.ownersOf(agent)
	if len(owners) == 0 {
		return InstanceRow{}, false
	}
	return owners[0], true
}

// pickAPI spreads api.<apex> across the live pool. Login-flow state lives in
// Postgres (#522), so any relay answers any control-plane request; a stable
// order plus a cursor gives each relay its turn. Eviction or a resync just
// shifts the cursor's target by one, which is harmless. A draining relay (#523)
// is left out, as in pickLocked.
func (s *edgeState) pickAPI() (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	live := make([]InstanceRow, 0, len(s.instances))
	for _, r := range s.instances {
		if r.Draining {
			continue
		}
		live = append(live, r)
	}
	if len(live) == 0 {
		return InstanceRow{}, false
	}
	sort.Slice(live, func(i, j int) bool { return earlier(live[i], live[j]) })
	n := s.apiNext.Add(1) - 1
	return live[int(n%uint64(len(live)))], true
}

// pickTunnel is :7000 placement for the agent named base: fewest sessions,
// ties to the earliest started, among relays that do not already own base
// (#530). exclude names instances a failed dial has just ruled out. The
// owner exclusion is soft: if it empties the pool the pick runs again over
// every relay and the one dialled rejects the duplicate after auth, so a
// claimed base never changes what an unauthenticated peer can observe.
func (s *edgeState) pickTunnel(base string, exclude map[string]bool) (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owned := map[string]bool{}
	for _, id := range s.owners[base] {
		owned[id] = true
	}
	less := func(a, b InstanceRow) bool {
		if a.Sessions != b.Sessions {
			return a.Sessions < b.Sessions
		}
		return earlier(a, b)
	}
	if r, ok := s.pickLocked(less, func(r InstanceRow) bool { return exclude[r.ID] || owned[r.ID] }); ok {
		return r, true
	}
	return s.pickLocked(less, func(r InstanceRow) bool { return exclude[r.ID] })
}

// earlier is a total order on instances: start time, then id.
func earlier(a, b InstanceRow) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.Before(b.StartedAt)
	}
	return a.ID < b.ID
}

// pickLocked is the shared placement scan. A draining instance is never a
// candidate (#523): it is on its way out and refuses new tunnels anyway.
// ownersOf does not go through here on purpose — a draining relay still
// holds its agents' sessions and must keep receiving their traffic.
func (s *edgeState) pickLocked(less func(a, b InstanceRow) bool, skip func(InstanceRow) bool) (InstanceRow, bool) {
	var best InstanceRow
	found := false
	for _, r := range s.instances {
		if skip(r) || r.Draining {
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

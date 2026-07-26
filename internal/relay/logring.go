package relay

import (
	"strings"
	"sync"
)

// LogRing is a fixed-capacity ring buffer of log lines. It implements
// io.Writer so main can tee the stdlib logger into it (log lines can then be
// served over HTTP); Lines returns the buffered lines oldest-first. A full
// ring overwrites its oldest line, so memory stays bounded for the process
// lifetime.
type LogRing struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func NewLogRing(capacity int) *LogRing {
	return &LogRing{lines: make([]string, capacity)}
}

// Write records p's newline-separated lines. One log.Printf call arrives as
// one Write ending in "\n", but nothing guarantees a single line per call, so
// split rather than assume.
func (r *LogRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		r.lines[r.next] = line
		r.next = (r.next + 1) % len(r.lines)
		if r.next == 0 {
			r.full = true
		}
	}
	return len(p), nil
}

// Lines returns a copy of the buffered lines, oldest first.
func (r *LogRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string(nil), r.lines[:r.next]...)
	}
	out := make([]string, 0, len(r.lines))
	out = append(out, r.lines[r.next:]...)
	out = append(out, r.lines[:r.next]...)
	return out
}

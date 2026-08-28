package contracts

import (
	"sync"
	"time"
)

// Violation is one observed payload/schema mismatch (§9A.3).
type Violation struct {
	TraceID   string `json:"trace_id"`
	SpanID    string `json:"span_id"`
	Caller    string `json:"caller"`
	Callee    string `json:"callee"`
	Route     string `json:"route"`
	Direction string `json:"direction"`
	Path      string `json:"path"`
	Rule      string `json:"rule"`
	Expected  string `json:"expected"`
	Observed  string `json:"observed"`
	Severity  string `json:"severity"`
	Timestamp int64  `json:"timestamp"`
}

type Group struct {
	Violation
	Count     int   `json:"count"`
	FirstSeen int64 `json:"first_seen"`
	LastSeen  int64 `json:"last_seen"`
}

// ViolationStore is a bounded, deduplicating violation ring.
type ViolationStore struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	groups []Group
	byKey  map[string]int
	next   int
	full   bool
}

func NewViolationStore(max int, window time.Duration) *ViolationStore {
	if max <= 0 {
		max = 1000
	}
	if window <= 0 {
		window = time.Minute
	}
	return &ViolationStore{max: max, window: window, groups: make([]Group, max), byKey: make(map[string]int, max)}
}

func violationKey(v Violation) string {
	return v.Caller + "\x00" + v.Callee + "\x00" + v.Route + "\x00" + v.Direction + "\x00" + v.Path + "\x00" + v.Rule
}

func (s *ViolationStore) Add(v Violation) Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := violationKey(v)
	if idx, ok := s.byKey[key]; ok && v.Timestamp-s.groups[idx].LastSeen <= s.window.Nanoseconds() {
		g := &s.groups[idx]
		g.Count++
		g.LastSeen = v.Timestamp
		g.Violation = v
		return *g
	}
	if s.full {
		delete(s.byKey, violationKey(s.groups[s.next].Violation))
	}
	g := Group{Violation: v, Count: 1, FirstSeen: v.Timestamp, LastSeen: v.Timestamp}
	s.groups[s.next] = g
	s.byKey[key] = s.next
	s.next++
	if s.next == s.max {
		s.next = 0
		s.full = true
	}
	return g
}

func (s *ViolationStore) Snapshot() []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.next
	if s.full {
		n = s.max
	}
	out := make([]Group, 0, n)
	for i := 0; i < n; i++ {
		idx := i
		if s.full {
			idx = (s.next + i) % s.max
		}
		out = append(out, s.groups[idx])
	}
	return out
}

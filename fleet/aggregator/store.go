// Package aggregator assembles spans from many services into traces and serves
// them to the dashboard's Fleet View (§8).
//
// # What this package is, and what it deliberately is not
//
// It is a live, in-memory, bounded view of recent traffic — the same model as the
// rest of the dashboard. Traces older than the retention window are gone. There
// is no disk, no database, and no durability: this is not a trace archive, and
// nothing here should be read as implying otherwise. A fleet that needs
// months-retained traces wants a Kafka-backed pipeline into something built for
// storage, not this.
//
// # Single process, on purpose, for now
//
// v1 runs one aggregator. Two instances would each hold a partial view, so a
// trace whose spans landed on different instances would render incomplete on
// both. Rather than hide that, storage sits behind the SpanStore interface below
// so a shared-backend implementation can be added later without touching the
// API, assembly, or topology code.
package aggregator

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/fleet"
)

// SpanStore is the storage seam (§17.2).
//
// Defined in v1 even though only memStore implements it, because the alternative
// — reaching into a concrete type from the API handlers, assembly, and topology
// code — is what makes a later multi-instance backend a rewrite instead of a new
// file. The interface is deliberately narrow: everything the read side needs is
// expressible as "give me a trace" or "give me recent trace summaries", so a
// Redis or NATS-backed version has a small surface to satisfy.
type SpanStore interface {
	// Add records one span, evicting as needed to stay inside configured
	// bounds. Never blocks on a full store and never returns an error: a
	// storage layer that could fail an ingest would push that failure back
	// to a service whose only recourse is to drop the span anyway.
	Add(span fleet.Span, now time.Time)

	// Trace returns the assembled trace for id.
	Trace(id string) (Trace, bool)

	// Recent returns summaries of the most recent traces, newest first,
	// filtered by q. Summaries rather than whole traces because the trace
	// list renders thousands of rows and needs none of the span detail.
	Recent(q TraceQuery) []TraceSummary

	// RecentPage is the cursor-paginated form used by Fleet View. Recent is
	// retained for callers that only need a bounded snapshot.
	RecentPage(q TraceQuery) TracePage

	// Sweep evicts traces idle longer than the TTL, returning how many went.
	// Called on a ticker rather than from Add so a burst of ingest does not
	// pay for eviction.
	Sweep(now time.Time) int

	// Stats reports counters for the Overview page (§8.2).
	Stats() StoreStats
}

// TraceQuery filters the trace list (§8.1.5).
type TraceQuery struct {
	Service       string
	Status        int
	MinDurationMs float64
	TagKey        string
	TagValue      string
	Limit         int
	OnlyErrors    bool
	MinServices   int
	Cursor        string
}

// TracePage is a stable, newest-first page of trace summaries.
type TracePage struct {
	Traces     []TraceSummary `json:"traces"`
	NextCursor string         `json:"next_cursor,omitempty"`
	HasMore    bool           `json:"has_more"`
}

type traceCursor struct {
	Start int64  `json:"start"`
	ID    string `json:"id"`
}

// StoreStats are the store's own counters.
type StoreStats struct {
	Traces        int    `json:"traces"`
	Spans         int    `json:"spans"`
	TracesEvicted uint64 `json:"traces_evicted"`
	SpansDropped  uint64 `json:"spans_dropped"`
	SpansRejected uint64 `json:"spans_rejected"`
}

// shardCount is the number of independently-locked partitions.
//
// Ingest is the concurrent path here: every service in the fleet POSTs batches at
// once, and one mutex over all traces would serialize the whole fleet through a
// single lock. 32 is chosen to match the sharding factor the framework already
// uses for its own concurrent maps, and because it comfortably exceeds the core
// count of the machines this runs on, which is the point past which more shards
// stop buying anything.
const shardCount = 32

// memStore is the in-memory SpanStore: sharded map of trace id to spans, plus a
// global insertion order for eviction.
type memStore struct {
	cfg    Config
	shards [shardCount]*shard

	// order is the eviction queue: trace ids in first-seen order, so
	// MaxTraces eviction is O(1) amortized. Separate from the shards and
	// separately locked, because it is the one structure every shard touches
	// and holding a shard lock while taking this one would reintroduce the
	// global serialization the shards exist to avoid.
	orderMu sync.Mutex
	order   []string

	// tagIndex maps "key:value" to trace ids, for §9C.1's tag search. Bounded
	// by the same retention as traces: entries are removed on eviction, so it
	// cannot outgrow the trace set it points into.
	tagMu    sync.Mutex
	tagIndex map[string]map[string]struct{}

	stats struct {
		mu            sync.Mutex
		tracesEvicted uint64
		spansDropped  uint64
		spansRejected uint64
	}
}

type shard struct {
	mu     sync.RWMutex
	traces map[string]*traceEntry
}

// traceEntry is one trace's accumulated spans plus the bookkeeping eviction and
// assembly need.
type traceEntry struct {
	id        string
	spans     []fleet.Span
	firstSeen time.Time
	lastSeen  time.Time

	// next is the write cursor once spans has reached MaxSpansPerTrace, at
	// which point the slice is used as a circular buffer.
	//
	// This exists because the obvious implementation — shift everything down
	// one and append — is O(MaxSpansPerTrace) per span, and §12.7 requires
	// that adding to an already-large trace not cost more than adding to a
	// small one. It measurably did: appending to a full 512-span trace ran 3x
	// slower than appending to a growing one, because every single span
	// triggered a ~100KB memmove. A cursor makes it one indexed store.
	//
	// Spans therefore come out of the buffer rotated rather than in arrival
	// order. That is harmless here because Assemble sorts by StartNanoUTC
	// before building the tree — arrival order was never load-bearing.
	next int

	// dropped counts spans discarded by MaxSpansPerTrace, so the UI can say
	// "this trace is incomplete" rather than showing a truncated tree as if
	// it were whole. A trace that lost spans is misleading precisely when it
	// matters most — a fan-out storm is both the cause of the overflow and
	// the thing someone is trying to debug.
	dropped int
}

// NewMemStore returns the in-memory store.
func NewMemStore(cfg Config) SpanStore {
	s := &memStore{
		cfg:      cfg,
		tagIndex: make(map[string]map[string]struct{}),
	}
	for i := range s.shards {
		s.shards[i] = &shard{traces: make(map[string]*traceEntry)}
	}
	return s
}

// shardFor picks a shard from a trace id.
//
// FNV-1a over the id's bytes rather than Go's map hash, because the id is
// already uniformly-distributed random hex — any cheap mixing function spreads
// it evenly, and this one has no per-process seed, which keeps a trace's shard
// assignment reproducible while debugging.
func (s *memStore) shardFor(traceID string) *shard {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(traceID); i++ {
		h ^= uint64(traceID[i])
		h *= prime64
	}
	return s.shards[h%shardCount]
}

// Add records one span.
//
// # Locking
//
// Two paths, deliberately. A span for a trace that already exists takes only
// that trace's shard lock, which is the common case and the one that has to
// scale — most spans join a trace some earlier span created.
//
// Creating a *new* trace additionally serializes on orderMu, because admission
// and eviction have to be atomic with respect to each other. Without that, two
// goroutines can each observe room for one more trace, both insert, and the live
// set exceeds MaxTraces — the exact bound this store exists to enforce. The
// concurrency test caught this: the count settled at 101 with a cap of 100.
//
// Lock order is always orderMu → shard → tagMu/stats. Nothing acquires them in
// the opposite direction, so there is no cycle: Recent and Sweep both release
// orderMu before touching a shard, and deleteTrace is only ever called with no
// shard lock held.
func (s *memStore) Add(span fleet.Span, now time.Time) {
	// Validation happens here, not only at the HTTP boundary, because every
	// transport funnels into this method and each one would otherwise need to
	// remember to validate. An invalid trace id is not a cosmetic problem: an
	// empty or malformed one becomes its own bucket, and an all-zero one
	// merges unrelated requests into a single fictional trace.
	if !span.Valid() {
		s.stats.mu.Lock()
		s.stats.spansRejected++
		s.stats.mu.Unlock()
		return
	}

	sh := s.shardFor(span.TraceID)

	// Fast path: the trace is already known.
	sh.mu.Lock()
	if e, ok := sh.traces[span.TraceID]; ok {
		s.appendLocked(e, span, now)
		sh.mu.Unlock()
		s.indexTags(span)
		return
	}
	sh.mu.Unlock()

	// Slow path: a new trace, admitted under orderMu so the cap holds exactly.
	s.orderMu.Lock()
	sh.mu.Lock()
	// Re-check: another goroutine may have created this trace while this one
	// waited for orderMu.
	e, exists := sh.traces[span.TraceID]
	if !exists {
		e = &traceEntry{
			id:        span.TraceID,
			spans:     make([]fleet.Span, 0, 4),
			firstSeen: now,
		}
		sh.traces[span.TraceID] = e
		s.order = append(s.order, span.TraceID)
	}
	s.appendLocked(e, span, now)
	sh.mu.Unlock()

	var evicted int
	for len(s.order) > s.cfg.MaxTraces {
		id := s.order[0]
		// Re-slicing rather than copying: the slice header advances and
		// the backing array is reclaimed when it eventually grows, which
		// avoids an O(n) shift on every single eviction at steady state.
		s.order = s.order[1:]
		s.deleteTrace(id)
		evicted++
	}
	s.orderMu.Unlock()

	if evicted > 0 {
		s.stats.mu.Lock()
		s.stats.tracesEvicted += uint64(evicted)
		s.stats.mu.Unlock()
	}
	s.indexTags(span)
}

// appendLocked adds one span to a trace, honouring MaxSpansPerTrace.
//
// Caller holds the trace's shard lock.
func (s *memStore) appendLocked(e *traceEntry, span fleet.Span, now time.Time) {
	if len(e.spans) >= s.cfg.MaxSpansPerTrace {
		// Overwrite the oldest span in place, advancing a cursor: O(1),
		// versus the O(MaxSpansPerTrace) memmove a shift-and-append costs
		// on *every* span once a trace is full. See traceEntry.next.
		//
		// Dropping the oldest rather than the newest is the deliberate
		// half of this: the later spans of a trace are where the failure
		// usually is, and a runaway trace is typically a retry loop whose
		// first hundred spans are identical.
		e.spans[e.next] = span
		e.next = (e.next + 1) % len(e.spans)
		e.dropped++

		s.stats.mu.Lock()
		s.stats.spansDropped++
		s.stats.mu.Unlock()
	} else {
		e.spans = append(e.spans, span)
	}

	// lastSeen drives TTL eviction, and is deliberately the arrival time of
	// the most recent span: a long-running trace must not be swept while its
	// later hops are still reporting.
	e.lastSeen = now
}

func (s *memStore) deleteTrace(id string) {
	sh := s.shardFor(id)
	sh.mu.Lock()
	e := sh.traces[id]
	delete(sh.traces, id)
	sh.mu.Unlock()

	if e == nil {
		return
	}
	// Tag index entries must go with the trace, or the index becomes the one
	// unbounded structure in a system whose whole storage story is "bounded".
	s.tagMu.Lock()
	for _, sp := range e.spans {
		for k, v := range sp.Tags {
			key := k + ":" + v
			if set, ok := s.tagIndex[key]; ok {
				delete(set, id)
				if len(set) == 0 {
					delete(s.tagIndex, key)
				}
			}
		}
	}
	s.tagMu.Unlock()
}

// indexTags records this span's tags for tag search (§9C.1).
func (s *memStore) indexTags(span fleet.Span) {
	if len(span.Tags) == 0 {
		return
	}
	s.tagMu.Lock()
	defer s.tagMu.Unlock()
	for k, v := range span.Tags {
		key := k + ":" + v
		set, ok := s.tagIndex[key]
		if !ok {
			set = make(map[string]struct{}, 1)
			s.tagIndex[key] = set
		}
		set[span.TraceID] = struct{}{}
	}
}

// Trace assembles and returns one trace.
func (s *memStore) Trace(id string) (Trace, bool) {
	sh := s.shardFor(id)
	sh.mu.RLock()
	e, ok := sh.traces[id]
	if !ok {
		sh.mu.RUnlock()
		return Trace{}, false
	}
	// Copy under the read lock and assemble outside it. Assembly sorts,
	// builds a tree, and walks it for root-cause marking; doing that while
	// holding the lock would block every concurrent ingest for this shard on
	// one dashboard request.
	spans := make([]fleet.Span, len(e.spans))
	copy(spans, e.spans)
	dropped := e.dropped
	firstSeen := e.firstSeen
	sh.mu.RUnlock()

	tr := Assemble(id, spans)
	tr.SpansDropped = dropped
	tr.FirstSeenUnixNano = firstSeen.UnixNano()
	return tr, true
}

// Recent returns matching trace summaries, newest first.
func (s *memStore) Recent(q TraceQuery) []TraceSummary {
	return s.RecentPage(q).Traces
}

// RecentPage returns matching summaries in descending start-time order.
// The cursor is opaque to callers and identifies the last row in the previous
// page, so traces arriving after a page was fetched do not shift its boundary.
func (s *memStore) RecentPage(q TraceQuery) TracePage {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		// Capped because this is a live view feeding a table, and an
		// unbounded limit lets one request copy the entire store.
		limit = 100
	}

	// A tag query is answered from the index rather than by scanning, which is the
	// is the entire reason the index exists: "find everything that touched
	// order 123" must not be O(traces).
	var candidates []string
	if q.TagKey != "" && q.TagValue != "" {
		s.tagMu.Lock()
		for id := range s.tagIndex[q.TagKey+":"+q.TagValue] {
			candidates = append(candidates, id)
		}
		s.tagMu.Unlock()
	} else {
		s.orderMu.Lock()
		candidates = make([]string, len(s.order))
		copy(candidates, s.order)
		s.orderMu.Unlock()
	}

	all := make([]TraceSummary, 0, len(candidates))
	for i := len(candidates) - 1; i >= 0; i-- {
		id := candidates[i]
		sh := s.shardFor(id)
		sh.mu.RLock()
		e, ok := sh.traces[id]
		if !ok {
			// Evicted between snapshotting the queue and reading it.
			// Normal under load, not an error.
			sh.mu.RUnlock()
			continue
		}
		sum := summarize(e)
		sh.mu.RUnlock()

		if matches(sum, q) {
			all = append(all, sum)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].StartNanoUTC != all[j].StartNanoUTC {
			return all[i].StartNanoUTC > all[j].StartNanoUTC
		}
		return all[i].TraceID > all[j].TraceID
	})
	if q.Cursor != "" {
		if cursor, ok := decodeTraceCursor(q.Cursor); ok {
			start := 0
			for start < len(all) && !beforeCursor(all[start], cursor) {
				start++
			}
			all = all[start:]
		}
	}
	page := TracePage{HasMore: len(all) > limit}
	if len(all) > limit {
		all = all[:limit]
	}
	page.Traces = all
	if page.HasMore && len(all) > 0 {
		page.NextCursor = encodeTraceCursor(traceCursor{Start: all[len(all)-1].StartNanoUTC, ID: all[len(all)-1].TraceID})
	}
	return page
}

func matches(sum TraceSummary, q TraceQuery) bool {
	if q.Service != "" && !sum.hasService(q.Service) {
		return false
	}
	if q.MinServices > 0 && len(sum.Services) < q.MinServices {
		return false
	}
	if q.Status != 0 && sum.Status != q.Status {
		return false
	}
	if q.MinDurationMs > 0 && sum.DurationMs < q.MinDurationMs {
		return false
	}
	if q.OnlyErrors && !sum.HasError {
		return false
	}
	return true
}

func beforeCursor(sum TraceSummary, c traceCursor) bool {
	return sum.StartNanoUTC < c.Start || (sum.StartNanoUTC == c.Start && sum.TraceID < c.ID)
}

func encodeTraceCursor(c traceCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeTraceCursor(s string) (traceCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return traceCursor{}, false
	}
	var c traceCursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		return traceCursor{}, false
	}
	return c, true
}

// Sweep evicts traces idle longer than TraceTTL.
func (s *memStore) Sweep(now time.Time) int {
	cutoff := now.Add(-s.cfg.TraceTTL)

	var expired []string
	for _, sh := range s.shards {
		sh.mu.RLock()
		for id, e := range sh.traces {
			if e.lastSeen.Before(cutoff) {
				expired = append(expired, id)
			}
		}
		sh.mu.RUnlock()
	}
	for _, id := range expired {
		s.deleteTrace(id)
	}
	if len(expired) > 0 {
		s.removeFromOrder(expired)
		s.stats.mu.Lock()
		s.stats.tracesEvicted += uint64(len(expired))
		s.stats.mu.Unlock()
	}
	return len(expired)
}

// removeFromOrder drops swept ids from the eviction queue.
//
// One pass building a fresh slice rather than n removals, so a sweep that expires
// a thousand traces is O(order) rather than O(order × expired).
func (s *memStore) removeFromOrder(ids []string) {
	gone := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		gone[id] = struct{}{}
	}
	s.orderMu.Lock()
	kept := s.order[:0]
	for _, id := range s.order {
		if _, dead := gone[id]; !dead {
			kept = append(kept, id)
		}
	}
	s.order = kept
	s.orderMu.Unlock()
}

// Stats reports store counters.
func (s *memStore) Stats() StoreStats {
	var traces, spans int
	for _, sh := range s.shards {
		sh.mu.RLock()
		traces += len(sh.traces)
		for _, e := range sh.traces {
			spans += len(e.spans)
		}
		sh.mu.RUnlock()
	}
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	return StoreStats{
		Traces:        traces,
		Spans:         spans,
		TracesEvicted: s.stats.tracesEvicted,
		SpansDropped:  s.stats.spansDropped,
		SpansRejected: s.stats.spansRejected,
	}
}

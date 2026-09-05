package diag

// counter.go — the counter subsystems embed.
//
// One type, used by compression, etag, ratelimit, cors, security, i18n, jwt,
// oauth2 and websocket. A shared type rather than nine hand-rolled ones because
// the gate check is the part that must not be got wrong: a subsystem that forgot
// it would put an unconditional atomic RMW on a response path, and nothing in a
// test would notice.

import (
	"sync/atomic"
	"time"
)

// Counter is a small set of atomics for one subsystem's hot-path facts.
//
// The zero value is ready to use, and a nil *Counter is safe for every method —
// so a subsystem that has not been given one needs no branch at its call site.
//
// # What the fields mean
//
// Deliberately generic, because the point is one type rather than nine. Each
// subsystem documents its own reading, and its probe labels them:
//
//	compression  Hits = responses compressed,  Misses = passed through
//	etag         Hits = 304s served,           Misses = full bodies
//	ratelimit    Hits = requests allowed,      Misses = rejected with 429
//	cors         Hits = preflights answered,   Misses = origins refused
//
// Errors is always failures, and Bytes is always bytes where the subsystem moves
// any. A subsystem that has no use for a field leaves it at zero and its probe
// omits it, rather than repurposing it into a third meaning.
type Counter struct {
	hits   atomic.Uint64
	misses atomic.Uint64
	errors atomic.Uint64
	bytes  atomic.Uint64
	// bytesSaved is separate from bytes because compression is the case that
	// needs both and a single field would have to pick one. Zero elsewhere.
	bytesSaved atomic.Uint64
	// lastNanos is the wall clock of the most recent activity, as Unix
	// nanoseconds so it fits in an atomic. Zero means never.
	lastNanos atomic.Int64
}

// Hit records one counted success. It is a no-op when counting is off, which is
// the default.
//
// The gate is read before the increment, not after: the entire cost of the
// disabled path is one shared-state load and a predicted branch.
func (c *Counter) Hit() {
	if c == nil || !counting.Load() {
		return
	}
	c.hits.Add(1)
	c.lastNanos.Store(time.Now().UnixNano())
}

// Miss records one counted non-application — a response left uncompressed, a
// request that did not match, a body served in full rather than as a 304.
func (c *Counter) Miss() {
	if c == nil || !counting.Load() {
		return
	}
	c.misses.Add(1)
	c.lastNanos.Store(time.Now().UnixNano())
}

// Error records one failure.
func (c *Counter) Error() {
	if c == nil || !counting.Load() {
		return
	}
	c.errors.Add(1)
	c.lastNanos.Store(time.Now().UnixNano())
}

// Bytes adds to the byte total. Negative and zero values are ignored, so a
// caller need not check a length it already has.
func (c *Counter) Bytes(n int64) {
	if c == nil || n <= 0 || !counting.Load() {
		return
	}
	c.bytes.Add(uint64(n))
}

// Saved adds to the bytes-saved total, for a subsystem that shrinks something.
func (c *Counter) Saved(n int64) {
	if c == nil || n <= 0 || !counting.Load() {
		return
	}
	c.bytesSaved.Add(uint64(n))
}

// HitBytes is Hit plus Bytes plus Saved in one call, with a single gate read.
//
// Compression's response path would otherwise do three independent gate loads
// for one response. Folding them is worth a method because that path runs for
// every compressible response in the process.
func (c *Counter) HitBytes(written, saved int64) {
	if c == nil || !counting.Load() {
		return
	}
	c.hits.Add(1)
	if written > 0 {
		c.bytes.Add(uint64(written))
	}
	if saved > 0 {
		c.bytesSaved.Add(uint64(saved))
	}
	c.lastNanos.Store(time.Now().UnixNano())
}

// CounterSnapshot is a Counter read out.
//
// Plain values, not atomics, so it can be put straight into a Report's Detail and
// encoded. The fields are individually consistent but the set is not a single
// atomic read: a request in flight can land between two loads. That is the right
// trade — the alternative is a lock on the response path, and no consumer of a
// diagnostic count needs the four numbers to be from the same instant.
type CounterSnapshot struct {
	Hits       uint64 `json:"hits"`
	Misses     uint64 `json:"misses"`
	Errors     uint64 `json:"errors"`
	Bytes      uint64 `json:"bytes,omitempty"`
	BytesSaved uint64 `json:"bytes_saved,omitempty"`

	// Last is the most recent activity, RFC 3339, or empty for never.
	Last string `json:"last,omitempty"`

	// Counting reports whether the gate was open when this was read. It travels
	// with the numbers on purpose: zeroes with Counting false mean "not
	// measured", and a reader that cannot tell that apart from "did not happen"
	// will draw the wrong conclusion from a healthy service.
	Counting bool `json:"counting"`
}

// Total is hits plus misses, the denominator for a rate.
func (s CounterSnapshot) Total() uint64 { return s.Hits + s.Misses }

// Rate is hits as a fraction of total, or zero when nothing was counted.
func (s CounterSnapshot) Rate() float64 {
	total := s.Total()
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// Snapshot reads the counter. Safe on a nil receiver, which reports zeroes with
// the current gate state.
func (c *Counter) Snapshot() CounterSnapshot {
	out := CounterSnapshot{Counting: counting.Load()}
	if c == nil {
		return out
	}

	out.Hits = c.hits.Load()
	out.Misses = c.misses.Load()
	out.Errors = c.errors.Load()
	out.Bytes = c.bytes.Load()
	out.BytesSaved = c.bytesSaved.Load()
	if nanos := c.lastNanos.Load(); nanos != 0 {
		out.Last = time.Unix(0, nanos).UTC().Format(time.RFC3339Nano)
	}
	return out
}

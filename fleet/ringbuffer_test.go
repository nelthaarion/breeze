package fleet

// Tests for the local span buffer.
//
// This buffer is the thing standing between "the aggregator went away" and "the
// service that was reporting to it fell over too". Its contract is narrow and
// absolute: never block a request, never grow without bound, and lose the oldest
// data first when it must lose something. Every test here is one of those three
// promises.

import (
	"sync"
	"testing"
)

// ringSpan builds a span identifiable by route, so ordering assertions can read
// as a sequence rather than a set of opaque structs.
func ringSpan(route string) Span {
	return Span{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7",
		Service: "svc",
		Route:   route,
		Method:  "GET",
		Status:  200,
	}
}

func routes(spans []Span) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Route
	}
	return out
}

func equalRoutes(got []Span, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Route != want[i] {
			return false
		}
	}
	return true
}

func TestSpanRingPushAndDrainFIFO(t *testing.T) {
	r := newSpanRing(8)
	for _, route := range []string{"/a", "/b", "/c"} {
		r.push(ringSpan(route))
	}
	if got := r.len(); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}

	got := r.drain(0)
	if !equalRoutes(got, "/a", "/b", "/c") {
		t.Errorf("drain = %v, want oldest-first [/a /b /c]", routes(got))
	}
	if r.len() != 0 {
		t.Error("drain left spans behind — it must remove what it returns")
	}
}

// TestSpanRingDrainEmptyReturnsNil pins the allocation-free skip. The flush loop
// calls drain on every tick, and most ticks on a quiet service find nothing; that
// case must not allocate a slice just to report emptiness.
func TestSpanRingDrainEmptyReturnsNil(t *testing.T) {
	if got := newSpanRing(4).drain(0); got != nil {
		t.Errorf("drain on empty = %v, want nil", got)
	}
}

func TestSpanRingDrainRespectsMax(t *testing.T) {
	r := newSpanRing(8)
	for _, route := range []string{"/a", "/b", "/c", "/d"} {
		r.push(ringSpan(route))
	}

	// MaxBatchSize caps an export batch; the remainder must stay queued for
	// the next one rather than being dropped or reordered.
	if got := r.drain(2); !equalRoutes(got, "/a", "/b") {
		t.Errorf("first batch = %v, want [/a /b]", routes(got))
	}
	if got := r.drain(2); !equalRoutes(got, "/c", "/d") {
		t.Errorf("second batch = %v, want [/c /d]", routes(got))
	}
}

// TestSpanRingDropsOldestWhenFull is the overflow rule. A service whose
// aggregator is unreachable sits in this state indefinitely, so what it drops is
// a design decision, not an accident: the newest spans are the ones an operator
// is currently looking at.
func TestSpanRingDropsOldestWhenFull(t *testing.T) {
	r := newSpanRing(3)
	for _, route := range []string{"/a", "/b", "/c", "/d", "/e"} {
		r.push(ringSpan(route))
	}

	if got := r.len(); got != 3 {
		t.Fatalf("len = %d, want the capacity 3 — the buffer grew", got)
	}
	if got := r.droppedCount(); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
	if got := r.drain(0); !equalRoutes(got, "/c", "/d", "/e") {
		t.Errorf("drain = %v, want the newest three [/c /d /e]", routes(got))
	}
}

// TestSpanRingDrainZeroesSlots guards a real leak, not a hypothetical one. A Span
// carries a tag map, a timeline slice, and payload bytes; leaving references in
// the backing array after export keeps all of that alive until the slot is
// overwritten, which on a quiet service could be indefinitely.
func TestSpanRingDrainZeroesSlots(t *testing.T) {
	r := newSpanRing(4)
	s := ringSpan("/a")
	s.Tags = map[string]string{"order_id": "123"}
	s.RequestPayload = []byte(`{"big":"payload"}`)
	r.push(s)

	if got := r.drain(0); len(got) != 1 {
		t.Fatalf("drained %d spans, want 1", len(got))
	}
	for i, e := range r.entries {
		if e.Tags != nil || e.RequestPayload != nil || e.Route != "" {
			t.Errorf("slot %d still holds data after drain: %+v", i, e)
		}
	}
}

// TestSpanRingWrapAround exercises the index arithmetic across the capacity
// boundary, where an off-by-one silently reorders or duplicates spans rather than
// failing loudly.
func TestSpanRingWrapAround(t *testing.T) {
	r := newSpanRing(3)
	r.push(ringSpan("/a"))
	r.push(ringSpan("/b"))
	if got := r.drain(0); !equalRoutes(got, "/a", "/b") {
		t.Fatalf("setup drain = %v", routes(got))
	}

	// head is now at index 2; these three wrap past the end of the array.
	for _, route := range []string{"/c", "/d", "/e"} {
		r.push(ringSpan(route))
	}
	if got := r.drain(0); !equalRoutes(got, "/c", "/d", "/e") {
		t.Errorf("post-wrap drain = %v, want [/c /d /e]", routes(got))
	}
}

// --- requeue ---------------------------------------------------------------

// TestSpanRingRequeuePutsSpansBackInOrder covers the aggregator-restart case: a
// failed export must not cost the batch it was carrying, and must not reorder it
// relative to spans recorded while the export was in flight.
func TestSpanRingRequeuePutsSpansBackInOrder(t *testing.T) {
	r := newSpanRing(8)
	r.push(ringSpan("/newer"))

	// The failed batch is older than what is already buffered, so it belongs
	// at the front.
	r.requeue([]Span{ringSpan("/a"), ringSpan("/b")})

	if got := r.len(); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}
	if got := r.drain(0); !equalRoutes(got, "/a", "/b", "/newer") {
		t.Errorf("drain = %v, want requeued spans first [/a /b /newer]", routes(got))
	}
}

func TestSpanRingRequeueEmptyIsNoop(t *testing.T) {
	r := newSpanRing(4)
	r.requeue(nil)
	r.requeue([]Span{})
	if r.len() != 0 || r.droppedCount() != 0 {
		t.Errorf("len = %d, dropped = %d, want 0 and 0", r.len(), r.droppedCount())
	}
}

// TestSpanRingRequeueKeepsNewestWhenOverCapacity is the interesting failure
// mode: an export failed while the buffer kept filling, so the returning batch no
// longer fits. Keeping the newest of it matches push's drop-oldest rule, and
// counting the rest keeps the dropped total honest.
func TestSpanRingRequeueKeepsNewestWhenOverCapacity(t *testing.T) {
	r := newSpanRing(3)
	r.push(ringSpan("/live"))

	// Room for 2, batch of 4: /c and /d survive, /a and /b are counted.
	r.requeue([]Span{ringSpan("/a"), ringSpan("/b"), ringSpan("/c"), ringSpan("/d")})

	if got := r.droppedCount(); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
	if got := r.drain(0); !equalRoutes(got, "/c", "/d", "/live") {
		t.Errorf("drain = %v, want [/c /d /live]", routes(got))
	}
}

// TestSpanRingRequeueIntoFullBufferDropsAll is the steady state of a service
// whose aggregator has been down for a while: the buffer is full of newer spans,
// so a returning batch has nowhere to go. It must be counted and discarded, not
// allowed to displace newer data or grow the buffer.
func TestSpanRingRequeueIntoFullBufferDropsAll(t *testing.T) {
	r := newSpanRing(2)
	r.push(ringSpan("/a"))
	r.push(ringSpan("/b"))

	r.requeue([]Span{ringSpan("/x"), ringSpan("/y"), ringSpan("/z")})

	if got := r.len(); got != 2 {
		t.Errorf("len = %d, want 2 — requeue grew a full buffer", got)
	}
	if got := r.droppedCount(); got != 3 {
		t.Errorf("dropped = %d, want 3", got)
	}
	if got := r.drain(0); !equalRoutes(got, "/a", "/b") {
		t.Errorf("drain = %v, want the resident spans [/a /b]", routes(got))
	}
}

// TestSpanRingRequeueWrapsBackwards exercises the backwards index walk when the
// head is at 0 and has to wrap to the end of the array — the mirror of the
// wrap-around case above, and equally easy to get off by one.
func TestSpanRingRequeueWrapsBackwards(t *testing.T) {
	r := newSpanRing(4)
	r.push(ringSpan("/a"))
	if got := r.drain(0); len(got) != 1 {
		t.Fatalf("setup drain returned %d spans", len(got))
	}
	// head is at 1 now; requeueing 2 moves it back through 0 to 3.
	r.requeue([]Span{ringSpan("/x"), ringSpan("/y")})

	if got := r.drain(0); !equalRoutes(got, "/x", "/y") {
		t.Errorf("drain = %v, want [/x /y]", routes(got))
	}
}

// --- bounds and concurrency ------------------------------------------------

// TestNewSpanRingClampsCapacity keeps a misconfigured MaxBufferSpans from
// producing a zero-length backing array, where the modulo arithmetic in push
// would divide by zero and panic on the request path.
func TestNewSpanRingClampsCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1, -100} {
		r := newSpanRing(capacity)
		r.push(ringSpan("/a")) // must not panic
		if got := r.len(); got != 1 {
			t.Errorf("newSpanRing(%d): len = %d, want 1", capacity, got)
		}
	}
}

// TestSpanRingConcurrentPushAndDrain is the shape this type actually runs in:
// many request goroutines pushing while one flush goroutine drains. The
// assertion that matters is conservation — every span pushed is either drained or
// counted as dropped, never silently lost and never duplicated.
func TestSpanRingConcurrentPushAndDrain(t *testing.T) {
	const (
		writers = 8
		perW    = 500
	)
	r := newSpanRing(64)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	drained := 0
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		for {
			select {
			case <-stop:
				// Final drain, so nothing pushed during the last
				// window is miscounted as lost.
				drained += len(r.drain(0))
				return
			default:
				drained += len(r.drain(16))
			}
		}
	}()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perW; i++ {
				r.push(ringSpan("/x"))
			}
		}()
	}
	wg.Wait()
	close(stop)
	drainWG.Wait()

	total := writers * perW
	if got := drained + int(r.droppedCount()) + r.len(); got != total {
		t.Errorf("drained %d + dropped %d + buffered %d = %d, want %d — spans were lost or duplicated",
			drained, r.droppedCount(), r.len(), got, total)
	}
}

// TestSpanRingNeverExceedsCapacity is the OOM guard stated as a test: sustained
// pushes far past capacity must leave both the live count and the backing array
// at exactly the configured bound.
func TestSpanRingNeverExceedsCapacity(t *testing.T) {
	const capacity = 16
	r := newSpanRing(capacity)
	for i := 0; i < 10_000; i++ {
		r.push(ringSpan("/a"))
	}
	if got := r.len(); got != capacity {
		t.Errorf("len = %d, want %d", got, capacity)
	}
	if got := len(r.entries); got != capacity {
		t.Errorf("backing array = %d, want %d — the buffer grew", got, capacity)
	}
	if got := r.droppedCount(); got != 10_000-capacity {
		t.Errorf("dropped = %d, want %d", got, 10_000-capacity)
	}
}

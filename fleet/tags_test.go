package fleet

// Tags are developer-facing and called from inside request handlers, which sets
// the bar these tests enforce: fleet.Tag must be impossible to misuse. It cannot
// panic on a nil or untraced context, it cannot grow a span without bound, and
// the tags it produces cannot alias state that the framework recycles underneath
// them.

import (
	"strings"
	"testing"

	"github.com/nelthaarion/breeze/v2"
)

// tracedContext returns a context with fleet state attached, as the middleware
// would leave it. Tests build state directly rather than running the middleware
// so a tag failure cannot be blamed on request plumbing.
func tracedContext() (*breeze.Context, *requestState) {
	ctx := &breeze.Context{}
	st := &requestState{tc: NewTraceContext()}
	ctx.Set(ctxStateKey, st)
	return ctx, st
}

func TestTagDoesNotPanicWithoutATrace(t *testing.T) {
	// Tagging code is written once and then runs on every request, including
	// requests where tracing is disabled, the middleware was never installed,
	// or there is no request at all. Every one of those must be a no-op —
	// a nil-check the developer has to remember would defeat the point.
	before := ReadMetrics().TagsNoTrace

	Tag(nil, "order_id", "123")
	Tag(&breeze.Context{}, "order_id", "123")

	if got := ReadMetrics().TagsNoTrace; got <= before {
		t.Fatal("a tag with no active trace was silently ignored instead of counted")
	}
}

func TestTagOnUnusableStateIsCountedNotPanicked(t *testing.T) {
	// The context store is shared with application code, so the key could
	// hold something unexpected. Whatever it holds, tagging must degrade to
	// a no-op rather than take the request down with a type-assertion panic.
	ctx := &breeze.Context{}
	ctx.Set(ctxStateKey, "not a requestState")

	before := ReadMetrics().TagsNoTrace
	Tag(ctx, "order_id", "123")
	if got := ReadMetrics().TagsNoTrace; got <= before {
		t.Fatal("a tag against a corrupted state slot was not counted")
	}
}

func TestTagIsAlsoBaggageSoItReachesDownstreamServices(t *testing.T) {
	// This is the behaviour that makes "find everything that touched order
	// 123" work across a fleet: a tag set at the gateway has to travel, not
	// just land on one span. If this regressed, tags would still appear in
	// the UI for the service that set them, which is exactly why it needs its
	// own test.
	ctx, st := tracedContext()

	Tag(ctx, "order_id", "abc-123")

	if st.tags["order_id"] != "abc-123" {
		t.Fatalf("tag missing from span: %v", st.tags)
	}
	if st.baggage["order_id"] != "abc-123" {
		t.Fatalf("tag was not mirrored into baggage: %v", st.baggage)
	}
}

func TestTagCapDropsOnlyGenuinelyNewKeys(t *testing.T) {
	// The cap protects the span and the downstream header budget, but it must
	// not make an already-tracked key unwritable — updating a tag you already
	// set is not growth, and refusing it at the cap would be surprising.
	ctx, st := tracedContext()

	for i := 0; i < MaxTagsPerSpan; i++ {
		Tag(ctx, "k"+string(rune('a'+i%26))+string(rune('0'+i/26)), "v")
	}
	if len(st.tags) != MaxTagsPerSpan {
		t.Fatalf("expected %d tags, got %d", MaxTagsPerSpan, len(st.tags))
	}

	droppedBefore := ReadMetrics().TagsDropped
	Tag(ctx, "one-key-too-many", "v")
	if len(st.tags) != MaxTagsPerSpan {
		t.Fatalf("cap exceeded: %d tags", len(st.tags))
	}
	if ReadMetrics().TagsDropped <= droppedBefore {
		t.Fatal("a dropped tag was not counted, so the cap is invisible to operators")
	}

	// An existing key must still be updatable at the cap.
	existing := "ka0"
	Tag(ctx, existing, "updated")
	if st.tags[existing] != "updated" {
		t.Fatalf("existing key was not updatable at the cap: %v", st.tags[existing])
	}
}

func TestOversizedTagIsTruncatedRatherThanDropped(t *testing.T) {
	// A tag too long to send is still worth most of its value truncated —
	// whereas dropping it loses the debugging signal entirely, and sending it
	// risks an intermediary rejecting the whole request over header size.
	ctx, st := tracedContext()

	longKey := strings.Repeat("k", MaxTagKeyBytes*2)
	longValue := strings.Repeat("v", MaxTagValueBytes*2)

	before := ReadMetrics().TagsTruncated
	Tag(ctx, longKey, longValue)

	if ReadMetrics().TagsTruncated <= before {
		t.Fatal("truncation was not counted")
	}
	for k, v := range st.tags {
		if len(k) > MaxTagKeyBytes {
			t.Fatalf("key not truncated: %d bytes", len(k))
		}
		if len(v) > MaxTagValueBytes {
			t.Fatalf("value not truncated: %d bytes", len(v))
		}
	}
	if len(st.tags) != 1 {
		t.Fatalf("expected the oversized tag to be kept, got %v", st.tags)
	}
}

func TestTagsSnapshotCannotBeMutatedByALaterRequest(t *testing.T) {
	// The span outlives the request that produced it: it sits in a ring
	// buffer and is serialized later, on another goroutine, while the
	// request's state is recycled by the pool. If the snapshot aliased the
	// live map, a recycled request could rewrite tags on a span already
	// queued for export — a race that would surface as the wrong order_id on
	// the wrong trace, which is near-impossible to diagnose from the UI.
	ctx, st := tracedContext()
	Tag(ctx, "order_id", "original")

	snap := st.tagsSnapshot()

	Tag(ctx, "order_id", "mutated-after-snapshot")
	Tag(ctx, "added_later", "x")

	if snap["order_id"] != "original" {
		t.Fatalf("snapshot aliased live state: %v", snap["order_id"])
	}
	if _, leaked := snap["added_later"]; leaked {
		t.Fatal("a tag added after the snapshot leaked into it")
	}
}

func TestUntaggedRequestAllocatesNoTagMaps(t *testing.T) {
	// Most requests never tag anything, so the maps must stay nil until the
	// first Tag call — otherwise every request in the fleet pays two map
	// allocations for a feature it does not use.
	_, st := tracedContext()

	if st.tags != nil {
		t.Fatal("tags map allocated before any tag was set")
	}
	if st.baggage != nil {
		t.Fatal("baggage map allocated before any tag was set")
	}
	if snap := st.tagsSnapshot(); snap != nil {
		t.Fatalf("snapshot of an untagged span should be nil, got %v", snap)
	}
}

func TestEmptyTagKeyIsRejected(t *testing.T) {
	// An empty key is always a bug at the call site (usually an unset
	// variable). Storing it would put an unnamed value on the span and, worse,
	// an empty key into the baggage header.
	ctx, st := tracedContext()

	Tag(ctx, "", "value")

	if len(st.tags) != 0 {
		t.Fatalf("empty key was stored: %v", st.tags)
	}
}

package dashboard

import (
	"testing"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/video"
)

// TestVideoLiveNilSafe pins the guarantee that makes the tracker free when
// video is not attached: every method tolerates a nil receiver, so the API
// handler needs no branch and a dashboard without video allocates nothing.
func TestVideoLiveNilSafe(t *testing.T) {
	var v *videoLive
	v.record(video.StreamServed{File: "a.mp4"}, time.Now())
	v.sweep(time.Second)
	snap := v.Snapshot()
	if snap.Files == nil {
		t.Fatal("Snapshot on nil tracker must return an empty slice, not nil: " +
			"the JSON handler marshals it directly and null breaks the table")
	}
	if len(snap.Files) != 0 {
		t.Fatalf("want no files, got %d", len(snap.Files))
	}
}

// TestVideoLiveGroupsByFile is the whole point of the page: many requests
// for one file must collapse into one row.
func TestVideoLiveGroupsByFile(t *testing.T) {
	v := newVideoLive()
	now := time.Now()

	for i := 0; i < 5; i++ {
		v.record(video.StreamServed{
			File:     "film.mp4",
			Status:   206,
			Bytes:    1000,
			Partial:  true,
			Duration: 2 * time.Millisecond,
		}, now)
	}
	v.record(video.StreamServed{File: "other.mp4", Status: 200, Bytes: 50}, now)

	snap := v.Snapshot()
	if len(snap.Files) != 2 {
		t.Fatalf("want 2 rows (one per file), got %d", len(snap.Files))
	}
	if snap.TotalRequests != 6 {
		t.Errorf("TotalRequests = %d, want 6", snap.TotalRequests)
	}
	if snap.TotalBytes != 5050 {
		t.Errorf("TotalBytes = %d, want 5050", snap.TotalBytes)
	}

	// Busiest first, so the file consuming the bandwidth is row one.
	if snap.Files[0].File != "film.mp4" {
		t.Errorf("want film.mp4 first (highest rate), got %q", snap.Files[0].File)
	}
	f := snap.Files[0]
	if f.Requests != 5 || f.Partial != 5 || f.Bytes != 5000 {
		t.Errorf("aggregation wrong: requests=%d partial=%d bytes=%d",
			f.Requests, f.Partial, f.Bytes)
	}
	if f.AvgMS < 1.9 || f.AvgMS > 2.1 {
		t.Errorf("AvgMS = %v, want ~2", f.AvgMS)
	}
}

// TestVideoLiveRateUsesWindow guards the deliberate choice to divide by the
// window rather than by the span between samples. Dividing by the span
// would turn two adjacent requests into an absurd rate.
func TestVideoLiveRateUsesWindow(t *testing.T) {
	v := newVideoLive()
	now := time.Now()

	// Two bursts 3ms apart. Span-based math would report ~66 MB/s.
	v.record(video.StreamServed{File: "a.mp4", Bytes: 100 << 10, Partial: true}, now)
	v.record(
		video.StreamServed{File: "a.mp4", Bytes: 100 << 10, Partial: true},
		now.Add(3*time.Millisecond),
	)

	got := v.Snapshot().Files[0].BytesPerSec
	want := float64(200<<10) / videoRateWindow.Seconds()
	if got < want*0.9 || got > want*1.1 {
		t.Fatalf("BytesPerSec = %v, want ~%v (total bytes over the fixed window)", got, want)
	}
}

// TestVideoLiveNotModifiedExcludedFromRate keeps a well-cached file from
// looking slow: a 304 moves no body, so it must not enter the rate window
// as a zero-byte sample.
func TestVideoLiveNotModifiedExcludedFromRate(t *testing.T) {
	v := newVideoLive()
	now := time.Now()

	v.record(video.StreamServed{File: "a.mp4", Status: 304}, now)
	v.record(video.StreamServed{File: "a.mp4", Status: 304}, now)

	f := v.Snapshot().Files[0]
	if f.NotModified != 2 {
		t.Errorf("NotModified = %d, want 2", f.NotModified)
	}
	if f.BytesPerSec != 0 {
		t.Errorf("BytesPerSec = %v, want 0 (304s carry no body)", f.BytesPerSec)
	}
	if f.Partial != 0 {
		t.Errorf("a 304 must not be counted as partial, got %d", f.Partial)
	}
}

// TestVideoLiveDisconnectNotError separates the two, because seeking
// abandons a response and is not a failure.
func TestVideoLiveDisconnectNotError(t *testing.T) {
	v := newVideoLive()
	now := time.Now()

	v.record(video.StreamServed{File: "a.mp4", Disconnected: true, Err: "broken pipe"}, now)
	v.record(video.StreamServed{File: "a.mp4", Err: "read failed"}, now)

	f := v.Snapshot().Files[0]
	if f.Disconnects != 1 {
		t.Errorf("Disconnects = %d, want 1", f.Disconnects)
	}
	if f.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the disconnect must not double-count)", f.Errors)
	}
}

// TestVideoLiveEvictionKeepsActive checks that a stream in progress is
// never dropped to make room for a one-off request.
func TestVideoLiveEvictionKeepsActive(t *testing.T) {
	v := newVideoLive()
	now := time.Now()

	// One active file, then flood past the cap with stale ones.
	v.record(video.StreamServed{File: "active.mp4", Bytes: 10, Partial: true}, now)
	stale := now.Add(-2 * videoIdleTTL)
	for i := 0; i < videoFileMax*2; i++ {
		v.record(video.StreamServed{File: string(rune('a'+i%26)) + "-old.mp4"}, stale)
	}

	if len(v.files) > videoFileMax {
		t.Fatalf("map unbounded: %d entries, cap is %d", len(v.files), videoFileMax)
	}
	if _, ok := v.files["active.mp4"]; !ok {
		t.Error("the active stream was evicted; idle files must go first")
	}
}

// TestVideoLiveSweep confirms idle files eventually leave the page.
func TestVideoLiveSweep(t *testing.T) {
	v := newVideoLive()
	v.record(video.StreamServed{File: "old.mp4"}, time.Now().Add(-time.Hour))
	v.record(video.StreamServed{File: "new.mp4"}, time.Now())

	v.sweep(time.Minute)

	snap := v.Snapshot()
	if len(snap.Files) != 1 || snap.Files[0].File != "new.mp4" {
		t.Fatalf("sweep should keep only new.mp4, got %+v", snap.Files)
	}
	// Totals are cumulative and must survive the trim, or the header
	// figures would drop whenever the table is pruned.
	if snap.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2 (cumulative across evicted files)", snap.TotalRequests)
	}
}

// TestAttachVideoEndToEnd runs the real path: a bus event reaches the
// collector's snapshot, and detaching stops it.
func TestAttachVideoEndToEnd(t *testing.T) {
	c := newCollector(Config{}, nil)
	if c.VideoAttached() {
		t.Fatal("a fresh collector must not report video attached")
	}

	bus := events.New()
	detach := c.AttachVideo(bus)

	if !c.VideoAttached() {
		t.Fatal("VideoAttached false after AttachVideo")
	}

	events.EmitBus(bus, video.StreamServed{
		File: "e2e.mp4", Status: 206, Bytes: 4096, Partial: true,
	})

	waitFor(t, func() bool {
		return c.videoTracker().Snapshot().TotalRequests == 1
	}, "event never reached the tracker")

	detach()
	if c.VideoAttached() {
		t.Error("VideoAttached true after detach")
	}

	// After detaching, further events must not be counted.
	events.EmitBus(bus, video.StreamServed{File: "e2e.mp4", Bytes: 1})
	time.Sleep(50 * time.Millisecond)
	if got := c.videoTracker().Snapshot().TotalRequests; got != 0 {
		t.Errorf("detached tracker still counting: TotalRequests=%d", got)
	}
}

// waitFor polls cond briefly, since async dispatch has no completion hook.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

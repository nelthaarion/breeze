package dashboard

import (
	"github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/events"
)

// AttachVideo starts tracking video streaming published on bus and returns
// a detach function.
//
// It is separate from [Collector.AttachEvents] because the two answer
// different questions from the same bus. AttachEvents records every event
// as history; this accumulates per-file streaming state, which only makes
// sense for an application that actually serves video. Folding it into
// AttachEvents would put a tracker and a sweep goroutine into every
// dashboard, including the ones with no media at all.
//
// Wiring is one line next to the mount:
//
//	video.Mount(router, video.Config{Root: "./media", Bus: bus})
//	defer coll.AttachVideo(bus)()
//
// Calling it twice replaces the first tracker: the previous subscription is
// detached, so events are not counted twice.
func (c *Collector) AttachVideo(bus *events.Bus) (detach func()) {
	if bus == nil {
		return func() {}
	}

	live := newVideoLive()
	detachLive := live.attach(bus)

	c.eventsMu.Lock()
	prev := c.vidDetach
	c.vidLive = live
	c.vidDetach = detachLive
	c.eventsMu.Unlock()

	// Detaching outside the lock: the previous detach closes a channel and
	// unsubscribes, and holding the collector's lock across that would
	// invite a deadlock with a listener that reads collector state.
	if prev != nil {
		prev()
	}

	return func() {
		c.eventsMu.Lock()
		if c.vidLive == live {
			c.vidLive = nil
			c.vidDetach = nil
		}
		c.eventsMu.Unlock()
		detachLive()
	}
}

// VideoAttached reports whether video tracking is wired up.
//
// The page uses this to distinguish "no video has been served yet" from
// "video tracking was never attached" — an empty table means something
// quite different in each case.
func (c *Collector) VideoAttached() bool {
	c.eventsMu.RLock()
	defer c.eventsMu.RUnlock()
	return c.vidLive != nil
}

// videoTracker returns the live tracker, or nil.
func (c *Collector) videoTracker() *videoLive {
	c.eventsMu.RLock()
	defer c.eventsMu.RUnlock()
	return c.vidLive
}

// handleVideo serves the video streaming snapshot.
//
// A dashboard with no video attached returns an empty snapshot with
// attached:false rather than 404, so the page renders its empty state
// instead of showing a failed request in the browser console.
func (c *Collector) handleVideo(ctx *breeze.Context) error {
	snap := c.videoTracker().Snapshot()
	return ctx.JSON(map[string]any{
		"attached":            c.VideoAttached(),
		"files":               snap.Files,
		"total_bytes_per_sec": snap.TotalBytesPerSec,
		"total_bytes":         snap.TotalBytes,
		"total_requests":      snap.TotalRequests,
		"active_files":        snap.ActiveFiles,

		// The window is reported so the UI can label the rate honestly
		// rather than hardcoding a number that drifts from the server's.
		"window_seconds": videoRateWindow.Seconds(),
	})
}

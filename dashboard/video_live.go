package dashboard

import (
	"sort"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/v2/events"
	"github.com/nelthaarion/breeze/v2/video"
)

// This file tracks video streaming while it is happening.
//
// The observability ring buffer already stores one signal per completed
// video request, which is enough to answer "what was served" and useless
// for "what is playing". A viewer watching a film is not one request; it
// is a few hundred range requests spread over two hours, and the ring
// buffer holds them as unrelated rows, oldest evicted first. Ask it for
// current bandwidth and it cannot tell you: it has no notion of a series
// of requests belonging to one file, and by the time a long session is
// interesting its early requests are gone.
//
// So per-file state is accumulated here from the same events the video
// package already publishes. The video package stays unaware of the
// dashboard, and finished requests stay in the ring buffer where they
// belong.
//
// Cost when nothing is watching: one nil check. The tracker is only
// created by AttachVideo, and every method tolerates a nil receiver, so a
// dashboard without video attached does no work and allocates nothing.

// videoFileMax bounds how many distinct files are tracked. A media root
// can hold more files than a page can show, and an unbounded map keyed by
// a request path is a memory leak that a scanner could drive.
const videoFileMax = 64

// videoRateWindow is how much history each file keeps for its bandwidth
// figure. Ten seconds is long enough to smooth the sawtooth of range
// requests — a player fetches a burst, then idles — and short enough that
// the number still reacts when a viewer stops.
const videoRateWindow = 10 * time.Second

// videoIdleTTL is how long a file with no traffic stays on the page. A
// player that has buffered ahead can legitimately go quiet for a while, so
// this is well beyond a normal gap: it marks "stopped watching", not
// "between requests".
const videoIdleTTL = 60 * time.Second

// videoSample is one request's contribution to the rate window.
type videoSample struct {
	at    time.Time
	bytes int64
}

// videoFile is the accumulated state of one file being served.
type videoFile struct {
	File string `json:"file"`

	// Requests is every request seen, including errors, so a file that
	// is failing does not look idle.
	Requests int64 `json:"requests"`

	// Partial counts 206 responses. A healthy video session is almost
	// entirely partial; a file with none is being downloaded whole,
	// which usually means a client that ignores ranges.
	Partial int64 `json:"partial"`

	// Bytes is the total sent for this file since tracking began.
	Bytes int64 `json:"bytes"`

	// Disconnects counts viewers who went away mid-transfer. This is
	// normal for video — a seek abandons the current response — so it is
	// reported separately from errors rather than mixed in.
	Disconnects int64 `json:"disconnects"`

	// Errors counts genuine failures, excluding disconnects.
	Errors int64 `json:"errors"`

	// NotModified counts 304s: requests served entirely from the
	// client's cache. This is the cache-effectiveness number, which is
	// why it is kept apart from Requests rather than inferred later.
	NotModified int64 `json:"not_modified"`

	// FirstSeen and LastSeen bound the session.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`

	// BytesPerSec is the smoothed rate over [videoRateWindow].
	BytesPerSec float64 `json:"bytes_per_sec"`

	// AvgMS is the mean request duration, which separates "slow disk"
	// from "slow link" when read next to the rate.
	AvgMS float64 `json:"avg_ms"`

	// Active reports recent traffic, so the UI can distinguish a stream
	// in progress from one that has stopped but is still listed.
	Active bool `json:"active"`

	// samples is the rate window. It is a plain slice because it holds a
	// few dozen entries at most and is scanned in full on every read; a
	// ring buffer here would be more code for no measurable gain.
	samples []videoSample `json:"-"`

	totalMS float64 `json:"-"`
	seq     uint64  `json:"-"`
}

// videoLive tracks per-file streaming state.
//
// Like workflowLive it is a separate type from Collector, so the locking
// stays visible: one mutex, held for map and slice updates only, never
// across a callback or an I/O call.
type videoLive struct {
	mu    sync.RWMutex
	files map[string]*videoFile
	seq   uint64

	// Totals are cumulative across every file, including files that have
	// since been evicted, so the header figures do not drop when the
	// table is trimmed.
	totalBytes    int64
	totalRequests int64
}

func newVideoLive() *videoLive {
	return &videoLive{files: make(map[string]*videoFile, 8)}
}

// videoSnapshot is what the API returns: the per-file rows plus the
// aggregate figures the page shows in its header.
type videoSnapshot struct {
	Files []videoFile `json:"files"`

	// TotalBytesPerSec is the sum of the per-file rates. It is computed
	// from the same window as the rows so the header cannot disagree
	// with the sum of the table.
	TotalBytesPerSec float64 `json:"total_bytes_per_sec"`

	TotalBytes    int64 `json:"total_bytes"`
	TotalRequests int64 `json:"total_requests"`

	// ActiveFiles counts files with recent traffic, which is the closest
	// honest answer to "how many people are watching": HTTP gives no
	// viewer identity, so a file is the unit that can be counted without
	// inventing one.
	ActiveFiles int `json:"active_files"`
}

// record folds one served request into the tracked state.
func (v *videoLive) record(e video.StreamServed, at time.Time) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	// A request that failed before the name was resolved has none, and
	// grouping those under an empty key would produce a phantom row.
	name := e.File
	if name == "" {
		name = "(unresolved)"
	}

	f := v.files[name]
	if f == nil {
		v.seq++
		f = &videoFile{File: name, FirstSeen: at, seq: v.seq}
		v.files[name] = f
	}

	f.Requests++
	f.LastSeen = at
	f.Bytes += e.Bytes
	f.totalMS += float64(e.Duration) / float64(time.Millisecond)

	switch {
	case e.Status == 304:
		f.NotModified++
	case e.Partial:
		f.Partial++
	}
	if e.Disconnected {
		f.Disconnects++
	} else if e.Err != "" {
		f.Errors++
	}

	v.totalBytes += e.Bytes
	v.totalRequests++

	// Only transfers enter the rate window. A 304 moves no body, and
	// counting it as a zero-byte sample would drag the average down and
	// make a well-cached file look slow.
	if e.Bytes > 0 {
		f.samples = append(f.samples, videoSample{at: at, bytes: e.Bytes})
		f.trimLocked(at)
	}

	v.evictLocked()
}

// trimLocked drops samples that have aged out of the rate window.
func (f *videoFile) trimLocked(now time.Time) {
	cutoff := now.Add(-videoRateWindow)
	i := 0
	for i < len(f.samples) && f.samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		f.samples = append(f.samples[:0], f.samples[i:]...)
	}
}

// rate returns the bytes/sec implied by the current window.
//
// The divisor is the window length, not the span between the first and
// last sample. Dividing by the span would report a burst of two adjacent
// requests as an enormous rate — 200 KiB over 3 ms is not 66 MB/s of
// sustained throughput — and the figure would swing wildly with request
// timing rather than describing the stream.
func (f *videoFile) rate(now time.Time) float64 {
	if len(f.samples) == 0 {
		return 0
	}
	var total int64
	cutoff := now.Add(-videoRateWindow)
	for _, s := range f.samples {
		if !s.at.Before(cutoff) {
			total += s.bytes
		}
	}
	if total == 0 {
		return 0
	}
	return float64(total) / videoRateWindow.Seconds()
}

// evictLocked bounds the map, dropping idle files before active ones so a
// stream in progress is never removed to make room for a one-off request.
func (v *videoLive) evictLocked() {
	if len(v.files) <= videoFileMax {
		return
	}
	now := time.Now()
	type ref struct {
		key    string
		seq    uint64
		active bool
	}
	refs := make([]ref, 0, len(v.files))
	for k, f := range v.files {
		refs = append(refs, ref{key: k, seq: f.seq, active: now.Sub(f.LastSeen) < videoIdleTTL})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].active != refs[j].active {
			return !refs[i].active
		}
		return refs[i].seq < refs[j].seq
	})
	for i := 0; i < len(refs) && len(v.files) > videoFileMax; i++ {
		delete(v.files, refs[i].key)
	}
}

// Snapshot returns the tracked files, busiest first.
//
// Ordering is by current rate rather than by name or recency, because the
// question this page answers is "what is consuming the bandwidth" and the
// answer should be the first row.
func (v *videoLive) Snapshot() videoSnapshot {
	if v == nil {
		return videoSnapshot{Files: []videoFile{}}
	}
	v.mu.RLock()
	defer v.mu.RUnlock()

	now := time.Now()
	out := make([]videoFile, 0, len(v.files))
	var totalRate float64
	active := 0

	for _, f := range v.files {
		// Copy: a caller marshalling this must not race with the next
		// event appending to the sample slice.
		cp := *f
		cp.samples = nil
		cp.BytesPerSec = f.rate(now)
		cp.Active = now.Sub(f.LastSeen) < videoIdleTTL
		if f.Requests > 0 {
			cp.AvgMS = f.totalMS / float64(f.Requests)
		}
		totalRate += cp.BytesPerSec
		if cp.Active {
			active++
		}
		out = append(out, cp)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].BytesPerSec != out[j].BytesPerSec {
			return out[i].BytesPerSec > out[j].BytesPerSec
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})

	return videoSnapshot{
		Files:            out,
		TotalBytesPerSec: totalRate,
		TotalBytes:       v.totalBytes,
		TotalRequests:    v.totalRequests,
		ActiveFiles:      active,
	}
}

// sweep drops files idle for longer than ttl.
func (v *videoLive) sweep(ttl time.Duration) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	cutoff := time.Now().Add(-ttl)
	for k, f := range v.files {
		if f.LastSeen.Before(cutoff) {
			delete(v.files, k)
		}
	}
}

// attach subscribes the tracker to a bus and returns the detach function.
//
// The listener returns nil unconditionally: the dashboard is an observer,
// and a bookkeeping problem here must never turn into a failed video
// response.
func (v *videoLive) attach(bus *events.Bus) func() {
	if bus == nil || v == nil {
		return func() {}
	}

	sub := events.OnTypeBus[video.StreamServed](
		bus,
		func(_ *events.Context, e video.StreamServed) error {
			v.record(e, time.Now())
			return nil
		},
	)

	// Idle files are swept on a timer rather than on the next event, so
	// a server that stops serving video does not leave its last file on
	// the page forever.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// Swept well after a file stops counting as active, so
				// a stopped stream is visible as stopped for a while
				// before it disappears.
				v.sweep(5 * time.Minute)
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			sub.Unsubscribe()
		})
	}
}

package dashboard

import (
	"math"
	"runtime"
	"sync"
	"time"
)

// finite replaces NaN and ±Inf with 0. A snapshot that holds one cannot be encoded to JSON, and
// the WebSocket hub would then have nothing to send until the value moved on.
func finite(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

// metricsSampler periodically captures MetricsSnapshot points into the
// collector's ring buffer. The sampling rate is fixed at 1 Hz which gives
// 10 minutes of history at the default ring buffer capacity of 600.
//
// Every snapshot is a FRESH read of runtime.MemStats — we never reuse old
// stats. Each snapshot stores CURRENT values (not cumulative, except for
// counters that are inherently cumulative: TotalAlloc, Mallocs, Frees,
// NumGC, PauseTotalNs).
//
// CPU usage is computed as the change in process CPU time (user+system)
// between samples divided by the wall-clock interval. 100 is one core fully
// busy, so a process using four cores reads 400 (the figure top and ps give);
// NumCPU is in every snapshot for a reader that wants a share of the machine.
type metricsSampler struct {
	c         *Collector
	stop      chan struct{}
	lastUser  time.Duration
	lastSys   time.Duration
	lastTime  time.Time
	lastReqs  int64
	lastErrs  int64
	lastDurUS int64
}

// startMetricsSampler launches the sampler goroutine and returns a stop fn.
func startMetricsSampler(c *Collector) func() {
	s := &metricsSampler{
		c:        c,
		stop:     make(chan struct{}),
		lastTime: time.Now(),
	}
	go s.loop()
	var once sync.Once
	return func() { once.Do(func() { close(s.stop) }) }
}

func (s *metricsSampler) loop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.sample(now)
		}
	}
}

func (s *metricsSampler) sample(now time.Time) {
	// Fresh MemStats every sample — never reuse.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// CPU: per-process user+system time.
	userTime, sysTime := cpuTimes()
	dt := now.Sub(s.lastTime)
	cpuPct := 0.0
	if dt > 0 && !s.lastTime.IsZero() {
		du := userTime - s.lastUser
		ds := sysTime - s.lastSys
		cpuPct = float64(du+ds) / float64(dt) * 100.0
	}

	// Requests per second.
	curReqs := s.c.requestsTotal.Load()
	rps := 0.0
	if !s.lastTime.IsZero() && dt > 0 {
		rps = float64(curReqs-s.lastReqs) / dt.Seconds()
		if rps < 0 {
			rps = 0
		}
	}

	// Average response time and error rate over the interval since the last sample.
	//
	// These were computed from the ring buffer of *captured* requests, which holds only the slow
	// and the failed ones while nobody is watching (and copied all of it, once a second, to do
	// it). So with the dashboard closed the "average response time" was the average of the slow
	// requests, and the "error rate" the share of failures among the failures — routinely 100% —
	// and neither covered the last second, as the comment said. The middleware keeps exact
	// counters for every request; the change in them between two samples is the interval.
	curErrs := s.c.errorsTotal.Load()
	curDurUS := s.c.totalDurationUS()
	avgRespMS, errRate := 0.0, 0.0
	if !s.lastTime.IsZero() {
		if dReqs := curReqs - s.lastReqs; dReqs > 0 {
			avgRespMS = float64(curDurUS-s.lastDurUS) / float64(dReqs) / 1000.0
			errRate = float64(curErrs-s.lastErrs) / float64(dReqs)
			if avgRespMS < 0 {
				avgRespMS = 0
			}
			errRate = math.Min(math.Max(errRate, 0), 1)
		}
	}

	// Cache hit ratio.
	cache := s.c.CacheStats()
	queue := s.c.QueueStats()

	// Sessions that are still valid. This read a counter nothing ever incremented, so the
	// Overview's "Active Sessions" card said 0 however many people were signed in.
	var sessions int64
	if s.c.sessions != nil {
		sessions = int64(s.c.sessions.count(now))
	}

	// time.Unix(0, 0) is 1970, which the page showed as a last GC fifty-six years ago.
	var lastGC time.Time
	if ms.LastGC > 0 {
		lastGC = time.Unix(0, int64(ms.LastGC))
	}

	// Most recent GC pause duration.
	//
	// runtime.MemStats.PauseNs is a circular buffer of 256 entries.
	// The entry for GC number n is at index n % 256. The most recent GC
	// is number ms.NumGC, so its pause is at PauseNs[(ms.NumGC-1+256)%256].
	//
	// We do NOT use debug.GCStats.Pause[0] — that slice is filled
	// oldest-first, so Pause[0] is the oldest pause, not the most recent.
	// This was the root cause of the GC chart showing a flat line.
	var lastPauseNs uint64
	if ms.NumGC > 0 {
		lastPauseNs = ms.PauseNs[(ms.NumGC-1+256)%256]
	}

	// GC is enabled unless GOGC=off (set via runtime/debug.SetGCPercent(-1)).
	gcEnabled := true
	if gcPct := debugGCPercent(); gcPct < 0 {
		gcEnabled = false
	}

	snap := MetricsSnapshot{
		Time: now,

		// HTTP
		RequestsTotal:  curReqs,
		RequestsToday:  s.c.TodayCount(),
		RequestsPerSec: finite(rps),
		AvgRespTimeMS:  finite(avgRespMS),
		ErrorRate:      finite(errRate),
		ActiveSessions: sessions,

		// Runtime — heap (snapshots from runtime.MemStats)
		Goroutines:   runtime.NumGoroutine(),
		HeapAlloc:    ms.HeapAlloc,    // live heap bytes — DROPS after GC
		HeapSys:      ms.HeapSys,      // heap bytes from OS
		HeapIdle:     ms.HeapIdle,     // idle heap bytes
		HeapInuse:    ms.HeapInuse,    // in-use heap bytes
		HeapReleased: ms.HeapReleased, // bytes returned to OS
		HeapObjects:  ms.HeapObjects,  // count of live heap objects
		TotalAlloc:   ms.TotalAlloc,   // cumulative (only grows)
		Mallocs:      ms.Mallocs,      // cumulative (only grows)
		Frees:        ms.Frees,        // cumulative (only grows)

		// Runtime — stack
		StackInUse: ms.StackInuse,
		StackSys:   ms.StackSys,

		// Runtime — other system memory
		MSpanInuse:  ms.MSpanInuse,
		MSpanSys:    ms.MSpanSys,
		MCacheInuse: ms.MCacheInuse,
		MCacheSys:   ms.MCacheSys,
		BuckHashSys: ms.BuckHashSys,
		OtherSys:    ms.OtherSys,

		// Runtime — total from OS
		Sys: ms.Sys,

		// GC
		NumGC:         ms.NumGC,
		LastGC:        lastGC,
		NextGC:        ms.NextGC,
		PauseTotalNs:  ms.PauseTotalNs, // cumulative (only grows)
		PauseNs:       lastPauseNs,     // most recent GC pause
		GCCPUFraction: finite(ms.GCCPUFraction),
		GCEnabled:     gcEnabled,

		// CPU
		CPUUsage:   finite(cpuPct),
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		CGOCalls:   runtime.NumCgoCall(),

		// Cache
		CacheHitRatio: finite(cache.HitRate),

		// Queue
		QueueJobs: queue.Pending + queue.Running,
	}

	s.c.metrics.Push(snap)

	s.lastUser = userTime
	s.lastSys = sysTime
	s.lastTime = now
	s.lastReqs = curReqs
	s.lastErrs = curErrs
	s.lastDurUS = curDurUS
}

// cpuTimes returns process CPU time (user, system).
func cpuTimes() (time.Duration, time.Duration) {
	return cpuUsage()
}

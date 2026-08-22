package dashboard

import (
	"os"
	"runtime"
	"time"
)

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
// between samples divided by the wall-clock interval, normalised by the
// number of cores.
type metricsSampler struct {
	c        *Collector
	stop     chan struct{}
	lastUser time.Duration
	lastSys  time.Duration
	lastTime time.Time
	lastReqs int64
}

// startMetricsSampler launches the sampler goroutine and returns a stop fn.
func startMetricsSampler(c *Collector) func() {
	s := &metricsSampler{
		c:        c,
		stop:     make(chan struct{}),
		lastTime: time.Now(),
	}
	go s.loop()
	return func() { close(s.stop) }
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

	// Average response time over the last 1s window.
	recent := s.c.Requests(0)
	avgRespMS := 0.0
	if len(recent) > 0 {
		var sum int64
		for _, r := range recent {
			sum += r.Duration
		}
		avgRespMS = float64(sum) / float64(len(recent)) / 1000.0
	}

	// Error rate over the last 1s window.
	errRate := 0.0
	if len(recent) > 0 {
		var errs int64
		for _, r := range recent {
			if r.Status >= 500 {
				errs++
			}
		}
		errRate = float64(errs) / float64(len(recent))
	}

	// Cache hit ratio.
	cache := s.c.CacheStats()

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
		RequestsPerSec: rps,
		AvgRespTimeMS:  avgRespMS,
		ErrorRate:      errRate,
		ActiveSessions: s.c.activeSessions.Load(),

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
		LastGC:        time.Unix(0, int64(ms.LastGC)),
		NextGC:        ms.NextGC,
		PauseTotalNs:  ms.PauseTotalNs, // cumulative (only grows)
		PauseNs:       lastPauseNs,     // most recent GC pause
		GCCPUFraction: ms.GCCPUFraction,
		GCEnabled:     gcEnabled,

		// CPU
		CPUUsage:   cpuPct,
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		CGOCalls:   runtime.NumCgoCall(),

		// Cache
		CacheHitRatio: cache.HitRate,

		// Queue
		QueueJobs: s.c.QueueStats().Pending + s.c.QueueStats().Running,
	}

	s.c.metrics.Push(snap)

	s.lastUser = userTime
	s.lastSys = sysTime
	s.lastTime = now
	s.lastReqs = curReqs
}

// cpuTimes returns process CPU time (user, system).
func cpuTimes() (time.Duration, time.Duration) {
	return cpuUsage()
}

// pid is cached so we don't re-read on every sample.
var procPid = os.Getpid()

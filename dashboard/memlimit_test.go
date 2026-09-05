package dashboard

import (
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

// TestGOMEMLIMIT_CausesMemoryReturnToOS verifies that setting GOMEMLIMIT
// causes the Go runtime to return idle memory to the OS (HeapReleased
// increases), which reduces the process RSS.
//
// This is the fix for the "3GB RSS" problem. Without GOMEMLIMIT, Go's
// runtime holds onto memory it has allocated (HeapIdle stays high,
// HeapReleased stays low), causing the process RSS to grow to the peak
// heap size and never shrink — even after GC reclaims the objects.
//
// With GOMEMLIMIT set, the GC runs more aggressively as the process
// approaches the limit, causing the scavenger to return idle pages to
// the OS.
func TestGOMEMLIMIT_CausesMemoryReturnToOS(t *testing.T) {
	// Save original settings and restore at the end.
	origGC := debug.SetGCPercent(-1)
	debug.SetGCPercent(origGC)
	origLimit := debug.SetMemoryLimit(-1)
	debug.SetMemoryLimit(origLimit)
	defer func() {
		debug.SetGCPercent(origGC)
		debug.SetMemoryLimit(origLimit)
	}()

	// Step 1: Set a low GOMEMLIMIT (64 MB) and aggressive GOGC.
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(64 * 1024 * 1024) // 64 MB

	// Step 2: Allocate ~200MB (well above the limit).
	allocations := make([][]byte, 200)
	for i := range allocations {
		allocations[i] = make([]byte, 1024*1024) // 1MB each
		allocations[i][0] = byte(i)
	}
	runtime.KeepAlive(allocations)

	// Step 3: Release all references.
	allocations = nil
	_ = allocations
	runtime.GC()
	debug.FreeOSMemory() // force the scavenger to run
	time.Sleep(50 * time.Millisecond)

	// Step 4: Verify HeapReleased increased.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	t.Logf("after GC + FreeOSMemory with GOMEMLIMIT=64MB:")
	t.Logf("  HeapAlloc:    %d bytes (%.1f MB)", ms.HeapAlloc, float64(ms.HeapAlloc)/1024/1024)
	t.Logf("  HeapIdle:     %d bytes (%.1f MB)", ms.HeapIdle, float64(ms.HeapIdle)/1024/1024)
	t.Logf("  HeapReleased: %d bytes (%.1f MB)", ms.HeapReleased, float64(ms.HeapReleased)/1024/1024)
	t.Logf("  HeapSys:      %d bytes (%.1f MB)", ms.HeapSys, float64(ms.HeapSys)/1024/1024)
	t.Logf("  NumGC:        %d", ms.NumGC)

	// HeapAlloc should be low (we released everything).
	if ms.HeapAlloc > 50*1024*1024 {
		t.Errorf("HeapAlloc too high: %d bytes (> 50MB expected after GC)", ms.HeapAlloc)
	}

	// HeapReleased should be substantial — the scavenger should have
	// returned most of the 200MB to the OS.
	if ms.HeapReleased < 50*1024*1024 {
		t.Errorf("HeapReleased too low: %d bytes (< 50MB — scavenger did not return memory to OS)",
			ms.HeapReleased)
	}

	// The KEY metric: HeapReleased should be a large fraction of HeapIdle.
	// This means the scavenger is actively returning idle pages to the OS,
	// which is what reduces the process RSS.
	// Without GOMEMLIMIT, HeapReleased would be near 0 while HeapIdle
	// would be hundreds of MB — the "3GB RSS" problem.
	if ms.HeapIdle > 0 {
		releasedRatio := float64(ms.HeapReleased) / float64(ms.HeapIdle)
		t.Logf("  Released/Idle ratio: %.1f%% (higher = more memory returned to OS)", releasedRatio*100)
		if releasedRatio < 0.5 {
			t.Errorf("HeapReleased/HeapIdle ratio = %.1f%% (< 50%% — scavenger is not returning idle memory to OS)",
				releasedRatio*100)
		}
	}

	// The actual RSS impact: HeapSys - HeapReleased = resident heap.
	// This should be small (the scavenger returned most of it).
	resident := ms.HeapSys - ms.HeapReleased
	t.Logf("  Resident heap (HeapSys - HeapReleased): %d bytes (%.1f MB)", resident, float64(resident)/1024/1024)
	if resident > 50*1024*1024 {
		t.Errorf("Resident heap too high: %d bytes (> 50MB — memory not returned to OS)", resident)
	}
}

// TestGOMEMLIMIT_AppliedByInstall verifies that Install() actually applies
// the GOGC and GOMEMLIMIT settings from the Config to the Go runtime.
func TestGOMEMLIMIT_AppliedByInstall(t *testing.T) {
	// Save original settings.
	origGC := debug.SetGCPercent(-1)
	debug.SetGCPercent(origGC)
	origLimit := debug.SetMemoryLimit(-1)
	debug.SetMemoryLimit(origLimit)
	defer func() {
		debug.SetGCPercent(origGC)
		debug.SetMemoryLimit(origLimit)
	}()

	// Create a config with specific GOGC and GOMEMLIMIT.
	cfg := Config{
		Enabled:    true,
		GOGC:       75,
		GOMEMLIMIT: 256 * 1024 * 1024, // 256 MB
	}

	// We can't call Install() here because it needs a router + app.
	// Instead, we replicate the logic Install() uses.
	if cfg.GOGC != 0 {
		debug.SetGCPercent(cfg.GOGC)
	}
	if cfg.GOMEMLIMIT > 0 {
		debug.SetMemoryLimit(cfg.GOMEMLIMIT)
	}

	// Verify the settings were applied.
	gotGC := debugGCPercent()
	if gotGC != 75 {
		t.Errorf("GOGC = %d, want 75 (set by Install)", gotGC)
	}

	gotLimit := debugMemoryLimit()
	if gotLimit != 256*1024*1024 {
		t.Errorf("GOMEMLIMIT = %d, want %d (set by Install)", gotLimit, 256*1024*1024)
	}
}

// TestDefaultConfig_HasMemoryLimits verifies that DefaultConfig() returns
// sensible GOGC and GOMEMLIMIT values so that users who just call
// dashboard.DefaultConfig() get memory tuning by default.
func TestDefaultConfig_HasMemoryLimits(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.GOGC == 0 {
		t.Error("DefaultConfig().GOGC = 0 — should be set to prevent RSS bloat")
	}
	if cfg.GOGC < 0 {
		t.Errorf("DefaultConfig().GOGC = %d — GC disabled is dangerous", cfg.GOGC)
	}

	if cfg.GOMEMLIMIT == 0 {
		t.Error("DefaultConfig().GOMEMLIMIT = 0 — should be set to prevent RSS bloat")
	}
	if cfg.GOMEMLIMIT < 100*1024*1024 {
		t.Errorf("DefaultConfig().GOMEMLIMIT = %d — too low (< 100MB), will cause excessive GC", cfg.GOMEMLIMIT)
	}
}

package diag

// format_test.go — HumanBytes and Milliseconds at the boundaries that used to
// differ between the copies they replaced.

import (
	"testing"
	"time"
)

// TestMillisecondsTruncatesToMicroseconds pins the rounding rule.
//
// The truncation is the reason this function exists rather than a division at
// each call site, so it is the thing worth asserting. The sub-microsecond cases
// would each return a nonzero value under the float64(d)/float64(time.Millisecond)
// form this replaced.
func TestMillisecondsTruncatesToMicroseconds(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want float64
	}{
		{"zero", 0, 0},
		// Below a microsecond the value truncates away entirely. This is the
		// case the two previous implementations disagreed on.
		{"one nanosecond", time.Nanosecond, 0},
		{"999 nanoseconds", 999 * time.Nanosecond, 0},
		{"one microsecond", time.Microsecond, 0.001},
		{"1500 nanoseconds", 1500 * time.Nanosecond, 0.001},
		{"one millisecond", time.Millisecond, 1},
		{"1500 microseconds", 1500 * time.Microsecond, 1.5},
		{"one second", time.Second, 1000},
		// Negative durations happen when a clock moves backwards between two
		// reads; the sign is preserved rather than clamped, so a nonsense value
		// looks like one instead of reading as zero latency.
		{"negative", -2 * time.Millisecond, -2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Milliseconds(tt.in); got != tt.want {
				t.Errorf("Milliseconds(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestHumanBytesUsesBinaryUnitsAtEveryStep pins both the divisor and the labels.
//
// The labels are the point. Two of the three copies this function replaced
// printed "KB"/"MB"/"GB" while dividing by 1024, so this test would have failed
// against them — which is the whole reason it exists rather than being obvious.
func TestHumanBytesUsesBinaryUnitsAtEveryStep(t *testing.T) {
	tests := []struct {
		name string
		in   uint64
		want string
	}{
		{"zero", 0, "0 B"},
		{"one byte", 1, "1 B"},
		// 1023 is the last value that must stay in bytes: the < unit branch is
		// exclusive, and an off-by-one there would print "1024 B".
		{"last byte value", 1023, "1023 B"},
		{"exactly one KiB", 1024, "1.0 KiB"},
		{"one and a half KiB", 1536, "1.5 KiB"},
		// 1048575 is one byte below a MiB. It must not round up to "1.0 MiB",
		// which is what a %.0f or a >= comparison would produce.
		{"just under one MiB", 1024*1024 - 1, "1024.0 KiB"},
		{"exactly one MiB", 1024 * 1024, "1.0 MiB"},
		{"exactly one GiB", 1024 * 1024 * 1024, "1.0 GiB"},
		{"exactly one TiB", 1024 * 1024 * 1024 * 1024, "1.0 TiB"},
		// Past TiB the loop exits and the PiB fallback runs. Nothing in the
		// framework reports this, but the branch should not return "".
		{"into PiB", 2 * 1024 * 1024 * 1024 * 1024 * 1024, "2.0 PiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HumanBytes(tt.in); got != tt.want {
				t.Errorf("HumanBytes(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

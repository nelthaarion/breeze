package mcp

// bench_limits_test.go — adversarial tests for the benchmark arguments.
//
// The filter reaches `go test` as an argv element, so the hazard is not a shell escape
// but Docker's and Go's own flag parsers. -exec is the one that matters: it replaces the
// program that runs the compiled test binary, which is arbitrary execution with no shell
// anywhere in the path.

import (
	"strings"
	"testing"
	"time"
)

// TestBenchArgRefusesFlagSmuggling is the case with the worst consequence.
//
// A filter of "-exec=/bin/sh" lands in `go test ./... -run ^$ -bench -exec=/bin/sh` and
// Go reads it as its own flag, replacing the test runner with a shell. -coverprofile and
// -o are the write-a-file variants of the same shape.
func TestBenchArgRefusesFlagSmuggling(t *testing.T) {
	for _, value := range []string{
		"-exec=/bin/sh",
		"-exec=curl",
		"-coverprofile=/tmp/x",
		"-o=/tmp/binary",
		"-toolexec=/bin/sh",
		"--count=1000000",
		"-",
	} {
		if err := validateBenchArg("filter", value); err == nil {
			t.Errorf("validateBenchArg(%q) was accepted; `go test` reads a leading dash as one "+
				"of its own flags, and -exec replaces the program that runs the tests", value)
		}
	}

	err := validateBenchArg("filter", "-exec=/bin/sh")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "-exec") {
		t.Errorf("the refusal does not name the flag that makes this dangerous: %v", err)
	}
}

// TestBenchArgAcceptsRealPatterns is the other half. These flags take a regular
// expression, so a check narrowed to identifiers would refuse the values the tool is
// documented to accept.
func TestBenchArgAcceptsRealPatterns(t *testing.T) {
	for _, value := range []string{
		".",
		"BenchmarkRouter",
		"^BenchmarkRouter$",
		"Benchmark(Router|Context)",
		"Benchmark.*Alloc",
		"BenchmarkRouter/static",
		`Benchmark\w+`,
		"Benchmark[A-Z]{1,3}",
	} {
		if err := validateBenchArg("filter", value); err != nil {
			t.Errorf("validateBenchArg(%q) was refused: %v — this is a benchmark pattern a "+
				"caller would legitimately send", value, err)
		}
	}
}

// TestBenchArgBoundsLength keeps an argument list from becoming the payload. RE2 does not
// backtrack, so this is not an execution risk — it is the shape of an input nobody wrote
// by hand.
func TestBenchArgBoundsLength(t *testing.T) {
	if err := validateBenchArg("filter", strings.Repeat("B", maxBenchArgLength+1)); err == nil {
		t.Errorf("a %d-character filter was accepted; the limit is %d",
			maxBenchArgLength+1, maxBenchArgLength)
	}
	if err := validateBenchArg("filter", strings.Repeat("B", maxBenchArgLength)); err != nil {
		t.Errorf("a filter of exactly the limit (%d) was refused: %v", maxBenchArgLength, err)
	}
}

// TestBenchtimeRefusesUnboundedWork is the denial-of-service half.
//
// -benchtime takes a duration or an iteration count, and either form can ask for a run
// that will not finish. benchTimeout kills it eventually, so the consequence is ten
// minutes of pinned CPU per call rather than a permanent hang — repeatable, which is the
// definition of a denial of service on the host the server runs on.
func TestBenchtimeRefusesUnboundedWork(t *testing.T) {
	for _, value := range []string{
		"100h",
		"24h",
		"10m",
		"100000000000x",
		"999999999x",
		"-1s",
		"-5x",
		"0s",
		"0x",
		"nonsense",
		"10",
		"1e9x",
		"",
	} {
		if err := validateBenchtime(value); err == nil {
			t.Errorf("validateBenchtime(%q) was accepted; it asks for more work than the %s "+
				"benchmark timeout allows, or is not a value `go test` understands",
				value, benchTimeout)
		}
	}
}

// TestBenchtimeAcceptsRealValues states the bound as a limit rather than a prohibition:
// both spellings `go test` accepts are usable up to the cap, because refusing one of them
// would send a caller to the other with the same intent.
func TestBenchtimeAcceptsRealValues(t *testing.T) {
	for _, value := range []string{"1s", "100ms", "2s", "30s", "5m", "10x", "1000x", "100000x"} {
		if err := validateBenchtime(value); err != nil {
			t.Errorf("validateBenchtime(%q) was refused: %v", value, err)
		}
	}

	// Exactly at each limit, so the boundary is where it is documented to be.
	if err := validateBenchtime(maxBenchtime.String()); err != nil {
		t.Errorf("benchtime of exactly the limit (%s) was refused: %v", maxBenchtime, err)
	}
	if err := validateBenchtime("100000000x"); err != nil {
		t.Errorf("benchtime of exactly %d iterations was refused: %v", maxBenchIterations, err)
	}
}

// TestBenchtimeLimitLeavesRoomInsideTheTimeout is a consistency check between two
// constants that have to agree.
//
// maxBenchtime is half benchTimeout so that a single benchmark asking for the maximum
// still leaves room for others to be measured. If someone raises maxBenchtime past the
// timeout, every maximal request would be killed mid-run and report a truncated
// measurement as a complete one — so the relationship is asserted rather than left in a
// comment.
func TestBenchtimeLimitLeavesRoomInsideTheTimeout(t *testing.T) {
	if maxBenchtime >= benchTimeout {
		t.Fatalf("maxBenchtime (%s) is not below benchTimeout (%s); a benchmark asking for the "+
			"maximum would be killed before it finished and the truncated result would be "+
			"reported as a complete one", maxBenchtime, benchTimeout)
	}
	if maxBenchtime > benchTimeout/2 {
		t.Errorf("maxBenchtime (%s) is more than half benchTimeout (%s); one benchmark could "+
			"consume the whole budget and the rest would not be measured", maxBenchtime, benchTimeout)
	}
	if maxBenchtime <= time.Second {
		t.Errorf("maxBenchtime (%s) is too small to measure anything meaningful", maxBenchtime)
	}
}

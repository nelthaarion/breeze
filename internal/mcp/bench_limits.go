package mcp

// bench_limits.go — bounding what a benchmark request can ask for.
//
// # What is being defended against
//
// breeze_run_benchmarks builds `go test ./... -run ^$ -bench <filter> -benchmem
// [-benchtime <value>]` as an argument array, so nothing here is shell injection.
// Two other things are possible and neither is theoretical:
//
//   - **Argument smuggling into `go test`.** A filter of `-coverprofile=/tmp/x` or
//     `-exec=/bin/sh` lands in argv where `go test` reads it as its own flag. The
//     second of those is the serious one: -exec replaces the program that runs the
//     compiled test binary, which is arbitrary execution with no shell involved.
//     A leading dash is therefore refused outright.
//
//   - **Unbounded work.** -benchtime takes a duration or an iteration count, and
//     `-benchtime 100h` or `-benchtime 100000000000x` is a request for a run that
//     will not finish. The 10-minute benchTimeout does eventually kill it, so this
//     is a resource-exhaustion window rather than a permanent hang — but ten minutes
//     of pinned CPU per call, repeatable, is a denial of service on the machine the
//     server runs on. Bounding the argument turns that into an immediate refusal
//     naming the limit.
//
// # What is deliberately not bounded
//
// The benchmarks themselves. A project's own benchmark code is the code this tool
// exists to measure, and capping its runtime past -benchtime would mean reporting a
// truncated measurement as if it were a complete one. benchTimeout is the backstop,
// and a timed-out run already says so in its notes.
//
// Nor is CPU or memory limited per subprocess. Go's standard library has no portable
// interface for it: RLIMIT_AS and cgroups are Linux-specific, Job Objects are
// Windows-specific, and a partial implementation would mean the same tool call
// behaves differently per platform — with the platform that silently has no limit
// being the one nobody notices. The honest boundary is the process boundary, which
// is what provisioning exists for: run untrusted code in a container, not in a
// subprocess of the server.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxBenchtime bounds a -benchtime duration.
//
// Five minutes is half benchTimeout, so a single benchmark asking for the maximum
// still leaves room for the rest of the suite to be measured before the timeout stops
// the run. Anything longer is not a measurement, it is an occupation.
const maxBenchtime = 5 * time.Minute

// maxBenchIterations bounds the -benchtime Nx form.
//
// A hundred million iterations of a nanosecond-scale benchmark is a tenth of a
// second; of a millisecond-scale one it is over a day. The count alone cannot say
// which, so the limit is set where an honest measurement stops needing more.
const maxBenchIterations = 100_000_000

// benchArgPattern is what a -bench or -run pattern may contain.
//
// Go's own test flags take a regular expression, so this cannot be a simple
// identifier check. It permits the regexp metacharacters and rejects everything that
// would make the value ambiguous as a shell word or as a path — even though no shell
// sees it, because a value that cannot be one is one fewer thing to reason about.
var benchArgPattern = regexp.MustCompile(`^[A-Za-z0-9_.^$|()\[\]*+?/\\{},:-]+$`)

// maxBenchArgLength bounds a pattern.
//
// A regular expression long enough to matter is one nobody wrote by hand. It is also
// where a catastrophically backtracking pattern would come from — Go's RE2 does not
// backtrack, so that is not an execution risk here, but the bound costs nothing and
// removes the question.
const maxBenchArgLength = 256

// validateBenchArg checks a -bench or -run pattern.
func validateBenchArg(field, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q begins with a dash, so `go test` would read it as one of its "+
			"own flags rather than as a pattern — `-exec=...` passed here would replace the "+
			"program that runs the compiled tests", field, value)
	}
	if len(value) > maxBenchArgLength {
		return fmt.Errorf("%s is %d characters; the limit is %d", field, len(value), maxBenchArgLength)
	}
	if !benchArgPattern.MatchString(value) {
		return fmt.Errorf("%s %q contains characters that are not part of a benchmark pattern; "+
			"expected a regular expression over names", field, value)
	}
	return nil
}

// benchtimeIterations matches the Nx form: a count rather than a duration.
var benchtimeIterations = regexp.MustCompile(`^([0-9]+)x$`)

// validateBenchtime checks a -benchtime value and bounds what it asks for.
//
// Both forms `go test` accepts are handled, because rejecting one of them would send
// a caller to the other with the same intent and no limit.
func validateBenchtime(value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("benchtime %q begins with a dash, so `go test` would read it as a flag", value)
	}

	if m := benchtimeIterations.FindStringSubmatch(value); m != nil {
		count, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return fmt.Errorf("benchtime %q is not a usable number of iterations", value)
		}
		if count <= 0 {
			return fmt.Errorf("benchtime %q asks for no iterations", value)
		}
		if count > maxBenchIterations {
			return fmt.Errorf("benchtime %q asks for %d iterations; the limit is %d. A count this "+
				"large is a request for a run that will not finish inside the %s benchmark timeout",
				value, count, maxBenchIterations, benchTimeout)
		}
		return nil
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("benchtime %q is neither a duration (100ms, 2s) nor an iteration "+
			"count (10x): %w", value, err)
	}
	if d <= 0 {
		return fmt.Errorf("benchtime %q is not positive", value)
	}
	if d > maxBenchtime {
		return fmt.Errorf("benchtime %s exceeds the %s limit. Each benchmark would run at least "+
			"that long, so a suite of them cannot finish inside the %s timeout and the result "+
			"would be a truncated list reported as a complete one", d, maxBenchtime, benchTimeout)
	}
	return nil
}

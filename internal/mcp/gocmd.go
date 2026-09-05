package mcp

// gocmd.go — running the Go toolchain and turning its output into data.
//
// The verification tools all work the same way: run a `go` subcommand against a
// project and report what it said. The reason this is a separate file from the
// tools is that the interesting part is not the running, it is the parsing.
//
// A tool that returned the toolchain's stdout would be a tool that made an agent
// write a compiler-output parser, and it would write a worse one than this
// because it would be guessing at the format from one example. `go build` reports
// a failure as file, line, column and message; that is four fields, and handing
// back the concatenation of them as prose throws away the only structure the
// caller needs — which file to open and which line to look at. So every runner
// here returns records, and the raw output is kept only as a fallback for the
// cases the parsers do not recognise.
//
// Nothing here uses captureStdout or runInDir. Those exist because the generators
// run in-process and print to a stdout that is also the protocol stream; a
// subprocess has its own stdout and its own working directory, so `cmd.Dir` does
// the job without taking a process-wide lock. That difference matters: it means a
// verification run does not serialise against generator calls and cannot deadlock
// against one.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Timeouts are per command, and differ because the commands do.
//
// A build and a vet are bounded by the size of the tree. Tests are bounded by
// whatever the project's authors wrote, which can legitimately include a few
// minutes of work. Benchmarks are deliberately the most generous: -benchtime
// defaults to a second per benchmark and a project with thirty of them is not
// misbehaving.
//
// The timeouts exist so a hung test suite is reported as a hung test suite
// rather than as an MCP client that stopped responding.
const (
	buildTimeout    = 2 * time.Minute
	vetTimeout      = 2 * time.Minute
	testTimeout     = 5 * time.Minute
	benchTimeout    = 10 * time.Minute
	coverageTimeout = 5 * time.Minute
)

// goRun is one completed toolchain invocation.
type goRun struct {
	// Command is the command line as it would be typed, for a caller that wants
	// to reproduce the run by hand.
	Command string
	// Output is stdout and stderr interleaved, as the toolchain wrote them.
	// The two are combined on purpose: `go test` prints package results to
	// stdout and build failures to stderr, and splitting them would break the
	// ordering that makes a mixed run readable.
	Output string
	// Duration is how long the command took.
	Duration time.Duration
	// ExitCode is the process's exit status, or -1 if it never ran.
	ExitCode int
	// TimedOut reports that the command was killed rather than finishing.
	TimedOut bool
	// StartErr is set only when the command could not be started at all — a
	// missing toolchain, an unreadable directory. This is kept apart from a
	// non-zero exit because the two mean opposite things to a caller: one says
	// the environment is wrong, the other says the code is.
	StartErr error
}

// ok reports whether the command succeeded.
func (r goRun) ok() bool { return r.StartErr == nil && !r.TimedOut && r.ExitCode == 0 }

// runGo runs a `go` subcommand in dir.
//
// The toolchain is looked up rather than assumed so that a machine without Go
// produces one clear answer — "the Go toolchain is not on PATH" — instead of an
// exec error repeated once per verification step.
func runGo(dir string, timeout time.Duration, args ...string) goRun {
	run := goRun{Command: "go " + strings.Join(args, " "), ExitCode: -1}

	binary, err := exec.LookPath("go")
	if err != nil {
		run.StartErr = fmt.Errorf("the Go toolchain is not on PATH, so this project cannot be "+
			"built, vetted or tested from here: %w", err)
		return run
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir

	// GOFLAGS is cleared of anything the parent process set that would change
	// what is being measured, and the module cache is left alone: a verification
	// run should behave exactly as the same command typed in that directory.
	cmd.Env = append(cmd.Environ(), "GO111MODULE=on")

	started := time.Now()
	output, runErr := cmd.CombinedOutput()
	run.Duration = time.Since(started)
	run.Output = string(output)

	if ctx.Err() != nil {
		run.TimedOut = true
		return run
	}

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		run.ExitCode = 0
	case errors.As(runErr, &exitErr):
		run.ExitCode = exitErr.ExitCode()
	default:
		run.StartErr = runErr
	}
	return run
}

// ─── diagnostics ─────────────────────────────────────────────────────────────

// diagnostic is one compiler or vet complaint, located.
type diagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
	// Package is the package the toolchain was reporting on when this appeared,
	// taken from the `# import/path` header that precedes a group. It is what
	// lets a caller tell two same-named files in different packages apart.
	Package string `json:"package,omitempty"`
}

// diagnosticLine matches the toolchain's location format: an optional ./ prefix,
// a path, a line, an optional column, then the message.
//
// The column is optional because vet omits it for some checks and the go command
// omits it for errors it did not get from the type checker. Requiring it would
// silently drop exactly the diagnostics a caller most wants.
var diagnosticLine = regexp.MustCompile(`^\s*(\S+\.go):(\d+)(?::(\d+))?:\s+(.*)$`)

// packageHeader matches the `# example.com/mod/pkg` line that groups
// diagnostics by package.
var packageHeader = regexp.MustCompile(`^#\s+(\S+)`)

// parseDiagnostics pulls located complaints out of toolchain output.
//
// Lines that match nothing are returned as leftovers rather than discarded. The
// toolchain says things that are not diagnostics and are still the answer — "no
// required module provides package", "build constraints exclude all Go files" —
// and a tool that reported "0 diagnostics" for a build that failed for one of
// those reasons would be actively misleading.
func parseDiagnostics(output string) (found []diagnostic, leftovers []string) {
	pkg := ""

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}

		if header := packageHeader.FindStringSubmatch(trimmed); header != nil {
			pkg = header[1]
			continue
		}

		match := diagnosticLine.FindStringSubmatch(trimmed)
		if match == nil {
			leftovers = append(leftovers, strings.TrimSpace(trimmed))
			continue
		}

		lineNo, _ := strconv.Atoi(match[2])
		column, _ := strconv.Atoi(match[3])
		found = append(found, diagnostic{
			File:    normaliseDiagPath(match[1]),
			Line:    lineNo,
			Column:  column,
			Message: match[4],
			Package: pkg,
		})
	}
	return found, leftovers
}

// normaliseDiagPath strips the leading ./ the toolchain emits and squares the
// separators, so a path can be handed straight back to a file-reading tool.
func normaliseDiagPath(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	return strings.TrimPrefix(path, "./")
}

// ─── test results ────────────────────────────────────────────────────────────

// testFailure is one failing test, with the assertion output beneath it.
type testFailure struct {
	Name    string `json:"name"`
	Package string `json:"package,omitempty"`
	// Elapsed is the test's own reported duration in seconds.
	Elapsed float64 `json:"elapsed_seconds,omitempty"`
	// Detail is the indented output the test produced — the t.Errorf messages.
	// This is the part that says what was expected and what happened, so it is
	// carried through verbatim rather than summarised.
	Detail []string `json:"detail,omitempty"`
}

// packageResult is one package's test outcome.
type packageResult struct {
	Package string `json:"package"`
	// Status is ok, FAIL, or no-test-files.
	Status  string  `json:"status"`
	Elapsed float64 `json:"elapsed_seconds,omitempty"`
	// Coverage is the percentage of statements covered, when the run asked for
	// it. It is a pointer so that "not measured" and "measured as zero" are
	// distinguishable — a package with no tests and a package whose tests
	// exercise nothing are different problems.
	Coverage *float64 `json:"coverage_percent,omitempty"`
	// CoverageNote carries the bracketed remark the toolchain adds instead of a
	// number, such as "no test files" or "no statements".
	CoverageNote string `json:"coverage_note,omitempty"`
}

var (
	// failHeader matches "--- FAIL: TestName (0.03s)", including the subtest
	// form where the name contains a slash.
	failHeader = regexp.MustCompile(`^\s*--- (FAIL|SKIP): (\S+) \(([\d.]+)s\)`)
	// packageLine matches the per-package summary the test binary prints:
	// "ok  \tpath\t0.1s" or "FAIL\tpath\t0.2s" or "?   \tpath\t[no test files]".
	packageLine = regexp.MustCompile(`^(ok|FAIL|\?)\s+(\S+)\s*(.*)$`)
	// elapsedField matches the duration in a package line.
	elapsedField = regexp.MustCompile(`([\d.]+)s`)
	// coverageField matches either a percentage or the bracketed excuse.
	coverageField = regexp.MustCompile(`coverage:\s+(?:([\d.]+)%\s+of\s+statements|\[([^\]]+)\])`)
	// noTestFiles matches the bracketed remark on a `?` line.
	bracketNote = regexp.MustCompile(`\[([^\]]+)\]`)
	// coverageOnlyLine matches the form `go test -cover` uses for a package that
	// has no test files:
	//
	//	\texample.com/mod/untested\t\tcoverage: 0.0% of statements
	//
	// It is indented and has no ok/FAIL/? verb, so the verb-anchored pattern
	// above cannot see it and the package would vanish from the report entirely.
	// The 0.0% is not a measurement: the package was instrumented and then
	// nothing ran, which is why the percentage is dropped rather than recorded.
	// Keeping it would report an untested package as one whose statements were
	// checked and missed, and those call for different work.
	coverageOnlyLine = regexp.MustCompile(`^\s+(\S+)\s+coverage:\s+([\d.]+)%\s+of\s+statements\s*$`)
)

// parseTestOutput turns `go test` output into per-package results and per-test
// failures.
//
// The failure detail is collected by indentation, which is how the test binary
// marks it: everything more indented than the "--- FAIL" header belongs to that
// test. This is why the detail is worth keeping — the header says a test failed,
// and the indented lines say why.
func parseTestOutput(output string) (packages []packageResult, failures []testFailure) {
	var current *testFailure

	flush := func() {
		if current != nil {
			failures = append(failures, *current)
			current = nil
		}
	}

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		if header := failHeader.FindStringSubmatch(line); header != nil {
			flush()
			if header[1] == "SKIP" {
				// A skip is not a failure, and collecting its detail would put
				// "short mode" in a list a caller reads as things to fix.
				continue
			}
			elapsed, _ := strconv.ParseFloat(header[3], 64)
			current = &testFailure{Name: header[2], Elapsed: elapsed}
			continue
		}

		if match := packageLine.FindStringSubmatch(line); match != nil {
			flush()
			result := packageResult{Package: match[2]}
			rest := match[3]

			switch match[1] {
			case "ok":
				result.Status = "ok"
			case "FAIL":
				result.Status = "FAIL"
			default:
				result.Status = "no-test-files"
				if note := bracketNote.FindStringSubmatch(rest); note != nil {
					result.CoverageNote = note[1]
				}
			}

			if elapsed := elapsedField.FindStringSubmatch(rest); elapsed != nil {
				result.Elapsed, _ = strconv.ParseFloat(elapsed[1], 64)
			}
			if cover := coverageField.FindStringSubmatch(rest); cover != nil {
				if cover[1] != "" {
					value, err := strconv.ParseFloat(cover[1], 64)
					if err == nil {
						result.Coverage = &value
					}
				} else {
					result.CoverageNote = cover[2]
				}
			}

			packages = append(packages, result)
			continue
		}

		// A coverage-only line is a package with no test files, reported by
		// `-cover` in a shape that carries no verb. It is checked before the
		// indented-continuation branch below because it is also indented, and
		// would otherwise be swallowed as detail of a preceding failure.
		if match := coverageOnlyLine.FindStringSubmatch(line); match != nil {
			flush()
			packages = append(packages, packageResult{
				Package:      match[1],
				Status:       "no-test-files",
				CoverageNote: "no test files",
			})
			continue
		}

		// Indented continuation belongs to the failure above it.
		if current != nil && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			current.Detail = append(current.Detail, strings.TrimSpace(line))
			continue
		}
		if current != nil {
			flush()
		}
	}
	flush()

	// The package each failure came from is only knowable from the FAIL line
	// that follows it, so it is attached afterwards rather than guessed.
	attributeFailures(failures, output)
	return packages, failures
}

// attributeFailures fills in each failure's package by replaying the output and
// noting which FAIL line came after it.
//
// The slice is mutated in place rather than returned because the elements are
// structs and the caller already owns the backing array; copying it back would
// only add a way for the two to disagree.
func attributeFailures(failures []testFailure, output string) {
	if len(failures) == 0 {
		return
	}

	index := 0
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")

		if header := failHeader.FindStringSubmatch(line); header != nil && header[1] == "FAIL" {
			continue
		}
		match := packageLine.FindStringSubmatch(line)
		if match == nil || match[1] != "FAIL" {
			continue
		}
		// Every failure not yet attributed, up to this point, belongs here.
		for index < len(failures) && failures[index].Package == "" {
			failures[index].Package = match[2]
			index++
		}
	}
}

// ─── benchmarks ──────────────────────────────────────────────────────────────

// benchmarkResult is one benchmark's measurement.
//
// The fields are the ones -benchmem reports. They are kept as numbers because
// the entire point of asking for a benchmark is to compare it with another one,
// and "1234 ns/op" as a string cannot be compared without parsing it again.
type benchmarkResult struct {
	Name string `json:"name"`
	// Procs is the GOMAXPROCS suffix the toolchain appends to the name, split
	// out so the name matches what the source calls it.
	Procs int `json:"procs,omitempty"`
	// Iterations is how many times the body ran. A low count on a slow
	// benchmark is the toolchain's own signal that the number is noisy.
	Iterations  int      `json:"iterations"`
	NsPerOp     float64  `json:"ns_per_op"`
	BytesPerOp  *int64   `json:"bytes_per_op,omitempty"`
	AllocsPerOp *int64   `json:"allocs_per_op,omitempty"`
	MBPerSecond *float64 `json:"mb_per_second,omitempty"`
}

// benchmarkLine matches a result row. The name, iteration count and ns/op are
// always present; the memory columns appear only under -benchmem, and custom
// metrics can follow, so the tail is captured and scanned separately.
var (
	benchmarkLine = regexp.MustCompile(`^(Benchmark\S*?)(?:-(\d+))?\s+(\d+)\s+([\d.]+)\s+ns/op(.*)$`)
	bytesPerOp    = regexp.MustCompile(`([\d.]+)\s+B/op`)
	allocsPerOp   = regexp.MustCompile(`(\d+)\s+allocs/op`)
	mbPerSecond   = regexp.MustCompile(`([\d.]+)\s+MB/s`)
)

// parseBenchmarks pulls measurements out of `go test -bench` output.
func parseBenchmarks(output string) []benchmarkResult {
	var out []benchmarkResult

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		match := benchmarkLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		result := benchmarkResult{Name: match[1]}
		result.Procs, _ = strconv.Atoi(match[2])
		result.Iterations, _ = strconv.Atoi(match[3])
		result.NsPerOp, _ = strconv.ParseFloat(match[4], 64)

		tail := match[5]
		if found := bytesPerOp.FindStringSubmatch(tail); found != nil {
			if value, err := strconv.ParseFloat(found[1], 64); err == nil {
				rounded := int64(value)
				result.BytesPerOp = &rounded
			}
		}
		if found := allocsPerOp.FindStringSubmatch(tail); found != nil {
			if value, err := strconv.ParseInt(found[1], 10, 64); err == nil {
				result.AllocsPerOp = &value
			}
		}
		if found := mbPerSecond.FindStringSubmatch(tail); found != nil {
			if value, err := strconv.ParseFloat(found[1], 64); err == nil {
				result.MBPerSecond = &value
			}
		}

		out = append(out, result)
	}
	return out
}

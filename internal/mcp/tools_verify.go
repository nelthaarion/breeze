package mcp

// tools_verify.go — the tools that check a project rather than describe or
// change one.
//
// These are separated from the knowledge tools because they answer a different
// question. describe_schema and explain_idiom answer "what is true of the
// framework"; these answer "what is true of this project right now". The first
// kind is constant and can be served from data in this package; the second has
// to read the tree, and can fail in ways that matter to a caller — a path that
// is not a project, a file that does not parse.
//
// Every result here is structured. A caller deciding whether to keep editing
// needs to branch on counts and severities, and asking it to parse prose to do
// that would make the tool useless for the thing it exists for.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func registerVerificationTools(s *Server) {
	s.addTool(checkIdiomsTool())
	s.addTool(verifyProjectTool())
	s.addTool(runBenchmarksTool())
	s.addTool(testCoverageTool())
}

// verifyRoot resolves the path argument the toolchain tools share.
//
// An empty path means the server's working directory, matching check_idioms. The
// directory is confirmed to be a module root before anything is run, because the
// alternative is four consecutive toolchain invocations all failing with "go.mod
// file not found" — which is one fact reported four times, and which reads like a
// broken project rather than a mistyped path.
//
// Confinement happens first, in resolvePath. That ordering matters here more than
// anywhere else in the package: the tools downstream of this run `go test`, which
// compiles and executes whatever is in the directory. An unconfined path argument
// would make this the most powerful tool on the server rather than a verifier.
func verifyRoot(path string) (string, error) {
	root := strings.TrimSpace(path)
	if root == "" {
		root = "."
	}

	abs, err := resolvePath(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf(
			"%s is a file; these tools need the directory a project lives in",
			root,
		)
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		return "", fmt.Errorf("%s has no go.mod, so it is not the root of a Go module. "+
			"Point this at the directory the project's go.mod is in", root)
	}
	return abs, nil
}

// ─── check_idioms ────────────────────────────────────────────────────────────

type checkIdiomsArgs struct {
	Path string `json:"path"`
}

func checkIdiomsTool() *tool {
	return &tool{
		name: "breeze_check_idioms",
		description: "Check a project against the framework's conventions and report every violation " +
			"with file, line, rule, message, and severity. Covers reflection on the request " +
			"path, Fleet/dashboard middleware order, the deprecated Swagger spellings, and " +
			"hand-written table names. Pass a rule to breeze_explain_idiom for the reasoning. " +
			"Reads only; writes nothing.",
		schema: objectSchema(map[string]any{
			"path": stringProp(
				"Project root to check. Defaults to the server's working directory.",
			),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a checkIdiomsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return checkIdioms(a.Path)
		},
	}
}

func checkIdioms(path string) toolCallResult {
	// Confined before it is read. This tool only parses Go files, so the worst a
	// free path could do is disclose source — but "only discloses source" is not a
	// property worth relying on when the check is one call.
	root, err := resolvePath(path)
	if err != nil {
		return errorResult("checking idioms: " + err.Error())
	}

	report, err := runIdiomCheck(root)
	if err != nil {
		return errorResult("checking idioms: " + err.Error())
	}

	// The summary states what was looked at as well as what was found, because
	// "0 violations" over one file and over two hundred are very different
	// claims and a caller reading only the summary cannot otherwise tell.
	summary := fmt.Sprintf("%d violation(s) across %d file(s): %d error(s), %d warning(s)",
		report.Count, report.Files, report.Errors, report.Warnings)

	// A violation is not a tool failure. The call did exactly what it was asked
	// to; reporting IsError here would make an agent treat a successful check
	// with findings as a broken tool and retry it.
	return structuredResult(summary, report)
}

// ─── verify_project ──────────────────────────────────────────────────────────

// verifyStep is one stage of a verification run.
//
// Every step carries its own status and its own findings rather than being
// folded into a single pass/fail, because the stages fail for unrelated reasons
// and the remedies differ. A build failure is a syntax or type error; a vet
// failure is code that compiles and is wrong; a test failure is behaviour; an
// idiom finding is a convention. Collapsing those into one boolean would throw
// away the only information that tells a caller what to do next.
type verifyStep struct {
	// Name is build, vet, test or idioms.
	Name string `json:"name"`
	// Status is passed, failed, skipped or errored. "errored" is kept apart
	// from "failed" because it means the step could not run — no toolchain, a
	// timeout — and retrying it may work, whereas a failure needs an edit.
	Status string `json:"status"`
	// Command is what was run, absent for the in-process idiom check.
	Command string `json:"command,omitempty"`
	// DurationMS is how long the step took.
	DurationMS int64 `json:"duration_ms"`
	// Summary is the one-line reading of this step.
	Summary string `json:"summary"`
	// Diagnostics are located compiler or vet complaints.
	Diagnostics []diagnostic `json:"diagnostics,omitempty"`
	// Failures are failing tests with their assertion output.
	Failures []testFailure `json:"failures,omitempty"`
	// Packages is the per-package test outcome.
	Packages []packageResult `json:"packages,omitempty"`
	// Findings are idiom violations.
	Findings []idiomFinding `json:"findings,omitempty"`
	// Unparsed holds output lines no parser recognised. This is the honesty
	// valve: if a step failed and produced nothing structured, the caller still
	// gets the toolchain's own words instead of an empty list.
	Unparsed []string `json:"unparsed_output,omitempty"`
}

// verifyReport is the whole run.
type verifyReport struct {
	Path string `json:"path"`
	// OK is true only when every step that ran passed. It is the field an agent
	// branches on, so it is deliberately strict: a vet complaint or an idiom
	// error is enough to make it false.
	OK    bool         `json:"ok"`
	Steps []verifyStep `json:"steps"`
	// FirstFailure names the earliest step that did not pass, which is where a
	// caller should start reading. Later steps are often consequences of it —
	// tests cannot pass if the package does not build.
	FirstFailure string `json:"first_failure,omitempty"`
	// Counts is a flat tally for a caller that only wants to know how much is
	// wrong before deciding whether to look closer.
	Counts       verifyCounts `json:"counts"`
	DurationMS   int64        `json:"duration_ms"`
	SkippedTests bool         `json:"tests_skipped"`
	Notes        []string     `json:"notes,omitempty"`
}

// verifyCounts tallies findings across steps.
type verifyCounts struct {
	BuildErrors   int `json:"build_errors"`
	VetFindings   int `json:"vet_findings"`
	TestFailures  int `json:"test_failures"`
	IdiomErrors   int `json:"idiom_errors"`
	IdiomWarnings int `json:"idiom_warnings"`
}

type verifyProjectArgs struct {
	Path string `json:"path"`
	// SkipTests exists because a test suite is the slowest and least
	// predictable part of a verification, and an agent that has just edited one
	// file usually wants to know whether it still compiles before it waits
	// several minutes to learn whether it still behaves.
	SkipTests bool `json:"skip_tests"`
	// TestFilter is passed to -run.
	TestFilter string `json:"test_filter"`
}

func verifyProjectTool() *tool {
	return &tool{
		name: "breeze_verify_project",
		description: "Verify a project end to end and report each stage separately: go build, " +
			"go vet, go test, and the framework idiom check. Compiler and vet complaints come " +
			"back as file/line/message records and failing tests come back with their assertion " +
			"output, so nothing has to be parsed out of raw toolchain text. Use this after " +
			"generating or editing code to find out whether it actually works. Reads only.",
		schema: objectSchema(map[string]any{
			"path": stringProp(
				"Project root to verify. Defaults to the server's working directory.",
			),
			"skip_tests": boolProp("Skip the test stage. Useful for a fast compile-and-vet check " +
				"after an edit, when the suite is slow."),
			"test_filter": stringProp("Regular expression passed to go test -run, to verify one " +
				"area rather than the whole suite."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a verifyProjectArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return verifyProject(a)
		},
	}
}

func verifyProject(a verifyProjectArgs) toolCallResult {
	root, err := verifyRoot(a.Path)
	if err != nil {
		return errorResult("verifying: " + err.Error())
	}

	started := time.Now()
	report := verifyReport{Path: filepath.ToSlash(root), SkippedTests: a.SkipTests}

	build := runGo(root, buildTimeout, "build", "./...")
	buildStep := diagnosticStep("build", build,
		"the project compiles",
		"the project does not compile")
	report.Counts.BuildErrors = len(buildStep.Diagnostics)
	report.Steps = append(report.Steps, buildStep)

	// vet runs even when the build failed, because it is a superset of the type
	// checking and its output frequently includes the same errors with better
	// context. Tests, by contrast, are skipped: `go test` on a package that does
	// not compile reports the build failure again, and reporting it a third time
	// as a test failure would suggest three problems where there is one.
	vet := runGo(root, vetTimeout, "vet", "./...")
	vetStep := diagnosticStep("vet", vet,
		"vet found nothing",
		"vet reported problems")
	report.Counts.VetFindings = len(vetStep.Diagnostics)
	report.Steps = append(report.Steps, vetStep)

	testStep := verifyStep{Name: "test"}
	switch {
	case a.SkipTests:
		testStep.Status = "skipped"
		testStep.Summary = "tests were skipped because skip_tests was set"
	case !build.ok():
		testStep.Status = "skipped"
		testStep.Summary = "tests were skipped because the project does not compile; " +
			"fix the build first, since go test would only report the same errors again"
	default:
		args := []string{"test", "./..."}
		if filter := strings.TrimSpace(a.TestFilter); filter != "" {
			args = append(args, "-run", filter)
		}
		// The toolchain's own timeout is set just under the process timeout so
		// a hung test produces Go's panic dump — which names the goroutine and
		// the line — rather than an opaque kill.
		args = append(args, "-timeout", strconv.Itoa(int(testTimeout.Seconds())-15)+"s")

		run := runGo(root, testTimeout, args...)
		packages, failures := parseTestOutput(run.Output)

		testStep.Command = run.Command
		testStep.DurationMS = run.Duration.Milliseconds()
		testStep.Packages = packages
		testStep.Failures = failures
		report.Counts.TestFailures = len(failures)

		switch {
		case run.StartErr != nil:
			testStep.Status = "errored"
			testStep.Summary = run.StartErr.Error()
		case run.TimedOut:
			testStep.Status = "errored"
			testStep.Summary = fmt.Sprintf(
				"the test run was still going after %s and was stopped, "+
					"so its result is unknown; a hang is usually a deadlock or a test waiting on a "+
					"service that is not running",
				testTimeout,
			)
		case run.ok():
			testStep.Status = "passed"
			testStep.Summary = fmt.Sprintf("%d package(s) tested, all passing", len(packages))
		default:
			testStep.Status = "failed"
			testStep.Summary = fmt.Sprintf("%d test(s) failed across %d package(s)",
				len(failures), countFailedPackages(packages))
			if len(failures) == 0 {
				// A non-zero exit with no parsed failure is almost always a
				// build error inside a test file, which the test binary reports
				// in a different shape. Handing the output back is the only
				// honest answer.
				testStep.Diagnostics, testStep.Unparsed = parseDiagnostics(run.Output)
				testStep.Summary = "the test run failed without reporting a failing test, which " +
					"usually means a test file itself does not compile"
			}
		}
	}
	report.Steps = append(report.Steps, testStep)

	idiomStep := verifyStep{Name: "idioms"}
	idiomStarted := time.Now()
	if idioms, idiomErr := runIdiomCheck(root); idiomErr != nil {
		idiomStep.Status = "errored"
		idiomStep.Summary = idiomErr.Error()
	} else {
		idiomStep.Findings = idioms.Findings
		report.Counts.IdiomErrors = idioms.Errors
		report.Counts.IdiomWarnings = idioms.Warnings

		// A warning does not fail the step. The idiom rules include advisory
		// ones, and making a suggestion fail a verification would train a
		// caller to ignore the result.
		if idioms.Errors > 0 {
			idiomStep.Status = "failed"
		} else {
			idiomStep.Status = "passed"
		}
		idiomStep.Summary = fmt.Sprintf("%d error(s) and %d warning(s) across %d file(s)",
			idioms.Errors, idioms.Warnings, idioms.Files)
	}
	idiomStep.DurationMS = time.Since(idiomStarted).Milliseconds()
	report.Steps = append(report.Steps, idiomStep)

	report.OK = true
	for _, step := range report.Steps {
		if step.Status == "failed" || step.Status == "errored" {
			report.OK = false
			if report.FirstFailure == "" {
				report.FirstFailure = step.Name
			}
		}
	}
	report.DurationMS = time.Since(started).Milliseconds()

	if report.OK && a.SkipTests {
		report.Notes = append(report.Notes, "this project compiles, vets clean and follows the "+
			"conventions, but its tests were not run, so nothing here says its behaviour is correct")
	}
	if strings.TrimSpace(a.TestFilter) != "" {
		report.Notes = append(report.Notes, "only tests matching "+a.TestFilter+" ran, so a pass "+
			"here is not a pass for the whole suite")
	}

	summary := "verified: everything passed"
	if !report.OK {
		summary = fmt.Sprintf("verification failed at %s: %d build error(s), %d vet finding(s), "+
			"%d test failure(s), %d idiom error(s)", report.FirstFailure,
			report.Counts.BuildErrors, report.Counts.VetFindings,
			report.Counts.TestFailures, report.Counts.IdiomErrors)
	}

	// A failed verification is not a failed call: the tool was asked whether the
	// project works and it found out. Flagging IsError would make an agent treat
	// a correct "no" as a broken tool.
	return structuredResult(summary, report)
}

// diagnosticStep builds the result of a build or vet stage.
//
// The two are identical in shape — a command, an exit code, and located
// complaints — so sharing this avoids the two drifting into reporting the same
// situation differently.
func diagnosticStep(name string, run goRun, passSummary, failSummary string) verifyStep {
	step := verifyStep{
		Name:       name,
		Command:    run.Command,
		DurationMS: run.Duration.Milliseconds(),
	}

	switch {
	case run.StartErr != nil:
		step.Status = "errored"
		step.Summary = run.StartErr.Error()
		return step
	case run.TimedOut:
		step.Status = "errored"
		step.Summary = fmt.Sprintf("%s did not finish in time and was stopped", run.Command)
		return step
	}

	step.Diagnostics, step.Unparsed = parseDiagnostics(run.Output)

	if run.ok() {
		step.Status = "passed"
		step.Summary = passSummary
		// Successful runs are silent, and anything they did print is noise a
		// caller would have to filter. Dropping it keeps a passing step small.
		step.Unparsed = nil
		return step
	}

	step.Status = "failed"
	step.Summary = fmt.Sprintf("%s: %d located problem(s)", failSummary, len(step.Diagnostics))
	if len(step.Diagnostics) == 0 {
		step.Summary = failSummary + ", but not at a specific line; see unparsed_output"
	}
	return step
}

// countFailedPackages counts packages whose tests failed.
func countFailedPackages(packages []packageResult) int {
	failed := 0
	for _, p := range packages {
		if p.Status == "FAIL" {
			failed++
		}
	}
	return failed
}

// ─── run_benchmarks ──────────────────────────────────────────────────────────

type benchmarkReport struct {
	Path    string `json:"path"`
	Command string `json:"command"`
	Filter  string `json:"filter"`
	// Count is how many measurements came back.
	Count      int               `json:"count"`
	Benchmarks []benchmarkResult `json:"benchmarks"`
	// Fastest and Slowest name the extremes by ns/op, so a caller comparing a
	// change does not have to sort the list itself.
	Fastest string `json:"fastest,omitempty"`
	Slowest string `json:"slowest,omitempty"`
	// TotalAllocations is the summed allocs/op, which is the number the
	// framework's own performance work is usually chasing.
	TotalAllocations int64 `json:"total_allocs_per_op"`
	// ZeroAllocation names the benchmarks that allocate nothing per operation.
	// This is called out because it is the framework's stated goal on the hot
	// path, so it is the fact a caller is checking for.
	ZeroAllocation []string `json:"zero_allocation,omitempty"`
	DurationMS     int64    `json:"duration_ms"`
	Unparsed       []string `json:"unparsed_output,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

type runBenchmarksArgs struct {
	Path   string `json:"path"`
	Filter string `json:"filter"`
	// Benchtime is passed straight through to -benchtime.
	Benchtime string `json:"benchtime"`
}

func runBenchmarksTool() *tool {
	return &tool{
		name: "breeze_run_benchmarks",
		description: "Run a project's benchmarks and return the measurements as numbers: ns/op, " +
			"B/op and allocs/op per benchmark, with the fastest and slowest named and the " +
			"zero-allocation ones listed. Use this to check the performance effect of a change, " +
			"or to confirm a hot path still allocates nothing. Reads only.",
		schema: objectSchema(map[string]any{
			"path": stringProp("Project root. Defaults to the server's working directory."),
			"filter": stringProp("Regular expression passed to go test -bench. Defaults to '.', " +
				"which runs every benchmark."),
			"benchtime": stringProp("Passed to -benchtime, for example '10x' to run each " +
				"benchmark a fixed number of times or '100ms' to shorten the run. Shorter runs " +
				"are noisier."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a runBenchmarksArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return runBenchmarks(a)
		},
	}
}

func runBenchmarks(a runBenchmarksArgs) toolCallResult {
	root, err := verifyRoot(a.Path)
	if err != nil {
		return errorResult("running benchmarks: " + err.Error())
	}

	filter := strings.TrimSpace(a.Filter)
	if filter == "" {
		filter = "."
	}
	if err := validateBenchArg("filter", filter); err != nil {
		return errorResult("running benchmarks: " + err.Error())
	}

	// -run with a pattern that matches nothing is what keeps this from running
	// the test suite as well. Without it `go test -bench` runs every test first,
	// which both wastes minutes and lets a failing test hide the measurements.
	args := []string{"test", "./...", "-run", "^$", "-bench", filter, "-benchmem"}
	if benchtime := strings.TrimSpace(a.Benchtime); benchtime != "" {
		if err := validateBenchtime(benchtime); err != nil {
			return errorResult("running benchmarks: " + err.Error())
		}
		args = append(args, "-benchtime", benchtime)
	}

	run := runGo(root, benchTimeout, args...)
	if run.StartErr != nil {
		return errorResult("running benchmarks: " + run.StartErr.Error())
	}

	report := benchmarkReport{
		Path:       filepath.ToSlash(root),
		Command:    run.Command,
		Filter:     filter,
		Benchmarks: parseBenchmarks(run.Output),
		DurationMS: run.Duration.Milliseconds(),
	}
	report.Count = len(report.Benchmarks)

	if run.TimedOut {
		report.Notes = append(report.Notes, fmt.Sprintf("the run was stopped after %s, so this "+
			"list may be incomplete; narrow it with filter or shorten it with benchtime",
			benchTimeout))
	}

	for _, b := range report.Benchmarks {
		if b.AllocsPerOp != nil {
			report.TotalAllocations += *b.AllocsPerOp
			if *b.AllocsPerOp == 0 {
				report.ZeroAllocation = append(report.ZeroAllocation, b.Name)
			}
		}
	}

	if report.Count > 0 {
		sorted := make([]benchmarkResult, len(report.Benchmarks))
		copy(sorted, report.Benchmarks)
		sort.SliceStable(
			sorted,
			func(i, j int) bool { return sorted[i].NsPerOp < sorted[j].NsPerOp },
		)
		report.Fastest = sorted[0].Name
		report.Slowest = sorted[len(sorted)-1].Name
	}

	if report.Count == 0 {
		// An empty result is ambiguous and the two readings need different
		// responses, so both are named rather than leaving the caller to guess.
		_, leftovers := parseDiagnostics(run.Output)
		report.Unparsed = leftovers
		note := "no benchmark matched " + filter + ". Either this project has no benchmarks, " +
			"or the pattern did not match any of their names"
		if !run.ok() {
			note = "the benchmark run failed before producing measurements; see unparsed_output"
		}
		report.Notes = append(report.Notes, note)

		return structuredResult("no benchmark results", report)
	}

	summary := fmt.Sprintf("%d benchmark(s): fastest %s, slowest %s, %d alloc(s)/op in total",
		report.Count, report.Fastest, report.Slowest, report.TotalAllocations)
	return structuredResult(summary, report)
}

// ─── get_test_coverage ───────────────────────────────────────────────────────

type coverageReport struct {
	Path    string `json:"path"`
	Command string `json:"command"`
	// TotalPercent is coverage across every package that has statements,
	// weighted by nothing — it is the mean of the per-package figures, which is
	// what `go test -cover` itself reports per package and no more than that.
	// It is a pointer so an unmeasurable project is not reported as 0%.
	TotalPercent *float64        `json:"total_percent,omitempty"`
	Packages     []packageResult `json:"packages"`
	// Untested names packages with no test files at all. These are the ones
	// where a coverage number would be misleading rather than low.
	Untested []string `json:"untested_packages,omitempty"`
	// LeastCovered names the measured package with the lowest figure, which is
	// where a caller adding tests should look first.
	LeastCovered string `json:"least_covered,omitempty"`
	// TestsPassed reports whether the run that produced these numbers passed.
	// Coverage from a failing suite is still coverage, but it was measured
	// while things were broken, and a caller should know that.
	TestsPassed bool          `json:"tests_passed"`
	Failures    []testFailure `json:"failures,omitempty"`
	DurationMS  int64         `json:"duration_ms"`
	Notes       []string      `json:"notes,omitempty"`
}

type coverageArgs struct {
	Path string `json:"path"`
}

func testCoverageTool() *tool {
	return &tool{
		name: "breeze_get_test_coverage",
		description: "Measure a project's statement coverage and report it per package, naming " +
			"the least covered package and the ones with no tests at all. Also reports whether " +
			"the suite passed while being measured. Use this to find where tests are missing " +
			"before adding them. Reads only.",
		schema: objectSchema(map[string]any{
			"path": stringProp("Project root. Defaults to the server's working directory."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a coverageArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			return getTestCoverage(a.Path)
		},
	}
}

func getTestCoverage(path string) toolCallResult {
	root, err := verifyRoot(path)
	if err != nil {
		return errorResult("measuring coverage: " + err.Error())
	}

	run := runGo(root, coverageTimeout, "test", "./...", "-cover",
		"-timeout", strconv.Itoa(int(coverageTimeout.Seconds())-15)+"s")
	if run.StartErr != nil {
		return errorResult("measuring coverage: " + run.StartErr.Error())
	}

	packages, failures := parseTestOutput(run.Output)
	report := coverageReport{
		Path:        filepath.ToSlash(root),
		Command:     run.Command,
		Packages:    packages,
		TestsPassed: run.ok(),
		Failures:    failures,
		DurationMS:  run.Duration.Milliseconds(),
	}

	if run.TimedOut {
		report.Notes = append(report.Notes, fmt.Sprintf("the run was stopped after %s, so these "+
			"figures cover only the packages that finished", coverageTimeout))
	}

	var (
		sum     float64
		counted int
		lowest  = -1.0
	)
	for _, p := range packages {
		if p.Coverage == nil {
			if p.Status == "no-test-files" {
				report.Untested = append(report.Untested, p.Package)
			}
			continue
		}
		sum += *p.Coverage
		counted++
		if lowest < 0 || *p.Coverage < lowest {
			lowest = *p.Coverage
			report.LeastCovered = p.Package
		}
	}

	if counted > 0 {
		mean := sum / float64(counted)
		report.TotalPercent = &mean
	}

	if len(report.Untested) > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%d package(s) have no test files; they "+
			"are listed separately because they are absent from the average rather than counted "+
			"as zero", len(report.Untested)))
	}
	if !report.TestsPassed {
		report.Notes = append(report.Notes, "the suite failed while coverage was being measured, "+
			"so these figures describe a project that is currently broken")
	}
	if counted > 0 {
		report.Notes = append(
			report.Notes,
			"total_percent is the mean of the per-package figures, "+
				"not a statement-weighted total; a small package counts as much as a large one",
		)
	}

	summary := "coverage could not be measured for any package"
	if report.TotalPercent != nil {
		summary = fmt.Sprintf("%.1f%% mean coverage across %d measured package(s)",
			*report.TotalPercent, counted)
		if report.LeastCovered != "" {
			summary += fmt.Sprintf("; lowest is %s at %.1f%%", report.LeastCovered, lowest)
		}
	}
	return structuredResult(summary, report)
}

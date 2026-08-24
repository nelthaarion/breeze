package main

// End-to-end verification: scaffold a project, run every generator and every
// feature against it, and compile the result.
//
// This is the only test in the package that can catch the failure mode that
// matters here. Every generator routes its output through format.Source, which
// validates syntax and nothing else — a stub calling
// `dashboard.InstallDashboard(app)` when the real function is
// `dashboard.Install(app, router, cfg)` formats perfectly and passes every
// unit test in this package. It fails at `go build` in the user's project, which
// is the first place anyone would notice. So this test builds the project.
//
// Slow (a full compile of the framework plus the scaffold) and needs the module
// cache populated, so it skips under -short.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// e2eFeatures is every `breeze add` feature except migrator, with the flags
// that exercise the configurable paths through each block.
//
// migrator is excluded deliberately: it blank-imports a third-party SQL driver
// into the scaffold, which `go mod tidy` must fetch. That would turn this test
// into a skip on any machine without that driver cached, hiding the other
// twenty features. It gets its own test below.
//
// Keep this list exhaustive — TestE2EFeatureListIsComplete fails if a feature
// is added to the registry without being covered here.
var e2eFeatures = [][]string{
	{"recovery"},
	{"logging"},
	{"security"},
	{"cors", "--origins=https://example.com"},
	{"compression"},
	{"ratelimit", "--requests=100", "--per=1m"},
	{"observability"},
	{"events", "--async"},
	{"workflow"},
	{"dashboard", "--allow-writes", "--basepath=/admin"},
	{"i18n", "--locales=en,fr"},
	{"jwt", "--refresh"},
	{"oauth2", "--provider=github", "--session=jwt"},
	{"etag"},
	{"docs"},
	{"static"},
	{"video", "--signed"},
	{"websocket"},
	{"templates", "--spa"},
	{"tuning", "--inline"},
}

// e2eGenerators covers every generator kind, including the flag combinations
// that change which declarations get emitted — --methods in particular, because
// an import emitted for an excluded handler is a compile error.
var e2eGenerators = [][]string{
	{"handler", "Session", "--methods=list,get"},
	{"resource", "User", "name:string", "email:string", "age:int", "signed_up_at:time.Time"},
	{"resource", "Order", "total:float64", "paid:bool", "--methods=list,create", "--path=/api/v1/orders"},
	{"model", "Product", "sku:string", "price:float64", "tags:[]string"},
	{"event", "UserCreated", "id:int64", "email:string"},
	{"listener", "UserCreated", "--name=SendWelcomeEmail"},
	{"workflow", "Signup", "--steps=validate,charge-card,notify", "--retry", "--compensate"},
	{"middleware", "RequestID"},
	{"ws", "Chat"},
	{"job", "CleanupSessions", "--every=30s"},
}

// scaffoldE2E creates a project in a temp dir, points it at this working tree,
// and chdirs into it. The replace directive is the point: the generated code
// must compile against the APIs in this checkout, not against whatever version
// the module proxy last published.
func scaffoldE2E(t *testing.T, name string) {
	t.Helper()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Confirm we resolved the framework root, so a wrong path does not
	// masquerade as a generator bug.
	if _, err := os.Stat(filepath.Join(repoRoot, "websocket.go")); err != nil {
		t.Fatalf("repo root %s does not look like the breeze module: %v", repoRoot, err)
	}

	t.Chdir(t.TempDir())
	if err := runNew([]string{name, "--module=example.com/" + name}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}

	goMod := filepath.Join(name, "go.mod")
	content, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	b.Write(content)
	// A released build of the CLI already pins a version; a dev build does not,
	// and a replace with no require is not enough to resolve the module.
	if !strings.Contains(string(content), "require "+breezeModulePath) {
		b.WriteString("\nrequire " + breezeModulePath + " v0.0.0\n")
	}
	b.WriteString("\nreplace " + breezeModulePath + " => " + filepath.ToSlash(repoRoot) + "\n")

	if err := os.WriteFile(goMod, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(name)
}

// buildE2E runs tidy, build, and vet in the scaffolded project.
func buildE2E(t *testing.T) {
	t.Helper()

	// The scaffold's go.sum has no entries for breeze's own dependencies, which
	// the replace directive pulls in.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Skipf("go mod tidy failed (module cache or network unavailable): %v\n%s", err, out)
	}

	if out, err := exec.Command("go", "build", "./...").CombinedOutput(); err != nil {
		t.Fatalf("the generated project does not compile: %v\n%s", err, out)
	}
	if out, err := exec.Command("go", "vet", "./...").CombinedOutput(); err != nil {
		t.Errorf("go vet on the generated project: %v\n%s", err, out)
	}
}

func TestEndToEndScaffoldBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a scaffolded project; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	scaffoldE2E(t, "e2e")

	for _, args := range e2eGenerators {
		if err := runGenerate(args); err != nil {
			t.Fatalf("breeze generate %s: %v", strings.Join(args, " "), err)
		}
	}
	for _, args := range e2eFeatures {
		if err := runAdd(args); err != nil {
			t.Fatalf("breeze add %s: %v", strings.Join(args, " "), err)
		}
	}

	// `generate view` requires the templates block to have declared Templates,
	// so it has to run after the feature loop rather than alongside the other
	// generators.
	if err := runGenerate([]string{"view", "About", "--data"}); err != nil {
		t.Fatalf("breeze generate view: %v", err)
	}

	buildE2E(t)
}

// TestE2EFeatureListIsComplete guards the list above against the registry
// growing past it.
//
// Without this, a new feature is silently untested: e2eFeatures is a hand-kept
// list, and nothing else notices when it falls behind. It also catches the
// inverse — an entry naming a feature that does not exist, which is how
// `workerpool` sat in this list while the registry called it `tuning`.
func TestE2EFeatureListIsComplete(t *testing.T) {
	covered := make(map[string]bool, len(e2eFeatures))
	for _, args := range e2eFeatures {
		if _, ok := features[args[0]]; !ok {
			t.Errorf("e2eFeatures covers %q, which is not a registered feature", args[0])
			continue
		}
		covered[args[0]] = true
	}

	for name, f := range features {
		// migrator has its own test, for the reason given above e2eFeatures.
		if name == "migrator" {
			continue
		}
		if !covered[name] {
			t.Errorf("feature %q (%s) is not covered by e2eFeatures — add it so the "+
				"scaffold test compiles its generated block", name, f.Summary)
		}
	}
}

// TestEndToEndMigratorBuilds compiles the generated migration runner. Separate
// from the main test because it needs a third-party SQL driver in the module
// cache; sqlite is used rather than postgres only because it needs no server to
// be useful afterwards.
func TestEndToEndMigratorBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a scaffolded project; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	scaffoldE2E(t, "mig")

	if err := runAdd([]string{"migrator", "--driver=sqlite"}); err != nil {
		t.Fatalf("breeze add migrator: %v", err)
	}
	if err := runGenerate([]string{"model", "Product", "sku:string", "price:float64"}); err != nil {
		t.Fatalf("breeze generate model: %v", err)
	}

	// The runner is the thing that was broken: `breeze migrate` shelled out to
	// a package that blank-imported no driver.
	if _, err := os.Stat(filepath.Join("cmd", "migrate", "main.go")); err != nil {
		t.Fatalf("add migrator did not write cmd/migrate/main.go: %v", err)
	}

	buildE2E(t)
}

// TestEndToEndMigrateCycle runs the whole migration lifecycle against a real
// sqlite database, through the runner `add migrator` generates.
//
// This is the only test that exercises migrate.Runner against a live database at
// all, and both bugs it was written for needed one:
//
//   - Down read the ledger while holding the lock, and the lock is a row in that
//     same ledger. It saw a migration numbered -1, found no file for it, and
//     failed with "applied migration -1 not found in migration files" — every
//     invocation, on every project.
//   - The rollback order was sorted ascending under a comment claiming
//     descending, so `down 1` rolled back the *oldest* migration. That one is
//     silent: dropping the first table succeeds exactly as quietly as dropping
//     the last, and the user finds out later.
//
// Three migrations rather than one, because a single-migration project cannot
// tell the two orderings apart.
func TestEndToEndMigrateCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a scaffolded project against sqlite; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	scaffoldE2E(t, "cycle")

	if err := runAdd([]string{"migrator", "--driver=sqlite"}); err != nil {
		t.Fatalf("breeze add migrator: %v", err)
	}

	// Distinct tables per migration, so the down SQL of the wrong one leaves
	// evidence: rolling back 0001 while 0002 and 0003 still exist is exactly the
	// ascending-sort bug.
	for _, m := range []struct{ version, table string }{
		{"0001", "alpha"},
		{"0002", "beta"},
		{"0003", "gamma"},
	} {
		up := filepath.Join("migrations", m.version+"_create_"+m.table+".up.sql")
		down := filepath.Join("migrations", m.version+"_create_"+m.table+".down.sql")
		if err := os.WriteFile(up, []byte("CREATE TABLE "+m.table+" (id INTEGER PRIMARY KEY);"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(down, []byte("DROP TABLE "+m.table+";"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Skipf("go mod tidy failed (module cache or network unavailable): %v\n%s", err, out)
	}

	// A file DSN rather than :memory:, because each `go run` is a fresh process
	// and an in-memory database would not survive between them — every command
	// would see an empty ledger and the test would pass no matter what.
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "cycle.db"))
	migrate := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("go", append([]string{"run", "./cmd/migrate"}, args...)...)
		cmd.Env = append(os.Environ(), "BREEZE_DATABASE_URL="+dsn)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// applied reports which versions status says are applied.
	applied := func(stage string) map[string]bool {
		t.Helper()
		out, err := migrate("status")
		if err != nil {
			t.Fatalf("migrate status (%s): %v\n%s", stage, err, out)
		}
		got := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			if strings.Contains(line, " applied") {
				got[fields[0]] = true
			}
		}
		return got
	}

	assertApplied := func(stage string, want ...string) {
		t.Helper()
		got := applied(stage)
		if len(got) != len(want) {
			t.Fatalf("%s: %d migrations applied, want %d (%v)", stage, len(got), len(want), got)
		}
		for _, v := range want {
			if !got[v] {
				t.Fatalf("%s: migration %s is not applied; applied set is %v", stage, v, got)
			}
		}
	}

	if out, err := migrate("up"); err != nil {
		t.Fatalf("migrate up: %v\n%s", err, out)
	}
	assertApplied("after up", "0001", "0002", "0003")

	// The bug that failed outright. Before the lock sentinel was filtered out of
	// the ledger read, this returned "applied migration -1 not found".
	if out, err := migrate("down", "1"); err != nil {
		t.Fatalf("migrate down 1: %v\n%s", err, out)
	}
	// And the bug that did not fail: down 1 must have taken 0003, the newest.
	assertApplied("after down 1", "0001", "0002")

	// Repeatable, and still newest-first on the second call.
	if out, err := migrate("down", "1"); err != nil {
		t.Fatalf("migrate down 1 (second): %v\n%s", err, out)
	}
	assertApplied("after down 1 twice", "0001")

	// n greater than what is applied rolls back what there is rather than
	// erroring, so `down 5` is a safe way to say "all of it".
	if out, err := migrate("down", "5"); err != nil {
		t.Fatalf("migrate down 5: %v\n%s", err, out)
	}
	assertApplied("after down 5")

	// Rolling back everything must leave the ledger clean enough to re-apply —
	// a leaked lock row would make this fail with "another migration is running".
	if out, err := migrate("up"); err != nil {
		t.Fatalf("migrate up after full rollback: %v\n%s", err, out)
	}
	assertApplied("after re-up", "0001", "0002", "0003")

	// down with nothing applied is an error, not a silent success: `down` when
	// the ledger is empty means the user's mental model is wrong.
	if _, err := migrate("down", "9"); err != nil {
		t.Fatalf("migrate down 9 with 3 applied should roll back all three: %v", err)
	}
	if out, err := migrate("down", "1"); err == nil {
		t.Errorf("migrate down 1 on an empty ledger should report there is nothing to roll back, got success:\n%s", out)
	}
}

// TestEndToEndAddIsIdempotent runs every feature repeatedly and asserts the
// project settles. Re-running add has to be safe: it is the documented way to
// change a feature's flags, and a version that appended instead of replacing
// would duplicate declarations and stop compiling.
//
// The fixed point arrives after the second pass rather than the first, and that
// is composition rather than a defect. observability is added before events in
// the list above, so its first block attaches the collector to events.Default;
// once the bus exists, the next add points it at EventBus. `add events` reports
// this when it happens, and the re-run needs no --force because the block still
// carries the checksum it was written with.
//
// What must hold is that the passes converge and then stay put — a block that
// kept being rewritten would mean a generator whose output is not a function of
// its inputs.
func TestEndToEndAddIsIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew([]string{"idem", "--module=example.com/idem"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}
	t.Chdir("idem")

	apply := func(pass string) string {
		t.Helper()
		for _, args := range e2eFeatures {
			if err := runAdd(args); err != nil {
				t.Fatalf("breeze add %s (%s): %v", strings.Join(args, " "), pass, err)
			}
		}
		content, err := os.ReadFile(featuresFileName)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}

	apply("first pass")
	if second, third := apply("second pass"), apply("third pass"); second != third {
		t.Errorf("%s kept changing after the second full pass — add does not converge", featuresFileName)
	}
}

// TestAddIsIdempotentPerFeature is the strict half of the claim above.
//
// One feature in a fresh project has nothing to compose with, so a second add
// has nothing it could legitimately change: any difference at all is a bug in
// that feature. Running them in isolation is also what makes a failure legible
// — the whole-project test can only report that some block moved, whereas this
// names the feature.
//
// The failure it was written for: upsertBlock gofmts the file it writes, and
// gofmt inserts a blank line between a declaration and the comment that follows
// it, so the stored block never matched the freshly built one byte-for-byte.
// Every feature's second add failed, demanding --force for a block nobody had
// touched.
func TestAddIsIdempotentPerFeature(t *testing.T) {
	for _, args := range e2eFeatures {
		t.Run(args[0], func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := runNew([]string{"p", "--module=example.com/p"}); err != nil {
				t.Fatalf("breeze new: %v", err)
			}
			t.Chdir("p")

			if err := runAdd(args); err != nil {
				t.Fatalf("breeze add %s: %v", strings.Join(args, " "), err)
			}
			first, err := os.ReadFile(featuresFileName)
			if err != nil {
				t.Fatal(err)
			}

			if err := runAdd(args); err != nil {
				t.Fatalf("breeze add %s (second run): %v", strings.Join(args, " "), err)
			}
			second, err := os.ReadFile(featuresFileName)
			if err != nil {
				t.Fatal(err)
			}

			if string(first) != string(second) {
				t.Errorf("a second `breeze add %s` rewrote %s:\n--- first\n%s\n--- second\n%s",
					strings.Join(args, " "), featuresFileName, first, second)
			}
		})
	}
}

// TestAddReplacesWhenFlagsChange covers the other half of what the checksum
// buys: changing a flag is a re-run, not an error.
//
// This is the documented way to reconfigure a feature ("re-running replaces
// that block"), and before blocks carried a checksum it failed — a block whose
// body differed was indistinguishable from one edited by hand, so add refused
// both and warned about losing edits that did not exist.
func TestAddReplacesWhenFlagsChange(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew([]string{"flags", "--module=example.com/flags"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}
	t.Chdir("flags")

	if err := runAdd([]string{"dashboard", "--basepath=/admin"}); err != nil {
		t.Fatalf("breeze add dashboard --basepath=/admin: %v", err)
	}
	if err := runAdd([]string{"dashboard", "--basepath=/_debug"}); err != nil {
		t.Fatalf("changing a flag should not need --force: %v", err)
	}

	content, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"/_debug"`) {
		t.Errorf("the new basepath is not in %s:\n%s", featuresFileName, content)
	}
	if strings.Contains(string(content), `"/admin"`) {
		t.Errorf("the old basepath survived in %s — the block was not replaced", featuresFileName)
	}
}

// TestAddRefusesToClobberHandEdits is the case --force exists for. The checksum
// on the marker is what makes this distinguishable from the test above.
func TestAddRefusesToClobberHandEdits(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew([]string{"edited", "--module=example.com/edited"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}
	t.Chdir("edited")

	if err := runAdd([]string{"cors"}); err != nil {
		t.Fatalf("breeze add cors: %v", err)
	}

	content, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(content), "func setupCors(", "// tuned by hand\nfunc setupCors(", 1)
	if edited == string(content) {
		t.Fatalf("could not find setupCors to edit in %s:\n%s", featuresFileName, content)
	}
	if err := os.WriteFile(featuresFileName, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	err = runAdd([]string{"cors"})
	if err == nil {
		t.Fatal("add overwrote a hand-edited block without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the error should name the escape hatch, got: %v", err)
	}

	after, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "tuned by hand") {
		t.Error("the refused add still modified the block")
	}

	if err := runAdd([]string{"cors", "--force"}); err != nil {
		t.Fatalf("breeze add cors --force: %v", err)
	}
	forced, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(forced), "tuned by hand") {
		t.Error("--force did not replace the hand-edited block")
	}
}

// TestAddReportsStaleDependents checks that adding a feature late says which
// installed blocks were generated without it.
//
// Silence here is the bad outcome: `add dashboard` then `add events` leaves the
// dashboard's live Events page permanently empty, and nothing about the project
// — it compiles, it serves, the page renders — indicates why.
func TestAddReportsStaleDependents(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew([]string{"stale", "--module=example.com/stale"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}
	t.Chdir("stale")

	if err := runAdd([]string{"dashboard"}); err != nil {
		t.Fatalf("breeze add dashboard: %v", err)
	}
	// Generated before the bus existed, so the bridge is absent.
	before, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "DashboardCollector.AttachEvents") {
		t.Fatal("dashboard bridged to a bus that does not exist yet")
	}

	if err := runAdd([]string{"events"}); err != nil {
		t.Fatalf("breeze add events: %v", err)
	}
	// And the re-run the warning points at must work without --force.
	if err := runAdd([]string{"dashboard"}); err != nil {
		t.Fatalf("re-running add dashboard after add events: %v", err)
	}
	after, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "DashboardCollector.AttachEvents(EventBus)") {
		t.Errorf("dashboard did not pick up the bus on re-run:\n%s", after)
	}
}

// TestGeneratedFeatureBlocksAreIdempotent covers the same guarantee for the
// generators that write into features_generated.go. They go through
// upsertGeneratedFeature rather than the add path, so idempotency is a separate
// claim.
func TestGeneratedFeatureBlocksAreIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew([]string{"idem2", "--module=example.com/idem2"}); err != nil {
		t.Fatalf("breeze new: %v", err)
	}
	t.Chdir("idem2")

	run := func() string {
		t.Helper()
		for _, args := range [][]string{
			{"ws", "Chat", "--force"},
			{"job", "Cleanup", "--every=1m", "--force"},
		} {
			if err := runGenerate(args); err != nil {
				t.Fatalf("breeze generate %s: %v", strings.Join(args, " "), err)
			}
		}
		content, err := os.ReadFile(featuresFileName)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}

	if first, second := run(), run(); first != second {
		t.Errorf("re-running ws/job generators changed %s — the blocks are not idempotent", featuresFileName)
	}
}

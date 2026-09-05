package mcp

// changeset_limits_test.go — the denial-of-service bounds on change sets.
//
// These are the cheapest resource attacks in this package, because
// breeze_begin_change_set needs no arguments at all: project_path defaults to the
// server's working directory, so a client can open a full project copy per call with an
// empty object. Nothing expires a change set, so unbounded meant unbounded disk.
//
// The tests use the store directly rather than going through the tool. Driving 33 calls
// through the JSON layer would test the same bound while copying a real project 33 times,
// and the bound is a property of the store.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newChangeSetFixture writes the smallest thing a change set can be opened over.
//
// begin copies the tree and snapshots it, so an empty directory is enough — this is not
// testing generation, only how many copies may exist at once.
func newChangeSetFixture(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("writing the fixture module: %v", err)
	}
	return root
}

// TestOpenChangeSetsAreBounded is the disk-exhaustion bound.
//
// Each open set is a copy of the project held until commit or discard. The limit is
// asserted at exactly the boundary — the last permitted one succeeds, the next fails —
// because an off-by-one in either direction is invisible otherwise: a limit one too low
// refuses legitimate work, and one too high is a bound nobody notices is wrong.
func TestOpenChangeSetsAreBounded(t *testing.T) {
	root := newChangeSetFixture(t)
	store := newChangeSetStore()

	for i := 0; i < maxOpenChangeSets; i++ {
		set, err := store.begin(root)
		if err != nil {
			t.Fatalf("opening change set %d of %d: %v", i+1, maxOpenChangeSets, err)
		}
		// Discarded through the store at the end of the test rather than here: the
		// point is to hold them all open at once.
		t.Cleanup(set.discard)
	}

	_, err := store.begin(root)
	if err == nil {
		t.Fatalf("a %dth change set was opened; the limit is %d. Each one is a full project copy "+
			"that is never expired, so this is unbounded disk from a tool that takes no arguments",
			maxOpenChangeSets+1, maxOpenChangeSets)
	}
	if !strings.Contains(err.Error(), "discard") {
		t.Errorf("the refusal does not tell the caller how to recover: %v", err)
	}
}

// TestDiscardingAChangeSetFreesASlot is what makes the bound a limit rather than a quota.
//
// A cap that could not be recovered from would turn a client that opened its allowance
// into one that can never open another, which is a worse failure than the one being
// prevented.
func TestDiscardingAChangeSetFreesASlot(t *testing.T) {
	root := newChangeSetFixture(t)
	store := newChangeSetStore()

	var first *changeSet
	for i := 0; i < maxOpenChangeSets; i++ {
		set, err := store.begin(root)
		if err != nil {
			t.Fatalf("opening change set %d: %v", i+1, err)
		}
		if i == 0 {
			first = set
		}
	}

	if _, err := store.begin(root); err == nil {
		t.Fatal("the limit was not reached, so this test is not exercising recovery")
	}

	// take is what discard_change_set does: remove it from the store, then delete the
	// copy. Only the first of those frees the slot, which is what is being asserted.
	taken, err := store.take(first.ID)
	if err != nil {
		t.Fatalf("taking the first change set: %v", err)
	}
	taken.discard()

	set, err := store.begin(root)
	if err != nil {
		t.Fatalf("a slot was freed but a new change set was still refused: %v", err)
	}
	set.discard()
}

// TestStagedCallsAreBounded is the per-set bound.
//
// Every staged call keeps its arguments and its output so commit can write them to the
// history, so a set that grows without limit is memory as well as an unbounded run. The
// runner here does nothing: what is being measured is how many calls are accepted, not
// what they do.
func TestStagedCallsAreBounded(t *testing.T) {
	root := newChangeSetFixture(t)
	store := newChangeSetStore()

	set, err := store.begin(root)
	if err != nil {
		t.Fatalf("opening the change set: %v", err)
	}
	t.Cleanup(set.discard)

	args := json.RawMessage(`{}`)
	noop := func(string) error { return nil }

	for i := 0; i < maxStagedCalls; i++ {
		if _, err := set.stage("breeze_add", args, noop); err != nil {
			t.Fatalf("staging call %d of %d: %v", i+1, maxStagedCalls, err)
		}
	}

	_, err = set.stage("breeze_add", args, noop)
	if err == nil {
		t.Fatalf("a %dth call was staged; the limit is %d", maxStagedCalls+1, maxStagedCalls)
	}
	if !strings.Contains(err.Error(), "Commit") {
		t.Errorf("the refusal does not tell the caller how to proceed: %v", err)
	}
	// The set stays usable for the operations that end it — refusing a call must not
	// strand the work already staged.
	if len(set.Calls) != maxStagedCalls {
		t.Errorf("the refused call was recorded anyway: %d calls staged, want %d",
			len(set.Calls), maxStagedCalls)
	}
}

// TestChangeSetLimitsAreEnforcedBeforeTheWork asserts the ordering, which is the whole
// value of the bound.
//
// A limit checked after the project is copied still copies the project, and a limit
// checked after the generator runs still runs it. So the failing call must not have
// produced a sandbox or invoked the runner.
func TestChangeSetLimitsAreEnforcedBeforeTheWork(t *testing.T) {
	root := newChangeSetFixture(t)
	store := newChangeSetStore()

	// One slot short of the limit, so the staging half below has a set to work with
	// after the begin half has established that the cap refuses without copying.
	var spare *changeSet
	for i := 0; i < maxOpenChangeSets; i++ {
		set, err := store.begin(root)
		if err != nil {
			t.Fatalf("opening change set %d: %v", i+1, err)
		}
		t.Cleanup(set.discard)
		if i == 0 {
			spare = set
		}
	}

	// The refused begin must not have left a sandbox behind. Counting the temporary
	// directories the sandboxes use is the only way to see that from outside.
	before := countSandboxes(t)
	if _, err := store.begin(root); err == nil {
		t.Fatal("expected the open-set limit to refuse this call")
	}
	if after := countSandboxes(t); after > before {
		t.Errorf("the refused begin created %d sandbox directory/ies; the limit is meant to "+
			"prevent the copy, not to clean up after it", after-before)
	}

	// The staged-call limit, same argument: the runner must not be invoked. An
	// already-open set is reused rather than opening another, because every slot is
	// taken — which is the state the first half of this test just established.
	args := json.RawMessage(`{}`)
	for i := len(spare.Calls); i < maxStagedCalls; i++ {
		if _, err := spare.stage(
			"breeze_add",
			args,
			func(string) error { return nil },
		); err != nil {
			t.Fatalf("staging call %d: %v", i+1, err)
		}
	}

	ran := false
	if _, err := spare.stage(
		"breeze_add",
		args,
		func(string) error { ran = true; return nil },
	); err == nil {
		t.Fatal("expected the staged-call limit to refuse this call")
	}
	if ran {
		t.Error("the refused call ran the generator anyway; the bound exists to stop the work, " +
			"not to discard its result")
	}
}

// countSandboxes counts the change-set sandbox directories in the temporary directory.
//
// newSandbox creates them with a known prefix, which is what makes this observable
// without exporting anything.
func countSandboxes(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("reading the temporary directory: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), sandboxPrefix) {
			count++
		}
	}
	return count
}

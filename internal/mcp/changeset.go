package mcp

// changeset.go — several tool calls, one outcome.
//
// An agent building a feature does not make one call. It scaffolds, adds a
// middleware, generates a model, generates the routes that use it. Any of those
// can be refused — a block edited by hand, a name that already exists, a field
// type that does not resolve — and the refusal arrives after the earlier calls
// have already written files. What is left on disk is then neither the old
// project nor the intended one, and the agent has to work out which half
// happened.
//
// A change set removes that state. Every staged call runs against a private copy
// of the project, so a sequence that fails has changed nothing at all, and
// commit is a single operation that either applies every file or none of them.
// Discarding is free, which is what makes staging worth doing: the cost of
// finding out that a plan does not work is a deleted temporary directory.
//
// The history a commit appends is the other half. `breeze add` prints what it
// did and the words scroll away; a record on disk survives the session, so a
// later question — what changed this file, and what was asked for — has an
// answer that does not depend on anyone having kept the transcript.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nelthaarion/breeze/internal/generator"
)

// stateDirName is the per-project directory the history lives in.
//
// It sits inside the project rather than in a user-level cache because the
// history describes this project: a copy of the repository taken elsewhere
// should carry its own record, and a project deleted should not leave one
// behind.
const stateDirName = ".breeze"

// historyFileName is JSON Lines rather than a JSON array.
//
// Appending to an array means reading, parsing and rewriting the whole file to
// add one entry, and a process interrupted mid-rewrite leaves a file that
// parses as nothing. One object per line appends in a single write, and a
// truncated last line costs one record instead of all of them.
const historyFileName = "mcp-history.jsonl"

// stageableTools are the tools a change set will run.
//
// Only the three that write files. A read-only tool has nothing to stage — it
// would return its answer at stage time and contribute nothing to the commit —
// and allowing it would suggest otherwise.
var stageableTools = []string{"breeze_add", "breeze_generate", "breeze_new"}

// stagedCall is one call recorded in a change set.
type stagedCall struct {
	// Tool and Arguments are what was asked for, kept verbatim so the history
	// records the request rather than this package's reading of it.
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`

	// Changes is what the call did to the sandbox.
	Changes []fileChange `json:"changes"`

	// Output is what the generator printed. It is kept in the history for the
	// same reason a build log is kept: it names the files it touched in the
	// generator's own words.
	Output string `json:"output,omitempty"`

	StagedAt time.Time `json:"staged_at"`
}

// changeSet is an open sequence of calls against a private copy of a project.
type changeSet struct {
	ID string `json:"id"`
	// ProjectPath is the absolute path commit will write to.
	ProjectPath string    `json:"project_path"`
	CreatedAt   time.Time `json:"created_at"`

	// Calls are the staged calls in order.
	Calls []stagedCall `json:"calls"`

	// box is the copy the calls ran against, and baseline the snapshot it
	// started from. Commit diffs the sandbox against baseline rather than
	// concatenating the per-call changes: a file written by one call and
	// rewritten by the next appears once, and a file created and then deleted
	// does not appear at all.
	box      *sandbox
	baseline map[string]fileState
}

// changeSetStore holds the open change sets.
//
// In-process and not persisted: a change set is a sandbox directory plus the
// intent to commit it, and neither outlives the server usefully. A restart
// leaves temporary directories to be cleaned by the operating system rather
// than a half-open set that a later session might commit without knowing what
// is in it.
type changeSetStore struct {
	mu   sync.Mutex
	sets map[string]*changeSet
}

// maxOpenChangeSets bounds how many change sets may be open at once.
//
// Each one is a full copy of a project on disk, held until it is committed or
// discarded, and nothing expires them: the store is deliberately in-process and
// unpersisted, so a client that opens sets and never closes them grows the temporary
// directory without bound. That is reachable from one tool with no arguments —
// breeze_begin_change_set defaults project_path to the working directory — which makes
// it the cheapest denial of service in this package.
//
// Thirty-two is far above any real session. A person working through a feature has one
// open; an agent exploring alternatives might have a handful. Reaching this number means
// something is looping, and the error says so rather than filling the disk.
const maxOpenChangeSets = 32

// maxStagedCalls bounds one change set's length.
//
// Every staged call runs a generator against the copy and its arguments and output are
// retained for the history, so an unbounded sequence is unbounded memory as well as an
// unbounded run. A hundred is more calls than any coherent change needs — the largest
// scaffold in this repository's own documentation is under ten — and it is a limit on
// one *set*, not on how much work a session can do: commit and open another.
const maxStagedCalls = 100

func newChangeSetStore() *changeSetStore {
	return &changeSetStore{sets: make(map[string]*changeSet)}
}

// begin copies a project and opens a change set over the copy.
//
// The path is confined here as well as in newSandbox below. That is not redundant:
// this one is what ends up as changeSet.ProjectPath, which is the directory commit
// later writes to — so the check has to be on the value that is stored, not only on
// the value that was copied.
func (s *changeSetStore) begin(projectPath string) (*changeSet, error) {
	abs, err := resolvePath(projectPath)
	if err != nil {
		return nil, err
	}

	// Checked before the copy, because the copy is the expensive part and the point
	// of the bound is not to make it. Under the same lock the map is written below,
	// so two concurrent calls cannot both see room for the last slot.
	s.mu.Lock()
	if len(s.sets) >= maxOpenChangeSets {
		open := len(s.sets)
		s.mu.Unlock()
		return nil, fmt.Errorf("%d change sets are already open and the limit is %d. Each one is a "+
			"full copy of a project that is kept until it is committed or discarded, so they do not "+
			"expire on their own. Commit or discard one with breeze_commit_change_set or "+
			"breeze_discard_change_set", open, maxOpenChangeSets)
	}
	s.mu.Unlock()

	box, err := newSandbox(abs)
	if err != nil {
		return nil, err
	}
	baseline, err := snapshotTree(box.projectDir())
	if err != nil {
		box.remove()
		return nil, err
	}

	id, err := newChangeSetID()
	if err != nil {
		box.remove()
		return nil, err
	}

	set := &changeSet{
		ID:          id,
		ProjectPath: abs,
		CreatedAt:   time.Now().UTC(),
		box:         box,
		baseline:    baseline,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-checked with the copy already made: another goroutine may have taken the
	// last slot while this one was copying. Removing the sandbox here rather than
	// storing a set past the limit is the whole point of having a limit.
	if len(s.sets) >= maxOpenChangeSets {
		box.remove()
		return nil, fmt.Errorf("%d change sets are already open and the limit is %d; another call "+
			"took the last slot while this project was being copied. Commit or discard one and retry",
			len(s.sets), maxOpenChangeSets)
	}
	s.sets[id] = set
	return set, nil
}

// get returns an open change set.
func (s *changeSetStore) get(id string) (*changeSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	set, ok := s.sets[id]
	if !ok {
		return nil, fmt.Errorf("unknown change set %q — it was committed, discarded, or never opened", id)
	}
	return set, nil
}

// take removes a change set from the store and returns it.
//
// Commit and discard both take rather than get, so an id cannot be committed
// twice: the second attempt finds nothing, which is the correct answer.
func (s *changeSetStore) take(id string) (*changeSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	set, ok := s.sets[id]
	if !ok {
		return nil, fmt.Errorf("unknown change set %q — it was committed, discarded, or never opened", id)
	}
	delete(s.sets, id)
	return set, nil
}

// newChangeSetID returns a random identifier.
//
// Random rather than sequential because an id is quoted back by a caller that
// may be one of several: a counter would make cs-3 mean different things to two
// clients of the same server.
func newChangeSetID() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mcp: cannot generate a change set id: %w", err)
	}
	return "cs_" + hex.EncodeToString(buf[:]), nil
}

// pendingChanges is the net difference the change set would commit.
func (c *changeSet) pendingChanges() ([]fileChange, error) {
	after, err := snapshotTree(c.box.projectDir())
	if err != nil {
		return nil, err
	}
	return diffSnapshots(c.baseline, after), nil
}

// stage runs one call against the change set's copy.
//
// A call the generator refuses is not recorded. The alternative — keeping it
// with its error — would mean a change set could be committed while containing
// a step that did not happen, and the caller would have to inspect each entry
// to find out whether the sequence is still coherent.
func (c *changeSet) stage(toolName string, args json.RawMessage, run func(dir string) error) (stagedCall, error) {
	// Bounded before the generator runs. Each staged call retains its arguments and
	// output for the history, so the sequence is memory as well as time.
	if len(c.Calls) >= maxStagedCalls {
		return stagedCall{}, fmt.Errorf("change set %s already has %d staged calls and the limit "+
			"is %d. Every staged call is retained with its arguments and output so the commit can "+
			"record them, so a set does not grow without bound. Commit this one and open another",
			c.ID, len(c.Calls), maxStagedCalls)
	}

	dir := c.box.projectDir()

	result, err := runInSandbox(dir, dir, func() error { return run(dir) })
	if err != nil {
		return stagedCall{}, err
	}
	if result.Err != nil {
		return stagedCall{}, result.Err
	}

	call := stagedCall{
		Tool:      toolName,
		Arguments: args,
		Changes:   result.Changes,
		Output:    result.Output,
		StagedAt:  time.Now().UTC(),
	}
	c.Calls = append(c.Calls, call)
	return call, nil
}

// commit writes the change set into the project and records it.
//
// The history is appended after the files land, and a failure to write it is
// returned as a warning rather than an error: the commit has already happened,
// and reporting it as a failure would invite a caller to retry a change that is
// already applied.
func (c *changeSet) commit() (changes []fileChange, warning string, err error) {
	defer c.box.remove()

	changes, err = c.pendingChanges()
	if err != nil {
		return nil, "", err
	}
	if len(changes) == 0 {
		return nil, "", nil
	}

	if err := applyChanges(c.box.projectDir(), c.ProjectPath, changes); err != nil {
		return nil, "", fmt.Errorf("committing change set %s: %w (the project was left unchanged)", c.ID, err)
	}

	for _, call := range c.Calls {
		record := historyEntry{
			Timestamp:   time.Now().UTC(),
			ChangeSet:   c.ID,
			Tool:        call.Tool,
			Arguments:   call.Arguments,
			Changes:     call.Changes,
			Output:      call.Output,
			ProjectPath: c.ProjectPath,
		}
		if writeErr := appendHistory(c.ProjectPath, record); writeErr != nil {
			warning = "the change was applied, but the history could not be written: " + writeErr.Error()
			break
		}
	}
	return changes, warning, nil
}

// discard deletes the copy without touching the project.
func (c *changeSet) discard() { c.box.remove() }

// historyEntry is one record in a project's change history.
type historyEntry struct {
	Timestamp time.Time `json:"timestamp"`
	// ChangeSet is the set this call was committed as part of, or "" for a
	// single call applied directly.
	ChangeSet string          `json:"change_set,omitempty"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Changes   []fileChange    `json:"changes"`
	Output    string          `json:"output,omitempty"`
	// ProjectPath is recorded so a history file that is copied elsewhere still
	// says which project it describes.
	ProjectPath string `json:"project_path,omitempty"`
}

// historyPath is the history file for a project.
func historyPath(projectPath string) string {
	return filepath.Join(projectPath, stateDirName, historyFileName)
}

// appendHistory adds one record to a project's history.
func appendHistory(projectPath string, entry historyEntry) error {
	path := historyPath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readHistory returns a project's history, newest first, at most limit entries.
//
// A line that does not parse is skipped rather than failing the read: the most
// likely cause is a write interrupted at the end of the file, and one damaged
// record is not a reason to withhold the rest.
func readHistory(projectPath string, limit int) ([]historyEntry, error) {
	data, err := os.ReadFile(historyPath(projectPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	out := make([]historyEntry, 0, len(lines))

	// Read backwards: newest first is the order a caller asking for "the last
	// few" wants, and it means limit can stop the loop instead of trimming
	// afterwards.
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry historyEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		out = append(out, entry)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// stagedRunner returns the generator call a staged tool name and arguments mean.
//
// The argv comes from the same argv() methods the tools themselves use, so a
// staged call and a direct call cannot differ: there is one translation from
// arguments to command line per tool, and this reads it rather than repeating
// it.
//
// dir is ignored for two of the three. `breeze new` is the exception — it
// creates a directory rather than working inside one — and staging it is
// refused for that reason: a change set is opened over a project that exists,
// so scaffolding into it would either fail on the existing name or create a
// project inside a project.
func stagedRunner(toolName string, args json.RawMessage) (func(dir string) error, error) {
	switch toolName {
	case "breeze_add":
		var a addFeatureArgs
		if err := decodeArgs(args, &a); err != nil {
			return nil, fmt.Errorf("arguments for %s: %w", toolName, err)
		}
		if a.Dir != "" {
			return nil, fmt.Errorf("%s cannot set dir inside a change set — the change set decides where the call runs", toolName)
		}
		argv, err := a.argv()
		if err != nil {
			return nil, err
		}
		return func(string) error { return generator.Add(argv) }, nil

	case "breeze_generate":
		var a generateArgs
		if err := decodeArgs(args, &a); err != nil {
			return nil, fmt.Errorf("arguments for %s: %w", toolName, err)
		}
		if a.Dir != "" {
			return nil, fmt.Errorf("%s cannot set dir inside a change set — the change set decides where the call runs", toolName)
		}
		argv, err := a.argv()
		if err != nil {
			return nil, err
		}
		return func(string) error { return generator.Generate(argv) }, nil

	case "breeze_new":
		return nil, errors.New("breeze_new cannot be staged: a change set is opened over a project that already exists. " +
			"Use plan_project to see what a scaffold would create, then breeze_new to create it")

	default:
		return nil, fmt.Errorf("%s cannot be staged; stageable tools are: %s",
			toolName, strings.Join(stageableTools, ", "))
	}
}

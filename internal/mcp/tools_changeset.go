package mcp

// tools_changeset.go — the change-set and history tools.
//
// These are adapters. Everything that decides anything lives in changeset.go:
// the sandbox a staged call runs against, the net diff a commit applies, the
// rollback if part of it fails, the record appended afterwards. What is left
// here is turning arguments into those calls and shaping the answers, which is
// exactly as much as a tool layer should do.
//
// One decision is visible in this file rather than that one. stage_call reports
// the pending change list on every call, not just at commit: an agent that
// stages four calls and looks once at the end has no way to tell which of them
// produced which file, and the whole point of staging is to be able to change
// course before anything lands.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func registerChangeSetTools(s *Server) {
	sets := newChangeSetStore()

	s.addTool(beginChangeSetTool(sets))
	s.addTool(stageCallTool(sets))
	s.addTool(commitChangeSetTool(sets))
	s.addTool(discardChangeSetTool(sets))
	s.addTool(changeHistoryTool())
}

// ─── begin_change_set ────────────────────────────────────────────────────────

type beginChangeSetArgs struct {
	ProjectPath string `json:"project_path"`
}

// changeSetView is how an open change set is reported.
//
// It is a separate type from changeSet because a caller has no use for the
// sandbox path, and publishing it would invite writes to a directory that is
// about to be deleted.
type changeSetView struct {
	ID          string    `json:"id"`
	ProjectPath string    `json:"project_path"`
	CreatedAt   time.Time `json:"created_at"`

	// Staged is the calls accepted so far, in order.
	Staged []stagedCall `json:"staged"`
	// Pending is the net difference the change set would commit — one entry per
	// file, however many calls touched it.
	Pending []fileChange `json:"pending"`

	// Stageable names the tools stage_call accepts, so a caller does not have
	// to discover the restriction by being refused.
	Stageable []string `json:"stageable_tools,omitempty"`
}

func beginChangeSetTool(sets *changeSetStore) *tool {
	return &tool{
		name: "breeze_begin_change_set",
		description: "Open a change set over a project: a private copy that staged calls run " +
			"against, so a sequence that fails partway through leaves the project " +
			"untouched. Stage calls with breeze_stage_call, then apply them all at " +
			"once with breeze_commit_change_set or throw them away with " +
			"breeze_discard_change_set. Use this whenever a change needs more than " +
			"one generator call.",
		schema: objectSchema(map[string]any{
			"project_path": stringProp("Root of the project to open the change set over. Defaults to the server's working directory."),
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a beginChangeSetArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}

			root, err := orWorkingDir(a.ProjectPath)
			if err != nil {
				return errorResult(err.Error())
			}
			set, err := sets.begin(root)
			if err != nil {
				return errorResult("the change set could not be opened: " + err.Error())
			}

			view := changeSetView{
				ID:          set.ID,
				ProjectPath: set.ProjectPath,
				CreatedAt:   set.CreatedAt,
				Staged:      []stagedCall{},
				Pending:     []fileChange{},
				Stageable:   stageableTools,
			}
			return structuredResult(fmt.Sprintf("change set %s is open over %s; nothing is staged yet",
				set.ID, set.ProjectPath), view)
		},
	}
}

// ─── stage_call ──────────────────────────────────────────────────────────────

type stageCallArgs struct {
	ChangeSet string `json:"change_set"`
	Tool      string `json:"tool"`
	// Arguments is the argument object the named tool would receive, passed
	// through unchanged. It is raw JSON rather than a decoded map because the
	// staged tool decodes it with its own argument type, and re-encoding a map
	// would be a second chance to change its meaning.
	Arguments json.RawMessage `json:"arguments"`
}

// stageResult is what stage_call answers.
type stageResult struct {
	ChangeSet string `json:"change_set"`
	// Call is the call as staged, with what it changed in the sandbox.
	Call stagedCall `json:"call"`
	// Pending is the net difference the whole set would now commit.
	Pending []fileChange `json:"pending"`
	// StagedCount saves a caller counting the array to find out how far along
	// the sequence is.
	StagedCount int `json:"staged_count"`
	// AppliedToProject is always false: a staged call has run against the copy
	// only.
	AppliedToProject bool `json:"applied_to_project"`
}

func stageCallTool(sets *changeSetStore) *tool {
	return &tool{
		name: "breeze_stage_call",
		description: "Run one generator call inside an open change set, against its private " +
			"copy rather than the project. Stageable tools: " +
			strings.Join(stageableTools, ", ") + ". A call the generator refuses is " +
			"not recorded, so the change set stays a sequence that actually worked.",
		schema: objectSchema(map[string]any{
			"change_set": stringProp("The change set id returned by breeze_begin_change_set."),
			"tool": map[string]any{
				"type":        "string",
				"enum":        stageableTools,
				"description": "Which tool to stage.",
			},
			"arguments": map[string]any{
				"type":        "object",
				"description": "The arguments that tool would be called with. dir is not allowed — the change set decides where the call runs.",
			},
		}, "change_set", "tool"),
		run: func(raw json.RawMessage) toolCallResult {
			var a stageCallArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			if a.ChangeSet == "" {
				return errorResult("change_set is required; open one with breeze_begin_change_set")
			}
			if a.Tool == "" {
				return errorResult("tool is required; stageable tools are: " + strings.Join(stageableTools, ", "))
			}

			set, err := sets.get(a.ChangeSet)
			if err != nil {
				return errorResult(err.Error())
			}

			run, err := stagedRunner(a.Tool, a.Arguments)
			if err != nil {
				return errorResult(err.Error())
			}

			call, err := set.stage(a.Tool, a.Arguments, run)
			if err != nil {
				// The set stays open. A refused call is usually a fixable
				// mistake — a name that exists, a flag that does not — and
				// discarding the earlier work because of it would be the wrong
				// response to a correctable error.
				pending, pendErr := set.pendingChanges()
				if pendErr != nil {
					return errorResult(err.Error())
				}
				return structuredErrorResult(a.Tool+" was refused, so nothing was staged; the change set is still open",
					stageResult{
						ChangeSet:   set.ID,
						Call:        stagedCall{Tool: a.Tool, Arguments: a.Arguments, Output: err.Error()},
						Pending:     pending,
						StagedCount: len(set.Calls),
					})
			}

			pending, err := set.pendingChanges()
			if err != nil {
				return errorResult(err.Error())
			}

			return structuredResult(fmt.Sprintf("%s staged in %s; %d file(s) pending",
				a.Tool, set.ID, len(pending)), stageResult{
				ChangeSet:   set.ID,
				Call:        call,
				Pending:     pending,
				StagedCount: len(set.Calls),
			})
		},
	}
}

// ─── commit_change_set ───────────────────────────────────────────────────────

type changeSetIDArgs struct {
	ChangeSet string `json:"change_set"`
}

// commitResult is what commit_change_set answers.
type commitResult struct {
	ChangeSet   string `json:"change_set"`
	ProjectPath string `json:"project_path"`
	// Applied is the files written to the project.
	Applied []fileChange `json:"applied"`
	// Calls is what produced them, recorded in the project's history too.
	Calls []stagedCall `json:"calls"`
	// Warning carries a commit that succeeded with something worth saying —
	// currently only a history file that could not be written.
	Warning string `json:"warning,omitempty"`
}

func commitChangeSetTool(sets *changeSetStore) *tool {
	return &tool{
		name: "breeze_commit_change_set",
		description: "Apply every staged call to the project in one operation, and record them " +
			"in the project's change history. If any file cannot be written the whole " +
			"commit is rolled back, so the project is never left half-changed. The " +
			"change set is closed either way.",
		schema: objectSchema(map[string]any{
			"change_set": stringProp("The change set id to commit."),
		}, "change_set"),
		run: func(raw json.RawMessage) toolCallResult {
			var a changeSetIDArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			if a.ChangeSet == "" {
				return errorResult("change_set is required")
			}

			// take, not get: an id that has been committed cannot be committed
			// again, and the second attempt finding nothing is the right answer.
			set, err := sets.take(a.ChangeSet)
			if err != nil {
				return errorResult(err.Error())
			}

			applied, warning, err := set.commit()
			if err != nil {
				return errorResult(err.Error())
			}

			result := commitResult{
				ChangeSet:   set.ID,
				ProjectPath: set.ProjectPath,
				Applied:     applied,
				Calls:       set.Calls,
				Warning:     warning,
			}
			if result.Applied == nil {
				result.Applied = []fileChange{}
			}
			if result.Calls == nil {
				result.Calls = []stagedCall{}
			}

			if len(applied) == 0 {
				return structuredResult(fmt.Sprintf("%s committed with nothing to apply — no staged call changed a file", set.ID), result)
			}
			summary := fmt.Sprintf("%s applied %d file(s) to %s", set.ID, len(applied), set.ProjectPath)
			if warning != "" {
				summary += "\n" + warning
			}
			return structuredResult(summary, result)
		},
	}
}

// ─── discard_change_set ──────────────────────────────────────────────────────

// discardResult is what discard_change_set answers.
type discardResult struct {
	ChangeSet   string `json:"change_set"`
	ProjectPath string `json:"project_path"`
	// Discarded is what would have been applied, reported so the reply is a
	// record of what was thrown away rather than an acknowledgement.
	Discarded []fileChange `json:"discarded"`
	// FilesWritten is always 0. Discarding is the operation whose whole value is
	// that it changed nothing, so it says so.
	FilesWritten int `json:"files_written"`
}

func discardChangeSetTool(sets *changeSetStore) *tool {
	return &tool{
		name: "breeze_discard_change_set",
		description: "Throw away an open change set and delete its private copy. The project " +
			"is not touched. Discarding costs nothing, which is what makes staging " +
			"worth doing: finding out that a plan does not work is a deleted " +
			"temporary directory.",
		schema: objectSchema(map[string]any{
			"change_set": stringProp("The change set id to discard."),
		}, "change_set"),
		run: func(raw json.RawMessage) toolCallResult {
			var a changeSetIDArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}
			if a.ChangeSet == "" {
				return errorResult("change_set is required")
			}

			set, err := sets.take(a.ChangeSet)
			if err != nil {
				return errorResult(err.Error())
			}

			// Read the pending list before the sandbox goes away, so the reply
			// can say what was discarded.
			pending, pendErr := set.pendingChanges()
			set.discard()

			result := discardResult{
				ChangeSet:   set.ID,
				ProjectPath: set.ProjectPath,
				Discarded:   pending,
			}
			if result.Discarded == nil {
				result.Discarded = []fileChange{}
			}
			if pendErr != nil {
				return structuredResult(fmt.Sprintf("%s discarded; the project was not touched", set.ID), result)
			}
			return structuredResult(fmt.Sprintf("%s discarded, dropping %d pending file(s); the project was not touched",
				set.ID, len(pending)), result)
		},
	}
}

// ─── get_change_history ──────────────────────────────────────────────────────

type historyArgs struct {
	ProjectPath string `json:"project_path"`
	Limit       int    `json:"limit"`
}

// historyResult is what get_change_history answers.
type historyResult struct {
	ProjectPath string `json:"project_path"`
	// File is where the history is kept, so a caller can read or remove it
	// without guessing.
	File    string         `json:"file"`
	Entries []historyEntry `json:"entries"`
	Count   int            `json:"count"`
	Note    string         `json:"note,omitempty"`
}

func changeHistoryTool() *tool {
	return &tool{
		name: "breeze_get_change_history",
		description: "Read a project's record of committed generator calls, newest first: when " +
			"each ran, which tool it was, the arguments it was given, and the files " +
			"it changed. Use this to find out what produced a file, or what was " +
			"already tried.",
		schema: objectSchema(map[string]any{
			"project_path": stringProp("Project root. Defaults to the server's working directory."),
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Return at most this many of the most recent entries. Omit for all of them.",
			},
		}),
		run: func(raw json.RawMessage) toolCallResult {
			var a historyArgs
			if err := decodeArgs(raw, &a); err != nil {
				return errorResult("arguments: " + err.Error())
			}

			path, err := orWorkingDir(a.ProjectPath)
			if err != nil {
				return errorResult(err.Error())
			}
			entries, err := readHistory(path, a.Limit)
			if err != nil {
				return errorResult("the change history could not be read: " + err.Error())
			}

			result := historyResult{
				ProjectPath: path,
				File:        historyPath(path),
				Entries:     entries,
				Count:       len(entries),
			}
			if result.Entries == nil {
				result.Entries = []historyEntry{}
			}
			if len(entries) == 0 {
				// An empty history and a project that has never been committed
				// through a change set look identical on disk, so the reply
				// says which is the likely reading.
				result.Note = "no history has been recorded for this project; it is written when a change set is committed"
				return structuredResult("no recorded changes", result)
			}
			return structuredResult(fmt.Sprintf("%d recorded change(s), newest first", len(entries)), result)
		},
	}
}

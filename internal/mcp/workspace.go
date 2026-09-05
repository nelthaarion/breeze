package mcp

// workspace.go — running a generator somewhere it cannot do any harm.
//
// Several tools here have to answer "what would this do?" rather than doing it.
// The obvious implementation is a second copy of each generator that walks the
// same decisions and prints instead of writing, and it is the wrong one: the
// copy would be the thing that drifts. Which files a feature owns, whether a
// block may be replaced, what a checksum mismatch means — all of that lives in
// internal/generator and is worth exactly nothing if a preview reimplements it
// approximately.
//
// So a preview runs the real generator. It just runs it against a copy of the
// project in a temporary directory, snapshots the tree before and after, and
// reports the difference. Nothing in the answer is predicted; it is observed.
// A tool that previews cannot accidentally write to the project, because the
// project's path is never passed to a generator at all.
//
// Change sets are the same mechanism held open across several calls: the copy
// persists, each staged call runs against it, and commit is the one operation
// that copies files back. That is what makes a sequence atomic — a failure
// halfway through has touched nothing but a directory that is about to be
// deleted.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// changeKind is what happened to one file.
const (
	changeCreated  = "created"
	changeModified = "modified"
	changeDeleted  = "deleted"
)

// fileChange is one file's difference between two snapshots.
type fileChange struct {
	Path string `json:"path"`
	// Change is created, modified or deleted.
	Change string `json:"change"`
	// Bytes is the file's size afterwards, and is absent for a deletion.
	Bytes int64 `json:"bytes,omitempty"`
}

// fileState is what a snapshot records about one file.
//
// The digest is kept as well as the size because a generator that rewrites a
// block in place very often produces a file of exactly the same length — a
// re-run with one flag changed, for instance — and a size-only comparison would
// report no change at all.
type fileState struct {
	size   int64
	digest string
}

// snapshotSkip are directories a snapshot does not descend into.
//
// .git is excluded because a diff of an object database is noise, and vendor
// and node_modules because they are large, machine-owned, and never what a
// generator changed.
var snapshotSkip = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
}

// snapshotTree records every file under root, keyed by slash-separated path
// relative to root.
//
// Paths are normalised to forward slashes so a change list reads the same on
// every platform: a caller comparing "handlers\user.go" against "handlers/user.go"
// would otherwise see a spurious difference depending on where the server runs.
func snapshotTree(root string) (map[string]fileState, error) {
	out := map[string]fileState{}

	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		// A project that does not exist yet is an empty snapshot rather than an
		// error: that is exactly the "before" state of a scaffold.
		return out, nil
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if snapshotSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks and devices are not something a generator writes, and
			// hashing one would follow it out of the tree.
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		digest, err := fileDigest(path)
		if err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = fileState{size: info.Size(), digest: digest}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fileDigest is the SHA-256 of a file's contents, hex-encoded.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// diffSnapshots reports what changed between two snapshots, path-sorted.
func diffSnapshots(before, after map[string]fileState) []fileChange {
	var out []fileChange

	for path, now := range after {
		was, existed := before[path]
		switch {
		case !existed:
			out = append(out, fileChange{Path: path, Change: changeCreated, Bytes: now.size})
		case was.digest != now.digest:
			out = append(out, fileChange{Path: path, Change: changeModified, Bytes: now.size})
		}
	}
	for path := range before {
		if _, still := after[path]; !still {
			out = append(out, fileChange{Path: path, Change: changeDeleted})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// changedPaths is the paths of a change list, in order.
func changedPaths(changes []fileChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
}

// copyTree copies src into dst, creating dst.
//
// The same directories a snapshot skips are skipped here, for the same reasons
// plus one more: copying a .git directory to run a generator against would make
// every preview cost the size of the repository's history.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			if snapshotSkip[d.Name()] {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

// copyFile copies one file, preserving its mode and creating parents.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// offlineEnv are the environment settings a sandboxed generator run is given.
//
// `breeze new` finishes by running `go mod tidy`, which resolves the breeze
// dependency over the network. In a preview that is both pointless and slow —
// the answer is which files would be written, and go.sum's contents are not part
// of it — and on a machine with no network it would add several seconds of
// timeout to every call. GOPROXY=off makes the attempt fail immediately;
// runNew already treats a tidy failure as a warning on stderr, so the preview
// is unaffected.
var offlineEnv = map[string]string{
	"GOFLAGS": "-mod=mod",
	"GOPROXY": "off",
}

// withEnv runs fn with environment variables set, restoring them afterwards.
//
// Like the working directory, the environment is process-global — so this must
// only be called from inside the capture lock, which every caller here is.
func withEnv(vars map[string]string, fn func() error) error {
	type saved struct {
		value string
		set   bool
	}
	previous := make(map[string]saved, len(vars))

	for key, value := range vars {
		old, had := os.LookupEnv(key)
		previous[key] = saved{value: old, set: had}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	defer func() {
		for key, old := range previous {
			if old.set {
				_ = os.Setenv(key, old.value)
				continue
			}
			_ = os.Unsetenv(key)
		}
	}()

	return fn()
}

// sandbox is a temporary copy of a project that generators may write to.
type sandbox struct {
	// root is the temporary directory the copy lives in.
	root string
	// dirName is the project directory's name inside root, or "" when the
	// sandbox is the parent of a project that does not exist yet.
	dirName string
}

// projectDir is the directory a generator should run in.
func (s *sandbox) projectDir() string {
	if s.dirName == "" {
		return s.root
	}
	return filepath.Join(s.root, s.dirName)
}

// remove deletes the sandbox.
func (s *sandbox) remove() { _ = os.RemoveAll(s.root) }

// sandboxPrefix is the temporary-directory prefix every sandbox uses.
//
// One spelling, in one place, because two of them are how a cleanup routine ends up
// missing half the directories it was written to find.
const sandboxPrefix = "breeze-mcp-"

// newSandbox copies an existing project into a temporary directory.
//
// The copy keeps the project's own directory name, because a generator can read
// it: `breeze new` refuses a name that already exists, and a feature resolves
// paths relative to the project root.
//
// The source is confined even though the destination is a temporary directory. What
// is read here is the caller's path, and a sandbox of an arbitrary directory would
// copy its contents somewhere a change set then reports file by file. The temporary
// destination bounds what a sandbox can write, not what it can read.
func newSandbox(projectPath string) (*sandbox, error) {
	abs, err := resolvePath(projectPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot read the project at %s: %w", projectPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", projectPath)
	}

	root, err := os.MkdirTemp("", sandboxPrefix+"*")
	if err != nil {
		return nil, err
	}
	name := filepath.Base(abs)

	if err := copyTree(abs, filepath.Join(root, name)); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("copying %s into a sandbox: %w", projectPath, err)
	}
	return &sandbox{root: root, dirName: name}, nil
}

// newEmptySandbox creates a temporary directory to scaffold into.
func newEmptySandbox() (*sandbox, error) {
	root, err := os.MkdirTemp("", sandboxPrefix+"*")
	if err != nil {
		return nil, err
	}
	return &sandbox{root: root}, nil
}

// sandboxRun is the outcome of running one generator call in a sandbox.
type sandboxRun struct {
	// Output is what the generator printed, trimmed. It is kept for the record
	// a change set writes, not returned as a tool's result.
	Output string
	// Changes is the difference the call made to the sandbox.
	Changes []fileChange
	// Err is the generator's own error, if it refused.
	Err error
}

// runInSandbox runs fn inside dir with stdout captured and the working
// directory set, and reports what it changed under watch.
//
// watch is separate from dir because the two differ for a scaffold: `breeze new`
// runs in the parent and creates a directory beneath it, so the interesting
// tree is the parent while the working directory must not be the project.
func runInSandbox(dir, watch string, fn func() error) (sandboxRun, error) {
	before, err := snapshotTree(watch)
	if err != nil {
		return sandboxRun{}, err
	}

	var genErr error
	out, capErr := captureStdout(func() error {
		return runInDir(dir, func() error {
			return withEnv(offlineEnv, func() error {
				genErr = fn()
				// The generator's own refusal is carried out separately: it is
				// an outcome to report, not a failure of the mechanism.
				return nil
			})
		})
	})
	if capErr != nil {
		return sandboxRun{}, capErr
	}

	after, err := snapshotTree(watch)
	if err != nil {
		return sandboxRun{}, err
	}

	return sandboxRun{
		Output:  strings.TrimSpace(out),
		Changes: diffSnapshots(before, after),
		Err:     genErr,
	}, nil
}

// applyChanges copies the named paths from a sandbox into a target directory,
// and undoes the whole set if any one of them fails.
//
// This is the only place in the package that writes to a real project. It is
// small on purpose: everything upstream has already decided what the change is,
// so the one thing that can go wrong here is I/O, and the one thing that must
// not happen is a half-applied commit.
func applyChanges(from, to string, changes []fileChange) (err error) {
	type undo struct {
		path    string
		content []byte
		mode    os.FileMode
		existed bool
	}
	var history []undo

	defer func() {
		if err == nil {
			return
		}
		// Restore in reverse, so a path that was created and then modified
		// ends up back at absent rather than at its intermediate content.
		for i := len(history) - 1; i >= 0; i-- {
			u := history[i]
			if !u.existed {
				_ = os.Remove(u.path)
				continue
			}
			_ = os.WriteFile(u.path, u.content, u.mode)
		}
	}()

	for _, change := range changes {
		target := filepath.Join(to, filepath.FromSlash(change.Path))

		record := undo{path: target}
		if content, readErr := os.ReadFile(target); readErr == nil {
			record.existed = true
			record.content = content
			if info, statErr := os.Stat(target); statErr == nil {
				record.mode = info.Mode().Perm()
			} else {
				record.mode = 0o644
			}
		} else if !errors.Is(readErr, fs.ErrNotExist) {
			return readErr
		}
		history = append(history, record)

		switch change.Change {
		case changeDeleted:
			if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				return removeErr
			}
		default:
			source := filepath.Join(from, filepath.FromSlash(change.Path))
			if copyErr := copyFile(source, target); copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

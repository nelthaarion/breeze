package mcp

// confine.go — the one place a caller-supplied path becomes a path this server
// will touch.
//
// # The hole this closes
//
// Every filesystem tool in this package took a path and used it. verifyRoot,
// orWorkingDir and newSandbox each called filepath.Abs and stopped there, so
// `{"path": "/etc"}` or `{"path": "C:\\Users\\someone\\else"}` was simply honoured.
//
// That is worse than it sounds, because two of those tools do not merely read.
// breeze_verify_project and breeze_run_benchmarks run `go test`, which compiles and
// executes whatever is in the directory they are pointed at. An agent that could
// name any path could therefore run any code already on the host, under this
// server's identity, and get its stdout back as a tool result. No injection was
// needed — the feature did it on request.
//
// # Why the check is centralised and not per call site
//
// There were seven entry points and there will be more. A per-call-site check is a
// list that has to stay complete, and the failure mode of an incomplete list is
// silent: the tool that was missed works exactly as before. So the resolution and
// the confinement are the same function, and a tool cannot obtain a usable path
// without going through it.
//
// It is package-level state rather than a field on Server for the same reason.
// runGenerator, verifyRoot and orWorkingDir are package functions reached from
// tools, sandboxes, change sets and the planner; threading a receiver through all
// of them would mean the compiler stops complaining as soon as one path passes nil,
// and nil would mean unconfined.
//
// # Symlinks
//
// Both the root and the candidate are resolved with filepath.EvalSymlinks before
// comparison. A prefix test on unresolved paths is not a containment test: a
// symlink inside the workspace pointing at / passes it trivially, and that is the
// standard way out of a directory jail.
//
// A path that does not exist yet cannot be resolved, so its nearest existing
// ancestor is resolved instead and the remainder appended. That case is normal
// rather than exceptional — `breeze new` names a directory precisely because it is
// not there.

// Symlinks are the resolvable case. A Windows directory *junction* is not:
// filepath.EvalSymlinks returns a junction's own path unchanged rather than its
// target, so the "resolved" path is a lie and a prefix test on it passes. Those are
// refused by name — see unresolvedLinkBelow.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// confinement is the active policy, or nil for unconfined.
//
// atomic because it is written once during startup, on whatever goroutine built
// the server, and read by tool calls on net/http goroutines.
var confinement atomic.Pointer[confinePolicy]

// confinementDecided records that something chose a confinement posture on purpose: a
// policy was installed, or Unconfine was called. It exists to tell "nobody has decided
// yet" from "somebody decided to run unconfined", which a nil policy cannot — and that
// distinction is what lets an embedded endpoint install a safe default without
// overriding an explicit choice made earlier in the same process.
var confinementDecided atomic.Bool

// confinePolicy is the set of directories this process may touch.
type confinePolicy struct {
	// roots are absolute, symlink-resolved directory paths.
	roots []string
	// declared are the roots as the caller wrote them, for error messages. A
	// message naming a resolved path the operator never typed is a message they
	// cannot act on.
	declared []string
}

// ConfineToWorkspace restricts every filesystem tool to the given roots.
//
// Called by each entrypoint that starts a real server. Passing no roots is an
// error rather than a way to disable confinement: "confine to nothing" and "do not
// confine" are opposite intentions and must not share a spelling.
//
// The roots must exist. A root that does not is almost always a typo, and the
// consequence of accepting it silently is a server that refuses every path with a
// message about confinement rather than about the missing directory.
func ConfineToWorkspace(roots ...string) error {
	if len(roots) == 0 {
		return errors.New("mcp: ConfineToWorkspace needs at least one root; " +
			"call Unconfine to run without confinement")
	}

	policy := &confinePolicy{}
	for _, raw := range roots {
		root := strings.TrimSpace(raw)
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return fmt.Errorf("mcp: workspace root %q cannot be resolved: %w", raw, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("mcp: workspace root %q cannot be read: %w", raw, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("mcp: workspace root %q is a file, not a directory", raw)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return fmt.Errorf("mcp: workspace root %q cannot be resolved: %w", raw, err)
		}
		// EvalSymlinks reports success for a Windows directory junction while
		// returning the junction's own path, so "resolved" can still be a
		// redirection. A root like that is refused rather than stored: every later
		// containment decision would be made against a path that does not say where
		// the filesystem goes, and the failure would look like tools refusing
		// legitimate directories.
		if link, err := os.Lstat(resolved); err == nil &&
			link.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("mcp: workspace root %q is a link this platform cannot resolve "+
				"(a Windows directory junction). Confinement compares resolved paths, and a root "+
				"whose target cannot be established would refuse the very directories it is "+
				"meant to permit. Name the real directory instead", raw)
		}
		policy.roots = append(policy.roots, filepath.Clean(resolved))
		policy.declared = append(policy.declared, filepath.Clean(abs))
	}
	if len(policy.roots) == 0 {
		return errors.New("mcp: ConfineToWorkspace was given only blank roots")
	}

	confinement.Store(policy)
	confinementDecided.Store(true)
	return nil
}

// Unconfine removes confinement, allowing any path.
//
// Exported and named plainly because it must be searchable. A deployment that has
// called this has given whoever holds its token the ability to run `go test` in any
// directory on the host, and the only defensible reason to do so is a sandbox that
// is already the boundary — a container whose whole filesystem is disposable.
func Unconfine() {
	confinementDecided.Store(true)
	confinement.Store(nil)
}

// confineEmbedded settles the confinement of an embedded generator-mode endpoint.
//
// Every standalone entrypoint installs a policy before it serves anything. An embedded
// endpoint has no entrypoint of its own — the host application starts it — and the
// installer is not reachable from outside this module, so without this an embed in
// ModeGenerator served breeze_verify_project, breeze_new and the rest against any path
// on the host: the one deployment of the filesystem tools with no jail at all, and the
// one reached by a network listener.
//
// With roots, the endpoint is confined to them. Without, and with no decision made yet,
// it is confined to the working directory — the same default the standalone binary uses.
// A posture already chosen (an installed policy, or an explicit Unconfine) is left
// alone, so embedding never silently widens or replaces what the process decided.
func confineEmbedded(roots []string) error {
	if len(roots) > 0 {
		return ConfineToWorkspace(roots...)
	}
	if confinementDecided.Load() {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("mcp: an embedded generator-mode endpoint must be confined to a workspace, "+
			"and the working directory cannot be determined: %w. Set InProcessConfig.Workspace", err)
	}
	return ConfineToWorkspace(wd)
}

// WorkspaceRoots reports the active roots as declared, or nil when unconfined.
//
// Used by the startup banner and by the capability report, so an operator can see
// what the server will accept without inferring it from a refusal.
func WorkspaceRoots() []string {
	policy := confinement.Load()
	if policy == nil {
		return nil
	}
	return append([]string(nil), policy.declared...)
}

// errOutsideWorkspace is returned for a path outside every root.
//
// A distinct type so a caller can test for it — the adversarial tests do — and so
// the message can name the roots without every call site rebuilding the sentence.
type errOutsideWorkspace struct {
	requested string
	roots     []string
}

func (e *errOutsideWorkspace) Error() string {
	return fmt.Sprintf("%s is outside this server's workspace. Permitted root(s): %s. "+
		"Filesystem tools are confined to the configured workspace, so a path that escapes it "+
		"is refused rather than resolved", e.requested, strings.Join(e.roots, ", "))
}

// resolvePath turns a caller-supplied path into an absolute path inside the
// workspace, or refuses it.
//
// This is the choke point. Every tool that touches the filesystem calls it, directly
// or through verifyRoot / orWorkingDir / newSandbox.
//
// An empty path means the working directory, which is the documented default for
// every tool's path argument. That default is confined too: a server started outside
// its own workspace would otherwise operate on wherever it happened to be launched.
func resolvePath(path string) (string, error) {
	candidate := strings.TrimSpace(path)
	if candidate == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("mcp: cannot determine the working directory: %w", err)
		}
		candidate = wd
	}

	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("%s cannot be resolved to a path: %w", path, err)
	}
	abs = filepath.Clean(abs)

	policy := confinement.Load()
	if policy == nil {
		return abs, nil
	}

	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", err
	}
	for _, root := range policy.roots {
		if !within(root, resolved) {
			continue
		}
		// Inside a root by the resolved path — but resolution is only trustworthy
		// if every component actually resolved. A Windows junction does not, so
		// the containment decision above was made on a path that does not say
		// where reads and writes will land.
		if link, err := unresolvedLinkBelow(root, resolved); err != nil {
			return "", err
		} else if link != "" {
			return "", &errUnresolvedLink{requested: abs, link: link}
		}
		// The unresolved absolute path is returned, not the symlink-resolved
		// one. Resolution was for the containment decision; handing back a
		// rewritten path would make error messages and change lists name a
		// directory the caller never mentioned.
		return abs, nil
	}
	return "", &errOutsideWorkspace{requested: abs, roots: policy.declared}
}

// errUnresolvedLink is returned for a path traversing a link that could not be
// resolved.
//
// Its own type, like errOutsideWorkspace, because it is a different answer: the path
// looked contained, and the reason it is refused is that the containment could not be
// established rather than that it failed.
type errUnresolvedLink struct {
	requested string
	link      string
}

func (e *errUnresolvedLink) Error() string {
	return fmt.Sprintf("%s passes through %s, which is a link this platform cannot resolve "+
		"(a Windows directory junction). Where it actually points cannot be established, so it "+
		"cannot be shown to be inside the workspace — a junction is how a path that looks "+
		"contained reaches outside. Name the real directory instead", e.requested, e.link)
}

// unresolvedLinkBelow reports the first component at or below root that is a link
// EvalSymlinks did not resolve, or "" when there is none.
//
// # Why this is needed at all
//
// filepath.EvalSymlinks handles symlinks on every platform. It does not handle a
// Windows directory junction: given one, it returns the junction's own path rather
// than the target. So resolveExisting's output can still contain a component that
// redirects elsewhere, and `within` would then be comparing a path that does not
// describe where the filesystem will actually go.
//
// # Why it stops at root
//
// The workspace root itself was resolved by ConfineToWorkspace, and components *above*
// the root are the operator's own configuration rather than caller input. Walking
// higher would refuse a whole class of legitimate deployments — a workspace under a
// junctioned drive layout — for a redirection the operator chose. Only the part of the
// path the caller contributed is checked.
func unresolvedLinkBelow(root, candidate string) (string, error) {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", nil
	}
	if rel == "." {
		return "", nil
	}

	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)

		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				// The rest does not exist yet, so nothing below here can be a
				// link. This is the ordinary `breeze new` case.
				return "", nil
			}
			return "", fmt.Errorf("%s cannot be examined: %w", current, err)
		}
		// ModeSymlink would already have been followed by EvalSymlinks, so its
		// presence here means resolution did not happen. ModeIrregular is what a
		// junction reports.
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return current, nil
		}
	}
	return "", nil
}

// within reports whether candidate is root or lies beneath it.
//
// filepath.Rel rather than strings.HasPrefix: a prefix test says /srv/appdata is
// inside /srv/app, which it is not. Rel produces a path starting with ".." for
// anything outside, and "." for the root itself.
func within(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		// Different volumes on Windows, which is genuinely outside.
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves symlinks as far up the path as exists.
//
// A path that is about to be created cannot be resolved, and refusing it would break
// `breeze new` and every tool that writes a file — so its nearest existing ancestor
// is resolved instead and the not-yet-existing remainder appended. That is the
// correct containment test either way: what matters is where the parent really is,
// because that is where the new entry will land.
func resolveExisting(abs string) (string, error) {
	remainder := ""
	current := abs

	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Clean(filepath.Join(resolved, remainder)), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("%s cannot be resolved: %w", abs, err)
		}

		parent := filepath.Dir(current)
		if parent == current {
			// Walked to the volume root without finding anything that exists.
			// Nothing can contain this, so it is outside by definition.
			return filepath.Clean(abs), nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// ─── what happens inside a workspace ─────────────────────────────────────────
//
// resolvePath answers "may a tool be pointed at this directory?". It cannot answer "can
// writing *inside* that directory escape it?", and a project is not necessarily trusted:
// a cloned repository can contain `cmd -> /home/user/.ssh` or `llms.txt -> ~/.bashrc`, and
// a generator that writes cmd/main.go or a tool that rewrites llms.txt writes straight
// through it. The two functions below close that, one for the writes this package makes
// itself and one for the writes a generator makes on its own.

// errWriteEscapes is returned for a write that a symbolic link would carry out of its
// project. Its own type, like errOutsideWorkspace, so a test can assert on the refusal.
type errWriteEscapes struct {
	target string
	via    string
}

func (e *errWriteEscapes) Error() string {
	return fmt.Sprintf("refusing to write %s: it would land outside the project through a symbolic link (%s). "+
		"Remove or retarget the link, or work on a project whose directories are plain directories",
		e.target, e.via)
}

// ensureWriteStaysInside refuses a write to target — a path meant to lie under root — that
// a symbolic link would carry out of root.
//
// Two things are checked. The nearest existing ancestor of the target is resolved, and
// the result must lie inside the resolved root: that catches an intermediate directory
// that is a link, which os.Lstat on the final path does not (Lstat follows every
// component but the last). And a link at the target itself is refused outright, because
// opening it with O_TRUNC would overwrite whatever it points at.
//
// The check is made against the project root, not the workspace policy, and applies
// whether or not a workspace is configured: a write leaving its own project is wrong in
// both cases.
func ensureWriteStaysInside(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("%s cannot be resolved to a path: %w", root, err)
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("the project root %s cannot be resolved: %w", root, err)
	}
	rootResolved = filepath.Clean(rootResolved)

	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("%s cannot be resolved to a path: %w", target, err)
	}
	targetAbs = filepath.Clean(targetAbs)

	parent, err := resolveExisting(filepath.Dir(targetAbs))
	if err != nil {
		return err
	}
	if !within(rootResolved, parent) {
		return &errWriteEscapes{target: targetAbs, via: "its directory resolves to " + parent}
	}
	// Resolution is only trustworthy if every component resolved; a Windows junction
	// reports success while still being a redirection.
	if link, err := unresolvedLinkBelow(rootResolved, parent); err != nil {
		return err
	} else if link != "" {
		return &errWriteEscapes{target: targetAbs, via: "a link at " + link + " could not be resolved"}
	}

	if info, err := os.Lstat(targetAbs); err == nil && info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return &errWriteEscapes{target: targetAbs, via: "it is itself a link"}
	}
	return nil
}

// firstEscapingLink walks a project and returns the first symbolic link whose target
// lies outside both the project and the workspace, or "" when there is none.
//
// It exists for the generators, which run in this process and write relative to the
// project with ordinary file calls; there is no seam at which to check each write. So the
// check is made once, up front: if no link in the tree leads out, no generator write can.
// WalkDir does not follow links, so the walk itself cannot be led astray, and .git,
// node_modules and vendor are skipped because no generator writes into them and they are
// where large trees live.
//
// With no workspace configured there is no boundary to enforce and nothing is walked.
// A link whose target cannot be resolved is treated as escaping: refusing is the safe way
// to be wrong here.
func firstEscapingLink(root string) (link, dest string, err error) {
	policy := confinement.Load()
	if policy == nil {
		return "", "", nil
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	rootResolved = filepath.Clean(rootResolved)

	found := errors.New("escaping link found")
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// An entry that cannot be read is not a link this walk can judge; the
			// generator will fail on it for its own, clearer reason.
			return nil
		}
		if d.IsDir() {
			if path != root && snapshotSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		resolved, resolveErr := resolveExisting(filepath.Clean(target))
		if resolveErr != nil {
			link, dest = path, target
			return found
		}
		if within(rootResolved, resolved) {
			return nil
		}
		for _, allowed := range policy.roots {
			if within(allowed, resolved) {
				return nil
			}
		}
		link, dest = path, resolved
		return found
	})
	if errors.Is(walkErr, found) {
		return link, dest, nil
	}
	return "", "", walkErr
}

// errEscapingLink is returned when a project contains a symbolic link that leads out of
// the workspace and a generator was about to run in it.
type errEscapingLink struct {
	link string
	dest string
}

func (e *errEscapingLink) Error() string {
	return fmt.Sprintf("%s is a symbolic link to %s, which is outside this server's workspace. "+
		"Generators write through a link as if it were a directory, so a project containing one "+
		"could be made to write outside the workspace. Remove the link or point it inside the project",
		e.link, e.dest)
}

// preflightWrites is what runGenerator checks before a generator runs in dir.
//
// creates are the names the generator will create directly under dir; a link already
// standing at one of them is refused. When dir is itself a project (it has a go.mod) its
// whole tree is searched for a link that leads out of the workspace. A directory that is
// not a project — the parent breeze_new is about to create one in — is not walked: it can
// be anything, including a very large tree, and the generator writes only under the name
// it was given, which creates already covers.
func preflightWrites(dir string, creates []string) error {
	for _, name := range creates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if err := ensureWriteStaysInside(dir, filepath.Join(dir, name)); err != nil {
			return err
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil
	}
	link, dest, err := firstEscapingLink(dir)
	if err != nil {
		return err
	}
	if link != "" {
		return &errEscapingLink{link: link, dest: dest}
	}
	return nil
}

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
	return nil
}

// Unconfine removes confinement, allowing any path.
//
// Exported and named plainly because it must be searchable. A deployment that has
// called this has given whoever holds its token the ability to run `go test` in any
// directory on the host, and the only defensible reason to do so is a sandbox that
// is already the boundary — a container whose whole filesystem is disposable.
func Unconfine() { confinement.Store(nil) }

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

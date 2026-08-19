package video

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// resolved is a request path that has been proven safe to open.
//
// The type exists so that "a string that came from the network" and "a
// filesystem path that has been validated" are not the same type. A
// function that takes a resolved cannot be handed a raw request path by
// mistake.
type resolved struct {
	// Name is the cleaned, slash-separated path relative to the mount
	// root. It is what appears in logs, events and signatures.
	Name string

	// Path is the absolute filesystem path to open.
	Path string

	// Size is the file's length in bytes.
	Size int64

	// ModTime is the file's modification time, used for Last-Modified
	// and the ETag.
	ModTime int64
}

// resolve is [mount.normalize] followed by [mount.stat]: the whole
// pipeline in one call, for callers with no authorisation step in between.
func (m *mount) resolve(raw string) (resolved, error) {
	name, err := m.normalize(raw)
	if err != nil {
		return resolved{}, err
	}
	return m.stat(name)
}

// normalize turns the raw wildcard capture from a route into a cleaned,
// root-relative name, without touching the filesystem.
//
// Keeping this half pure matters twice over. It lets signature
// verification and authorisation run against the final name before a
// single syscall is spent, so an unauthenticated flood costs no disk I/O;
// and it makes every traversal rule testable without a temp directory.
//
// The order of the checks is the security property. Decoding happens
// first, so that %2e%2e%2f cannot smuggle a traversal past a cleaner that
// only understands literal dots. Cleaning happens before any filesystem
// call, so a traversal is rejected without touching the disk.
func (m *mount) normalize(raw string) (string, error) {
	// The router hands over the wildcard verbatim, which may still be
	// percent-encoded. A path that will not decode is not a path we can
	// reason about, so it is refused rather than used as-is.
	name, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("%w: undecodable path %q", ErrNotFound, raw)
	}

	// A NUL byte truncates the path in any C-based syscall layer, so
	// "safe.mp4\x00../../etc/passwd" can pass a Go-side suffix check and
	// still open something else. Refuse outright.
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: NUL in path", ErrForbiddenPath)
	}

	// On Windows a backslash is a path separator, so "..\..\secret"
	// traverses there but survives a slash-only cleaner. Rejecting
	// backslashes on every platform keeps behaviour identical across
	// operating systems, which matters because tests usually run on one
	// and production on another.
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("%w: backslash in path", ErrForbiddenPath)
	}

	// Reject any dot-dot segment outright, before cleaning.
	//
	// Cleaning first would be safe — path.Clean("/"+"../x") yields "/x",
	// so a traversal collapses to something inside the root rather than
	// above it, which is what http.Dir relies on. But collapsing quietly
	// rewrites a hostile request into a legitimate-looking one: an
	// operator reading the logs, and the Authorize callback, would both
	// see a lookup of "x" and never learn that a traversal was attempted.
	// No real client emits ".." in a media URL, so treating it as an
	// attack and failing closed loses nothing and keeps the attempt
	// visible in OnError and in the event stream.
	for _, s := range strings.Split(name, "/") {
		if s == ".." {
			return "", fmt.Errorf("%w: %q contains a dot-dot segment", ErrForbiddenPath, name)
		}
	}

	// Clean the remainder. With ".." already refused this only collapses
	// "." segments, duplicate slashes and a trailing slash. path.Clean is
	// the slash-only cleaner, which is correct for URLs regardless of the
	// host OS; conversion to a native path happens later.
	trimmed := strings.TrimPrefix(path.Clean("/"+name), "/")
	if trimmed == "" || trimmed == "." {
		// The mount prefix itself was requested. There is no index file
		// concept here, and directory listing is not offered.
		return "", fmt.Errorf("%w: empty path", ErrNotFound)
	}

	if !m.allowHid {
		for _, s := range strings.Split(trimmed, "/") {
			if strings.HasPrefix(s, ".") {
				// Reported as not-found, not as forbidden: a client must
				// not be able to confirm that .env exists by observing a
				// different status than for a name that does not.
				return "", fmt.Errorf("%w: hidden segment %q", ErrNotFound, s)
			}
		}
	}

	// The extension gate runs before any filesystem access, so probing
	// for /videos/../../etc/passwd costs no syscall at all.
	if !m.allowsExt(trimmed) {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedType, filepath.Ext(trimmed))
	}

	return trimmed, nil
}

// stat proves that a normalized name refers to a regular file inside the
// mount root, and captures the metadata the response head needs.
//
// name must already have passed [mount.normalize]; this function assumes
// the traversal and extension rules have been applied and concerns itself
// only with what the filesystem says.
func (m *mount) stat(name string) (resolved, error) {
	full := filepath.Join(m.root, filepath.FromSlash(name))

	// Lstat, not Stat: Stat follows links and would report the target's
	// type, hiding the fact that a link is involved at all.
	li, err := os.Lstat(full)
	if err != nil {
		return resolved{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if li.Mode()&os.ModeSymlink != 0 && !m.followLink {
		return resolved{}, fmt.Errorf("%w: %q is a symlink", ErrNotFound, name)
	}

	// Prove containment against the real path. Cleaning already stopped
	// literal traversal; this stops the subtler case where every segment
	// is innocent but one of them is a link pointing out of the root.
	// EvalSymlinks resolves intermediate directory links too, which a
	// naive check on the final component would miss.
	realPath, err := filepath.EvalSymlinks(full)
	if err != nil {
		return resolved{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if !contained(m.root, realPath) {
		return resolved{}, fmt.Errorf("%w: %q resolves outside root", ErrForbiddenPath, name)
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return resolved{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if info.IsDir() {
		return resolved{}, fmt.Errorf("%w: %q", ErrDirectory, name)
	}
	// Devices, sockets and FIFOs have no length and would block or stream
	// forever on read. Only regular files are servable.
	if !info.Mode().IsRegular() {
		return resolved{}, fmt.Errorf("%w: %q is not a regular file", ErrNotFound, name)
	}

	return resolved{
		Name:    name,
		Path:    realPath,
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Unix(),
	}, nil
}

// contained reports whether target is root itself or lies beneath it.
//
// The separator is appended before comparing so that a sibling directory
// whose name merely starts with the root's name — /srv/media-private next
// to /srv/media — is not mistaken for a child.
func contained(root, target string) bool {
	if target == root {
		return true
	}
	if !strings.HasSuffix(root, string(os.PathSeparator)) {
		root += string(os.PathSeparator)
	}
	return strings.HasPrefix(target, root)
}

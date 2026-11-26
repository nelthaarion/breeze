package video

import (
	"errors"
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

	// Size is the file's length in bytes.
	Size int64

	// ModTime is the file's modification time, used for Last-Modified
	// and the ETag.
	ModTime int64

	// file is the descriptor opened while resolving the name. Keeping it
	// here means the stream never reopens a path after the containment
	// check.
	file *os.File
}

func (r resolved) close() {
	if r.file != nil {
		_ = r.file.Close()
	}
}

// normalize turns the raw wildcard capture from a route into a cleaned,
// root-relative name, without touching the filesystem.
//
// Keeping this half pure matters twice over. It lets signature
// verification and authorisation run against the final name before a
// single syscall is spent, so an unauthenticated flood costs no disk I/O;
// and it makes every traversal rule testable without a temp directory.
//
// decodePath performs only URL decoding and the byte-limit check. Opaque
// tokens use it too, so a forged oversized token is rejected before crypto
// work and a valid token cannot bypass the mount's path budget.
func (m *mount) decodePath(raw string) (string, error) {
	// Percent-encoding can make a request path up to three times longer
	// than its decoded form. The division avoids integer overflow for a
	// caller-supplied MaxPathBytes near the platform's int limit.
	if m.maxPath > 0 && len(raw)/3 > m.maxPath {
		return "", fmt.Errorf("%w: path exceeds %d bytes", ErrPathTooLong, m.maxPath)
	}
	name, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("%w: undecodable path %q", ErrNotFound, raw)
	}
	if m.maxPath > 0 && len(name) > m.maxPath {
		return "", fmt.Errorf("%w: path exceeds %d bytes", ErrPathTooLong, m.maxPath)
	}
	return name, nil
}

// The order of the checks is the security property. Decoding happens
// first, so that %2e%2e%2f cannot smuggle a traversal past a cleaner that
// only understands literal dots. Cleaning happens before any filesystem
// call, so a traversal is rejected without touching the disk.
func (m *mount) normalize(raw string) (string, error) {
	// The router hands over the wildcard verbatim, which may still be
	// percent-encoded. A path that will not decode is not a path we can
	// reason about, so it is refused rather than used as-is.
	name, err := m.decodePath(raw)
	if err != nil {
		return "", err
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
	// A drive or UNC volume is another absolute-path spelling on Windows.
	// It is not enough to prepend a slash and clean it: filepath.Join can
	// discard the mount root when it sees a volume-qualified second path.
	if filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("%w: volume-qualified path", ErrForbiddenPath)
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
// only with what the filesystem says. On Linux, openResolved performs the
// containment proof and the open in one openat2 operation anchored at the
// mount's root descriptor. That is intentionally the first filesystem action:
// a root directory can be renamed after the mount is created, and its path is
// no longer a reliable authority once that happens.
func (m *mount) stat(name string) (resolved, error) {
	full := filepath.Join(m.root, filepath.FromSlash(name))

	f, err := openResolved(m, name, full)
	if err != nil {
		return resolved{}, m.resolveOpenError(name, full, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return resolved{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if info.IsDir() {
		_ = f.Close()
		return resolved{}, fmt.Errorf("%w: %q", ErrDirectory, name)
	}
	// Devices, sockets and FIFOs have no length and would block or stream
	// forever on read. Only regular files are servable.
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return resolved{}, fmt.Errorf("%w: %q is not a regular file", ErrNotFound, name)
	}

	return resolved{
		Name:    name,
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Unix(),
		file:    f,
	}, nil
}

// checkedRealPath is the portable, path-based resolver used when the platform
// has no descriptor-relative containment primitive (and by the Linux fallback
// on kernels without openat2). It is deliberately kept separate from stat so
// the Linux openat2 success path never consults the mount path before opening.
func (m *mount) checkedRealPath(name, full string) (string, error) {
	// Lstat every component when links are disabled. Checking only the final
	// component would let an innocent-looking "alias/clip.mp4" traverse an
	// intermediate symlink; openat2's NO_SYMLINKS policy rejects that case on
	// Linux, so the portable fallback must match it.
	if !m.followLink {
		current := m.root
		for _, segment := range strings.Split(name, "/") {
			current = filepath.Join(current, filepath.FromSlash(segment))
			li, err := os.Lstat(current)
			if err != nil {
				return "", fmt.Errorf("%w: %q", ErrNotFound, name)
			}
			if li.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("%w: %q is a symlink", ErrNotFound, name)
			}
		}
	}

	// Prove containment against the real path. Cleaning already stopped
	// literal traversal; this stops the subtler case where every segment is
	// innocent but one of them is a link pointing out of the root.
	// EvalSymlinks resolves intermediate directory links too, which a
	// naive check on the final component would miss.
	realPath, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if !contained(m.root, realPath) {
		return "", fmt.Errorf("%w: %q resolves outside root", ErrForbiddenPath, name)
	}
	return realPath, nil
}

// resolveOpenError preserves the useful internal distinction for an escaping
// symlink while keeping all other failed opens indistinguishable from a miss.
// The distinction is for diagnostics only; both are 404 on the wire.
func (m *mount) resolveOpenError(name, full string, openErr error) error {
	if errors.Is(openErr, ErrForbiddenPath) || errors.Is(openErr, ErrDirectory) {
		return openErr
	}
	if m.followLink {
		// openat2 with RESOLVE_IN_ROOT intentionally reports an outside
		// symlink as ENOENT, because it must not disclose the target. Re-run
		// the path-only check only to classify the internal diagnostic; no
		// descriptor is returned from this branch, so it cannot create a
		// serving race.
		if _, checkErr := m.checkedRealPath(name, full); errors.Is(checkErr, ErrForbiddenPath) {
			return checkErr
		}
	}
	return fmt.Errorf("%w: %q", ErrNotFound, name)
}

// contained reports whether target is root itself or lies beneath it.
// filepath.Rel keeps the check correct for separators and volume names on
// Windows; a textual prefix check is vulnerable to siblings such as
// /srv/media-private and to drive-qualified paths.
func contained(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

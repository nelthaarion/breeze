//go:build !linux

package video

import "os"

// Non-Linux platforms do not expose Linux's openat2 resolver. The resolver
// still performs the absolute-path, symlink and containment checks before this
// open, and stat consumes the descriptor returned here rather than reopening a
// path after it has been checked.
func openRootDir(string) (*os.File, error) { return nil, nil }

func openResolved(m *mount, name, full string) (*os.File, error) {
	realPath, err := m.checkedRealPath(name, full)
	if err != nil {
		return nil, err
	}
	return openCheckedRealPath(realPath)
}

// openCheckedRealPath rejects non-regular targets before opening them. This
// matters on platforms without openat2: opening a FIFO read-only can block
// forever, while the media server should classify it as an unavailable file.
// The path-based fallback cannot make the check and open atomic on every
// platform; it does, however, avoid blocking for the ordinary no-race case and
// still returns a descriptor that the caller verifies before streaming.
func openCheckedRealPath(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, ErrDirectory
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	return os.Open(path)
}

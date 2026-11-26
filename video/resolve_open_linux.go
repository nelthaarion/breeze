//go:build linux

package video

import (
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// openRootDir keeps a directory descriptor for the lifetime of a mount. A
// descriptor is a stable object even if the path is renamed later, and is the
// anchor openat2 needs for a beneath-root lookup.
func openRootDir(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		// O_PATH is available on supported Linux kernels, but retaining the
		// ordinary fallback keeps a mount usable on older kernels. The actual
		// request open still uses openat2 when the kernel provides it.
		return os.Open(name)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// openResolved opens name relative to the mount root. On Linux the normal
// path uses openat2 so the kernel checks the entire lookup and the returned
// descriptor is the same object whose metadata the caller inspects. That
// removes the stat-then-open replacement window in the streaming path.
func openResolved(m *mount, name, full string) (*os.File, error) {
	if m.rootDir == nil {
		realPath, err := m.checkedRealPath(name, full)
		if err != nil {
			return nil, err
		}
		return openCheckedRealPath(realPath)
	}

	how := &unix.OpenHow{
		// O_NONBLOCK is immaterial for regular files, but prevents a
		// FIFO or device that appears under the root from blocking the
		// request before stat can reject it as non-regular.
		Flags: uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK),
	}
	if m.followLink {
		// IN_ROOT permits symlinks that stay inside the mount while making
		// the dirfd an unescapable root for absolute links and .. segments.
		// Magic links (for example /proc/self/fd links) are a separate
		// kernel feature and must never be followed by a media server.
		how.Resolve = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS
	} else {
		how.Resolve = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS
	}

	rootDir := m.rootDir
	fd, err := unix.Openat2(int(rootDir.Fd()), filepath.ToSlash(name), how)
	runtime.KeepAlive(rootDir)
	if err == nil {
		return os.NewFile(uintptr(fd), full), nil
	}
	if err == unix.ENOSYS {
		// Linux kernels predating openat2 cannot provide the atomic resolver.
		// Use the portable checked-path fallback rather than guessing that
		// EINVAL means the policy was unsupported: accepting that error would
		// turn a kernel policy failure into an unchecked path open.
		realPath, checkErr := m.checkedRealPath(name, full)
		if checkErr != nil {
			return nil, checkErr
		}
		return openCheckedRealPath(realPath)
	}
	return nil, err
}

// openCheckedRealPath is the path-based Linux fallback used when openat2 is
// unavailable. The metadata check avoids blocking on an ordinary FIFO in the
// common case; O_NONBLOCK also closes the replacement window for special files
// that might appear between the check and the open.
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
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

//go:build !windows

package kubehz

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// openNonblock opens file for reading without blocking: a FIFO opens at once
// instead of waiting for a writer, so the caller can check the open file.
func openNonblock(file string) (*os.File, error) {
	return os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// syncUnsupported reports a sync error that means the filesystem has no fsync
// (EINVAL, ENOTSUP). The data is written; only the flush is not available.
func syncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

// ownedByUs reports whether the entry belongs to this process's effective
// uid (bash `-O`, the twin's test).
func ownedByUs(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// fileOwner is the uid that owns the entry (false when the platform does
// not say).
func fileOwner(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// singleLink reports whether the entry has exactly one name (no hard
// link). It is false when the platform does not say, so a check that
// needs one name fails closed there.
func singleLink(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}

// overflowUID is the uid that an owner without a mapping shows in a user
// namespace: /proc/sys/kernel/overflowuid, else 65534 (the bash twin reads
// the same file).
func (c *Context) overflowUID() int {
	if c.OverflowUID != nil {
		return c.OverflowUID()
	}
	return readOverflowUID("/proc/sys/kernel/overflowuid")
}

// readOverflowUID reads the uid in file, else gives 65534 (the file
// cannot be read or does not hold a uid).
func readOverflowUID(file string) int {
	if b, err := os.ReadFile(file); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n >= 0 {
			return n
		}
	}
	return 65534
}

// errNotPrivate: the file is a link, not a regular file, not ours, or
// readable by others.
var errNotPrivate = errors.New("not a private file")

// errNotPrivateDir: the fresh directory of a join file is not a directory,
// not ours, or not 0700 after the chmod (the bash twin's text).
var errNotPrivateDir = errors.New("not a private directory (mode 0700)")

// readPrivateFile reads a regular file of ours that nobody else can read.
// O_NOFOLLOW refuses a link at open, and the checks run on the open file
// (fstat), so nothing can swap the file between the check and the read.
func readPrivateFile(file string) ([]byte, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || !ownedByUs(fi) {
		return nil, errNotPrivate
	}
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

//go:build !windows

package kubehz

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// ownedByUs reports whether the entry belongs to this process's effective
// uid (bash `-O`, the twin's test).
func ownedByUs(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// errNotPrivate: the file is a link, not a regular file, not ours, or
// readable by others.
var errNotPrivate = errors.New("not a private file")

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

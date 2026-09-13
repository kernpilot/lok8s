//go:build !windows

package assets

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByUs reports whether the entry belongs to this process's uid.
func ownedByUs(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

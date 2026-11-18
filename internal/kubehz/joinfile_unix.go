//go:build !windows

package kubehz

import (
	"os"
	"syscall"
)

// createExclNoFollow creates name in the directory of root, which must not
// exist yet (O_EXCL): an existing file, FIFO or link there fails, and
// O_NOFOLLOW refuses a link as the last part. The open goes through the
// directory handle, so nothing can swap the directory in between.
func createExclNoFollow(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

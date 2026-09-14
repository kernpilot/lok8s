package build

// The split assembles its output in scratch dirs and moves the files into
// place with a rename: atomic and cheap on one filesystem, and EXDEV
// ("invalid cross-device link") across two. The scratch dirs live next to
// the destination for that reason (newSplitRun). moveFile keeps the
// fallback for a rename that still crosses a device (a bind mount, a
// clusters/ symlink into another mount): copy the bytes, fsync, remove the
// source — what `mv` does, which is why the bash implementation never saw
// the failure.

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// renameFile is the rename the split uses. A test swaps it to force the
// cross-device path without a second filesystem.
var renameFile = os.Rename

// moveFile moves src to dst: rename first; on EXDEV copy + fsync + remove
// the source. Any other rename error is returned as is (the source stays).
func moveFile(src, dst string) error {
	err := renameFile(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyFileSync(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// copyFileSync writes src's bytes to dst with src's permission bits and
// fsyncs dst before it returns. A failed copy removes the partial dst.
func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

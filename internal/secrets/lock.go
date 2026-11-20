package secrets

// lock.go: the lock that add-key and remove-key hold for their whole run,
// from the read of .sops.yaml to the last re-encrypted file (bash:
// secrets::_lock). Both twins take flock(2) on the same file,
// <project>/.sops.yaml.lock (or the lock beside the file that a .sops.yaml
// symlink names), so a Go lo and a bash lo exclude each other too. Without
// it two runs can read the same .sops.yaml, and the second write puts back
// the recipient the first one removed. The pattern is the one of
// internal/driver/lo/lock.go, with one difference: a wait that runs out
// refuses the command instead of going on unlocked.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/kernpilot/lok8s/internal/clock"
	"github.com/kernpilot/lok8s/internal/ui"
)

// lockWaitEnv sets how many seconds add-key and remove-key wait for the
// lock (default 60, 0 = do not wait).
const lockWaitEnv = "LO_SECRETS_LOCK_WAIT"

const defaultLockWait = 60

var lockWaitRe = regexp.MustCompile(`^[0-9]+$`)

// lockWait is the wait in seconds: LO_SECRETS_LOCK_WAIT when it is a
// whole number, else the default.
func lockWait() int {
	v := os.Getenv(lockWaitEnv)
	if !lockWaitRe.MatchString(v) {
		return defaultLockWait
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultLockWait
	}
	return n
}

// sopsConfigLock is the lock file of the project's .sops.yaml: beside it, or,
// when it is a symlink, beside the file that it names. replaceFile writes
// that file, so two links to one file take one lock. A link that does not
// resolve keeps the lock beside the link: the check before the lock refuses
// such a link, so only a change during the run gets there.
func (c *Context) sopsConfigLock() string {
	path := c.sopsConfigPath()
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if target, err := filepath.EvalSymlinks(path); err == nil {
			path = target
		}
	}
	return path + ".lock"
}

// lockSopsConfig takes the lock and returns its release. It prints its own
// [error] line and returns ErrHandled when the lock path is not a regular
// file, when the file cannot be opened, or when another run holds the lock
// past the wait. Nothing is changed then.
//
// The lock can sit in the directory of the file that a .sops.yaml link
// names, and another user can write there. So the open never truncates and
// never follows a link. A lock path that is not a regular file or absent
// (encDestinationOK) is refused before the open, with the line of the bash
// twin. The open has O_NOFOLLOW and no O_TRUNC, and the opened file must be
// a regular file that the path still names (openedPathOK): a link put there
// after the check is not followed, and the run is refused.
func (c *Context) lockSopsConfig() (func(), error) {
	path := c.sopsConfigLock()
	if !encDestinationOK(path) {
		c.refuseLockPath(path)
		return nil, ErrHandled
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o644) // #nosec G302 G304 -- an empty lock file next to .sops.yaml
	if err != nil {
		ui.ErrorTo(c.ErrOut, "cannot open the lock file %s — the lock goes beside the file that .sops.yaml names, so lo needs write access to %s; nothing was changed", path, filepath.Dir(path))
		return nil, ErrHandled
	}
	if !openedPathOK(f, path) {
		_ = f.Close()
		c.refuseLockPath(path)
		return nil, ErrHandled
	}
	deadline := time.Now().Add(time.Duration(lockWait()) * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			ui.ErrorTo(c.ErrOut, "cannot lock %s", path)
			return nil, ErrHandled
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			ui.ErrorTo(c.ErrOut, "%s is held by another lo secrets add-key or remove-key — nothing was changed; run it again when that one ends", path)
			return nil, ErrHandled
		}
		_ = clock.Sleep(context.Background(), 200*time.Millisecond)
	}
}

// refuseLockPath prints the refusal of a lock path that is not a regular
// file (bash: the same line in secrets::_lock).
func (c *Context) refuseLockPath(path string) {
	ui.ErrorTo(c.ErrOut, "%s is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was changed — remove it, then run this command again", path)
}

// openedPathOK reports whether f, opened from path, is a regular file and
// path, read without following a link, still names it (bash: the /dev/fd
// test of secrets::_lock).
func openedPathOK(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	li, err := os.Lstat(path)
	return err == nil && os.SameFile(fi, li)
}

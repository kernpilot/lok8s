//go:build !windows

package kubehz

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

// privateEUID gives privateDirRefusal and privateLinkRefusal the
// effective uid of this process (bash: id -u), and privateLstat reads a
// link of the chain in privateLinkRefusal. A test replaces them.
var (
	privateEUID  = os.Geteuid
	privateLstat = os.Lstat
)

// privateLinkRefusal ports kubehz::_private_link_hops: "" when each link
// that the last name of dest follows (the link at dest, then the link that
// it names, and so on) belongs to this user or root, else the error line
// of bash. In a directory with the sticky bit, another user can put a link
// at the name, to a file of this user: the rename would then replace that
// file. A link of another user, a link whose owner cannot be read and a
// chain of more than maxLinkHops links are refused. A relative link names
// a path from the directory of the link (the text before the last "/", not
// cleaned, as in bash). A link to a directory on the way (not the last
// name) is not checked here. This is not linkTarget of the agent
// kubeconfig: that walk also checks each parent directory, wants a regular
// file of this user at the end, and gives the texts of the agent command.
func privateLinkRefusal(dest string) string {
	p := dest
	for hops := 0; ; hops++ {
		info, err := privateLstat(p)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			return ""
		}
		if hops == maxLinkHops {
			return "Cannot resolve the symlink " + dest
		}
		uid, ok := fileOwner(info)
		if !ok {
			return "Cannot read the owner of the link " + p + ": nothing is written"
		}
		if uid != 0 && uid != privateEUID() {
			return fmt.Sprintf("The link %s belongs to uid %d, not to this user or to root, so another user can point it at a file of this user: nothing is written. Remove the link, or use a path of your own.", p, uid)
		}
		next, err := os.Readlink(p)
		if err != nil || next == "" {
			return "Cannot resolve the symlink " + dest
		}
		if !strings.HasPrefix(next, "/") {
			if i := strings.LastIndex(p, "/"); i >= 0 {
				next = p[:i+1] + next
			} else {
				next = "./" + next
			}
		}
		p = next
	}
}

// privateDirRefusal ports kubehz::_private_dir_ok: "" when no other user
// can replace a file in dir, else the error line of bash. It refuses:
//   - a dir that os.Stat cannot read for a cause other than "not there"
//     (a parent that this user cannot search, a parent that is a file),
//     and a dir that is not there when its parent is not a directory;
//   - a path that is there but is not a directory (also a link to
//     nothing);
//   - a directory that belongs to a user other than this user or root
//     (that user can change its mode);
//   - without the sticky bit: a world-writable directory, and a
//     group-writable one whatever its group (a group can have other
//     members).
//
// A directory with the sticky bit passes: there, only the owner of a file
// can rename or remove it, and os.Rename goes onto the exact path (bash
// also wants mv -T there when other users can write into it). A directory
// that is not there passes when its parent is a directory: the write makes
// it with mode 0700, and the second check reads it. The check reads the
// mode bits only, not an ACL, and the directory only, not its parents
// (see writePrivateFile). os.Stat follows a link, as the create does.
func privateDirRefusal(dir string) string {
	unread := "Cannot read the owner and the mode of the directory " + dir + ": nothing is written"
	notDir := "The path " + dir + " is not a directory: nothing is written"
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := os.Lstat(dir); lerr == nil {
			return notDir
		}
		if parent, perr := os.Stat(filepath.Dir(dir)); perr != nil || !parent.IsDir() {
			return unread
		}
		return ""
	}
	if err != nil {
		return unread
	}
	if !info.IsDir() {
		return notDir
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return unread
	}
	if uid := int(st.Uid); uid != 0 && uid != privateEUID() {
		return fmt.Sprintf("The directory %s belongs to uid %d, not to this user or to root, so another user can replace a file in it: nothing is written. Use a directory of your own.", dir, uid)
	}
	mode := info.Mode()
	if mode&os.ModeSticky != 0 {
		return ""
	}
	if mode.Perm()&0o002 != 0 {
		return fmt.Sprintf("The directory %s is world-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", dir, dir)
	}
	if mode.Perm()&0o020 != 0 {
		return fmt.Sprintf("The directory %s is group-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", dir, dir)
	}
	return ""
}

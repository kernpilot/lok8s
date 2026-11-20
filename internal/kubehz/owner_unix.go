//go:build !windows

package kubehz

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
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

// dirCheckEUID, dirCheckEGID and dirCheckNames give privateDirRefusal the
// effective uid, the effective gid and the names of this process (bash:
// id -u, id -g, id -un, id -gn). A test replaces them.
var (
	dirCheckEUID  = os.Geteuid
	dirCheckEGID  = os.Getegid
	dirCheckNames = processNames
)

// processNames gives the name of the user uid and the name of the group
// gid (os/user). A static binary on Linux reads /etc/passwd and
// /etc/group only: a user of a directory service (LDAP, SSSD) is not
// found there, and the lookup fails.
func processNames(uid, gid int) (userName, groupName string, err error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", "", err
	}
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return "", "", err
	}
	return u.Username, g.Name, nil
}

// privateDirRefusal ports kubehz::_private_dir_ok: "" when no other user
// can replace a file in dir, else the error line of bash. It refuses a
// directory that belongs to a user other than this user or root (that
// user can change its mode), and, when the directory has no sticky bit,
// a world-writable directory and a group-writable one whose group is not
// the private group of this user (privateGroup). The sticky bit lets only
// the owner of a file rename or remove it. A directory that os.Stat
// cannot read passes (bash: [[ -d ]] is false): the create makes it, or
// fails. The check reads the mode bits only, not an ACL, and the
// directory only, not its parents (see writePrivateFile). os.Stat follows
// a link, as the create does.
func privateDirRefusal(dir string) string {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "Cannot read the owner and the mode of the directory " + dir + ": nothing is written"
	}
	uid, gid := int(st.Uid), int(st.Gid)
	if uid != 0 && uid != dirCheckEUID() {
		return fmt.Sprintf("The directory %s belongs to uid %d, not to this user or to root, so another user can replace a file in it: nothing is written. Use a directory of your own.", dir, uid)
	}
	mode := info.Mode()
	if mode&os.ModeSticky != 0 {
		return ""
	}
	if mode.Perm()&0o002 != 0 {
		return fmt.Sprintf("The directory %s is world-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", dir, dir)
	}
	if mode.Perm()&0o020 != 0 && !privateGroup(gid) {
		return fmt.Sprintf("The directory %s is group-writable and its group (gid %d) is not the private group of this user, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", dir, gid, dir)
	}
	return ""
}

// privateGroup ports kubehz::_private_group: gid is the private group of
// this user when it is the effective gid of this process and its name is
// the user name. An empty name or a lookup that fails is no private
// group. An administrator can still add members to that group.
func privateGroup(gid int) bool {
	if gid != dirCheckEGID() {
		return false
	}
	u, g, err := dirCheckNames(dirCheckEUID(), gid)
	return err == nil && u != "" && u == g
}

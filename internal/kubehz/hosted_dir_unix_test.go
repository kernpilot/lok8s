//go:build !windows

package kubehz

// hosted_dir_unix_test.go ports the cases of tests/unit/kubehz_hosted_test.bats
// for a kubeconfig directory that another user can write into
// (privateDirRefusal, the twin of kubehz::_private_dir_ok) and for a link of
// another user (privateWalk, the twin of kubehz::_private_link_hops).
// The texts are the texts of bash.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// dirRefused is the error line of a refused path (bash: _dir_refused).
func dirRefused(kind, path string, id int) string {
	switch kind {
	case "owner":
		return fmt.Sprintf("The directory %s belongs to uid %d, not to this user or to root, so another user can replace a file in it: nothing is written. Use a directory of your own.", path, id)
	case "world":
		return fmt.Sprintf("The directory %s is world-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", path, path)
	case "group":
		return fmt.Sprintf("The directory %s is group-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", path, path)
	case "notdir":
		return "The path " + path + " is not a directory: nothing is written"
	case "unread":
		return "Cannot read the owner and the mode of the directory " + path + ": nothing is written"
	case "link":
		return fmt.Sprintf("The link %s belongs to uid %d, not to this user or to root, so another user can point it at a file of this user: nothing is written. Remove the link, or use a path of your own.", path, id)
	case "linkread":
		return "Cannot read the owner of the link " + path + ": nothing is written"
	case "lstat":
		return "Cannot read the path " + path + ": nothing is written"
	case "resolve":
		return "Cannot resolve the symlink " + path
	}
	return ""
}

// dirUID gives the uid that owns dir.
func dirUID(t *testing.T, dir string) int {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no Stat_t for %s", dir)
	}
	return int(st.Uid)
}

// targetRefusal gives the text of the privateFileError of privateTarget
// for dest, or "" when privateTarget accepts dest.
func targetRefusal(t *testing.T, dest string) string {
	t.Helper()
	_, err := privateTarget(dest)
	if err == nil {
		return ""
	}
	var refused privateFileError
	if !errors.As(err, &refused) {
		t.Fatalf("%s: err = %v, not a privateFileError", dest, err)
	}
	return refused.Error()
}

// treeNames gives the path of each entry under root, relative to root and in
// lexical order (root itself left out). It does not follow a link. A
// directory that it cannot read is in the list, and its entries are not.
func treeNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && p != root {
				return nil
			}
			return err
		}
		if p != root {
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			names = append(names, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// leftovers gives what a write of dest left: the temp files beside the name
// dest, and each name under root that the write added or removed (before:
// treeNames of root before the write). The tree check covers each link of
// the walk and its end when they are under root.
func leftovers(t *testing.T, dest, root string, before []string) []string {
	t.Helper()
	left := hostedTemps(t, filepath.Dir(dest), filepath.Base(dest))
	after := treeNames(t, root)
	if !slices.Equal(after, before) {
		left = append(left, fmt.Sprintf("the tree under %s: %q, before %q", root, after, before))
	}
	return left
}

// mustRefusePrivate checks that the write of dest fails with the line want, that
// the old file stays, and that nothing is left in the directory of dest. dest
// is no link: for a link, use mustRefusePrivateAt.
func mustRefusePrivate(t *testing.T, dest, want string) {
	t.Helper()
	mustRefusePrivateAt(t, dest, filepath.Dir(dest), want)
}

// mustRefusePrivateAt is mustRefusePrivate for a dest whose walk goes through
// links or a "..": root holds each link of the walk and its end, and the
// write must leave no temp file beside dest and change no name under root
// (leftovers).
func mustRefusePrivateAt(t *testing.T, dest, root, want string) {
	t.Helper()
	before := treeNames(t, root)
	err := writePrivateFile(t.Context(), dest, []byte("new\n"))
	var refused privateFileError
	if !errors.As(err, &refused) || refused.Error() != want {
		t.Fatalf("%s: err = %v, want %q", dest, err, want)
	}
	if got := readFile(t, dest); got != "old\n" {
		t.Fatalf("%s: the old file changed: %q", dest, got)
	}
	if left := leftovers(t, dest, root, before); len(left) != 0 {
		t.Fatalf("%s: left: %q", dest, left)
	}
}

// mustWritePrivate checks that the write of dest succeeds, with mode 0600 and
// nothing left in the directory of dest. dest is no link: for a link, use
// mustWritePrivateAt.
func mustWritePrivate(t *testing.T, dest string) {
	t.Helper()
	mustWritePrivateAt(t, dest, filepath.Dir(dest))
}

// mustWritePrivateAt is mustWritePrivate for a dest whose walk goes through
// links or a "..": the file at the end of the walk is there before the
// write, so the write must change no name under root (see
// mustRefusePrivateAt).
func mustWritePrivateAt(t *testing.T, dest, root string) {
	t.Helper()
	before := treeNames(t, root)
	if err := writePrivateFile(t.Context(), dest, []byte("new\n")); err != nil {
		t.Fatalf("%s: %v", dest, err)
	}
	if got := readFile(t, dest); got != "new\n" {
		t.Fatalf("%s = %q", dest, got)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: mode: %v, %v", dest, info, err)
	}
	if left := leftovers(t, dest, root, before); len(left) != 0 {
		t.Fatalf("%s: left: %q", dest, left)
	}
}

// The check of the write helpers sees a file left at each name of a chain of
// links (the name of the link, the second link, the end of the chain), and a
// file that is gone. A check that looks only beside the name, or only beside
// the end, misses one of them.
func TestLeftoversSeesEachNameOfTheChain(t *testing.T) {
	t.Parallel()
	d := filepath.Join(t.TempDir(), "links")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	rc, kc, hop := filepath.Join(d, "home.rc"), filepath.Join(d, "kc.yaml"), filepath.Join(d, "hop")
	if err := os.WriteFile(rc, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop", kc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rc, hop); err != nil {
		t.Fatal(err)
	}
	before := treeNames(t, d)
	if got := leftovers(t, kc, d, before); len(got) != 0 {
		t.Fatalf("a clean tree: %q", got)
	}
	for _, p := range []string{kc, hop, rc} {
		leak := p + ".123"
		if err := os.WriteFile(leak, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := leftovers(t, kc, d, before); len(got) == 0 {
			t.Errorf("%s: not seen", leak)
		}
		if err := os.Remove(leak); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(hop); err != nil {
		t.Fatal(err)
	}
	if got := leftovers(t, kc, d, before); len(got) == 0 {
		t.Errorf("a removed %s: not seen", hop)
	}
}

// oldFileIn makes dir with the file kc.yaml ("old\n") and gives the path
// of the file. The mode of dir is set after the file.
func oldFileIn(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(dir, "kc.yaml")
	if err := os.WriteFile(kc, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return kc
}

// A world-writable directory without the sticky bit is refused before the
// temp file, and nothing is written. With the sticky bit (1777) and in a
// directory of this user only (0700), the write goes on: os.Rename goes
// onto the exact path, so the binary needs no mv -T (bash refuses 1777
// without it). A link to a directory gets the check of the directory that
// it names, where the create goes.
func TestWritePrivateFileRefusesAWorldWritableDirectory(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	world := oldFileIn(t, filepath.Join(d, "m0777"), 0o777)
	mustRefusePrivate(t, world, dirRefused("world", filepath.Dir(world), 0))
	mustWritePrivate(t, oldFileIn(t, filepath.Join(d, "m1777"), 0o777|os.ModeSticky))
	mustWritePrivate(t, oldFileIn(t, filepath.Join(d, "m0700"), 0o700))
	for _, l := range []string{"m0700", "m0777"} {
		if err := os.Symlink(l, filepath.Join(d, "l"+l[1:])); err != nil {
			t.Fatal(err)
		}
	}
	if got := privateDirRefusal(filepath.Join(d, "l0700")); got != "" {
		t.Fatalf("a link to a 0700 directory: %q", got)
	}
	if got, want := privateDirRefusal(filepath.Join(d, "l0777")), dirRefused("world", filepath.Join(d, "l0777"), 0); got != want {
		t.Fatalf("a link to a 0777 directory: %q, want %q", got, want)
	}
}

// A group-writable directory (0775) of another group of this user is
// refused: that group can have other members.
func TestWritePrivateFileRefusesAnotherGroupOfThisUser(t *testing.T) {
	t.Parallel()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	other := -1
	for _, g := range groups {
		if g != os.Getegid() {
			other = g
			break
		}
	}
	if other < 0 {
		t.Skip("this user is in no group other than its primary group")
	}
	kc := oldFileIn(t, filepath.Join(t.TempDir(), "grp"), 0o700)
	if err := os.Chown(filepath.Dir(kc), -1, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(kc), 0o775); err != nil {
		t.Fatal(err)
	}
	mustRefusePrivate(t, kc, dirRefused("group", filepath.Dir(kc), 0))
}

// A group-writable directory (0775) of the primary group of this user is
// refused too: the check does not ask who else is in the group. (The write
// makes the .kubeconfig directory with mode 0700: see
// TestProvisionHostedUmaskZeroMakesAPrivateKubeconfigDir.)
func TestWritePrivateFileRefusesTheGroupOfThisUser(t *testing.T) {
	t.Parallel()
	kc := oldFileIn(t, filepath.Join(t.TempDir(), "upg"), 0o700)
	if err := os.Chown(filepath.Dir(kc), -1, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(kc), 0o775); err != nil {
		t.Fatal(err)
	}
	mustRefusePrivate(t, kc, dirRefused("group", filepath.Dir(kc), 0))
}

// The owner of the directory and the uid of the process decide
// (privateEUID is faked here). A directory of another user is refused also
// with the sticky bit. A directory that is not there passes only when its
// parent is a directory: else the check cannot tell that it is not there
// (a path below a file, below a path that is not there, or below a
// directory that this user cannot search). A path
// that is there but is not a directory (a file, a link to nothing) is
// refused. A directory of root passes (bash: the same cases). Not
// parallel: it replaces privateEUID.
func TestPrivateDirRefusalOwnerMissingAndNotADirectory(t *testing.T) {
	t.Cleanup(func() { privateEUID = os.Geteuid })
	base := t.TempDir()
	d := filepath.Join(base, "ids")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	uid := dirUID(t, d)
	privateEUID = func() int { return uid }
	if got := privateDirRefusal(d); got != "" {
		t.Fatalf("a directory of this user: %q", got)
	}
	if uid != 0 {
		privateEUID = func() int { return uid + 1 }
		owner := dirRefused("owner", d, uid)
		if got := privateDirRefusal(d); got != owner {
			t.Fatalf("another owner: %q, want %q", got, owner)
		}
		if err := os.Chmod(d, 0o777|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
		got := targetRefusal(t, filepath.Join(d, "kc.yaml"))
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if got != owner {
			t.Fatalf("another owner, with the sticky bit: %q, want %q", got, owner)
		}
	}
	privateEUID = os.Geteuid
	none := filepath.Join(base, "none", "kc.yaml")
	if got, err := privateTarget(none); err != nil || got != none {
		t.Fatalf("a directory that is not there: %q, %v", got, err)
	}
	file := filepath.Join(d, "file")
	if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(d, "dangling")
	if err := os.Symlink(filepath.Join(d, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	below, belowNone := filepath.Join(file, "sub"), filepath.Join(base, "none", "sub")
	for _, tc := range []struct{ name, got, want string }{
		{"a file", privateDirRefusal(file), dirRefused("notdir", file, 0)},
		{"a file as the directory of the kubeconfig", targetRefusal(t, filepath.Join(file, "kc.yaml")), dirRefused("notdir", file, 0)},
		{"a link to nothing", privateDirRefusal(dangling), dirRefused("notdir", dangling, 0)},
		{"a path below a file", privateDirRefusal(below), dirRefused("unread", below, 0)},
		{"a path below a path that is not there", privateDirRefusal(belowNone), dirRefused("unread", belowNone, 0)},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if os.Geteuid() != 0 {
		closed := filepath.Join(d, "closed")
		if err := os.Mkdir(closed, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(closed, 0o600); err != nil {
			t.Fatal(err)
		}
		got := privateDirRefusal(filepath.Join(closed, "sub"))
		if err := os.Chmod(closed, 0o700); err != nil {
			t.Fatal(err)
		}
		if want := dirRefused("unread", filepath.Join(closed, "sub"), 0); got != want {
			t.Fatalf("a parent that this user cannot search: %q, want %q", got, want)
		}
	}
	if dirUID(t, "/") != 0 {
		t.Skip("/ does not belong to root here")
	}
	if got, err := privateTarget("/lok8s-none.yaml"); err != nil || got != "/lok8s-none.yaml" {
		t.Fatalf("a directory of root: %q, %v", got, err)
	}
}

// ownerInfo is the FileInfo of a link with another owner: Sys gives a copy
// of its Stat_t with the uid, or nil (no owner) when noOwner is set.
type ownerInfo struct {
	fs.FileInfo
	uid     uint32
	noOwner bool
}

func (o ownerInfo) Sys() any {
	if o.noOwner {
		return nil
	}
	st := *o.FileInfo.Sys().(*syscall.Stat_t)
	st.Uid = o.uid
	return &st
}

// fakeLinkOwner makes privateLstat give the entry at path the owner uid,
// or no owner when noOwner is set. Other paths get os.Lstat.
func fakeLinkOwner(path string, uid uint32, noOwner bool) {
	privateLstat = func(p string) (fs.FileInfo, error) {
		info, err := os.Lstat(p)
		if err != nil || p != path {
			return info, err
		}
		return ownerInfo{FileInfo: info, uid: uid, noOwner: noOwner}, nil
	}
}

// In a directory with the sticky bit, another user can put a link at the
// name of the file, to a file of this user: the rename would replace that
// file. So each link that the name follows must belong to this user or to
// root (privateLstat is faked for one link of the chain, uid 4242): a link
// of another user, at the name or as the second link, and a link whose
// owner cannot be read are refused before the temp file, and the file at
// the end of the chain stays as it was. A link of root passes, and so does
// a chain of links of this user: the write replaces the file at its end,
// and the links stay (bash: the same cases). Not parallel: it replaces
// privateLstat.
func TestWritePrivateFileRefusesALinkOfAnotherUser(t *testing.T) {
	if os.Geteuid() == 4242 {
		t.Skip("the uid of this user is the fake uid of the test")
	}
	t.Cleanup(func() { privateLstat = os.Lstat })
	d := filepath.Join(t.TempDir(), "links")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(d, "home.rc")
	if err := os.WriteFile(rc, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kc, hop := filepath.Join(d, "kc.yaml"), filepath.Join(d, "hop")
	if err := os.Symlink("hop", kc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rc, hop); err != nil {
		t.Fatal(err)
	}
	fakeLinkOwner(kc, 4242, false)
	mustRefusePrivateAt(t, kc, d, dirRefused("link", kc, 4242))
	fakeLinkOwner(hop, 4242, false)
	mustRefusePrivateAt(t, kc, d, dirRefused("link", hop, 4242))
	fakeLinkOwner(hop, 0, true)
	mustRefusePrivateAt(t, kc, d, dirRefused("linkread", hop, 0))
	fakeLinkOwner(hop, 0, false)
	if got, err := privateTarget(kc); err != nil || got != rc {
		t.Fatalf("a link of root: %q, %v, want %q", got, err, rc)
	}
	privateLstat = os.Lstat
	mustWritePrivateAt(t, kc, d)
	for _, l := range []string{kc, hop} {
		if info, err := os.Lstat(l); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a link: %v, %v", l, info, err)
		}
	}
}

// failLstat makes privateLstat fail for path with errno, as the lstat of
// the kernel does. Other paths get os.Lstat.
func failLstat(path string, errno syscall.Errno) {
	privateLstat = func(p string) (fs.FileInfo, error) {
		if p == path {
			return nil, &fs.PathError{Op: "lstat", Path: p, Err: errno}
		}
		return os.Lstat(p)
	}
}

// A lookup of a name of the chain that fails for a cause other than "not
// there" (EIO, ESTALE, EACCES) is refused: the check cannot tell that the
// name is no link, and a link there can be a link of another user. Before,
// each Lstat error ended the walk as a pass. Nothing is written, and the
// file at the end of the chain stays as it was (bash: "kubehz::_write_private
// refuses a lookup error of a name of the chain"). Not parallel: it
// replaces privateLstat.
func TestWritePrivateFileRefusesALookupErrorOfTheChain(t *testing.T) {
	t.Cleanup(func() { privateLstat = os.Lstat })
	d := filepath.Join(t.TempDir(), "lookup-links")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(d, "home.rc")
	if err := os.WriteFile(rc, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kc, hop := filepath.Join(d, "kc.yaml"), filepath.Join(d, "hop")
	if err := os.Symlink("hop", kc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rc, hop); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{kc, hop} {
		for _, errno := range []syscall.Errno{syscall.EIO, syscall.ESTALE, syscall.EACCES} {
			failLstat(p, errno)
			mustRefusePrivateAt(t, kc, d, dirRefused("lstat", p, 0))
		}
	}
	if tmp := hostedTemps(t, d, "home.rc"); len(tmp) != 0 {
		t.Fatalf("temp files left beside the target: %q", tmp)
	}
}

// A directory that os.Stat does not find, and whose Lstat fails for a
// cause other than "not there", is refused: the directory can be there
// (bash: "kubehz::_private_dir_ok refuses a lookup error other than not
// there"). A Lstat that says "not there" passes, when the parent is a
// directory. Not parallel: it replaces privateLstat.
func TestPrivateDirRefusalRefusesALookupError(t *testing.T) {
	t.Cleanup(func() { privateLstat = os.Lstat })
	dir := filepath.Join(t.TempDir(), "kube")
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ESTALE, syscall.EACCES} {
		failLstat(dir, errno)
		if got, want := privateDirRefusal(dir), dirRefused("unread", dir, 0); got != want {
			t.Fatalf("%v: %q, want %q", errno, got, want)
		}
	}
	failLstat(dir, syscall.ENOENT)
	if got := privateDirRefusal(dir); got != "" {
		t.Fatalf("ENOENT: %q, want a pass", got)
	}
}

// insertLinkAfterLookup makes privateLstat put a link at dest, to victim,
// right after the first lookup of dest that finds nothing there. Another
// user who can write into the directory can do that between the lookup
// and the write. Other lookups get os.Lstat. The result reports whether
// the link was put there.
func insertLinkAfterLookup(t *testing.T, dest, victim string) *bool {
	t.Helper()
	done := new(bool)
	privateLstat = func(p string) (fs.FileInfo, error) {
		info, err := os.Lstat(p)
		if p == dest && !*done && errors.Is(err, fs.ErrNotExist) {
			*done = true
			if serr := os.Symlink(victim, dest); serr != nil {
				t.Errorf("the insertion of the link: %v", serr)
			}
		}
		return info, err
	}
	return done
}

// In a directory with the sticky bit and a shared group (mode 1770),
// another user can put a link at the name of the file when nothing is
// there, to a file of this user. The walk found nothing at the name, and
// the write does not look the name up again: the rename replaces the link
// that was put there after the walk, and the file that the link names
// stays as it was (privateLstat puts the link there right after the
// lookup of the walk). Before, a second resolution after the walk followed
// that link and replaced the file of this user (bash: "kubehz::_write_private
// replaces a link put at the name after the walk"). Not parallel: it
// replaces privateLstat.
func TestWritePrivateFileReplacesALinkPutAfterTheWalk(t *testing.T) {
	t.Cleanup(func() { privateLstat = os.Lstat })
	d := t.TempDir()
	victim := oldFileIn(t, filepath.Join(d, "home"), 0o700)
	shared := filepath.Join(d, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o770|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(shared, "kc.yaml")
	done := insertLinkAfterLookup(t, dest, victim)
	if err := writePrivateFile(t.Context(), dest, []byte("new\n")); err != nil {
		t.Fatalf("%s: %v", dest, err)
	}
	if !*done {
		t.Fatal("the walk did not look up the name: no link was put there")
	}
	if got := readFile(t, victim); got != "old\n" {
		t.Fatalf("the file that the link names changed: %q", got)
	}
	info, err := os.Lstat(dest)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s is not a regular file with mode 0600: %v, %v", dest, info, err)
	}
	if got := readFile(t, dest); got != "new\n" {
		t.Fatalf("%s = %q", dest, got)
	}
	for _, dir := range []string{shared, filepath.Dir(victim)} {
		if tmp := hostedTemps(t, dir, "kc.yaml"); len(tmp) != 0 {
			t.Fatalf("temp files left in %s: %q", dir, tmp)
		}
	}
}

// Each link of the chain is read only when no other user can replace it:
// a link in a directory that another user can write into without the
// sticky bit (0777, 0770) is refused, at the name and as the second link,
// and the file that the chain names stays as it was. A link in a directory
// with the sticky bit passes: there, another user cannot rename or remove
// a link of this user (bash: the same cases).
func TestWritePrivateFileChecksTheDirectoryOfEachLink(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	kc := oldFileIn(t, filepath.Join(d, "safe"), 0o700)
	linkIn := func(dir string, mode os.FileMode, to string) string {
		t.Helper()
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		l := filepath.Join(dir, "kc.yaml")
		if err := os.Symlink(to, l); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		return l
	}
	world := linkIn(filepath.Join(d, "world"), 0o777, kc)
	mustRefusePrivateAt(t, world, d, dirRefused("world", filepath.Dir(world), 0))
	group := linkIn(filepath.Join(d, "group"), 0o770, kc)
	mustRefusePrivateAt(t, group, d, dirRefused("group", filepath.Dir(group), 0))
	first := linkIn(filepath.Join(d, "first"), 0o700, world)
	mustRefusePrivateAt(t, first, d, dirRefused("world", filepath.Dir(world), 0))
	sticky := linkIn(filepath.Join(d, "sticky"), 0o777|os.ModeSticky, kc)
	mustWritePrivateAt(t, sticky, d)
	if got := readFile(t, kc); got != "new\n" {
		t.Fatalf("%s = %q", kc, got)
	}
}

// The end of the walk keeps a ".." after a link to a directory, and the
// kernel reads that ".." from the directory that the link names:
// d/alias/../store is d/deep/store, not d/store. So the check and the
// create take the directory of the path as it is (privateDir), not a
// cleaned path (filepath.Dir). A world-writable d/deep/store is refused
// although d/store is a directory of this user only, and the write goes
// into d/deep/store when no d/store is there (bash: the same case).
func TestWritePrivateFileTakesTheDirectoryOfTheWalkAsItIs(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	kc := oldFileIn(t, filepath.Join(d, "deep", "store"), 0o777)
	real := filepath.Join(d, "deep", "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../store/kc.yaml", filepath.Join(real, "kc.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(d, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d, "store"), 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(d, "alias", "kc.yaml")
	mustRefusePrivateAt(t, dest, d, dirRefused("world", d+"/alias/../store", 0))
	if err := os.Remove(filepath.Join(d, "store")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(kc), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWritePrivateAt(t, dest, d)
	if got := readFile(t, kc); got != "new\n" {
		t.Fatalf("%s = %q", kc, got)
	}
	if _, err := os.Lstat(filepath.Join(d, "store")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the write made %s: %v", filepath.Join(d, "store"), err)
	}
}

// A chain of 8 links passes, and a chain of 9 links is refused (bash: the
// same case).
func TestPrivateTargetFollowsEightLinksAndRefusesNine(t *testing.T) {
	t.Parallel()
	d := filepath.Join(t.TempDir(), "chain")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(d, "f")
	if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(d, "c8")); err != nil {
		t.Fatal(err)
	}
	for i := 7; i >= 0; i-- {
		if err := os.Symlink(fmt.Sprintf("c%d", i+1), filepath.Join(d, fmt.Sprintf("c%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := privateTarget(filepath.Join(d, "c1")); err != nil || got != f {
		t.Fatalf("8 links: %q, %v, want %q", got, err, f)
	}
	c0 := filepath.Join(d, "c0")
	if got, want := targetRefusal(t, c0), dirRefused("resolve", c0, 0); got != want {
		t.Fatalf("9 links: %q, want %q", got, want)
	}
}

// A kubeconfig directory that another user can write into stops the
// provision before the create (bash: the same case in "provision_hosted:
// a refused kubeconfig path stops the provision before the create").
func TestProvisionHostedWorldWritableKubeconfigDirCreatesNoCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	hostedKubeconfigRoutes(h, 200, "new\n")
	dir := filepath.Join(h.base, ".kubeconfig")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), dirRefused("world", dir, 0))
	if h.anyReq("POST", "/api/clusters") {
		t.Fatalf("the create ran: %+v", h.reqs())
	}
	if _, err := os.Stat(filepath.Join(dir, "test.kubehz.dev.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a kubeconfig was written: %v", err)
	}
}

// Under umask 000, a .kubeconfig directory with the default mode of the
// umask would be open to other users. The write makes it with mode 0700,
// so the provision goes on: the file is there (mode 0600), and the
// directory has mode 0700 (bash: "provision_hosted: under umask 000, a new
// .kubeconfig directory gets mode 0700 and the write goes on"). Not
// parallel: the umask belongs to the process.
func TestProvisionHostedUmaskZeroMakesAPrivateKubeconfigDir(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	hostedKubeconfigRoutes(h, 200, "new\n")
	dir := filepath.Join(h.base, ".kubeconfig")
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the directory is there before the provision: %v", err)
	}
	old := syscall.Umask(0)
	err := h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec)
	syscall.Umask(old)
	if err != nil {
		t.Fatalf("ProvisionHosted: %v\n%s", err, h.output())
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s: mode: %v, %v", dir, info, err)
	}
	kc := filepath.Join(dir, "test.kubehz.dev.yaml")
	if got := readFile(t, kc); got != "new\n" {
		t.Fatalf("%s = %q", kc, got)
	}
	if info, err := os.Stat(kc); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: mode: %v, %v", kc, info, err)
	}
}

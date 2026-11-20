//go:build !windows

package kubehz

// hosted_dir_unix_test.go ports the cases of tests/unit/kubehz_hosted_test.bats
// for a directory that another user can write into (privateDirRefusal, the
// twin of kubehz::_private_dir_ok). The texts are the texts of bash.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// dirRefused is the error line of a refused directory (bash: _dir_refused).
func dirRefused(kind, dir string, id int) string {
	switch kind {
	case "owner":
		return fmt.Sprintf("The directory %s belongs to uid %d, not to this user or to root, so another user can replace a file in it: nothing is written. Use a directory of your own.", dir, id)
	case "world":
		return fmt.Sprintf("The directory %s is world-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", dir, dir)
	case "group":
		return fmt.Sprintf("The directory %s is group-writable and its group (gid %d) is not the private group of this user, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.", dir, id, dir)
	}
	return ""
}

// dirIDs gives the uid and the gid that own dir.
func dirIDs(t *testing.T, dir string) (uid, gid int) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no Stat_t for %s", dir)
	}
	return int(st.Uid), int(st.Gid)
}

// mustRefusePrivate checks that the write of dest fails with the line want, that
// the old file stays, and that no temp file is left.
func mustRefusePrivate(t *testing.T, dest, want string) {
	t.Helper()
	err := writePrivateFile(t.Context(), dest, []byte("new\n"))
	var refused privateFileError
	if !errors.As(err, &refused) || refused.Error() != want {
		t.Fatalf("%s: err = %v, want %q", dest, err, want)
	}
	if got := readFile(t, dest); got != "old\n" {
		t.Fatalf("%s: the old file changed: %q", dest, got)
	}
	if tmp := hostedTemps(t, filepath.Dir(dest), filepath.Base(dest)); len(tmp) != 0 {
		t.Fatalf("%s: temp files left: %q", dest, tmp)
	}
}

// mustWritePrivate checks that the write of dest succeeds, with mode 0600 and no
// temp file left.
func mustWritePrivate(t *testing.T, dest string) {
	t.Helper()
	if err := writePrivateFile(t.Context(), dest, []byte("new\n")); err != nil {
		t.Fatalf("%s: %v", dest, err)
	}
	if got := readFile(t, dest); got != "new\n" {
		t.Fatalf("%s = %q", dest, got)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: mode: %v, %v", dest, info, err)
	}
	if tmp := hostedTemps(t, filepath.Dir(dest), filepath.Base(dest)); len(tmp) != 0 {
		t.Fatalf("%s: temp files left: %q", dest, tmp)
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
// directory of this user only (0700), the write goes on. A link to a
// directory gets the check of the directory that it names, where the
// create goes.
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
	mustRefusePrivate(t, kc, dirRefused("group", filepath.Dir(kc), other))
}

// A group-writable directory (0775) of the private group of this user is
// what mkdir -p makes under umask 002 on a system with a group per user:
// the write goes on.
func TestWritePrivateFileWritesIntoThePrivateGroupDirectory(t *testing.T) {
	t.Parallel()
	if u, g, err := processNames(os.Geteuid(), os.Getegid()); err != nil || u != g {
		t.Skipf("the primary group of this user is not a private group (%q, %q, %v)", u, g, err)
	}
	kc := oldFileIn(t, filepath.Join(t.TempDir(), "upg"), 0o700)
	if err := os.Chown(filepath.Dir(kc), -1, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(kc), 0o775); err != nil {
		t.Fatal(err)
	}
	mustWritePrivate(t, kc)
}

// The ids and the names of the process decide (the seams are faked here,
// so the cases do not depend on the groups of the host). A group-writable
// directory is allowed only when its gid is the gid of the process and
// the group name is the user name; names that are empty or that the
// lookup cannot give are no private group. A directory of another user
// is refused also with the sticky bit. A directory of root passes, and a
// directory that is not there passes (the create makes it, or fails).
// Not parallel: it replaces the seams.
func TestPrivateDirRefusalOwnerGroupAndNames(t *testing.T) {
	t.Cleanup(func() { dirCheckEUID, dirCheckEGID, dirCheckNames = os.Geteuid, os.Getegid, processNames })
	base := t.TempDir()
	d := filepath.Join(base, "ids")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o775); err != nil {
		t.Fatal(err)
	}
	uid, gid := dirIDs(t, d)
	names := func(u, g string, err error) func(int, int) (string, string, error) {
		return func(int, int) (string, string, error) { return u, g, err }
	}
	group := dirRefused("group", d, gid)
	for _, tc := range []struct {
		name  string
		egid  int
		names func(int, int) (string, string, error)
		want  string
	}{
		{"the private group", gid, names("n", "n", nil), ""},
		{"another group name", gid, names("n", "staff", nil), group},
		{"another gid", gid + 1, names("n", "n", nil), group},
		{"empty names", gid, names("", "", nil), group},
		{"a lookup that fails", gid, names("n", "n", errors.New("injected")), group},
	} {
		dirCheckEGID = func() int { return tc.egid }
		dirCheckNames = tc.names
		if got := privateDirRefusal(d); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	dirCheckEGID, dirCheckNames = os.Getegid, processNames
	if uid != 0 {
		if err := os.Chmod(d, 0o777|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
		dirCheckEUID = func() int { return uid + 1 }
		owner := dirRefused("owner", d, uid)
		if got := privateDirRefusal(d); got != owner {
			t.Fatalf("another owner: %q, want %q", got, owner)
		}
		_, err := privateTarget(filepath.Join(d, "kc.yaml"))
		var refused privateFileError
		if !errors.As(err, &refused) || refused.Error() != owner {
			t.Fatalf("privateTarget: err = %v, want %q", err, owner)
		}
		dirCheckEUID = os.Geteuid
	}
	if got, err := privateTarget(filepath.Join(base, "none", "kc.yaml")); err != nil || got != filepath.Join(base, "none", "kc.yaml") {
		t.Fatalf("a directory that is not there: %q, %v", got, err)
	}
	if rootUID, _ := dirIDs(t, "/"); rootUID != 0 {
		t.Skip("/ does not belong to root here")
	}
	if got, err := privateTarget("/lok8s-none.yaml"); err != nil || got != "/lok8s-none.yaml" {
		t.Fatalf("a directory of root: %q, %v", got, err)
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

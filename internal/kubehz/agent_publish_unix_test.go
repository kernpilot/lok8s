//go:build !windows

package kubehz

// The publish of the kubeconfig (publishKubeconfig) opens the directory of
// the target one time and does each step relative to it. These cases put a
// link to another directory in place of that directory after the open
// (swapAfterOpen). Each step must stay in the directory that the publish
// opened, and no step makes, changes or removes a file in the other
// directory. The bash twin does each step by path, so it has no such case.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// publishDirs makes the two directories of a case in the test dir: work,
// where the kubeconfig goes, and other, where a step by path goes after
// the swap.
func publishDirs(t *testing.T) (work, other string) {
	t.Helper()
	base := linkTestDir(t)
	work, other = filepath.Join(base, "work"), filepath.Join(base, "other")
	must(t, os.Mkdir(work, 0o700))
	must(t, os.Mkdir(other, 0o700))
	return work, other
}

// swapAfterOpen replaces openKubeconfigRoot. Right after the open of the
// directory, it moves work to work+".orig" and puts a link to other in its
// place, so a step by path after the open goes to other. It returns the
// moved directory: the directory that the publish opened. The publish must
// open it one time.
func swapAfterOpen(t *testing.T, work, other string) string {
	t.Helper()
	open := openKubeconfigRoot
	t.Cleanup(func() { openKubeconfigRoot = open })
	opened := work + ".orig"
	opens := 0
	openKubeconfigRoot = func(dir string) (*os.Root, error) {
		opens++
		root, err := open(dir)
		if err == nil && opens == 1 {
			must(t, os.Rename(work, opened))
			must(t, os.Symlink(other, work))
		}
		return root, err
	}
	t.Cleanup(func() {
		if opens != 1 {
			t.Errorf("the publish opened the directory %d times, want 1", opens)
		}
	})
	return opened
}

// plant writes a file with the text "decoy" at dir/name, and returns its
// path.
func plant(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	must(t, os.WriteFile(path, []byte("decoy"), 0o600))
	return path
}

// checkPublished fails the test unless dir holds only kc.yaml, a regular
// file with mode 0600 and the text want: no temporary file stays.
func checkPublished(t *testing.T, dir, want string) {
	t.Helper()
	if got := dirNames(t, dir); got != "kc.yaml" {
		t.Errorf("the opened directory holds %q, want only kc.yaml", got)
	}
	file := filepath.Join(dir, "kc.yaml")
	fi, err := os.Lstat(file)
	if err != nil {
		t.Fatalf("the kubeconfig is not in the opened directory: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want a regular file with 0600", fi.Mode())
	}
	if got := readFile(t, file); got != want {
		t.Errorf("kc.yaml = %q, want %q", got, want)
	}
}

// checkDecoys fails the test unless dir holds the files decoys and no
// other file, each with the text "decoy".
func checkDecoys(t *testing.T, dir string, decoys ...string) {
	t.Helper()
	names := make([]string, 0, len(decoys))
	for _, d := range decoys {
		names = append(names, filepath.Base(d))
	}
	slices.Sort(names)
	if got, want := dirNames(t, dir), strings.Join(names, " "); got != want {
		t.Errorf("the other directory holds %q, want %q", got, want)
	}
	for _, d := range decoys {
		if raw, err := os.ReadFile(d); err != nil || string(raw) != "decoy" {
			t.Errorf("decoy %s holds %q (read: %v): a step went to the other directory", filepath.Base(d), raw, err)
		}
	}
}

// The link path: the kubeconfig lands in the opened directory, as a hard
// link of the temporary file there. A file with the name of the temporary
// file in the other directory stays: a link by path would publish it, and
// a cleanup by path would remove it. Not parallel: it replaces
// openKubeconfigRoot, linkKubeconfig and chmodKubeconfig.
func TestPublishKubeconfigLinkStaysInTheOpenedDirectory(t *testing.T) {
	work, other := publishDirs(t)
	opened := swapAfterOpen(t, work, other)
	link, chmod := linkKubeconfig, chmodKubeconfig
	t.Cleanup(func() { linkKubeconfig, chmodKubeconfig = link, chmod })
	decoy := ""
	linkKubeconfig = func(root *os.Root, oldname, newname string) error {
		decoy = plant(t, other, oldname)
		return link(root, oldname, newname)
	}
	creates := 0
	chmodKubeconfig = func(f *os.File) error {
		creates++
		return chmod(f)
	}
	if _, err := publishKubeconfig(filepath.Join(work, "kc.yaml"), []byte(agentKubeconfigYAML), false); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if creates != 0 {
		t.Errorf("the exclusive create ran %d times, want 0: the link must publish the file", creates)
	}
	checkPublished(t, opened, agentKubeconfigYAML)
	checkDecoys(t, other, decoy)
}

// The fallback path (a file system with no hard links): the check of the
// temporary file, the exclusive create and the check of the new file use
// the names in the opened directory. A file with the name of the temporary
// file in the other directory stays, and no kc.yaml appears there. Not
// parallel: it replaces openKubeconfigRoot and linkKubeconfig.
func TestPublishKubeconfigFallbackStaysInTheOpenedDirectory(t *testing.T) {
	work, other := publishDirs(t)
	opened := swapAfterOpen(t, work, other)
	link := linkKubeconfig
	t.Cleanup(func() { linkKubeconfig = link })
	decoy := ""
	calls := 0
	linkKubeconfig = func(_ *os.Root, oldname, newname string) error {
		calls++
		decoy = plant(t, other, oldname)
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOSYS}
	}
	wide, err := publishKubeconfig(filepath.Join(work, "kc.yaml"), []byte(agentKubeconfigYAML), false)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if wide != 0 {
		t.Errorf("wide = %04o, want 0", wide)
	}
	if calls != 1 {
		t.Errorf("the link ran %d times, want 1", calls)
	}
	checkPublished(t, opened, agentKubeconfigYAML)
	checkDecoys(t, other, decoy)
}

// A write that fails after the exclusive create: the publish removes the
// part and the temporary file in the opened directory only. Files with
// their names in the other directory stay. Not parallel: it replaces
// openKubeconfigRoot, linkKubeconfig and writeKubeconfigFile.
func TestPublishKubeconfigWriteFailureRemovesOnlyInTheOpenedDirectory(t *testing.T) {
	work, other := publishDirs(t)
	target := plant(t, other, "kc.yaml")
	opened := swapAfterOpen(t, work, other)
	link, write := linkKubeconfig, writeKubeconfigFile
	t.Cleanup(func() { linkKubeconfig, writeKubeconfigFile = link, write })
	temp := ""
	linkKubeconfig = func(_ *os.Root, oldname, newname string) error {
		temp = plant(t, other, oldname)
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	writes := 0
	writeKubeconfigFile = failWrite(t, &writes, nil)
	_, err := publishKubeconfig(filepath.Join(work, "kc.yaml"), []byte(agentKubeconfigYAML), false)
	var werr *kubeconfigWriteError
	if !errors.As(err, &werr) {
		t.Fatalf("publish: err = %v, want a kubeconfigWriteError", err)
	}
	if werr.left {
		t.Error("left = true: the part in the opened directory was not removed")
	}
	if writes != 1 {
		t.Errorf("the write ran %d times, want 1", writes)
	}
	if got := dirNames(t, opened); got != "" {
		t.Errorf("the opened directory holds %q, want nothing: the part or the temporary file stays", got)
	}
	checkDecoys(t, other, target, temp)
}

// The open needs the read right on the directory (os.OpenRoot opens it
// read-only). In a directory with only the write and the search rights
// (mode 0300), the publish fails and writes nothing. The command prints
// "cannot write" and a hint for the read right on the directory, not the
// hint for a directory that the user cannot write to. The bash twin writes
// the file there, so the hint is Go-only. Skips as root, which can read
// any directory.
func TestPublishKubeconfigNeedsTheReadRightOnTheDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	dir := linkTestDir(t)
	must(t, os.Chmod(dir, 0o300))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	file := filepath.Join(dir, "kc.yaml")
	_, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("publish: err = %v, want the refused open of the directory", err)
	}
	h := agentHarness(t)
	h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	_, err = h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", file, false)
	mustErr(t, err)
	want := "[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write " + file + "\n" +
		"  lo also needs the read right on " + dir + ". Name a file in a directory that you can read and write to.\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	must(t, os.Chmod(dir, 0o700))
	if got := dirNames(t, dir); got != "" {
		t.Errorf("dir holds %q, want nothing", got)
	}
}

// With force, the rename replaces the file in the opened directory, and
// the file with the same name in the other directory stays. Not parallel:
// it replaces openKubeconfigRoot.
func TestPublishKubeconfigRenameStaysInTheOpenedDirectory(t *testing.T) {
	work, other := publishDirs(t)
	must(t, os.WriteFile(filepath.Join(work, "kc.yaml"), []byte("old"), 0o600))
	target := plant(t, other, "kc.yaml")
	opened := swapAfterOpen(t, work, other)
	if _, err := publishKubeconfig(filepath.Join(work, "kc.yaml"), []byte(agentKubeconfigYAML), true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	checkPublished(t, opened, agentKubeconfigYAML)
	checkDecoys(t, other, target)
}

// A last name that names no file in the directory (empty, ".", "..", or a
// "/" at the end) is refused before the publish makes the temporary file,
// with and without force. The command prints the refusal of a directory.
// kubeconfigTarget refuses these paths first, so this is a second check.
// Not parallel: it changes the working directory.
func TestPublishKubeconfigRefusesANameThatNamesNoFile(t *testing.T) {
	base := linkTestDir(t)
	a := filepath.Join(base, "a")
	must(t, os.Mkdir(a, 0o700))
	t.Chdir(base)
	for _, target := range []string{"", "a/", ".", "..", "a/.", "a/..", a + "/.."} {
		for _, force := range []bool{false, true} {
			if _, err := publishKubeconfig(target, []byte(agentKubeconfigYAML), force); !errors.Is(err, errKubeconfigNoName) {
				t.Errorf("publish(%q, force %v): err = %v, want errKubeconfigNoName", target, force, err)
			}
		}
	}
	if got := dirNames(t, base); got != "a" {
		t.Errorf("the test dir holds %q, want only a: a temporary file was left behind", got)
	}
	if got := dirNames(t, a); got != "" {
		t.Errorf("a holds %q, want nothing: a temporary file was left behind", got)
	}
	h := agentHarness(t)
	mustErr(t, h.ctx.kubeconfigPublishFailed("cluster kubeconfig cl-1a2b3c4d", "a/..", false, errKubeconfigNoName))
	want := "[error] kubehz cluster kubeconfig cl-1a2b3c4d: a/.. is a directory\n  Name a file, not a directory.\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// The cleanup makes one attempt to remove the temporary file, and only
// while its name names that file. Here the link step moves the temporary
// file away and puts another file at its name: the fallback does not run
// (the check of the temporary file fails), and the other file stays. Not
// parallel: it replaces linkKubeconfig.
func TestPublishKubeconfigCleanupKeepsAFileAtTheTemporaryName(t *testing.T) {
	link := linkKubeconfig
	t.Cleanup(func() { linkKubeconfig = link })
	dir := t.TempDir()
	moved := filepath.Join(dir, "moved")
	other := ""
	linkKubeconfig = func(_ *os.Root, oldname, newname string) error {
		must(t, os.Rename(filepath.Join(dir, oldname), moved))
		other = plant(t, dir, oldname)
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOSYS}
	}
	_, err := publishKubeconfig(filepath.Join(dir, "kc.yaml"), []byte(agentKubeconfigYAML), false)
	if !errors.Is(err, syscall.ENOSYS) {
		t.Fatalf("publish: err = %v, want the link error ENOSYS", err)
	}
	if raw, err := os.ReadFile(other); err != nil || string(raw) != "decoy" {
		t.Errorf("the file at the temporary name holds %q (read: %v): the cleanup removed or changed it", raw, err)
	}
	if got, want := dirNames(t, dir), filepath.Base(other)+" moved"; got != want {
		t.Errorf("dir holds %q, want %q", got, want)
	}
}

// With force, a rename that fails (here: a directory at the target) leaves
// the target as it was, and the cleanup removes the temporary file.
func TestPublishKubeconfigFailedRenameRemovesTheTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "kc.yaml")
	must(t, os.Mkdir(target, 0o700))
	inside := plant(t, target, "inside")
	if _, err := publishKubeconfig(target, []byte(agentKubeconfigYAML), true); err == nil {
		t.Fatal("the rename onto a directory worked")
	}
	if got := dirNames(t, dir); got != "kc.yaml" {
		t.Errorf("dir holds %q, want only kc.yaml: a temporary file was left behind", got)
	}
	if got := dirNames(t, target); got != "inside" {
		t.Errorf("the target directory holds %q, want only inside", got)
	}
	if got := readFile(t, inside); got != "decoy" {
		t.Errorf("inside = %q: the target was changed", got)
	}
}

// A close of the fallback file that fails after the write (EIO on a
// network file system) is a failed write: the publish removes the part and
// returns a kubeconfigWriteError with the cause and with left false. Not
// parallel: it replaces linkKubeconfig and closeKubeconfigFile.
func TestPublishKubeconfigCloseFailureRemovesThePart(t *testing.T) {
	link, closeFile := linkKubeconfig, closeKubeconfigFile
	t.Cleanup(func() { linkKubeconfig, closeKubeconfigFile = link, closeFile })
	links, closes := 0, 0
	linkKubeconfig = failLink(t, syscall.EPERM, &links, nil)
	closeKubeconfigFile = func(f *os.File) error {
		closes++
		must(t, f.Close())
		return &os.PathError{Op: "close", Path: f.Name(), Err: syscall.EIO}
	}
	dir := t.TempDir()
	_, err := publishKubeconfig(filepath.Join(dir, "kc.yaml"), []byte(agentKubeconfigYAML), false)
	var werr *kubeconfigWriteError
	if !errors.As(err, &werr) {
		t.Fatalf("publish: err = %v, want a kubeconfigWriteError", err)
	}
	if got := werr.cause(); got != "input/output error" {
		t.Errorf("cause = %q", got)
	}
	if werr.left {
		t.Error("left = true: the part was not removed")
	}
	if closes != 1 {
		t.Errorf("the close ran %d times, want 1", closes)
	}
	if got := dirNames(t, dir); got != "" {
		t.Errorf("dir holds %q, want nothing: the part or the temporary file stays", got)
	}
}

// kubeconfigTempNameRe is the name of a temporary file: ".kubeconfig-" and
// 26 characters of the base32 alphabet (crypto/rand.Text).
var kubeconfigTempNameRe = regexp.MustCompile(`^\.kubeconfig-[A-Z2-7]{26}$`)

// Each temporary file gets a new name of that form, mode 0600.
func TestCreateKubeconfigTempName(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	names := map[string]bool{}
	for i := range 2 {
		f, name, err := createKubeconfigTemp(root)
		if err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
		fi, serr := f.Stat()
		must(t, f.Close())
		if serr != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("create %d: mode %v (stat: %v), want 0600", i+1, fi.Mode(), serr)
		}
		if !kubeconfigTempNameRe.MatchString(name) {
			t.Errorf("create %d: name %q, want %s", i+1, name, kubeconfigTempNameRe)
		}
		names[name] = true
	}
	if len(names) != 2 {
		t.Errorf("two creates gave the names %v, want two names", names)
	}
}

// openFDsUnder counts the descriptors of this process that name dir or a
// file under it (/proc/self/fd).
func openFDsUnder(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	n := 0
	for _, e := range entries {
		dest, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err == nil && (dest == dir || strings.HasPrefix(dest, dir+"/")) {
			n++
		}
	}
	return n
}

// The publish closes each descriptor that it opens: the directory (the
// os.Root), the temporary file and the file of the fallback. Linux only:
// it reads /proc/self/fd. Not parallel: it replaces linkKubeconfig.
func TestPublishKubeconfigClosesItsDescriptors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/self/fd")
	}
	link := linkKubeconfig
	t.Cleanup(func() { linkKubeconfig = link })
	dir := linkTestDir(t)
	calls := 0
	for _, tc := range []struct {
		name     string
		force    bool
		fallback bool
	}{
		{name: "link.yaml"},
		{name: "rename.yaml", force: true},
		{name: "fallback.yaml", fallback: true},
	} {
		linkKubeconfig = link
		if tc.fallback {
			linkKubeconfig = failLink(t, syscall.EPERM, &calls, nil)
		}
		before := openFDsUnder(t, dir)
		if _, err := publishKubeconfig(filepath.Join(dir, tc.name), []byte(agentKubeconfigYAML), tc.force); err != nil {
			t.Fatalf("publish %s: %v", tc.name, err)
		}
		if after := openFDsUnder(t, dir); after != before {
			t.Errorf("publish %s: %d descriptors in the test dir before, %d after", tc.name, before, after)
		}
	}
	if got := dirNames(t, dir); got != "fallback.yaml link.yaml rename.yaml" {
		t.Errorf("dir holds %q", got)
	}
}

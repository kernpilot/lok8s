//go:build !windows

package kubehz

// The link walk of kubeconfigTarget (linkTarget): with --force, lo writes
// through a link only when no other user can change a link or a directory
// on the way. A link or a directory of another user needs root to make, so
// those cases set the owner with the Lstat seam (ownerAs). The mode rules
// and the hop limit use real links and directories. The bash twin of each
// case is in tests/unit/kubehz_agent_test.bats.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// linkTestDir is t.TempDir() with mode 0700 and each link resolved:
// t.TempDir follows the umask (a group-writable test dir fails every
// case), and the walk names resolved paths (/var is a link on macOS).
func linkTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0o700))
	return realDir(t, dir)
}

// ownerAs is an Lstat seam: path shows the owner uid, each other path its
// real owner.
func ownerAs(path string, uid int) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		fi, err := os.Lstat(p)
		if err != nil || p != path {
			return fi, err
		}
		st := *fi.Sys().(*syscall.Stat_t)
		st.Uid = uint32(uid)
		return ownedInfo{fi, &st}, nil
	}
}

// ownedInfo is a FileInfo with another owner.
type ownedInfo struct {
	fs.FileInfo
	st *syscall.Stat_t
}

func (o ownedInfo) Sys() any { return o.st }

// linkChain makes dir/l1 -> l2 -> ... -> ln -> last, each link relative.
func linkChain(t *testing.T, dir string, n int, last string) string {
	t.Helper()
	for i := 1; i <= n; i++ {
		dest := fmt.Sprintf("l%d", i+1)
		if i == n {
			dest = last
		}
		must(t, os.Symlink(dest, filepath.Join(dir, fmt.Sprintf("l%d", i))))
	}
	return filepath.Join(dir, "l1")
}

// writeReal makes dir/rel, a file of this user that holds "contexts".
func writeReal(t *testing.T, dir, rel string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, rel), []byte("contexts"), 0o600))
}

const linkNotSafeHint = "  lo writes through a link only to a file of yours. Each link and each directory on the way must belong to root or to you,\n" +
	"  and must not be writable by its group or by others unless it has the sticky bit. Name another file.\n"

const linkHopHint = "  Point the link at the file, or name the file itself.\n"

const linkNotRegularHint = "  lo writes through a link only to a regular file of yours. Name another file.\n"

const unwritableHint = "  Name a file in a directory that exists and that you can write to.\n"

const unmappedHint = "  The uid of this user is not mapped in the namespace, so lo cannot check who owns the path.\n" +
	"  Run it where the uid of this user is mapped (unshare --map-current-user), or name a file that is not a link.\n"

// ownersAs is an Lstat seam: each path in uids shows its uid, each other
// directory shows root, and each other path its real owner.
func ownersAs(uids map[string]int) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		fi, err := os.Lstat(p)
		if err != nil {
			return fi, err
		}
		uid, ok := uids[p]
		switch {
		case ok:
		case fi.IsDir():
			uid = 0
		default:
			return fi, nil
		}
		st := *fi.Sys().(*syscall.Stat_t)
		st.Uid = uint32(uid)
		return ownedInfo{fi, &st}, nil
	}
}

func TestAgentKubeconfigLinkChain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test needs a user other than root: root passes each owner check")
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string) string // the --file value
		seam  func(c *Context, dir string)
		want  string // stderr after the action ("" for success); DIR is the test dir
		land  string // success: the file under DIR that gets the kubeconfig
	}{
		{name: "a clean chain of 3 passes, and the file lands at the final target", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "a", 0o755)
			modeDir(t, dir, "b", 0o700)
			modeDir(t, dir, "kube", 0o700)
			writeReal(t, dir, "kube/real")
			must(t, os.Symlink("a/l2", filepath.Join(dir, "l1")))
			must(t, os.Symlink(filepath.Join(dir, "b", "l3"), filepath.Join(dir, "a", "l2")))
			must(t, os.Symlink("../kube/real", filepath.Join(dir, "b", "l3")))
			return filepath.Join(dir, "l1")
		}, land: "kube/real"},
		{name: "8 links with a link to a directory among them pass", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "kube", 0o700)
			writeReal(t, dir, "kube/real")
			must(t, os.Symlink("kube", filepath.Join(dir, "sub")))
			return linkChain(t, dir, 7, "sub/real")
		}, land: "kube/real"},
		{name: "9 links with a link to a directory among them are refused: that link counts", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "kube", 0o700)
			writeReal(t, dir, "kube/real")
			must(t, os.Symlink("kube", filepath.Join(dir, "sub")))
			return linkChain(t, dir, 8, "sub/real")
		}, want: "DIR/l1 is a link, and its path is not safe: DIR/sub is link 9 on the way, and lo follows at most 8 (a loop, or a chain that is too long)\n" + linkHopHint},
		{name: "a relative path starts at the working directory", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			linkChain(t, dir, 2, "real")
			t.Chdir(dir)
			return "l1"
		}, land: "real"},
		{name: "a relative path starts at the real working directory, not at a stale PWD", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			linkChain(t, dir, 1, "real")
			other := modeDir(t, dir, "other", 0o700)
			writeReal(t, other, "real")
			linkChain(t, other, 1, "real")
			t.Chdir(dir)
			t.Setenv("PWD", other)
			return "l1"
		}, land: "real"},
		{name: "a link to real/ is refused: a file is not a directory", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 1, "real/")
		}, want: "cannot write DIR/l1: DIR/real is not a directory\n" + unwritableHint},
		{name: "a link to real/. is refused", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 1, "real/.")
		}, want: "cannot write DIR/l1: DIR/real is not a directory\n" + unwritableHint},
		{name: "a link to missing/../real is refused: the name before .. must exist", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 1, "missing/../real")
		}, want: "cannot write DIR/l1: DIR/missing does not exist or cannot be read\n" + unwritableHint},
		{name: "a link to regular/../real is refused: the name before .. must be a directory", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			writeReal(t, dir, "regular")
			return linkChain(t, dir, 1, "regular/../real")
		}, want: "cannot write DIR/l1: DIR/regular is not a directory\n" + unwritableHint},
		{name: "a .. after a link to a directory goes up from where the link points", setup: func(t *testing.T, dir string) string {
			kube := modeDir(t, dir, "kube", 0o700)
			modeDir(t, kube, "sub", 0o700)
			writeReal(t, dir, "kube/real")
			must(t, os.Symlink("kube/sub", filepath.Join(dir, "dl")))
			return linkChain(t, dir, 1, "dl/../real")
		}, land: "kube/real"},
		{name: "a link to a FIFO is refused: the target must be a regular file", setup: func(t *testing.T, dir string) string {
			must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600))
			return linkChain(t, dir, 1, "fifo")
		}, want: "DIR/l1 is a link, and its target DIR/fifo is not a regular file\n" + linkNotRegularHint},
		{name: "the argument real/ is refused before any request", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return dir + "/real/"
		}, want: "cannot write DIR/real/: DIR/real is not a directory\n" + unwritableHint},
		{name: "the argument real/. is refused before any request", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return dir + "/real/."
		}, want: "cannot write DIR/real/.: DIR/real is not a directory\n" + unwritableHint},
		{name: "the argument missing/../real is refused before any request", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return dir + "/missing/../real"
		}, want: "cannot write DIR/missing/../real: DIR/missing does not exist or cannot be read\n" + unwritableHint},
		{name: "the argument regular/../real is refused before any request", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			writeReal(t, dir, "regular")
			return dir + "/regular/../real"
		}, want: "cannot write DIR/regular/../real: DIR/regular is not a directory\n" + unwritableHint},
		{name: "the argument dl/../l1 goes up from where the link points", setup: func(t *testing.T, dir string) string {
			kube := modeDir(t, dir, "kube", 0o700)
			modeDir(t, kube, "sub", 0o700)
			writeReal(t, dir, "kube/real")
			linkChain(t, kube, 1, "real")
			must(t, os.Symlink("kube/sub", filepath.Join(dir, "dl")))
			return dir + "/dl/../l1"
		}, land: "kube/real"},
		{name: "a chain of 8 passes", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 8, "real")
		}, land: "real"},
		{name: "a chain of 9 is refused", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 9, "real")
		}, want: "DIR/l1 is a link, and its path is not safe: DIR/l9 is link 9 on the way, and lo follows at most 8 (a loop, or a chain that is too long)\n" + linkHopHint},
		{name: "a loop is refused at the hop limit", setup: func(t *testing.T, dir string) string {
			must(t, os.Symlink("b", filepath.Join(dir, "a")))
			must(t, os.Symlink("a", filepath.Join(dir, "b")))
			return filepath.Join(dir, "a")
		}, want: "DIR/a is a link, and its path is not safe: DIR/a is link 9 on the way, and lo follows at most 8 (a loop, or a chain that is too long)\n" + linkHopHint},
		{name: "a group-writable directory that holds a link is refused", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "mid", 0o775)
			writeReal(t, dir, "real")
			must(t, os.Symlink("mid/l2", filepath.Join(dir, "l1")))
			must(t, os.Symlink("../real", filepath.Join(dir, "mid", "l2")))
			return filepath.Join(dir, "l1")
		}, want: "DIR/l1 is a link, and its path is not safe: DIR/mid is writable by its group and has no sticky bit\n" + linkNotSafeHint},
		{name: "each directory above a link is checked, from / down", setup: func(t *testing.T, dir string) string {
			open := modeDir(t, dir, "open", 0o777)
			modeDir(t, open, "sub", 0o700)
			writeReal(t, dir, "real")
			must(t, os.Symlink("open/sub/l2", filepath.Join(dir, "l1")))
			must(t, os.Symlink("../../real", filepath.Join(open, "sub", "l2")))
			return filepath.Join(dir, "l1")
		}, want: "DIR/l1 is a link, and its path is not safe: DIR/open is writable by others and has no sticky bit\n" + linkNotSafeHint},
		{name: "a sticky world-writable directory passes", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "st", 0o1777)
			writeReal(t, dir, "st/real")
			must(t, os.Symlink("st/l2", filepath.Join(dir, "l1")))
			must(t, os.Symlink("real", filepath.Join(dir, "st", "l2")))
			return filepath.Join(dir, "l1")
		}, land: "st/real"},
		{name: "the directory of the target is checked", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "open", 0o777)
			writeReal(t, dir, "open/real")
			must(t, os.Symlink("open/real", filepath.Join(dir, "l1")))
			return filepath.Join(dir, "l1")
		}, want: "DIR/l1 is a link, and its path is not safe: DIR/open is writable by others and has no sticky bit\n" + linkNotSafeHint},
		{name: "a link of another user on the way is refused", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 2, "real")
		}, seam: func(c *Context, dir string) { c.Lstat = ownerAs(filepath.Join(dir, "l2"), 4242) },
			want: "DIR/l1 is a link, and its path is not safe: DIR/l2 belongs to another user (uid 4242)\n" + linkNotSafeHint},
		{name: "a link of root passes", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 2, "real")
		}, seam: func(c *Context, dir string) { c.Lstat = ownerAs(filepath.Join(dir, "l2"), 0) }, land: "real"},
		{name: "a directory of another user that holds a link is refused", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "mid", 0o755)
			writeReal(t, dir, "real")
			must(t, os.Symlink("mid/l2", filepath.Join(dir, "l1")))
			must(t, os.Symlink("../real", filepath.Join(dir, "mid", "l2")))
			return filepath.Join(dir, "l1")
		}, seam: func(c *Context, dir string) { c.Lstat = ownerAs(filepath.Join(dir, "mid"), 4242) },
			want: "DIR/l1 is a link, and its path is not safe: DIR/mid belongs to another user (uid 4242)\n" + linkNotSafeHint},
		{name: "a target of another user is refused", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 1, "real")
		}, seam: func(c *Context, dir string) { c.Lstat = ownerAs(filepath.Join(dir, "real"), 4242) },
			want: "DIR/l1 is a link, and its path is not safe: DIR/real belongs to another user (uid 4242)\n" + linkNotSafeHint},
		{name: "a target of root is refused: the target must be a file of yours", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			return linkChain(t, dir, 1, "real")
		}, seam: func(c *Context, dir string) { c.Lstat = ownerAs(filepath.Join(dir, "real"), 0) },
			want: "DIR/l1 is a link, and its path is not safe: DIR/real belongs to another user (uid 0)\n" + linkNotSafeHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := agentHarness(t)
			h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
			dir := linkTestDir(t)
			file := tc.setup(t, dir)
			if tc.seam != nil {
				tc.seam(h.ctx, dir)
			}
			_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", file, true)
			got := strings.ReplaceAll(h.errOut.String(), dir, "DIR")
			if tc.want != "" {
				mustErr(t, err)
				if want := "[error] kubehz cluster kubeconfig cl-1a2b3c4d: " + tc.want; got != want {
					t.Errorf("stderr = %q, want %q", got, want)
				}
				if n := len(h.reqs()); n != 0 {
					t.Errorf("%d requests: a refused path must cost no grant and no download", n)
				}
				return
			}
			mustOK(t, err, got)
			if got := readFile(t, filepath.Join(dir, tc.land)); got != agentKubeconfigYAML {
				t.Errorf("%s = %q, want the kubeconfig", tc.land, got)
			}
			if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Error("the first link was replaced")
			}
			if tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Join(dir, tc.land)), ".kubeconfig-*")); len(tmp) != 0 {
				t.Errorf("a temporary file was left behind: %v", tmp)
			}
		})
	}
}

// Without a mapping for this user in a user namespace, this user and every
// other unmapped owner show the overflow uid: "ours" proves nothing there.
// The seams make this user the overflow uid, so the first part of ours on
// the way is refused with the hint for an unmapped user.
func TestAgentKubeconfigLinkForAnUnmappedUser(t *testing.T) {
	me := os.Geteuid()
	if me == 0 {
		t.Skip("the test needs a user other than root")
	}
	h := agentHarness(t)
	h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	h.ctx.Euid = func() int { return me }
	h.ctx.OverflowUID = func() int { return me }
	dir := linkTestDir(t)
	writeReal(t, dir, "real")
	_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", linkChain(t, dir, 1, "real"), true)
	mustErr(t, err)
	mustContain(t, h.errOut.String(), "/l1 is a link, and its path is not safe: ")
	mustContain(t, h.errOut.String(), fmt.Sprintf(" cannot be checked (uid %d)\n", me)+
		"  The uid of this user is not mapped in the namespace, so lo cannot check who owns the path.\n"+
		"  Run it where the uid of this user is mapped (unshare --map-current-user), or name a file that is not a link.\n")
	if got := readFile(t, filepath.Join(dir, "real")); got != "contexts" {
		t.Errorf("real = %q", got)
	}
}

// Under the unmapped state, every directory on the way shows root (the
// seam), so the walk reaches the owner checks of the link and of the
// target: a part of ours there proves nothing, and the refusal gives the
// hint for an unmapped user.
func TestAgentKubeconfigLinkOwnersForAnUnmappedUser(t *testing.T) {
	me := os.Geteuid()
	if me == 0 {
		t.Skip("the test needs a user other than root")
	}
	for _, tc := range []struct {
		name string
		uids func(dir string) map[string]int
		part string
	}{
		{"a link of ours cannot be checked", func(string) map[string]int { return nil }, "l1"},
		{"a target of ours cannot be checked", func(dir string) map[string]int {
			return map[string]int{filepath.Join(dir, "l1"): 0}
		}, "real"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := agentHarness(t)
			h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
			h.ctx.Euid = func() int { return me }
			h.ctx.OverflowUID = func() int { return me }
			dir := linkTestDir(t)
			writeReal(t, dir, "real")
			file := linkChain(t, dir, 1, "real")
			h.ctx.Lstat = ownersAs(tc.uids(dir))
			_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", file, true)
			mustErr(t, err)
			want := "[error] kubehz cluster kubeconfig cl-1a2b3c4d: DIR/l1 is a link, and its path is not safe: DIR/" + tc.part +
				fmt.Sprintf(" cannot be checked (uid %d)\n", me) + unmappedHint
			if got := strings.ReplaceAll(h.errOut.String(), dir, "DIR"); got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
			if n := len(h.reqs()); n != 0 {
				t.Errorf("%d requests: a refused path must cost no grant and no download", n)
			}
			if got := readFile(t, filepath.Join(dir, "real")); got != "contexts" {
				t.Errorf("real = %q", got)
			}
		})
	}
}

// Two refusals that kubeconfigTarget does not reach, so the test calls
// linkTarget itself: a link to a directory (the os.Stat check of
// kubeconfigTarget refuses it first), and a relative path when the working
// directory was removed (the path then does not exist, so it is not a
// link).
func TestAgentKubeconfigLinkTargetDirect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test needs a user other than root: root passes each owner check")
	}
	const action = "cluster kubeconfig cl-1a2b3c4d"
	t.Run("a directory as the final target is not a regular file", func(t *testing.T) {
		h := agentHarness(t)
		dir := linkTestDir(t)
		modeDir(t, dir, "sub", 0o700)
		_, err := h.ctx.linkTarget(action, linkChain(t, dir, 1, "sub"))
		mustErr(t, err)
		want := "[error] kubehz " + action + ": DIR/l1 is a link, and its target DIR/sub is not a regular file\n" + linkNotRegularHint
		if got := strings.ReplaceAll(h.errOut.String(), dir, "DIR"); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
	t.Run("a relative path in a removed working directory is refused", func(t *testing.T) {
		h := agentHarness(t)
		wd := modeDir(t, linkTestDir(t), "wd", 0o700)
		t.Chdir(wd)
		must(t, os.Remove(wd))
		if got, err := os.Getwd(); err == nil {
			t.Skipf("os.Getwd still works in a removed directory here (%s)", got)
		}
		_, err := h.ctx.linkTarget(action, "l1")
		mustErr(t, err)
		want := "[error] kubehz " + action + ": cannot write l1: the working directory cannot be read\n" +
			"  Run lo in a directory that exists, or name the file with its full path.\n"
		if got := h.errOut.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
}

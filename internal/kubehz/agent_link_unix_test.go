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
		{name: "a link to a directory on the way counts, and passes", setup: func(t *testing.T, dir string) string {
			modeDir(t, dir, "kube", 0o700)
			writeReal(t, dir, "kube/real")
			must(t, os.Symlink("kube", filepath.Join(dir, "sub")))
			must(t, os.Symlink("sub/real", filepath.Join(dir, "l1")))
			return filepath.Join(dir, "l1")
		}, land: "kube/real"},
		{name: "a relative path starts at the working directory", setup: func(t *testing.T, dir string) string {
			writeReal(t, dir, "real")
			linkChain(t, dir, 2, "real")
			t.Chdir(dir)
			return "l1"
		}, land: "real"},
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
		}, want: "DIR/l1 is a link, and its path is not safe: DIR/mid is writable by others and has no sticky bit\n" + linkNotSafeHint},
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
			if fi, err := os.Lstat(filepath.Join(dir, "l1")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
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

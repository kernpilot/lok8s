//go:build !windows

package kubehz

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A FIFO planted at the path of the file, or a link to one, gets no byte of
// the ticket: the O_EXCL create through the directory handle refuses both,
// the live-ticket note follows, and the directory goes. A reader is open on
// the FIFO, so a create that went through would not block, and the bytes
// would wait in the pipe.
func TestSpaceMintJoinNeverWritesIntoAPlantedFIFO(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, ext, what, body string }{
		{"fifo.token", ".token", "join ticket", noScriptJSON},
		{"fifo.sh", ".sh", "join script", joinTicketJSON},
		{"link.token", ".token", "join ticket", noScriptJSON},
		{"link.sh", ".sh", "join script", joinTicketJSON},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			priv := filepath.Join(h.base, "priv")
			if err := os.Mkdir(priv, 0o700); err != nil {
				t.Fatal(err)
			}
			at := filepath.Join(priv, "kubehz-join-worker-1"+c.ext)
			fifo := at
			if c.name[:4] == "link" {
				fifo = filepath.Join(h.base, "fifo")
				if err := os.Symlink(fifo, at); err != nil {
					t.Fatal(err)
				}
			}
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			var parent string
			h.ctx.MkdirTemp = joinDir(priv, &parent)
			h.handle("POST /api/spaces/sp-123/join-token", 201, c.body)
			mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false))
			mustContain(t, h.output(), "could not write the "+c.what+" for 'worker-1'")
			mustContain(t, h.output(), "The ticket was minted before the write failed")
			buf := make([]byte, 4096)
			if n, _ := r.Read(buf); n != 0 {
				t.Fatalf("%d bytes went into the planted FIFO", n)
			}
			if _, err := os.Lstat(priv); !os.IsNotExist(err) {
				t.Fatalf("the directory stays after the refused create: %v", err)
			}
		})
	}
}

// A TMPDIR whose path a foreign owner could change is refused BEFORE the
// mint, so it costs no ticket: a part that its group or others can write
// without the sticky bit (the TMPDIR itself or a parent), a part of another user (the
// Euid seam makes us someone else), the same part when its owner is the
// overflow uid (only "/" may be), and a link to such a directory. With
// the Euid seam each part of ours is another user's, so the top-down walk
// refuses the first part of ours from the top (firstPartOwnedBy). No
// join-token request goes out, and no live-ticket note follows. A part
// with the sticky bit passes, as /tmp does.
func TestSpaceMintJoinRefusesATMPDIROthersCanChange(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, why string
		other     bool
		overflow  bool
		mk        func(t *testing.T, base string) (tmpdir, bad string)
	}{
		{"group-writable", "is writable by its group and has no sticky bit", false, false, func(t *testing.T, base string) (string, string) {
			return modeDir(t, base, "gw", 0o770), filepath.Join(base, "gw")
		}},
		{"other-writable", "is writable by others and has no sticky bit", false, false, func(t *testing.T, base string) (string, string) {
			return modeDir(t, base, "ow", 0o707), filepath.Join(base, "ow")
		}},
		{"writable-parent", "is writable by others and has no sticky bit", false, false, func(t *testing.T, base string) (string, string) {
			parent := modeDir(t, base, "wp", 0o777)
			return modeDir(t, parent, "sub", 0o700), parent
		}},
		{"link-to-writable", "is writable by its group and has no sticky bit", false, false, func(t *testing.T, base string) (string, string) {
			target := modeDir(t, base, "lw", 0o770)
			link := filepath.Join(base, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link, target
		}},
		{"another-user", "belongs to another user (uid " + strconv.Itoa(os.Geteuid()) + ")", true, false, func(t *testing.T, base string) (string, string) {
			return modeDir(t, base, "au", 0o700), filepath.Join(base, "au")
		}},
		{"overflow-uid-not-root", "belongs to another user (uid " + strconv.Itoa(os.Geteuid()) + ")", true, true, func(t *testing.T, base string) (string, string) {
			return modeDir(t, base, "ov", 0o700), filepath.Join(base, "ov")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			tmpdir, bad := c.mk(t, h.base)
			h.env["TMPDIR"] = tmpdir
			if c.other {
				h.ctx.Euid = func() int { return os.Geteuid() + 1 }
				bad = firstPartOwnedBy(t, tmpdir, os.Geteuid())
			}
			if c.overflow {
				// Our own uid is now the overflow uid: a part that is not "/"
				// with that owner is still refused.
				h.ctx.OverflowUID = os.Geteuid
			}
			h.handle("POST /api/spaces/sp-123/join-token", 201, noScriptJSON)
			mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false))
			mustContain(t, h.output(), "the temp dir for the join ticket is not safe: "+realDir(t, bad)+" "+c.why+"\n")
			mustContain(t, h.output(), "Set TMPDIR to a directory of your own that only you can write (mode 0700).")
			if h.anyReq("POST", "/join-token") {
				t.Fatal("a refused TMPDIR still minted a ticket")
			}
			mustNotContain(t, h.output(), "could not write")
			mustNotContain(t, h.output(), "The ticket was minted")
			mustNotContain(t, h.output(), joinTicket)
			if dirs, _ := filepath.Glob(filepath.Join(tmpdir, "kubehz-join-*")); len(dirs) != 0 {
				t.Fatalf("a directory was made in a refused TMPDIR: %v", dirs)
			}
		})
	}
	t.Run("sticky", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.env["TMPDIR"] = modeDir(t, h.base, "st", 0o1777)
		h.handle("POST /api/spaces/sp-123/join-token", 201, noScriptJSON)
		mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
		if got := joinTickets(t, h.env["TMPDIR"], "worker-1"); len(got) != 1 {
			t.Fatalf("ticket files below a sticky TMPDIR: %v", got)
		}
	})
}

// The writer checks the TMPDIR again: it can change between the check
// before the mint and the write. Here the api stub makes it group-writable
// while it answers the mint. The writer refuses, no directory is made, and
// the live-ticket note follows (the mint has happened).
func TestSpaceMintJoinChecksTheTMPDIRAgainAtTheWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tmpdir := modeDir(t, h.base, "changes", 0o700)
	h.env["TMPDIR"] = tmpdir
	h.handleFunc("POST /api/spaces/sp-123/join-token", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.Chmod(tmpdir, 0o770); err != nil {
			t.Error(err)
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, noScriptJSON)
	})
	mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false))
	mustContain(t, h.output(), "the temp dir for the join ticket is not safe: "+realDir(t, tmpdir)+" is writable by its group and has no sticky bit\n")
	mustContain(t, h.output(), "could not write the join ticket for 'worker-1'\n")
	mustContain(t, h.output(), "The ticket was minted before the write failed")
	mustNotContain(t, h.output(), joinTicket)
	if dirs, _ := filepath.Glob(filepath.Join(tmpdir, "kubehz-join-*")); len(dirs) != 0 {
		t.Fatalf("a directory was made in a refused TMPDIR: %v", dirs)
	}
}

// The overflow uid is trusted for "/" alone: in a user namespace that does
// not map root, the host's "/" shows that owner. /tmp or any other part
// with that owner is refused; root and the caller own any part.
func TestOwnerTrustedTakesTheOverflowUIDOnlyForRoot(t *testing.T) {
	t.Parallel()
	c := &Context{Euid: func() int { return 1000 }, OverflowUID: func() int { return 65534 }}
	for _, tc := range []struct {
		part string
		uid  int
		want bool
	}{
		{"/", 65534, true},
		{"/tmp", 65534, false},
		{"/home/me/tmp", 65534, false},
		{"/", 1234, false},
		{"/tmp", 0, true},
		{"/home/me/tmp", 1000, true},
		{"/home/me/tmp", 1001, false},
	} {
		if got := c.ownerTrusted(tc.part, tc.uid); got != tc.want {
			t.Errorf("ownerTrusted(%q, %d) = %v, want %v", tc.part, tc.uid, got, tc.want)
		}
	}
}

// Without a mapping for this user in a user namespace, this user and every
// other unmapped owner show the overflow uid: "ours" proves nothing there.
// Then only root's parts and "/" (of the overflow uid) pass.
func TestOwnerTrustedForAnUnmappedCaller(t *testing.T) {
	t.Parallel()
	c := &Context{Euid: func() int { return 65534 }, OverflowUID: func() int { return 65534 }}
	for _, tc := range []struct {
		part string
		uid  int
		want bool
	}{
		{"/", 65534, true},
		{"/usr", 65534, false},
		{"/tmp/sticky", 65534, false},
		{"/home/me/tmp", 65534, false},
		{"/tmp", 0, true},
		{"/home/me/tmp", 1000, false},
	} {
		if got := c.ownerTrusted(tc.part, tc.uid); got != tc.want {
			t.Errorf("ownerTrusted(%q, %d) = %v, want %v", tc.part, tc.uid, got, tc.want)
		}
	}
}

// The seams make the euid the overflow uid (a user namespace without a
// mapping for this user): the parts of ours cannot be checked, and the
// first of them from the top (the walk goes top-down) is refused before
// the mint, with the hint for an unmapped user.
func TestSpaceMintJoinRefusesTheOwnPartsOfAnUnmappedCaller(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root owns its parts as root")
	}
	h := newHarness(t)
	tmpdir := modeDir(t, h.base, "mine", 0o700)
	h.env["TMPDIR"] = tmpdir
	h.ctx.Euid = os.Geteuid
	h.ctx.OverflowUID = os.Geteuid
	h.handle("POST /api/spaces/sp-123/join-token", 201, noScriptJSON)
	mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false))
	mustContain(t, h.output(), "the temp dir for the join ticket is not safe: "+firstPartOwnedBy(t, tmpdir, os.Geteuid())+" cannot be checked (uid "+strconv.Itoa(os.Geteuid())+")\n")
	mustContain(t, h.output(), "  The uid of this user is not mapped in the namespace, so the CLI cannot check who owns TMPDIR.\n")
	mustNotContain(t, h.output(), "Set TMPDIR to a directory of your own")
	if h.anyReq("POST", "/join-token") {
		t.Fatal("an unmapped caller's TMPDIR still minted a ticket")
	}
	if dirs, _ := filepath.Glob(filepath.Join(tmpdir, "kubehz-join-*")); len(dirs) != 0 {
		t.Fatalf("a directory was made in a refused TMPDIR: %v", dirs)
	}
}

// TMPDIR=<safe>/link/.. with link pointing to <unsafe>/child: ".." goes up
// from the target of the link, as the kernel and cd -P (the bash twin) do,
// so the check reads <unsafe> (its group can write it) and refuses it before
// the mint. filepath.Abs or Clean before EvalSymlinks would make it <safe>
// and pass. The relative form is joined to the working directory (t.Chdir,
// so no t.Parallel here).
func TestSpaceMintJoinResolvesDotDotAfterALinkPhysically(t *testing.T) {
	for _, rel := range []bool{false, true} {
		t.Run(map[bool]string{false: "absolute", true: "relative"}[rel], func(t *testing.T) {
			h := newHarness(t)
			safe := modeDir(t, h.base, "safe", 0o700)
			unsafe := modeDir(t, h.base, "unsafe", 0o770)
			child := modeDir(t, unsafe, "child", 0o700)
			if err := os.Symlink(child, filepath.Join(safe, "link")); err != nil {
				t.Fatal(err)
			}
			// Not filepath.Join: it would clean "link/.." away.
			tmpdir := safe + "/link/.."
			if rel {
				t.Chdir(safe)
				tmpdir = "link/.."
			}
			h.env["TMPDIR"] = tmpdir
			h.handle("POST /api/spaces/sp-123/join-token", 201, noScriptJSON)
			mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false))
			mustContain(t, h.output(), "the temp dir for the join ticket is not safe: "+realDir(t, unsafe)+" is writable by its group and has no sticky bit\n")
			if h.anyReq("POST", "/join-token") {
				t.Fatal("a TMPDIR that resolves to a directory its group can write still minted a ticket")
			}
		})
	}
}

// The overflow uid comes from the kernel file; a file that cannot be read
// or holds no uid gives 65534.
func TestReadOverflowUIDFallsBackTo65534(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name, content string
		want          int
	}{
		{"a-uid", "4242\n", 4242},
		{"empty", "", 65534},
		{"junk", "x\n", 65534},
		{"negative", "-1\n", 65534},
	} {
		file := filepath.Join(dir, tc.name)
		if err := os.WriteFile(file, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readOverflowUID(file); got != tc.want {
			t.Errorf("readOverflowUID(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := readOverflowUID(filepath.Join(dir, "missing")); got != 65534 {
		t.Errorf("readOverflowUID(missing) = %d, want 65534", got)
	}
}

// modeDir makes dir/name with exactly mode (the umask does not apply; the
// octal 01000 of mode is the sticky bit).
func modeDir(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	perm := mode & os.ModePerm
	if mode&0o1000 != 0 {
		perm |= os.ModeSticky
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

// The modes do not follow the umask: with 0400 (which would give a 0300
// directory and a 0200 file) and with 0077, the directory is 0700 and the
// file 0600, and the ticket is in it. Not parallel: the umask is the
// process's.
func TestSpaceMintJoinSetsTheModesUnderAnyUmask(t *testing.T) {
	for _, mask := range []int{0o400, 0o077} {
		t.Run(strconv.FormatInt(int64(mask), 8), func(t *testing.T) {
			h := newHarness(t)
			h.handle("POST /api/spaces/sp-123/join-token", 201, noScriptJSON)
			old := syscall.Umask(mask)
			err := h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false)
			syscall.Umask(old)
			mustOK(t, err, h.output())
			paths := joinTickets(t, h.env["TMPDIR"], "worker-1")
			if len(paths) != 1 {
				t.Fatalf("ticket files: %v", paths)
			}
			if st, err := os.Stat(paths[0]); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("umask %o: file mode want 0600: %v %v", mask, st, err)
			}
			if st, err := os.Stat(filepath.Dir(paths[0])); err != nil || st.Mode().Perm() != 0o700 {
				t.Fatalf("umask %o: directory mode want 0700: %v %v", mask, st, err)
			}
			if raw, err := os.ReadFile(paths[0]); err != nil || string(raw) != joinTicket+"\n" {
				t.Fatalf("umask %o: content %q %v", mask, raw, err)
			}
		})
	}
}

// The parts of the temp dir are read top-down, from "/" to the leaf (the
// Lstat seam logs each read): a part that passes lets nobody else swap an
// entry in it, so each part that was checked stays the same up to the
// create. A bottom-up walk would let the owner of an ancestor that is not
// checked yet replace it after the parts below it passed. The wanted list
// is built from the names of the path, not with filepath.Dir.
func TestJoinTempDirReadsThePartsTopDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leaf := modeDir(t, modeDir(t, modeDir(t, h.base, "td", 0o700), "a", 0o700), "b", 0o700)
	h.env["TMPDIR"] = leaf
	var got []string
	h.ctx.Lstat = func(p string) (fs.FileInfo, error) {
		got = append(got, p)
		return os.Lstat(p)
	}
	real, err := h.ctx.joinTempDir()
	mustOK(t, err, h.output())
	if real != realDir(t, leaf) {
		t.Fatalf("joinTempDir = %q, want %q", real, realDir(t, leaf))
	}
	want, acc := []string{"/"}, ""
	for name := range strings.SplitSeq(strings.TrimPrefix(real, "/"), "/") {
		acc += "/" + name
		want = append(want, acc)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the parts were read in this order:\n%q\nwant top-down:\n%q", got, want)
	}
}

// A refusal comes at the FIRST part that fails, and no part below it is
// read. Here a parent and the leaf below it are both writable by others:
// the message names the parent, it is the last part read, and the leaf is
// never read.
func TestJoinTempDirStopsAtTheFirstPartThatFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	parent := modeDir(t, h.base, "wp", 0o777)
	leaf := modeDir(t, parent, "wl", 0o777)
	h.env["TMPDIR"] = leaf
	var got []string
	h.ctx.Lstat = func(p string) (fs.FileInfo, error) {
		got = append(got, p)
		return os.Lstat(p)
	}
	_, err := h.ctx.joinTempDir()
	if !errors.Is(err, errTmpNotPrivate) {
		t.Fatalf("joinTempDir error = %v, want errTmpNotPrivate", err)
	}
	mustContain(t, h.output(), "the temp dir for the join ticket is not safe: "+realDir(t, parent)+" is writable by others and has no sticky bit\n")
	if len(got) == 0 || got[len(got)-1] != realDir(t, parent) {
		t.Fatalf("the last part read is not the refused parent: %q", got)
	}
	if slices.Contains(got, realDir(t, leaf)) {
		t.Fatalf("a part below the refused one was read: %q", got)
	}
}

// firstPartOwnedBy is the first part of the resolved path of dir, from the
// top, that uid owns: the part the top-down walk refuses when a seam makes
// the parts of uid untrusted.
func firstPartOwnedBy(t *testing.T, dir string, uid int) string {
	t.Helper()
	acc := ""
	for name := range strings.SplitSeq(strings.TrimPrefix(realDir(t, dir), "/"), "/") {
		acc += "/" + name
		fi, err := os.Lstat(acc)
		if err != nil {
			t.Fatal(err)
		}
		if owner, ok := fileOwner(fi); ok && owner == uid {
			return acc
		}
	}
	t.Fatalf("no part of %s belongs to uid %d", dir, uid)
	return ""
}

// In a setgid TMPDIR the fresh directory takes the setgid bit (2700). The
// writer sets it to 0700 before the check, as the bash twin does (chmod
// 00700): the ticket is written, and its directory has no setgid bit.
func TestSpaceMintJoinInASetgidTMPDIR(t *testing.T) {
	h := newHarness(t)
	tmp := h.env["TMPDIR"]
	if err := os.Chmod(tmp, 0o700|os.ModeSetgid); err != nil {
		t.Skipf("chmod g+s is refused here: %v", err)
	}
	probe := filepath.Join(tmp, "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(probe); err != nil || st.Mode()&os.ModeSetgid == 0 {
		t.Skip("a directory made in a setgid directory does not take the bit here")
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	h.handle("POST /api/spaces/sp-123/join-token", 201, noScriptJSON)
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
	paths := joinTickets(t, tmp, "worker-1")
	if len(paths) != 1 {
		t.Fatalf("ticket files: %v", paths)
	}
	bits := os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if st, err := os.Stat(filepath.Dir(paths[0])); err != nil || st.Mode()&bits != 0o700 {
		t.Fatalf("directory mode want 0700 and no setgid bit: %v %v", st, err)
	}
	if raw, err := os.ReadFile(paths[0]); err != nil || string(raw) != joinTicket+"\n" {
		t.Fatalf("content %q %v", raw, err)
	}
}

// The fresh directory must be ours: a directory of another user (the Euid
// seam) is refused with the text of the bash twin, and nothing goes in.
func TestWriteJoinIntoRefusesADirectoryOfAnotherUser(t *testing.T) {
	h := newHarness(t)
	priv := filepath.Join(h.base, "priv")
	if err := os.Mkdir(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	h.ctx.Euid = func() int { return os.Geteuid() + 1 }
	err := h.ctx.writeJoinInto(priv, "kubehz-join-worker-1.token", "content")
	if !errors.Is(err, errNotPrivateDir) || err.Error() != "not a private directory (mode 0700)" {
		t.Fatalf("want errNotPrivateDir, got %v", err)
	}
	if entries, rerr := os.ReadDir(priv); rerr != nil || len(entries) != 0 {
		t.Fatalf("the directory must stay empty: %v %v", entries, rerr)
	}
}

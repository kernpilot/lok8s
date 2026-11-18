//go:build !windows

package kubehz

import (
	"os"
	"path/filepath"
	"strconv"
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

// A TMPDIR whose path a foreign owner could change is refused before any
// directory is made: a part that others can write without the sticky bit
// (the TMPDIR itself or a parent), a part of another user (the Euid seam
// makes us someone else), and a link to such a directory. A part with the
// sticky bit passes, as /tmp does. The mint has happened, so the
// live-ticket note follows the reason.
func TestSpaceMintJoinRefusesATMPDIROthersCanChange(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, why string
		other     bool
		mk        func(t *testing.T, base string) (tmpdir, bad string)
	}{
		{"group-writable", "is writable by others and has no sticky bit", false, func(t *testing.T, base string) (string, string) {
			return modeDir(t, base, "gw", 0o770), filepath.Join(base, "gw")
		}},
		{"other-writable", "is writable by others and has no sticky bit", false, func(t *testing.T, base string) (string, string) {
			return modeDir(t, base, "ow", 0o707), filepath.Join(base, "ow")
		}},
		{"writable-parent", "is writable by others and has no sticky bit", false, func(t *testing.T, base string) (string, string) {
			parent := modeDir(t, base, "wp", 0o777)
			return modeDir(t, parent, "sub", 0o700), parent
		}},
		{"link-to-writable", "is writable by others and has no sticky bit", false, func(t *testing.T, base string) (string, string) {
			target := modeDir(t, base, "lw", 0o770)
			link := filepath.Join(base, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link, target
		}},
		{"another-user", "belongs to another user (uid " + strconv.Itoa(os.Geteuid()) + ")", true, func(t *testing.T, base string) (string, string) {
			return modeDir(t, base, "au", 0o700), filepath.Join(base, "au")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			tmpdir, bad := c.mk(t, h.base)
			h.env["TMPDIR"] = tmpdir
			if c.other {
				h.ctx.Euid = func() int { return os.Geteuid() + 1 }
			}
			h.handle("POST /api/spaces/sp-123/join-token", 201, noScriptJSON)
			mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false))
			mustContain(t, h.output(), "the temp dir for the join ticket is not safe: "+realDir(t, bad)+" "+c.why+"\n")
			mustContain(t, h.output(), "Set TMPDIR to a directory of your own that only you can write (mode 0700).")
			mustContain(t, h.output(), "could not write the join ticket for 'worker-1'\n")
			mustContain(t, h.output(), "The ticket was minted before the write failed")
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

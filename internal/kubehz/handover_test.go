package kubehz

// handover_test.go ports tests/unit/kubehz_handover_test.bats against a
// sandboxed /etc/kubernetes + /var/lib/etcd (the KUBEHZ_HANDOVER_* seams).
// Every node binary (etcdutl/etcdctl/kubeadm/kubectl/ip/aws/ssh/scp) is the
// fake Runner; the sandbox is t.TempDir.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"gopkg.in/yaml.v3"
)

type hoHarness struct {
	*harness
	bundle, snapshot, k8sDir, etcdDir string
	calls                             []string
}

func newHandover(t *testing.T) *hoHarness {
	h := newHarness(t)
	ho := &hoHarness{harness: h}
	ho.bundle = filepath.Join(h.base, "bundle")
	_ = os.MkdirAll(ho.bundle, 0o755)
	files := map[string]string{
		"ca.crt":             "-----BEGIN CERTIFICATE-----\ntest-cluster-ca\n-----END CERTIFICATE-----\n",
		"ca.key":             "-----BEGIN RSA PRIVATE KEY-----\ntest-ca-key\n-----END RSA PRIVATE KEY-----\n",
		"sa.pub":             "-----BEGIN PUBLIC KEY-----\ntest-sa-pub\n-----END PUBLIC KEY-----\n",
		"sa.key":             "-----BEGIN RSA PRIVATE KEY-----\ntest-sa-key\n-----END RSA PRIVATE KEY-----\n",
		"front-proxy-ca.crt": "-----BEGIN CERTIFICATE-----\ntest-fpca\n-----END CERTIFICATE-----\n",
		"front-proxy-ca.key": "-----BEGIN RSA PRIVATE KEY-----\ntest-fpca-key\n-----END RSA PRIVATE KEY-----\n",
		"encryption-key":     "dGVzdC1lbmNyeXB0aW9uLWtleQ==",
		"snapshot-location":  "s3://kubehz-backups/handover/cl-001/snap.db",
		"endpoint-dns":       "cl-001.kubermatic.kkp.kubehz.in.net",
	}
	for k, v := range files {
		_ = os.WriteFile(filepath.Join(ho.bundle, k), []byte(v), 0o644)
	}
	ho.snapshot = filepath.Join(h.base, "snap.db")
	_ = os.WriteFile(ho.snapshot, []byte("fake-etcd-snapshot"), 0o644)
	ho.k8sDir = filepath.Join(h.base, "etc-kubernetes")
	ho.etcdDir = filepath.Join(h.base, "var-lib-etcd")
	h.env["KUBEHZ_HANDOVER_K8S_DIR"] = ho.k8sDir
	h.env["KUBEHZ_HANDOVER_ETCD_DIR"] = ho.etcdDir
	h.env["KUBEHZ_HANDOVER_WAIT"] = "0"
	h.ctx.Hostname = func() (string, error) { return "Node-X", nil }
	return ho
}

// mockNodeBinaries is the happy path: every tool logs, etcdutl leaves a
// member dir behind (so "nothing was seeded" assertions are real).
func (ho *hoHarness) mockNodeBinaries(overrides map[string]func(c execx.Cmd) error) {
	ho.ctx.LookPath = func(tool string) bool { return tool == "etcdutl" || tool == "kubeadm" || tool == "kubectl" }
	ho.runner.handler = func(c execx.Cmd, _ string) error {
		ho.calls = append(ho.calls, argvLine(c))
		if fn, ok := overrides[c.Name]; ok {
			return fn(c)
		}
		switch c.Name {
		case "etcdutl":
			_ = os.MkdirAll(filepath.Join(ho.etcdDir, "member"), 0o755)
		case "ip":
			io.WriteString(c.Stdout, "1.1.1.1 via 10.9.8.1 dev eth0 src 10.9.8.7 uid 0\n")
		case "ssh":
			// The stage call of preseed prints a check line per dir, "stage"
			// and one staging name per key.
			if strings.HasPrefix(c.Args[len(c.Args)-1], preseedStagePrefix) {
				io.WriteString(c.Stdout, "check /etc/kubernetes 0 755 directory\ncheck /etc/kubernetes/pki 0 700 directory\nstage\n")
				for _, k := range handoverPKIKeys {
					io.WriteString(c.Stdout, preseedPKIDir+"/."+k+".Ab12Cd34Ef\n")
				}
			}
		}
		return nil
	}
}

func (ho *hoHarness) receive(o ReceiveOpts) error {
	if o.Bundle == "" {
		o.Bundle = ho.bundle
	}
	return ho.ctx.HandoverReceive(context.Background(), o)
}

func (ho *hoHarness) lineIndex(prefix string) int {
	for i, l := range ho.calls {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func tarBundle(t *testing.T, dir, out string) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		_ = tw.WriteHeader(&tar.Header{Name: "./" + e.Name(), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
}

// ── bundle validation ────────────────────────────────────

func TestHandoverValidateBundle(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	mustOK(t, ho.ctx.validateBundle(ho.bundle), ho.output())
	_ = os.Remove(filepath.Join(ho.bundle, "sa.key"))
	mustErr(t, ho.ctx.validateBundle(ho.bundle))
	mustContain(t, ho.output(), "missing or empty key: sa.key")
	ho.reset()
	_ = os.WriteFile(filepath.Join(ho.bundle, "sa.key"), []byte("k"), 0o644)
	_ = os.WriteFile(filepath.Join(ho.bundle, "encryption-key"), nil, 0o644)
	mustErr(t, ho.ctx.validateBundle(ho.bundle))
	mustContain(t, ho.output(), "missing or empty key: encryption-key")
}

func TestHandoverResolveBundleUnpacksTarball(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	tarball := filepath.Join(ho.base, "bundle.tar.gz")
	tarBundle(t, ho.bundle, tarball)
	dir, err := ho.ctx.resolveBundle(tarball, filepath.Join(ho.base, "work"))
	mustOK(t, err, ho.output())
	if !fsutil.Exists(filepath.Join(dir, "ca.crt")) || !fsutil.Exists(filepath.Join(dir, "endpoint-dns")) {
		t.Fatal("bundle not extracted")
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("private dir mode %o", info.Mode().Perm())
	}
}

func TestHandoverResolveBundleTraversalGuard(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"/abs/path", "..", "../x", "a/../b", "a/..", "./../x", "./..", "x/../"} {
		if !bundleEntryEscapes(entry) {
			t.Fatalf("%q must be refused", entry)
		}
	}
	for _, entry := range []string{"foo../bar", "a/..foo", "x..", "ok/path"} {
		if bundleEntryEscapes(entry) {
			t.Fatalf("%q is a literal dot-name, not a traversal", entry)
		}
	}
	// A real ../-prefixed archive entry is refused before any byte lands.
	ho := newHandover(t)
	evil := filepath.Join(ho.base, "evil.tar.gz")
	f, _ := os.Create(evil)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../escaped", Mode: 0o644, Size: 6, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("pwned\n"))
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	_, err := ho.ctx.resolveBundle(evil, filepath.Join(ho.base, "work"))
	mustErr(t, err)
	mustContain(t, ho.output(), "escapes the bundle dir")
	if fsutil.Exists(filepath.Join(ho.base, "escaped")) || fsutil.Exists(filepath.Join(ho.base, "work", "escaped")) {
		t.Fatal("entry escaped")
	}
}

// A failed write of an entry is reported as the write it is, not as an
// unreadable archive. /dev/full answers every write with ENOSPC.
func TestHandoverExtractBundleNamesAFailedWrite(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full is not available")
	}
	ho := newHandover(t)
	tarball := filepath.Join(ho.base, "bundle.tar.gz")
	tarBundle(t, ho.bundle, tarball)
	dir := filepath.Join(ho.base, "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	mustErr(t, ho.ctx.extractBundle(tarball, dir))
	mustContain(t, ho.output(), "handover: cannot write "+filepath.Join(dir, "ca.crt")+" from "+tarball)
	if strings.Contains(ho.output(), "neither a bundle directory") {
		t.Fatalf("a write failure was reported as an unreadable archive:\n%s", ho.output())
	}
	// A truncated archive is still the archive's fault.
	raw, _ := os.ReadFile(tarball)
	truncated := filepath.Join(ho.base, "truncated.tar.gz")
	_ = os.WriteFile(truncated, raw[:len(raw)/2], 0o600)
	ho.reset()
	mustErr(t, ho.ctx.extractBundle(truncated, filepath.Join(ho.base, "work2")))
	mustContain(t, ho.output(), "neither a bundle directory nor a readable .tar.gz archive")
}

// Only a write error on the target names the write. A read error on the
// bundle file is also a *fs.PathError, and that one is the archive's fault.
func TestHandoverExtractCopyErrorClassifiesByOperation(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	bundle := filepath.Join(ho.base, "bundle.tar.gz")
	target := filepath.Join(ho.base, "work", "ca.crt")

	mustErr(t, ho.ctx.extractCopyError(bundle, target, &fs.PathError{Op: "write", Path: target, Err: errors.New("no space left on device")}))
	mustContain(t, ho.output(), "handover: cannot write "+target+" from "+bundle)

	ho.reset()
	mustErr(t, ho.ctx.extractCopyError(bundle, target, &fs.PathError{Op: "read", Path: bundle, Err: errors.New("input/output error")}))
	mustContain(t, ho.output(), "neither a bundle directory nor a readable .tar.gz archive")
	if strings.Contains(ho.output(), "cannot write") {
		t.Fatalf("a read error on the bundle was reported as a write failure:\n%s", ho.output())
	}
}

// tarEntry is one header of writeTarGz; a regular entry gets body as its
// content.
type tarEntry struct {
	hdr  tar.Header
	body string
}

func writeTarGz(t *testing.T, out string, entries ...tarEntry) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := e.hdr
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []io.Closer{tw, gz, f} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A link entry can name a file outside the bundle, and preseed would send
// that file to the node. A symlink with an entry through it can write
// outside the dir. Each entry that is not a regular file or a directory
// refuses the whole bundle before a byte lands, and the dir goes.
func TestHandoverResolveBundleRefusesLinkEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		entries func(outside string) []tarEntry
	}{
		{"symlink", func(outside string) []tarEntry {
			return []tarEntry{
				{hdr: tar.Header{Name: "./ca.crt", Typeflag: tar.TypeReg}, body: "x\n"},
				{hdr: tar.Header{Name: "./ca.key", Linkname: filepath.Join(outside, "secret"), Typeflag: tar.TypeSymlink}},
			}
		}},
		{"hardlink", func(_ string) []tarEntry {
			return []tarEntry{
				{hdr: tar.Header{Name: "./ca.crt", Typeflag: tar.TypeReg}, body: "x\n"},
				{hdr: tar.Header{Name: "./sa.key", Linkname: "./ca.crt", Typeflag: tar.TypeLink}},
			}
		}},
		{"symlink then a file through it", func(outside string) []tarEntry {
			return []tarEntry{
				{hdr: tar.Header{Name: "./x", Linkname: outside, Typeflag: tar.TypeSymlink}},
				{hdr: tar.Header{Name: "./x/payload", Typeflag: tar.TypeReg}, body: "pwned\n"},
			}
		}},
		{"fifo", func(_ string) []tarEntry {
			return []tarEntry{{hdr: tar.Header{Name: "./ca.key", Typeflag: tar.TypeFifo}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ho := newHandover(t)
			outside := filepath.Join(ho.base, "outside")
			if err := os.MkdirAll(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-secret\n"), 0o600)
			tarball := filepath.Join(ho.base, "bundle.tar.gz")
			writeTarGz(t, tarball, tc.entries(outside)...)
			work := filepath.Join(ho.base, "work")
			_ = os.MkdirAll(work, 0o700)
			_, err := ho.ctx.resolveBundle(tarball, work)
			mustErr(t, err)
			mustContain(t, ho.output(), "handover: refusing "+tarball+": an archive entry is not a regular file or a directory (a link, a device or a FIFO)")
			if _, err := os.Lstat(filepath.Join(work, "bundle")); err == nil {
				t.Fatal("the bundle dir was not removed")
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 1 || readFile(t, filepath.Join(outside, "secret")) != "outside-secret\n" {
				t.Fatalf("the dir outside the bundle changed: %v", entries)
			}
		})
	}
}

// A directory the user gives is read as it is. A key in it that is a link
// (to a file outside the bundle) is refused before anything reads or sends
// it: by preseed and by receive, for a required key and an optional one.
func TestHandoverRefusesABundleKeyThatIsALink(t *testing.T) {
	for _, tc := range []struct {
		kind, key string
	}{{"symlink", "ca.key"}, {"symlink", "encryption-provider"}, {"hard link", "ca.key"}, {"hard link", "encryption-provider"}} {
		key := tc.key
		t.Run(tc.kind+"/"+key, func(t *testing.T) {
			ho := newHandover(t)
			ho.mockNodeBinaries(nil)
			elsewhere := filepath.Join(ho.base, "elsewhere")
			_ = os.WriteFile(elsewhere, []byte("outside-secret-content\n"), 0o600)
			path := filepath.Join(ho.bundle, key)
			_ = os.Remove(path)
			link, want := os.Symlink, "handover: bundle key "+key+" is not a regular file (a link, a directory or a special file): "+path
			if tc.kind == "hard link" {
				// A second name of a file outside the bundle.
				link, want = os.Link, "handover: bundle key "+key+" has more than one name (a hard link), so it can be a file outside the bundle: "+path
			}
			if err := link(elsewhere, path); err != nil {
				t.Fatal(err)
			}

			mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
			mustContain(t, ho.output(), want)
			ho.reset()
			mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
			mustContain(t, ho.output(), want)
			mustNotContain(t, ho.output(), "outside-secret-content")
			if len(ho.calls) != 0 || fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) {
				t.Fatalf("a linked key went further: %v", ho.calls)
			}
		})
	}
	// A key that is a directory is not a regular file either.
	ho := newHandover(t)
	_ = os.Remove(filepath.Join(ho.bundle, "ca.key"))
	_ = os.MkdirAll(filepath.Join(ho.bundle, "ca.key"), 0o700)
	mustErr(t, ho.ctx.validateBundle(ho.bundle))
	mustContain(t, ho.output(), "handover: bundle key ca.key is not a regular file")
}

// Extracted files are 0600 and dirs 0700, whatever modes the archive gives.
func TestHandoverExtractBundleWritesPrivateModes(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	tarball := filepath.Join(ho.base, "bundle.tar.gz")
	writeTarGz(t, tarball,
		tarEntry{hdr: tar.Header{Name: "./", Mode: 0o777, Typeflag: tar.TypeDir}},
		tarEntry{hdr: tar.Header{Name: "./sub/", Mode: 0o755, Typeflag: tar.TypeDir}},
		tarEntry{hdr: tar.Header{Name: "./ca.key", Mode: 0o644, Typeflag: tar.TypeReg}, body: "k\n"},
		tarEntry{hdr: tar.Header{Name: "./sub/f", Mode: 0o666, Typeflag: tar.TypeReg}, body: "f\n"},
	)
	work := filepath.Join(ho.base, "work")
	_ = os.MkdirAll(work, 0o700)
	dir, err := ho.ctx.resolveBundle(tarball, work)
	mustOK(t, err, ho.output())
	for path, want := range map[string]fs.FileMode{dir: 0o700, filepath.Join(dir, "sub"): 0o700,
		filepath.Join(dir, "ca.key"): 0o600, filepath.Join(dir, "sub", "f"): 0o600} {
		if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: mode %v, want %o (%v)", path, info.Mode(), want, err)
		}
	}
}

// The private dirs hold the bundle's keys. A chmod that fails, or a file
// system that leaves the dir open, stops the handover before anything
// lands in it, and the dir goes (TMPDIR is empty after).
func TestHandoverPrivateDirsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chmod func(string, fs.FileMode) error
		want  string
	}{
		{"chmod fails", func(string, fs.FileMode) error { return errors.New("operation not permitted") },
			"to mode 700, so the bundle's keys would not be private in it. Check the file system of TMPDIR, then retry."},
		{"dir stays open", func(p string, _ fs.FileMode) error { return os.Chmod(p, 0o755) }, // #nosec G302 -- a test dir that must stay open
			"has mode 755, not 700, so other users could read the bundle's keys in it."},
	} {
		for _, cmd := range []string{"receive", "preseed"} {
			t.Run(tc.name+"/"+cmd, func(t *testing.T) {
				ho := newHandover(t)
				ho.mockNodeBinaries(nil)
				scratch := filepath.Join(ho.base, "scratch")
				_ = os.MkdirAll(scratch, 0o700)
				t.Setenv("TMPDIR", scratch)
				ho.ctx.Chmod = tc.chmod
				var err error
				if cmd == "receive" {
					err = ho.receive(ReceiveOpts{Snapshot: ho.snapshot})
				} else {
					err = ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"})
				}
				mustErr(t, err)
				mustContain(t, ho.output(), scratch+"/")
				mustContain(t, ho.output(), tc.want)
				if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
					t.Fatalf("scratch left behind: %v", entries)
				}
				if len(ho.calls) != 0 {
					t.Fatalf("ran with a dir that is not private: %v", ho.calls)
				}
			})
		}
	}
	// The bundle dir of a .tar.gz gets the same check before the unpack.
	ho := newHandover(t)
	tarball := filepath.Join(ho.base, "bundle.tar.gz")
	tarBundle(t, ho.bundle, tarball)
	work := filepath.Join(ho.base, "work")
	_ = os.MkdirAll(work, 0o700)
	ho.ctx.Chmod = func(p string, m fs.FileMode) error {
		if p == filepath.Join(work, "bundle") {
			return os.Chmod(p, 0o755) // #nosec G302 -- a test dir that must stay open
		}
		return os.Chmod(p, m)
	}
	_, err := ho.ctx.resolveBundle(tarball, work)
	mustErr(t, err)
	mustContain(t, ho.output(), "handover: "+filepath.Join(work, "bundle")+" has mode 755, not 700")
	if _, err := os.Lstat(filepath.Join(work, "bundle")); err == nil {
		t.Fatal("the open bundle dir was not removed")
	}
}

// Each chmod of placePKI on its own: the pki dir, then the new file of
// each of the six PKI files (before its rename). The step stops, and the
// files that it placed go: no key is left.
func TestHandoverPlacePKIStopsAtEachChmod(t *testing.T) {
	t.Parallel()
	for _, target := range append([]string{"pki"}, handoverPKIKeys...) {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			ho := newHandover(t)
			pki := filepath.Join(ho.k8sDir, "pki")
			ho.ctx.Chmod = func(p string, m fs.FileMode) error {
				if (target == "pki" && p == pki) || (filepath.Dir(p) == pki && strings.HasPrefix(filepath.Base(p), "."+target+".")) {
					return errors.New("operation not permitted")
				}
				return os.Chmod(p, m)
			}
			mustErr(t, ho.ctx.placePKI(ho.bundle, ho.k8sDir))
			if target == "pki" {
				mustContain(t, ho.output(), "handover: cannot set "+pki+" to mode 700, so the keys would not be private in it")
			} else {
				mustContain(t, ho.output(), "handover: could not place "+target+" into "+pki+" with mode")
			}
			if entries, err := os.ReadDir(pki); err != nil || len(entries) != 0 {
				t.Fatalf("left in %s: %v (%v)", pki, entries, err)
			}
		})
	}
}

// A chmod that returns nil but leaves the pki dir open (a file system that
// keeps the bits) fails closed too: placePKI reads the mode after it.
func TestHandoverPlacePKIReadsTheModeOfThePKIDir(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	pki := filepath.Join(ho.k8sDir, "pki")
	ho.ctx.Chmod = func(p string, m fs.FileMode) error {
		if p == pki {
			return os.Chmod(p, 0o755) // #nosec G302 -- a test dir that must stay open
		}
		return os.Chmod(p, m)
	}
	mustErr(t, ho.ctx.placePKI(ho.bundle, ho.k8sDir))
	mustContain(t, ho.output(), "handover: cannot set "+pki+" to mode 700, so the keys would not be private in it")
	if entries, err := os.ReadDir(pki); err != nil || len(entries) != 0 {
		t.Fatalf("left in %s: %v (%v)", pki, entries, err)
	}
}

// kubeadm makes /etc/kubernetes/pki 0755, and a run with --force finds the
// files of an earlier run. The dir is 0700 before a key lands in it, and
// each file is replaced, not written into: a key of mode 0644 ends 0600, a
// second name of it outside keeps the old bytes, and a link at a key name
// is replaced, not followed.
func TestHandoverPlacePKIReplacesAnOpenDirAndItsFiles(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	pki := filepath.Join(ho.k8sDir, "pki")
	outside := filepath.Join(ho.base, "outside")
	for _, d := range []string{pki, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldKey := filepath.Join(pki, "ca.key")
	if err := os.WriteFile(oldKey, []byte("old-ca-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]fs.FileMode{pki: 0o755, oldKey: 0o644} {
		if err := os.Chmod(path, mode); err != nil { // #nosec G302 -- the open modes under test
			t.Fatal(err)
		}
	}
	if err := os.Link(oldKey, filepath.Join(outside, "second-name")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(pki, "sa.key")); err != nil {
		t.Fatal(err)
	}
	mustOK(t, ho.ctx.placePKI(ho.bundle, ho.k8sDir), ho.output())
	if info, err := os.Lstat(pki); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s: %v (%v), want 0700", pki, info.Mode(), err)
	}
	for _, key := range handoverPKIKeys {
		want := fs.FileMode(0o644)
		if strings.HasSuffix(key, ".key") {
			want = 0o600
		}
		path := filepath.Join(pki, key)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != want || !singleLink(info) {
			t.Fatalf("%s: %v (%v), want a regular file of mode %o with one name", key, info.Mode(), err, want)
		}
		if readFile(t, path) != readFile(t, filepath.Join(ho.bundle, key)) {
			t.Fatalf("%s does not hold the bytes of the bundle", key)
		}
	}
	if readFile(t, filepath.Join(outside, "second-name")) != "old-ca-key\n" || readFile(t, filepath.Join(outside, "secret")) != "outside-secret\n" {
		t.Fatal("a file outside the pki dir changed")
	}
	if entries, _ := os.ReadDir(pki); len(entries) != len(handoverPKIKeys) {
		t.Fatalf("a new file is left in %s: %v", pki, entries)
	}
}

// The encryption config holds the key: it is written 0600 from the first
// byte, also over a file of an earlier run with mode 0644. A chmod that
// fails stops the step and leaves no file.
func TestHandoverWriteEncryptionConfigReplacesAnOpenFile(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	out := filepath.Join(ho.k8sDir, "kubehz-encryption-config.yaml")
	if err := os.MkdirAll(ho.k8sDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o644); err != nil { // #nosec G302 -- the open mode under test
		t.Fatal(err)
	}
	got, err := ho.ctx.writeEncryptionConfig(ho.bundle, ho.k8sDir)
	mustOK(t, err, ho.output())
	if info, err := os.Lstat(got); got != out || err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v (%v), want %s with mode 0600", got, info.Mode(), err, out)
	}
	// The key comes from the bundle file, so no key-like literal is in the test.
	mustContain(t, readFile(t, out), "secret: "+readFile(t, filepath.Join(ho.bundle, "encryption-key")))

	_ = os.Remove(out)
	ho.reset()
	ho.ctx.Chmod = func(p string, m fs.FileMode) error {
		if filepath.Dir(p) == ho.k8sDir && strings.HasPrefix(filepath.Base(p), ".kubehz-encryption-config.yaml.") {
			return errors.New("operation not permitted")
		}
		return os.Chmod(p, m)
	}
	_, err = ho.ctx.writeEncryptionConfig(ho.bundle, ho.k8sDir)
	mustErr(t, err)
	mustContain(t, ho.output(), "handover: could not write "+out+" with mode 600")
	if entries, _ := os.ReadDir(ho.k8sDir); len(entries) != 0 {
		t.Fatalf("a file is left in %s: %v", ho.k8sDir, entries)
	}
}

// A chmod that fails at the workdir or at the bundle dir of a .tar.gz
// stops receive and preseed before anything runs, and TMPDIR is empty.
func TestHandoverStopsAtTheWorkdirOrTheBundleDirChmod(t *testing.T) {
	for _, at := range []string{"workdir", "bundle"} {
		for _, cmd := range []string{"receive", "preseed"} {
			t.Run(at+"/"+cmd, func(t *testing.T) {
				ho := newHandover(t)
				ho.mockNodeBinaries(nil)
				scratch := filepath.Join(ho.base, "scratch")
				_ = os.MkdirAll(scratch, 0o700)
				t.Setenv("TMPDIR", scratch)
				tarball := filepath.Join(ho.base, "bundle.tar.gz")
				tarBundle(t, ho.bundle, tarball)
				ho.ctx.Chmod = func(p string, m fs.FileMode) error {
					if (at == "workdir" && filepath.Dir(p) == scratch) ||
						(at == "bundle" && filepath.Base(p) == "bundle" && filepath.Dir(filepath.Dir(p)) == scratch) {
						return errors.New("operation not permitted")
					}
					return os.Chmod(p, m)
				}
				var err error
				if cmd == "receive" {
					err = ho.receive(ReceiveOpts{Bundle: tarball, Snapshot: ho.snapshot})
				} else {
					err = ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: tarball, Node: "203.0.113.7"})
				}
				mustErr(t, err)
				mustContain(t, ho.output(), "handover: cannot set "+scratch+"/")
				mustContain(t, ho.output(), " to mode 700, so the bundle's keys would not be private in it.")
				if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
					t.Fatalf("scratch left behind: %v", entries)
				}
				if len(ho.calls) != 0 || fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) {
					t.Fatalf("ran with a dir that is not private: %v", ho.calls)
				}
			})
		}
	}
}

// The downloaded snapshot is 0600 before its first byte (touch600) and
// after the aws CLI wrote it. A chmod that fails at either stops the
// receive before the restore, and the scratch goes.
func TestHandoverReceiveStopsAtEachSnapshotChmod(t *testing.T) {
	for _, nth := range []int{1, 2} {
		t.Run(fmt.Sprint(nth), func(t *testing.T) {
			ho := newHandover(t)
			ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{"aws": func(c execx.Cmd) error {
				return os.WriteFile(c.Args[len(c.Args)-1], []byte("snapshot-bytes"), 0o600)
			}})
			lookPath := ho.ctx.LookPath
			ho.ctx.LookPath = func(tool string) bool { return tool == "aws" || lookPath(tool) }
			scratch := filepath.Join(ho.base, "scratch")
			_ = os.MkdirAll(scratch, 0o700)
			t.Setenv("TMPDIR", scratch)
			seen := 0
			ho.ctx.Chmod = func(p string, m fs.FileMode) error {
				if filepath.Base(p) == "kubehz-handover-snapshot.db" {
					if seen++; seen == nth {
						return errors.New("operation not permitted")
					}
				}
				return os.Chmod(p, m)
			}
			mustErr(t, ho.receive(ReceiveOpts{}))
			if seen != nth {
				t.Fatalf("the snapshot got %d chmod calls, want %d", seen, nth)
			}
			if downloaded := ho.lineIndex("aws ") >= 0; downloaded != (nth == 2) {
				t.Fatalf("download ran: %v, want %v: %v", downloaded, nth == 2, ho.calls)
			}
			for _, tool := range []string{"etcdutl ", "etcdctl ", "kubeadm "} {
				if ho.lineIndex(tool) >= 0 {
					t.Fatalf("ran past a failed chmod of the snapshot: %v", ho.calls)
				}
			}
			if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
				t.Fatalf("scratch left behind: %v", entries)
			}
		})
	}
}

// A removal that fails names the dir: it can hold the bundle's keys.
func TestHandoverRemoveDirNamesADirItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a dir without owner write")
	}
	t.Parallel()
	ho := newHandover(t)
	dir := filepath.Join(ho.base, "work")
	held := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(held, "ca.key"), []byte("k\n"), 0o600)
	if err := os.Chmod(held, 0o500); err != nil { // #nosec G302 -- a dir without owner write, on purpose
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(held, 0o700) }) // #nosec G302 -- lets t.TempDir remove it
	ho.ctx.removeDir(dir)
	mustContain(t, ho.output(), "handover: could not remove "+dir+", which can hold the bundle's keys. Delete it yourself.")
	ho.reset()
	_ = os.Chmod(held, 0o700) // #nosec G302 -- a private dir
	ho.ctx.removeDir(dir)
	mustNotContain(t, ho.output(), "could not remove")
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s is still there (%v)", dir, err)
	}
}

// ── receive ──────────────────────────────────────────────

func TestHandoverReceiveRunsInOrder(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	mustContain(t, ho.output(), "handover: verified")
	mustContain(t, ho.output(), "handover: receive complete")

	probe := "kubectl --kubeconfig " + ho.k8sDir + "/admin.conf --server https://127.0.0.1:6443 --tls-server-name cl-001.kubermatic.kkp.kubehz.in.net get ns kube-system"
	if ho.lineIndex(probe) < 0 {
		t.Fatalf("verify probe missing:\n%s", strings.Join(ho.calls, "\n"))
	}
	restore, init, verify := ho.lineIndex("etcdutl snapshot restore"), ho.lineIndex("kubeadm init"), ho.lineIndex("kubectl --kubeconfig")
	if restore < 0 || init < 0 || verify < 0 || restore >= init || init >= verify {
		t.Fatalf("order restore=%d init=%d verify=%d", restore, init, verify)
	}
	if ho.lineIndex("etcdutl snapshot restore "+ho.snapshot+" --data-dir "+ho.etcdDir) < 0 {
		t.Fatal("restore args")
	}
	// The whole argv, not a prefix, and one init: an argument added after
	// --skip-token-print, or a second init, fails here.
	var inits []string
	for _, l := range ho.calls {
		if strings.HasPrefix(l, "kubeadm init") {
			inits = append(inits, l)
		}
	}
	if want := "kubeadm init --config " + ho.k8sDir + "/kubehz-handover-kubeadm.yaml --ignore-preflight-errors=DirAvailable--var-lib-etcd --skip-phases=addon/coredns,addon/kube-proxy --skip-token-print"; len(inits) != 1 || inits[0] != want {
		t.Fatalf("kubeadm init argv:\n%s\nwant exactly:\n%s", strings.Join(inits, "\n"), want)
	}
	for _, k := range []string{"ca.crt", "sa.key", "front-proxy-ca.key"} {
		if readFile(t, filepath.Join(ho.bundle, k)) != readFile(t, filepath.Join(ho.k8sDir, "pki", k)) {
			t.Fatalf("%s not seeded byte-identical", k)
		}
	}
	if info, _ := os.Stat(filepath.Join(ho.k8sDir, "pki", "ca.key")); info.Mode().Perm() != 0o600 {
		t.Fatal("ca.key mode")
	}
}

func TestHandoverReceiveGeneratedConfigsMatchBashGoldens(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	cfg := readFile(t, filepath.Join(ho.k8sDir, "kubehz-handover-kubeadm.yaml"))
	// The golden holds the real /etc/kubernetes; the test dir stands in.
	want := strings.ReplaceAll(golden(t, "kubeadm-config.yaml", strings.ReplaceAll(cfg, ho.k8sDir, "/etc/kubernetes")), "/etc/kubernetes", ho.k8sDir)
	if cfg != want {
		t.Fatalf("kubeadm config drift (run: go test ./internal/kubehz/ -update):\n%s\nwant:\n%s", cfg, want)
	}
	enc := filepath.Join(ho.k8sDir, "kubehz-encryption-config.yaml")
	if readFile(t, enc) != golden(t, "encryption-config.yaml", readFile(t, enc)) {
		t.Fatalf("encryption config drift (run: go test ./internal/kubehz/ -update):\n%s", readFile(t, enc))
	}
	if info, _ := os.Stat(enc); info.Mode().Perm() != 0o600 {
		t.Fatal("encryption config mode")
	}
}

// kubeadm init creates a bootstrap token with a TTL of 24 h by default. The
// handover gives it 1 h: no later step of the handover uses the token. The
// config is read as YAML, so a TTL in the wrong document fails too.
func TestHandoverReceiveBootstrapTokenTTLIsOneHour(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	dec := yaml.NewDecoder(strings.NewReader(readFile(t, filepath.Join(ho.k8sDir, "kubehz-handover-kubeadm.yaml"))))
	var kinds, ttls []string
	for {
		var doc struct {
			Kind            string `yaml:"kind"`
			BootstrapTokens []struct {
				TTL string `yaml:"ttl"`
			} `yaml:"bootstrapTokens"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the kubeadm config is not YAML: %v", err)
		}
		kinds = append(kinds, doc.Kind)
		if doc.Kind == "InitConfiguration" {
			for _, bt := range doc.BootstrapTokens {
				ttls = append(ttls, bt.TTL)
			}
		}
	}
	if got := strings.Join(kinds, ","); got != "ClusterConfiguration,InitConfiguration" {
		t.Fatalf("kubeadm config documents: %s", got)
	}
	if len(ttls) != 1 {
		t.Fatalf("want one bootstrap token, got %d", len(ttls))
	}
	if d, err := time.ParseDuration(ttls[0]); err != nil || d != time.Hour {
		t.Fatalf("bootstrap token ttl %q, want 1h", ttls[0])
	}
}

func TestHandoverReceiveSingleNodeUntaintsLocally(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot, SingleNode: true}), ho.output())
	i := ho.lineIndex("kubectl --kubeconfig " + ho.k8sDir + "/admin.conf --server https://127.0.0.1:6443 --tls-server-name cl-001.kubermatic.kkp.kubehz.in.net taint nodes --all")
	if i < 0 {
		t.Fatalf("untaint not pinned to the local apiserver:\n%s", strings.Join(ho.calls, "\n"))
	}
}

func TestHandoverReceiveEtcdVersionPin(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	_ = os.WriteFile(filepath.Join(ho.bundle, "etcd-version"), []byte("3.5.99-0\n"), 0o644)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	cfg := readFile(t, filepath.Join(ho.k8sDir, "kubehz-handover-kubeadm.yaml"))
	mustContain(t, cfg, `imageTag: "3.5.99-0"`)
	mustNotContain(t, cfg, "3.5.21-0")

	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	_ = os.WriteFile(filepath.Join(ho.bundle, "etcd-version"), []byte("3.5.99-0\n"), 0o644)
	ho.env["KUBEHZ_HANDOVER_ETCD_IMAGE_TAG"] = "3.5.88-0"
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	mustContain(t, readFile(t, filepath.Join(ho.k8sDir, "kubehz-handover-kubeadm.yaml")), `imageTag: "3.5.88-0"`)
	mustContain(t, ho.output(), "overrides the bundle's etcd-version '3.5.99-0'")

	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	_ = os.WriteFile(filepath.Join(ho.bundle, "etcd-version"), []byte("3.5.21-0\" evil\n"), 0o644)
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "not a plain image tag")
	if fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("seeded before the tag check")
	}

	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	ho.env["KUBEHZ_HANDOVER_ETCD_IMAGE_TAG"] = "3.5.21-0\" evil"
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "not a plain image tag")
	if fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("seeded before the tag check")
	}
}

func TestHandoverReceiveRefusesTamperedBundleBeforeMutation(t *testing.T) {
	cases := []struct{ key, content, want string }{
		{"encryption-provider", "kms\n", "is not one of secretbox/aescbc/aesgcm"},
		{"encryption-key-name", "k1\"\n            - name: pwn\n", "is not a plain name"},
		{"encryption-key", "k3y\"\ninjected: yaml\n", "is not base64"},
		{"endpoint-dns", "evil.example.com\"\n  extraArgs: pwn\n", "not a plain hostname"},
	}
	for _, tc := range cases {
		ho := newHandover(t)
		ho.mockNodeBinaries(nil)
		_ = os.WriteFile(filepath.Join(ho.bundle, tc.key), []byte(tc.content), 0o644)
		mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
		mustContain(t, ho.output(), tc.want)
		if fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) || fsutil.Exists(filepath.Join(ho.etcdDir, "member")) {
			t.Fatalf("%s: node mutated before the check", tc.key)
		}
	}
}

func TestHandoverReceiveBundleCarriedProviderAndKeyName(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	_ = os.WriteFile(filepath.Join(ho.bundle, "encryption-provider"), []byte("aesgcm\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ho.bundle, "encryption-key-name"), []byte("src-key-2\n"), 0o644)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	enc := readFile(t, filepath.Join(ho.k8sDir, "kubehz-encryption-config.yaml"))
	mustContain(t, enc, "- aesgcm:")
	mustContain(t, enc, "name: src-key-2")
	mustNotContain(t, enc, "secretbox")
	mustNotContain(t, enc, "kubehz-key-1")
}

func TestHandoverBundleValue(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	_ = os.WriteFile(filepath.Join(ho.bundle, "encryption-provider"), []byte("aes cbc\n"), 0o644)
	if v := bundleValue(ho.bundle, "encryption-provider", "secretbox"); v != "aes cbc" {
		t.Fatalf("interior whitespace welded: %q", v)
	}
	_, _, err := ho.ctx.encryptionMetadata(ho.bundle)
	mustErr(t, err)
	mustContain(t, ho.output(), "is not one of secretbox/aescbc/aesgcm")
	_ = os.WriteFile(filepath.Join(ho.bundle, "etcd-version"), []byte("-n\n"), 0o644)
	if v := bundleValue(ho.bundle, "etcd-version", "3.5.21-0"); v != "-n" {
		t.Fatalf("echo-flag value lost: %q", v)
	}
	_ = os.WriteFile(filepath.Join(ho.bundle, "etcd-version"), []byte("3.5.21-0\n"), 0o644)
	if v := bundleValue(ho.bundle, "etcd-version", "fallback"); v != "3.5.21-0" {
		t.Fatalf("%q", v)
	}
	if v := bundleValue(ho.bundle, "nosuchkey", "the-default"); v != "the-default" {
		t.Fatalf("%q", v)
	}
	_ = os.WriteFile(filepath.Join(ho.bundle, "encryption-provider"), []byte("\n"), 0o644)
	_, err = ho.ctx.writeEncryptionConfig(ho.bundle, ho.k8sDir)
	mustOK(t, err, ho.output())
	mustContain(t, readFile(t, filepath.Join(ho.k8sDir, "kubehz-encryption-config.yaml")), "- secretbox:")
}

func TestHandoverReceiveCAMismatchFailsVerify(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{
		"kubeadm": func(c execx.Cmd) error {
			return os.WriteFile(filepath.Join(ho.k8sDir, "pki", "ca.crt"), []byte("freshly-minted-DIFFERENT-ca\n"), 0o644)
		},
	})
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "CA hash mismatch")
	mustContain(t, ho.output(), "do NOT cut over")
}

func TestHandoverReceiveNonEmptyGuards(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	_ = os.MkdirAll(ho.k8sDir, 0o755)
	_ = os.WriteFile(filepath.Join(ho.k8sDir, "kubelet.conf"), nil, 0o644)
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "holds existing content")
	mustContain(t, ho.output(), ho.k8sDir+"/kubelet.conf")
	mustContain(t, ho.output(), "--force")
	if len(ho.calls) != 0 {
		t.Fatal("nothing may run against the node")
	}

	// --force flips the guard.
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot, Force: true}), ho.output())

	// kubeadm's empty package-skeleton dirs do NOT block.
	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	_ = os.MkdirAll(filepath.Join(ho.k8sDir, "manifests"), 0o755)
	_ = os.MkdirAll(filepath.Join(ho.k8sDir, "pki"), 0o755)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())

	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	_ = os.MkdirAll(filepath.Join(ho.etcdDir, "member"), 0o755)
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), ho.etcdDir+" is not empty — refusing to restore over existing etcd data (re-run with --force to overwrite).")
}

func TestHandoverReceiveRestoreRewritesMemberIdentity(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	restore := ho.calls[ho.lineIndex("etcdutl snapshot restore")]
	for _, w := range []string{"--skip-hash-check=true", "--name node-x", "--initial-cluster node-x=https://10.9.8.7:2380", "--initial-advertise-peer-urls https://10.9.8.7:2380"} {
		mustContain(t, restore, w)
	}
}

func TestHandoverReceiveEtcdctlFallback(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	ho.ctx.LookPath = func(tool string) bool { return tool == "etcdctl" }
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
	i := ho.lineIndex("etcdctl snapshot restore " + ho.snapshot + " --data-dir " + ho.etcdDir)
	if i < 0 {
		t.Fatalf("etcdctl not used:\n%s", strings.Join(ho.calls, "\n"))
	}
	mustContain(t, ho.calls[i], "--skip-hash-check=true")
	mustContain(t, ho.calls[i], "--initial-cluster node-x=https://10.9.8.7:2380")
	var env []string
	for _, c := range ho.runner.calls {
		if c.Name == "etcdctl" {
			env = c.Env
		}
	}
	if strings.Join(env, ",") != "ETCDCTL_API=3" {
		t.Fatalf("ETCDCTL_API env: %v", env)
	}
	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	ho.ctx.LookPath = func(string) bool { return false }
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "neither etcdutl nor etcdctl found")
}

func TestHandoverReceiveFailsBeforeMutation(t *testing.T) {
	ho := newHandover(t)
	_ = os.Remove(filepath.Join(ho.bundle, "front-proxy-ca.crt"))
	ho.mockNodeBinaries(nil)
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "missing or empty key: front-proxy-ca.crt")
	if len(ho.calls) != 0 || fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("mutated")
	}

	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	mustErr(t, ho.receive(ReceiveOpts{}))
	mustContain(t, ho.output(), "s3://kubehz-backups/handover/cl-001/snap.db")
	mustContain(t, ho.output(), "--snapshot")

	ho = newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{"ip": func(c execx.Cmd) error { return exitErr(1) }})
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "cannot determine this node's advertise address")
	if fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("stranded PKI")
	}

	ho = newHandover(t)
	_ = os.WriteFile(filepath.Join(ho.bundle, "snapshot-location"), []byte("kkp://s3-eu-central/cl-001/snap-2026-07-29"), 0o644)
	ho.mockNodeBinaries(nil)
	mustErr(t, ho.receive(ReceiveOpts{}))
	mustContain(t, ho.output(), "KKP-internal locator")
	mustContain(t, ho.output(), "--snapshot")
	if len(ho.calls) != 0 || fsutil.Exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("mutated")
	}
}

func TestHandoverReceivePKIAndEncryptionBeforeInit(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{
		"kubeadm": func(c execx.Cmd) error {
			for _, k := range handoverPKIKeys {
				if !fsutil.Exists(filepath.Join(ho.k8sDir, "pki", k)) {
					t.Fatalf("%s placed AFTER kubeadm init", k)
				}
			}
			info, err := os.Stat(filepath.Join(ho.k8sDir, "kubehz-encryption-config.yaml"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("encryption config not in place (0600) before init")
			}
			return nil
		},
	})
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}), ho.output())
}

func TestHandoverReceiveInitFailureNamesState(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{"kubeadm": func(c execx.Cmd) error { return exitErr(1) }})
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	for _, w := range []string{"state left behind", ho.k8sDir + "/pki", ho.etcdDir, "kubeadm reset", "--force"} {
		mustContain(t, ho.output(), w)
	}
}

func TestHandoverFetchSnapshotHTTPSLands0600(t *testing.T) {
	ho := newHandover(t)
	ho.handle("GET /snap.db", 200, "snapshot-bytes")
	_ = os.WriteFile(filepath.Join(ho.bundle, "snapshot-location"), []byte(ho.apiURL()+"/snap.db"), 0o644)
	work := filepath.Join(ho.base, "work")
	_ = os.MkdirAll(work, 0o700)
	out, err := ho.ctx.fetchSnapshot(t.Context(), ho.bundle, "", work)
	mustOK(t, err, ho.output())
	if out != filepath.Join(work, "kubehz-handover-snapshot.db") || readFile(t, out) != "snapshot-bytes" {
		t.Fatalf("out=%s", out)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

// A presigned URL carries its credential in the query: a failed download
// (a refusal, or no connection) names the object without the query. The
// request itself carries the query.
func TestHandoverFetchSnapshotFailureHidesTheQuery(t *testing.T) {
	query := "X-Amz-Signature=" + strings.Repeat("ab", 8) + "-query-sentinel"
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for _, c := range []struct{ name, origin string }{{"refused", ""}, {"no-connection", closedURL}} {
		t.Run(c.name, func(t *testing.T) {
			ho := newHandover(t)
			ho.handle("GET /snap.db", 403, "denied")
			origin := c.origin
			if origin == "" {
				origin = ho.apiURL()
			}
			_ = os.WriteFile(filepath.Join(ho.bundle, "snapshot-location"), []byte(origin+"/snap.db?"+query), 0o644)
			work := filepath.Join(ho.base, "work")
			_ = os.MkdirAll(work, 0o700)
			_, err := ho.ctx.fetchSnapshot(t.Context(), ho.bundle, "", work)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v", err)
			}
			mustContain(t, ho.output(), "handover: downloading the snapshot from "+origin+"/snap.db failed")
			mustNotContain(t, ho.output(), "query-sentinel")
			mustNotContain(t, ho.output(), "?")
			if c.origin == "" {
				if r := ho.lastReq("GET", "/snap.db"); r == nil || r.Query != query {
					t.Fatalf("the request must carry the query: %+v", r)
				}
			}
		})
	}
}

func TestHandoverReceiveScrubsScratch(t *testing.T) {
	ho := newHandover(t)
	ho.handle("GET /snap.db", 200, "snapshot-bytes")
	_ = os.WriteFile(filepath.Join(ho.bundle, "snapshot-location"), []byte(ho.apiURL()+"/snap.db"), 0o644)
	tarball := filepath.Join(ho.base, "bundle.tar.gz")
	tarBundle(t, ho.bundle, tarball)
	scratch := filepath.Join(ho.base, "scratch")
	_ = os.MkdirAll(scratch, 0o755)
	t.Setenv("TMPDIR", scratch)

	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{"kubeadm": func(c execx.Cmd) error { return exitErr(1) }})
	mustErr(t, ho.receive(ReceiveOpts{Bundle: tarball}))
	mustContain(t, ho.output(), "kubeadm init FAILED")
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Fatalf("scratch left behind: %v", entries)
	}
	if !fsutil.Exists(tarball) {
		t.Fatal("the user's archive must survive")
	}

	ho2 := newHandover(t)
	t.Setenv("TMPDIR", scratch)
	tarball2 := filepath.Join(ho2.base, "bundle.tar.gz")
	tarBundle(t, ho2.bundle, tarball2)
	ho2.mockNodeBinaries(nil)
	mustOK(t, ho2.receive(ReceiveOpts{Bundle: tarball2, Snapshot: ho2.snapshot}), ho2.output())
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Fatalf("scratch left behind: %v", entries)
	}
	if !fsutil.Exists(ho2.snapshot) {
		t.Fatal("the user-provided snapshot must survive")
	}
}

func TestHandoverWritersRejectInjection(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	_ = os.WriteFile(filepath.Join(ho.bundle, "endpoint-dns"), []byte("evil.example.com\"\n  extraArgs:\n    - name: pwn"), 0o644)
	out := filepath.Join(ho.base, "out.yaml")
	mustErr(t, ho.ctx.writeKubeadmConfig(ho.bundle, ho.k8sDir, out, "3.5.21-0"))
	mustContain(t, ho.output(), "not a plain hostname")
	if fsutil.Exists(out) {
		t.Fatal("wrote despite refusal")
	}
	ho = newHandover(t)
	_ = os.WriteFile(filepath.Join(ho.bundle, "encryption-key"), []byte("k3y\"\ninjected: yaml"), 0o644)
	_, err := ho.ctx.writeEncryptionConfig(ho.bundle, ho.k8sDir)
	mustErr(t, err)
	mustContain(t, ho.output(), "not base64")
	ho = newHandover(t)
	_ = os.WriteFile(filepath.Join(ho.bundle, "encryption-key-name"), []byte("k1\"\n            - name: pwn"), 0o644)
	_, err = ho.ctx.writeEncryptionConfig(ho.bundle, ho.k8sDir)
	mustErr(t, err)
	mustContain(t, ho.output(), "not a plain name")
}

// ── preseed ──────────────────────────────────────────────

// sshCalls are the ssh and scp lines of a run (not ssh-keygen).
func (ho *hoHarness) sshCalls() []string {
	var out []string
	for _, l := range ho.calls {
		if strings.HasPrefix(l, "ssh ") || strings.HasPrefix(l, "scp ") {
			out = append(out, l)
		}
	}
	return out
}

func TestHandoverPreseedChecksTheHostKey(t *testing.T) {
	// The PKI must never go to a host whose key is not known: every ssh and
	// scp runs with StrictHostKeyChecking=yes (never accept-new or no, and
	// never UserKnownHostsFile=/dev/null), and the default run asks
	// ssh-keygen nothing. With --known-hosts, that file is the only
	// known_hosts file (GlobalKnownHostsFile=/dev/null).
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	kh := filepath.Join(ho.base, "known_hosts")
	_ = os.WriteFile(kh, nil, 0o600)
	mustOK(t, ho.ctx.HandoverPreseed(context.Background(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7", KnownHosts: kh}), ho.output())
	calls := ho.sshCalls()
	if len(calls) != 9 {
		t.Fatalf("want 1 mkdir, 1 stage, 6 scp and 1 install, got %d:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	for _, l := range calls {
		if !strings.Contains(l, " -o StrictHostKeyChecking=yes ") || strings.Contains(l, "accept-new") ||
			strings.Contains(l, "StrictHostKeyChecking=no") || strings.Contains(l, "UserKnownHostsFile=/dev/null") {
			t.Fatalf("host key not checked: %s", l)
		}
		if !strings.Contains(l, " -o UserKnownHostsFile="+kh+" ") || !strings.Contains(l, " -o GlobalKnownHostsFile=/dev/null ") {
			t.Fatalf("--known-hosts is not the only known_hosts file: %s", l)
		}
		noSharedConnection(t, l)
		noConfigBypass(t, l)
	}
	if !strings.HasPrefix(calls[0], "ssh ") || !strings.HasSuffix(calls[0], " "+preseedMkdir) {
		t.Fatalf("the first call must make the pki dir 0700: %s", calls[0])
	}
	if ho.lineIndex("ssh-keygen ") >= 0 {
		t.Fatalf("the default run checks no record: %v", ho.calls)
	}
	mustNotContain(t, ho.output(), "--accept-new-host-key: the first contact")
	// Without the flag ssh's own known_hosts is used: no UserKnownHostsFile at all.
	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.ctx.HandoverPreseed(context.Background(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}), ho.output())
	for _, l := range ho.sshCalls() {
		if strings.Contains(l, "KnownHostsFile") || !strings.Contains(l, " -o StrictHostKeyChecking=yes ") {
			t.Fatalf("default run must check against ssh's own known_hosts files: %s", l)
		}
		noSharedConnection(t, l)
		noConfigBypass(t, l)
	}
}

// noConfigBypass fails when an ssh or scp line lacks an option that keeps
// the user's ssh config from skipping the host key check
// (NoHostAuthenticationForLocalhost), from taking a key that is in no
// known_hosts file (VerifyHostKeyDNS), or from letting the node change that
// file (UpdateHostKeys).
func noConfigBypass(t *testing.T, line string) {
	t.Helper()
	for _, opt := range []string{"NoHostAuthenticationForLocalhost=no", "VerifyHostKeyDNS=no", "UpdateHostKeys=no"} {
		if !strings.Contains(line, " -o "+opt+" ") {
			t.Fatalf("no -o %s, so the user's ssh config can weaken the host key check: %s", opt, line)
		}
	}
}

// preseedMkdir is the remote command of the first preseed call: the pki
// dir is 0700 before a key lands in it, also when kubeadm made it 0755.
const preseedMkdir = "umask 077 && mkdir -p /etc/kubernetes/pki && chmod 0700 /etc/kubernetes/pki"

// preseedStage is the remote command of the stage call, spelled out: a
// check of /etc/kubernetes and of the pki dir (each prints its stat line
// first), "stage", then a staging file per key. The bats suite spells out
// the same bytes.
const preseedStage = `umask 077` +
	` && echo "check /etc/kubernetes $(stat -c '%u %a %F' -- /etc/kubernetes)"` +
	` && test -n "$(find /etc/kubernetes -maxdepth 0 -type d ! -perm /022 \( -uid 0 -o -uid "$(id -u)" \) -print)"` +
	` && echo "check /etc/kubernetes/pki $(stat -c '%u %a %F' -- /etc/kubernetes/pki)"` +
	` && test -n "$(find /etc/kubernetes/pki -maxdepth 0 -type d ! -perm /022 \( -uid 0 -o -uid "$(id -u)" \) -print)"` +
	` && echo stage` +
	` && mktemp /etc/kubernetes/pki/.ca.crt.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.ca.key.XXXXXXXXXX` +
	` && mktemp /etc/kubernetes/pki/.sa.pub.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.sa.key.XXXXXXXXXX` +
	` && mktemp /etc/kubernetes/pki/.front-proxy-ca.crt.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.front-proxy-ca.key.XXXXXXXXXX`

// preseedStagePrefix is the start of the stage call, to find it.
const preseedStagePrefix = `umask 077 && echo "check `

// noSharedConnection fails when an ssh or scp line can use a master
// connection that the user's ssh config opens or shares: such a call
// would skip the host key check of a new connection.
func noSharedConnection(t *testing.T, line string) {
	t.Helper()
	if !strings.Contains(line, " -o ControlMaster=no ") || !strings.Contains(line, " -o ControlPath=none ") {
		t.Fatalf("a shared ssh connection could skip the host key check: %s", line)
	}
}

// --accept-new-host-key trusts an unknown key on the first call only: the
// mkdir (no key on it) takes it (accept-new), ssh-keygen then finds it in
// the known_hosts file, and every later call (the scp calls with the keys)
// uses yes. ssh records a node in lowercase, as [node]:port off port 22.
func TestHandoverPreseedAcceptNewHostKeyTrustsTheFirstCallOnly(t *testing.T) {
	for _, tc := range []struct {
		name, node string
		port       int
		ownFile    bool
		host       string
	}{
		{"own file, port 22", "203.0.113.7", 0, true, "203.0.113.7"},
		{"ssh's file, port 2222", "Node.Example", 2222, false, "[node.example]:2222"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ho := newHandover(t)
			ho.mockNodeBinaries(nil)
			home := filepath.Join(ho.base, "home")
			ho.env["HOME"] = home
			file := filepath.Join(home, ".ssh", "known_hosts")
			o := PreseedOpts{Bundle: ho.bundle, Node: tc.node, Port: tc.port, AcceptNewHostKey: true}
			if tc.ownFile {
				file = filepath.Join(ho.base, "known_hosts")
				_ = os.WriteFile(file, nil, 0o600)
				o.KnownHosts = file
			}
			mustOK(t, ho.ctx.HandoverPreseed(t.Context(), o), ho.output())
			calls := ho.sshCalls()
			if len(calls) != 9 || !strings.HasPrefix(calls[0], "ssh -o StrictHostKeyChecking=accept-new ") ||
				!strings.HasSuffix(calls[0], " "+preseedMkdir) {
				t.Fatalf("the first call must be the mkdir with accept-new:\n%s", strings.Join(calls, "\n"))
			}
			noConfigBypass(t, calls[0])
			for _, l := range calls[1:] {
				if !strings.Contains(l, " -o StrictHostKeyChecking=yes ") || strings.Contains(l, "accept-new") {
					t.Fatalf("a later call does not check the recorded key: %s", l)
				}
				noConfigBypass(t, l)
			}
			keygen := ho.lineIndex("ssh-keygen ")
			if want := "ssh-keygen -F " + tc.host + " -f " + file; keygen < 0 || ho.calls[keygen] != want {
				t.Fatalf("record check:\n%s\nwant %s", strings.Join(ho.calls, "\n"), want)
			}
			if keygen <= ho.lineIndex("ssh ") || keygen >= ho.lineIndex("scp ") {
				t.Fatalf("the record check must run after the first call and before the first scp:\n%s", strings.Join(ho.calls, "\n"))
			}
			warning := "handover: --accept-new-host-key: the first contact trusts the host key that " + tc.node + " shows, without a check"
			if n := strings.Count(ho.output(), warning); n != 1 {
				t.Fatalf("want the warning once, got %d:\n%s", n, ho.output())
			}
		})
	}
}

// A key that ssh did not record (a file it cannot write, for example)
// fails closed after the first call: no key goes out.
func TestHandoverPreseedUnrecordedHostKeyFailsClosed(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{"ssh-keygen": func(execx.Cmd) error { return exitErr(1) }})
	kh := filepath.Join(ho.base, "known_hosts")
	_ = os.WriteFile(kh, nil, 0o600)
	mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7", KnownHosts: kh, AcceptNewHostKey: true}))
	mustContain(t, ho.output(), "handover: the host key of 203.0.113.7 is not in "+kh+" after the first contact, so the next calls could not check it.")
	if calls := ho.sshCalls(); len(calls) != 1 || !strings.HasPrefix(calls[0], "ssh ") {
		t.Fatalf("only the first call may run: %v", calls)
	}
}

// A --known-hosts that is not a readable regular file holds no key to check
// against, and ssh cannot record one in it: refused before any ssh.
func TestHandoverPreseedRefusesAKnownHostsThatIsNotARegularFile(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	fifo := filepath.Join(base, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "dir")
	_ = os.MkdirAll(dir, 0o700)
	link := filepath.Join(base, "link-to-null")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(base, "missing"), "/dev/null", dir, fifo, "/dev/zero", link} {
		ho := newHandover(t)
		ho.mockNodeBinaries(nil)
		mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7", KnownHosts: path, AcceptNewHostKey: true}))
		mustContain(t, ho.output(), fmt.Sprintf("handover: --known-hosts must name an existing regular file that you can read: %q", path))
		if len(ho.calls) != 0 {
			t.Fatalf("%s: ran with a known_hosts that holds no key: %v", path, ho.calls)
		}
	}
}

// ssh splits an -o value on whitespace, so such a --known-hosts path is
// refused before the first connection.
func TestHandoverPreseedRefusesWhitespaceInKnownHosts(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	err := ho.ctx.HandoverPreseed(context.Background(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7", KnownHosts: "/tmp/known hosts"})
	mustErr(t, err)
	mustContain(t, ho.output(), `handover: --known-hosts path must not contain whitespace: "/tmp/known hosts"`)
	if len(ho.calls) != 0 {
		t.Fatalf("nothing may run with a broken ssh option: %v", ho.calls)
	}
}

// nodeHook can change or fail one call of the fake node before it runs. It
// gets the remote command of an ssh call, or the destination of an scp
// call. It returns the command to run (or the destination to write), or an
// error that fails the call.
type nodeHook func(c execx.Cmd, cmd string) (string, error)

// fakeNode is a node for preseed under root. ssh runs the remote command
// with sh in the C locale: each /etc/kubernetes in it points into root, and
// its output maps back. scp writes the source into its destination as scp
// does: into a file that is there already, also through a hard link or a
// symlink. root/etc/kubernetes is a 0755 dir of the test user.
func fakeNode(t *testing.T, root string, hook nodeHook) map[string]func(c execx.Cmd) error {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the node commands need GNU mv -T, stat -c and find -perm /mode")
	}
	k8s := filepath.Join(root, "etc", "kubernetes")
	if err := os.MkdirAll(k8s, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(k8s, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(c execx.Cmd, cmd string) (string, error) {
		if hook == nil {
			return cmd, nil
		}
		return hook(c, cmd)
	}
	return map[string]func(c execx.Cmd) error{
		"ssh": func(c execx.Cmd) error {
			cmd, err := run(c, c.Args[len(c.Args)-1])
			if err != nil {
				return err
			}
			var out bytes.Buffer
			sh := exec.Command("sh", "-c", strings.ReplaceAll(cmd, "/etc/kubernetes", root+"/etc/kubernetes"))
			sh.Env = append(os.Environ(), "LC_ALL=C")
			sh.Stdout, sh.Stderr = &out, c.Stderr
			err = sh.Run()
			if c.Stdout != nil {
				_, _ = io.WriteString(c.Stdout, strings.ReplaceAll(out.String(), root, ""))
			}
			return err
		},
		"scp": func(c execx.Cmd) error {
			dst, err := run(c, c.Args[len(c.Args)-1])
			if err != nil {
				return err
			}
			data, err := os.ReadFile(c.Args[len(c.Args)-2])
			if err != nil {
				return err
			}
			_, path, _ := strings.Cut(dst, ":")
			return os.WriteFile(root+path, data, 0o644)
		},
	}
}

// stagingLeft lists the dot files in the pki dir of the fake node: each
// staging file that preseed made and did not remove.
func stagingLeft(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root + preseedPKIDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			left = append(left, preseedPKIDir+"/"+e.Name())
		}
	}
	return left
}

// installedPKI fails unless each key is a file with one name in the pki
// dir of the fake node, with the bytes of the bundle and the mode of its
// kind (keys 0600, the certs and sa.pub 0644).
func (ho *hoHarness) installedPKI(t *testing.T, root string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		path := root + preseedPKIDir + "/" + key
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
			t.Fatalf("%s is not a file with one name: %v %v", path, info, err)
		}
		mode := fs.FileMode(0o644)
		if strings.HasSuffix(key, ".key") {
			mode = 0o600
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s has mode %v, want %v", path, info.Mode().Perm(), mode)
		}
		got, _ := os.ReadFile(path)
		want, _ := os.ReadFile(filepath.Join(ho.bundle, key))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s does not hold the bundle's bytes", path)
		}
	}
}

// Each file goes to a new staging file in the pki dir, and one call sets
// the modes and renames each over its destination: 1 mkdir, 1 stage, 6 scp
// to the staging names and 1 install call. Nothing of the restore goes out.
func TestHandoverPreseedStagesThenRenamesEachFile(t *testing.T) {
	ho := newHandover(t)
	root := filepath.Join(ho.base, "node")
	ho.mockNodeBinaries(fakeNode(t, root, nil))
	mustOK(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}), ho.output())
	mustContain(t, ho.output(), "PKI pre-seeded on 203.0.113.7")
	calls := ho.sshCalls()
	if len(calls) != 9 {
		t.Fatalf("want 1 mkdir, 1 stage, 6 scp and 1 install, got %d:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.HasSuffix(calls[0], " "+preseedMkdir) || !strings.HasPrefix(calls[1], "ssh ") ||
		!strings.HasSuffix(calls[1], " -p 22 root@203.0.113.7 "+preseedStage) {
		t.Fatalf("the mkdir, then the stage call:\n%s", strings.Join(calls, "\n"))
	}
	for i, key := range handoverPKIKeys {
		re := regexp.MustCompile(`^scp .* -P 22 ` + regexp.QuoteMeta(ho.bundle+"/"+key) +
			` root@203\.0\.113\.7:/etc/kubernetes/pki/\.` + regexp.QuoteMeta(key) + `\.[A-Za-z0-9]{10}$`)
		if !re.MatchString(calls[2+i]) {
			t.Fatalf("scp %d does not go to the staging file of %s: %s", i, key, calls[2+i])
		}
	}
	if !strings.HasPrefix(calls[8], "ssh ") || !strings.Contains(calls[8], " && echo 'mv ca.crt' && mv -f -T /etc/kubernetes/pki/.ca.crt.") {
		t.Fatalf("the last call must install the files: %s", calls[8])
	}
	for _, l := range calls {
		if strings.Contains(l, "encryption-key") || strings.Contains(l, "snapshot-location") {
			t.Fatalf("restore-only artifact on the wire: %s", l)
		}
	}
	ho.installedPKI(t, root, handoverPKIKeys...)
	if left := stagingLeft(t, root); len(left) != 0 {
		t.Fatalf("staging files left: %v", left)
	}
}

// A hard link or a symlink at a destination is replaced: the rename never
// writes through it, so its target never gets the key bytes. A symlink to
// a dir is replaced too; mv -T does not move the file into the dir.
func TestHandoverPreseedReplacesALinkAtADestination(t *testing.T) {
	ho := newHandover(t)
	root := filepath.Join(ho.base, "node")
	ho.mockNodeBinaries(fakeNode(t, root, nil))
	pki := root + preseedPKIDir
	outside := filepath.Join(root, "outside")
	for _, d := range []string{pki, filepath.Join(outside, "dir")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := []byte("old bytes\n")
	for _, f := range []string{"hard", "soft"} {
		if err := os.WriteFile(filepath.Join(outside, f), old, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(outside, "hard"), filepath.Join(pki, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "soft"), filepath.Join(pki, "sa.key")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(pki, "front-proxy-ca.key")); err != nil {
		t.Fatal(err)
	}
	mustOK(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}), ho.output())
	for _, f := range []string{"hard", "soft"} {
		if got, _ := os.ReadFile(filepath.Join(outside, f)); !bytes.Equal(got, old) {
			t.Fatalf("the link target %s got other bytes: %q", f, got)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(outside, "dir")); len(entries) != 0 {
		t.Fatalf("a file went into the dir of the symlink: %v", entries)
	}
	ho.installedPKI(t, root, handoverPKIKeys...)
	if left := stagingLeft(t, root); len(left) != 0 {
		t.Fatalf("staging files left: %v", left)
	}
}

// preseedHostileLine is a line that a node can print where a staging name
// belongs: a terminal control (ESC), a backslash, a double quote, a C1
// control in UTF-8 (CSI), a NUL byte and 250 more bytes. printf on the fake
// node makes the bytes.
const preseedHostileLine = `printf '\033[2J\\"\302\233\000%s\n' "$(printf 'A%.0s' $(seq 1 250))"`

// preseedHostileQuoted is that line as a message shows it: escaped, the NUL
// byte dropped (as bash command substitution does), and bounded to 200 of
// its 258 bytes (8 bytes of controls, then 192 of the 250 letters).
var preseedHostileQuoted = `"\x1b[2J\\\"\xc2\x9b` + strings.Repeat("A", 192) + `" (the first 200 of 258 bytes)`

// Each failure step stops preseed, names the step and removes the staging
// files: the stage call, a staging name of another shape, a valid name of
// another key, a name twice, a line with terminal controls, text before the
// check lines, a wrong stage marker, an extra line, the copy, the chmod, the
// mv, and an install call that fails and names no step. A name of another
// shape goes into no later call, and no message carries a raw ESC byte.
func TestHandoverPreseedEachFailureStepRemovesTheStagingFiles(t *testing.T) {
	isInstall := func(cmd string) bool { return strings.Contains(cmd, " && mv -f -T ") }
	dropStaging := func(root, key string) {
		entries, _ := os.ReadDir(root + preseedPKIDir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "."+key+".") {
				_ = os.Remove(root + preseedPKIDir + "/" + e.Name())
			}
		}
	}
	for _, tc := range []struct {
		name   string
		hook   func(root string) nodeHook
		want   string
		scps   int
		placed []string
	}{
		{"the stage call fails at sa.key", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				return strings.Replace(cmd, "mktemp /etc/kubernetes/pki/.sa.key.XXXXXXXXXX", "false", 1), nil
			}
		}, "handover: could not make the staging files /etc/kubernetes/pki/.<key>.XXXXXXXXXX on 203.0.113.7", 0, nil},
		{"a staging name of another shape", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				return strings.Replace(cmd, "mktemp /etc/kubernetes/pki/.ca.crt.XXXXXXXXXX", "echo /etc/kubernetes/pki/.ca.crt.AAAAAAAAA-", 1), nil
			}
		}, `handover: 203.0.113.7 printed a staging name for ca.crt that is not /etc/kubernetes/pki/.ca.crt. and 10 letters or digits: "/etc/kubernetes/pki/.ca.crt.AAAAAAAAA-"`, 0, nil},
		{"a valid name of another key", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				return strings.Replace(cmd, "mktemp /etc/kubernetes/pki/.ca.crt.XXXXXXXXXX", "echo /etc/kubernetes/pki/.ca.key.Ab12Cd34Ef", 1), nil
			}
		}, `handover: 203.0.113.7 printed a staging name for ca.crt that is not /etc/kubernetes/pki/.ca.crt. and 10 letters or digits: "/etc/kubernetes/pki/.ca.key.Ab12Cd34Ef"`, 0, nil},
		{"a name twice", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				cmd = strings.Replace(cmd, "mktemp /etc/kubernetes/pki/.ca.crt.XXXXXXXXXX", "echo /etc/kubernetes/pki/.ca.crt.Ab12Cd34Ef", 1)
				return strings.Replace(cmd, "mktemp /etc/kubernetes/pki/.ca.key.XXXXXXXXXX", "echo /etc/kubernetes/pki/.ca.crt.Ab12Cd34Ef", 1), nil
			}
		}, `handover: 203.0.113.7 printed a staging name for ca.key that is not /etc/kubernetes/pki/.ca.key. and 10 letters or digits: "/etc/kubernetes/pki/.ca.crt.Ab12Cd34Ef"`, 0, nil},
		{"a line with terminal controls", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				return strings.Replace(cmd, "mktemp /etc/kubernetes/pki/.ca.crt.XXXXXXXXXX", preseedHostileLine, 1), nil
			}
		}, "handover: 203.0.113.7 printed a staging name for ca.crt that is not /etc/kubernetes/pki/.ca.crt. and 10 letters or digits: " + preseedHostileQuoted, 0, nil},
		{"text before the check lines", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				if strings.HasPrefix(cmd, preseedStagePrefix) {
					return "echo 'Welcome to the node' && " + cmd, nil
				}
				return cmd, nil
			}
		}, `handover: 203.0.113.7 printed "Welcome to the node" where the stage call prints the check of /etc/kubernetes`, 0, nil},
		{"a wrong stage marker", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				return strings.Replace(cmd, " && echo stage && ", " && echo staged && ", 1), nil
			}
		}, `handover: 203.0.113.7 printed "staged" where the stage call prints "stage"`, 0, nil},
		{"an extra line", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				if strings.HasPrefix(cmd, preseedStagePrefix) {
					return cmd + " && echo extra", nil
				}
				return cmd, nil
			}
		}, "handover: 203.0.113.7 printed 7 lines for 6 staging names", 0, nil},
		{"the copy fails at sa.key", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				if c.Name == "scp" && strings.Contains(cmd, "/.sa.key.") {
					return "", exitErr(1)
				}
				return cmd, nil
			}
		}, "handover: copying sa.key to 203.0.113.7 failed", 4, nil},
		{"the chmod fails at ca.key", func(root string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				if isInstall(cmd) {
					dropStaging(root, "ca.key")
				}
				return cmd, nil
			}
		}, "handover: could not set the mode of the staging file of ca.key on 203.0.113.7; no file was moved into place", 6, nil},
		{"the mv fails at sa.key", func(root string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				if isInstall(cmd) {
					_ = os.MkdirAll(root+preseedPKIDir+"/sa.key/held", 0o700)
				}
				return cmd, nil
			}
		}, "handover: could not move the staging file of sa.key to /etc/kubernetes/pki/sa.key on 203.0.113.7; in place before it: ca.crt ca.key sa.pub", 6,
			[]string{"ca.crt", "ca.key", "sa.pub"}},
		{"the install call fails and names no step", func(string) nodeHook {
			return func(c execx.Cmd, cmd string) (string, error) {
				if isInstall(cmd) {
					return "", exitErr(255)
				}
				return cmd, nil
			}
		}, "handover: the install call on 203.0.113.7 failed, and its output names no step, so the state of the install is UNKNOWN. Check each of these files on 203.0.113.7: " +
			"/etc/kubernetes/pki/ca.crt /etc/kubernetes/pki/ca.key /etc/kubernetes/pki/sa.pub /etc/kubernetes/pki/sa.key /etc/kubernetes/pki/front-proxy-ca.crt /etc/kubernetes/pki/front-proxy-ca.key", 6, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ho := newHandover(t)
			root := filepath.Join(ho.base, "node")
			ho.mockNodeBinaries(fakeNode(t, root, tc.hook(root)))
			mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
			mustContain(t, ho.output(), tc.want)
			mustNotContain(t, ho.output(), "PKI pre-seeded")
			mustNotContain(t, ho.output(), "could not remove the staging files")
			mustNotContain(t, ho.output(), "\x1b")
			calls := ho.sshCalls()
			if last := calls[len(calls)-1]; !strings.HasPrefix(last, "ssh ") || !strings.Contains(last, " root@203.0.113.7 rm -f -- /etc/kubernetes/pki/.") {
				t.Fatalf("the last call must remove the staging files:\n%s", strings.Join(calls, "\n"))
			}
			scps := 0
			for _, l := range calls {
				if strings.HasPrefix(l, "scp ") {
					scps++
				}
				if strings.Contains(l, "AAAAAAAAA-") {
					t.Fatalf("a name of another shape went into a call: %s", l)
				}
			}
			if scps != tc.scps {
				t.Fatalf("want %d scp calls, got %d:\n%s", tc.scps, scps, strings.Join(calls, "\n"))
			}
			if left := stagingLeft(t, root); len(left) != 0 {
				t.Fatalf("staging files left: %v", left)
			}
			ho.installedPKI(t, root, tc.placed...)
			for _, key := range handoverPKIKeys[len(tc.placed):] {
				if info, err := os.Lstat(root + preseedPKIDir + "/" + key); err == nil && !info.IsDir() {
					t.Fatalf("%s went into place after the failure", key)
				}
			}
		})
	}
}

// The stage call refuses before its first mktemp, so before any scp, when
// /etc/kubernetes or the pki dir is not a dir (a link or a file), has a
// write bit for the group or others, or has an owner that is neither root
// nor the ssh user. The message names the dir and what stat showed. No
// staging file is made, so no cleanup call runs.
func TestHandoverPreseedRefusesAnUntrustedDir(t *testing.T) {
	uid := os.Getuid()
	for _, tc := range []struct {
		name, dir, facts string
		setup            func(t *testing.T, root string)
		hook             func(root string) nodeHook
		otherOwner       bool
	}{
		{"/etc/kubernetes with a group write bit", "/etc/kubernetes", fmt.Sprintf("%d 775 directory", uid),
			func(t *testing.T, root string) {
				if err := os.Chmod(root+"/etc/kubernetes", 0o775); err != nil {
					t.Fatal(err)
				}
			}, nil, false},
		{"the pki dir is a symlink to a dir", preseedPKIDir, fmt.Sprintf("%d 777 symbolic link", uid),
			func(t *testing.T, root string) {
				if err := os.MkdirAll(root+"/outside", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"/outside", root+preseedPKIDir); err != nil {
					t.Fatal(err)
				}
			}, nil, false},
		{"the pki dir is a file", preseedPKIDir, fmt.Sprintf("%d 600 regular empty file", uid), nil,
			func(root string) nodeHook {
				return func(c execx.Cmd, cmd string) (string, error) {
					if strings.HasPrefix(cmd, preseedStagePrefix) {
						if err := os.Remove(root + preseedPKIDir); err != nil {
							return "", err
						}
						if err := os.WriteFile(root+preseedPKIDir, nil, 0o600); err != nil {
							return "", err
						}
					}
					return cmd, nil
				}
			}, false},
		{"/etc/kubernetes of another user", "/etc/kubernetes", "", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ho := newHandover(t)
			root := filepath.Join(ho.base, "node")
			hook := nodeHook(nil)
			if tc.hook != nil {
				hook = tc.hook(root)
			}
			if tc.otherOwner {
				// The ssh user is another uid than the owner of the dir:
				// the fake node answers id -u with that uid. As root, the
				// dir goes to uid 4242 first, because root passes -uid 0.
				owner := uid
				if owner == 0 {
					owner = 4242
				}
				tc.facts = fmt.Sprintf("%d 755 directory", owner)
				tc.setup = func(t *testing.T, root string) {
					if uid == 0 {
						if err := os.Chown(root+"/etc/kubernetes", owner, -1); err != nil {
							t.Fatal(err)
						}
					}
				}
				hook = func(c execx.Cmd, cmd string) (string, error) {
					return strings.ReplaceAll(cmd, `"$(id -u)"`, fmt.Sprintf(`"%d"`, owner+1)), nil
				}
			}
			ho.mockNodeBinaries(fakeNode(t, root, hook))
			if tc.setup != nil {
				tc.setup(t, root)
			}
			mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
			mustContain(t, ho.output(), "handover: "+tc.dir+" on 203.0.113.7 must be a dir, not a link, that root or the ssh user owns,"+
				" with no write bit for the group or others. stat shows (uid, mode, type) \""+tc.facts+"\". No key was sent")
			calls := ho.sshCalls()
			if len(calls) != 2 || !strings.HasSuffix(calls[1], " "+preseedStage) {
				t.Fatalf("want the mkdir and the stage call only (no scp, no cleanup):\n%s", strings.Join(calls, "\n"))
			}
			for _, dir := range []string{root + preseedPKIDir, root + "/outside"} {
				if entries, err := os.ReadDir(dir); err == nil && len(entries) != 0 {
					t.Fatalf("a file was made in %s: %v", dir, entries)
				}
			}
		})
	}
}

// A cleanup call that fails names each staging file, and those are the
// files that are left.
func TestHandoverPreseedNamesTheStagingFilesItCannotRemove(t *testing.T) {
	ho := newHandover(t)
	root := filepath.Join(ho.base, "node")
	ho.mockNodeBinaries(fakeNode(t, root, func(c execx.Cmd, cmd string) (string, error) {
		if (c.Name == "scp" && strings.Contains(cmd, "/.sa.key.")) || strings.HasPrefix(cmd, "rm -f -- ") {
			return "", exitErr(1)
		}
		return cmd, nil
	}))
	mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
	mustContain(t, ho.output(), "handover: copying sa.key to 203.0.113.7 failed")
	left := stagingLeft(t, root)
	if len(left) != len(handoverPKIKeys) {
		t.Fatalf("want a staging file per key, got %v", left)
	}
	prefix := "handover: could not remove the staging files on 203.0.113.7; they can hold key bytes. Remove them yourself: "
	i := strings.Index(ho.output(), prefix)
	if i < 0 {
		t.Fatalf("the staging files are not named:\n%s", ho.output())
	}
	named := strings.Fields(strings.SplitN(ho.output()[i+len(prefix):], "\n", 2)[0])
	slices.Sort(named)
	if !slices.Equal(named, left) {
		t.Fatalf("named %v, left %v", named, left)
	}
}

// A new file of installFile that cannot go is named: it can hold key bytes.
func TestHandoverInstallFileNamesATempFileItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file from a dir without write, so the removal cannot fail")
	}
	ho := newHandover(t)
	dir := filepath.Join(ho.base, "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	// The chmod fails after it took the write bit of the dir, so the
	// removal of the new file fails too.
	ho.ctx.Chmod = func(string, fs.FileMode) error {
		if err := os.Chmod(dir, 0o500); err != nil {
			return err
		}
		return errors.New("operation not permitted")
	}
	mustErr(t, ho.ctx.installFile(filepath.Join(dir, "ca.key"), []byte("key bytes"), 0o600))
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".ca.key.") {
		t.Fatalf("want the one new file left, got %v", entries)
	}
	mustContain(t, ho.output(), "handover: could not remove "+filepath.Join(dir, entries[0].Name())+", which can hold key bytes. Delete it yourself.")
}

func TestHandoverPreseedUnreachableNode(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{"ssh": func(c execx.Cmd) error { return exitErr(255) }})
	mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
	mustContain(t, ho.output(), "cannot reach")
	mustContain(t, ho.output(), `handover: an unknown or changed host key also fails here. Compare the fingerprints of "ssh-keyscan -p 22 203.0.113.7 | ssh-keygen -lf -" with "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub" on the node's console, and add the key to the known_hosts file if they match.`)
	mustContain(t, ho.output(), "Or retry with --accept-new-host-key to trust the first contact.")
	if ho.runner.anyCall("scp ") {
		t.Fatal("scp ran")
	}
	// With the opt-in an unknown key cannot fail the first call; a changed one can.
	ho = newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{"ssh": func(c execx.Cmd) error { return exitErr(255) }})
	mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7", AcceptNewHostKey: true}))
	mustContain(t, ho.output(), "handover: a changed host key also fails here; check the node and the known_hosts entry before you retry")
	mustNotContain(t, ho.output(), "Or retry with --accept-new-host-key")
	if ho.runner.anyCall("scp ") || ho.lineIndex("ssh-keygen ") >= 0 {
		t.Fatal("ran past the failed first call")
	}
}

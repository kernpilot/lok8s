package kubehz

// handover_test.go ports tests/unit/kubehz_handover_test.bats against a
// sandboxed /etc/kubernetes + /var/lib/etcd (the KUBEHZ_HANDOVER_* seams).
// Every node binary (etcdutl/etcdctl/kubeadm/kubectl/ip/aws/ssh/scp) is the
// fake Runner; the sandbox is t.TempDir.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	for _, key := range []string{"ca.key", "encryption-provider"} {
		t.Run(key, func(t *testing.T) {
			ho := newHandover(t)
			ho.mockNodeBinaries(nil)
			elsewhere := filepath.Join(ho.base, "elsewhere")
			_ = os.WriteFile(elsewhere, []byte("outside-secret-content\n"), 0o600)
			path := filepath.Join(ho.bundle, key)
			_ = os.Remove(path)
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Fatal(err)
			}
			want := "handover: bundle key " + key + " is not a regular file (a link, a directory or a special file): " + path

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

// placePKI fails when the modes of the files it placed cannot be set.
func TestHandoverPlacePKIChecksEveryChmod(t *testing.T) {
	t.Parallel()
	ho := newHandover(t)
	ho.ctx.Chmod = func(p string, m fs.FileMode) error {
		if strings.HasSuffix(p, "/pki/sa.key") {
			return errors.New("operation not permitted")
		}
		return os.Chmod(p, m)
	}
	mustErr(t, ho.ctx.placePKI(ho.bundle, ho.k8sDir))
	mustContain(t, ho.output(), "handover: could not set the modes of the PKI files in "+ho.k8sDir+"/pki")
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
	// scp runs with StrictHostKeyChecking=yes (never accept-new, no or
	// /dev/null), and the default run asks ssh-keygen nothing.
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	kh := filepath.Join(ho.base, "known_hosts")
	_ = os.WriteFile(kh, nil, 0o600)
	mustOK(t, ho.ctx.HandoverPreseed(context.Background(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7", KnownHosts: kh}), ho.output())
	calls := ho.sshCalls()
	if len(calls) != 10 {
		t.Fatalf("want 1 mkdir, 6 scp and 3 chmod, got %d:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	for _, l := range calls {
		if !strings.Contains(l, " -o StrictHostKeyChecking=yes ") || strings.Contains(l, "accept-new") ||
			strings.Contains(l, "StrictHostKeyChecking=no") || strings.Contains(l, "/dev/null") {
			t.Fatalf("host key not checked: %s", l)
		}
		if !strings.Contains(l, "UserKnownHostsFile="+kh+" ") {
			t.Fatalf("--known-hosts not passed: %s", l)
		}
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
		if strings.Contains(l, "UserKnownHostsFile") || !strings.Contains(l, " -o StrictHostKeyChecking=yes ") {
			t.Fatalf("default run must check against ssh's own known_hosts files: %s", l)
		}
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
			if len(calls) != 10 || !strings.HasPrefix(calls[0], "ssh -o StrictHostKeyChecking=accept-new ") ||
				!strings.HasSuffix(calls[0], "umask 077 && mkdir -p /etc/kubernetes/pki") {
				t.Fatalf("the first call must be the mkdir with accept-new:\n%s", strings.Join(calls, "\n"))
			}
			for _, l := range calls[1:] {
				if !strings.Contains(l, " -o StrictHostKeyChecking=yes ") || strings.Contains(l, "accept-new") {
					t.Fatalf("a later call does not check the recorded key: %s", l)
				}
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

func TestHandoverPreseedTransfersSixFilesWithImmediateChmod(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}), ho.output())
	mustContain(t, ho.output(), "PKI pre-seeded on 203.0.113.7")
	if ho.lineIndex("ssh ") < 0 || !strings.Contains(ho.calls[ho.lineIndex("ssh ")], "umask 077 && mkdir -p /etc/kubernetes/pki") {
		t.Fatalf("mkdir: %v", ho.calls)
	}
	scps := 0
	for _, l := range ho.calls {
		if strings.HasPrefix(l, "scp ") {
			scps++
			if strings.Contains(l, "encryption-key") || strings.Contains(l, "snapshot-location") {
				t.Fatalf("restore-only artifact on the wire: %s", l)
			}
		}
	}
	if scps != 6 {
		t.Fatalf("scp count %d", scps)
	}
	for _, k := range handoverPKIKeys {
		if ho.lineIndex("scp ") < 0 || !ho.runner.anyCall(ho.bundle+"/"+k+" root@203.0.113.7:/etc/kubernetes/pki/"+k) {
			t.Fatalf("%s not transferred", k)
		}
	}
	chmodCA, scpSAPub := -1, -1
	for i, l := range ho.calls {
		if strings.Contains(l, "chmod 600 /etc/kubernetes/pki/ca.key") && chmodCA < 0 {
			chmodCA = i
		}
		if strings.HasPrefix(l, "scp ") && strings.Contains(l, "/sa.pub") && scpSAPub < 0 {
			scpSAPub = i
		}
	}
	if chmodCA < 0 || scpSAPub < 0 || chmodCA > scpSAPub {
		t.Fatalf("per-file chmod order: ca=%d sa.pub=%d", chmodCA, scpSAPub)
	}
	for _, k := range []string{"sa.key", "front-proxy-ca.key"} {
		if !ho.runner.anyCall("chmod 600 /etc/kubernetes/pki/" + k) {
			t.Fatalf("%s not tightened", k)
		}
	}
	if !ho.runner.anyCall("-p 22 root@203.0.113.7") || !ho.runner.anyCall("-P 22 ") {
		t.Fatalf("ssh/scp port args: %v", ho.calls)
	}
}

func TestHandoverPreseedMidTransferFailure(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{
		"scp": func(c execx.Cmd) error {
			if strings.Contains(argvLine(c), "/sa.key ") {
				return exitErr(1)
			}
			return nil
		},
	})
	mustErr(t, ho.ctx.HandoverPreseed(t.Context(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
	mustContain(t, ho.output(), "copying sa.key")
	if !ho.runner.anyCall("chmod 600 /etc/kubernetes/pki/ca.key") {
		t.Fatal("ca.key was not tightened before the failure")
	}
	if ho.runner.anyCall("front-proxy-ca.crt") {
		t.Fatal("transfer continued past the failure")
	}
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

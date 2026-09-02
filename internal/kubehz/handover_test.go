package kubehz

// handover_test.go ports tests/unit/kubehz_handover_test.bats against a
// sandboxed /etc/kubernetes + /var/lib/etcd (the KUBEHZ_HANDOVER_* seams).
// Every node binary (etcdutl/etcdctl/kubeadm/kubectl/ip/aws/ssh/scp) is the
// fake Runner; the sandbox is t.TempDir.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
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

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

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
	ho := newHandover(t)
	tarball := filepath.Join(ho.base, "bundle.tar.gz")
	tarBundle(t, ho.bundle, tarball)
	dir, err := ho.ctx.resolveBundle(tarball, filepath.Join(ho.base, "work"))
	mustOK(t, err, ho.output())
	if !exists(filepath.Join(dir, "ca.crt")) || !exists(filepath.Join(dir, "endpoint-dns")) {
		t.Fatal("bundle not extracted")
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("private dir mode %o", info.Mode().Perm())
	}
}

func TestHandoverResolveBundleTraversalGuard(t *testing.T) {
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
	if exists(filepath.Join(ho.base, "escaped")) || exists(filepath.Join(ho.base, "work", "escaped")) {
		t.Fatal("entry escaped")
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
	if restore < 0 || init < 0 || verify < 0 || !(restore < init && init < verify) {
		t.Fatalf("order restore=%d init=%d verify=%d", restore, init, verify)
	}
	if ho.lineIndex("etcdutl snapshot restore "+ho.snapshot+" --data-dir "+ho.etcdDir) < 0 {
		t.Fatal("restore args")
	}
	if ho.lineIndex("kubeadm init --config "+ho.k8sDir+"/kubehz-handover-kubeadm.yaml --ignore-preflight-errors=DirAvailable--var-lib-etcd --skip-phases=addon/coredns,addon/kube-proxy") < 0 {
		t.Fatalf("kubeadm init args:\n%s", strings.Join(ho.calls, "\n"))
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
	golden := strings.ReplaceAll(readFile(t, filepath.Join("testdata", "golden", "kubeadm-config.yaml")), "/etc/kubernetes", ho.k8sDir)
	if cfg != golden {
		t.Fatalf("kubeadm config drift:\n%s\nwant:\n%s", cfg, golden)
	}
	enc := filepath.Join(ho.k8sDir, "kubehz-encryption-config.yaml")
	if readFile(t, enc) != readFile(t, filepath.Join("testdata", "golden", "encryption-config.yaml")) {
		t.Fatalf("encryption config drift:\n%s", readFile(t, enc))
	}
	if info, _ := os.Stat(enc); info.Mode().Perm() != 0o600 {
		t.Fatal("encryption config mode")
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
	if exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("seeded before the tag check")
	}

	ho = newHandover(t)
	ho.mockNodeBinaries(nil)
	ho.env["KUBEHZ_HANDOVER_ETCD_IMAGE_TAG"] = "3.5.21-0\" evil"
	mustErr(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot}))
	mustContain(t, ho.output(), "not a plain image tag")
	if exists(filepath.Join(ho.k8sDir, "pki")) {
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
		if exists(filepath.Join(ho.k8sDir, "pki")) || exists(filepath.Join(ho.etcdDir, "member")) {
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
	if len(ho.calls) != 0 || exists(filepath.Join(ho.k8sDir, "pki")) {
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
	if exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("stranded PKI")
	}

	ho = newHandover(t)
	_ = os.WriteFile(filepath.Join(ho.bundle, "snapshot-location"), []byte("kkp://s3-eu-central/cl-001/snap-2026-07-29"), 0o644)
	ho.mockNodeBinaries(nil)
	mustErr(t, ho.receive(ReceiveOpts{}))
	mustContain(t, ho.output(), "KKP-internal locator")
	mustContain(t, ho.output(), "--snapshot")
	if len(ho.calls) != 0 || exists(filepath.Join(ho.k8sDir, "pki")) {
		t.Fatal("mutated")
	}
}

func TestHandoverReceivePKIAndEncryptionBeforeInit(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{
		"kubeadm": func(c execx.Cmd) error {
			for _, k := range handoverPKIKeys {
				if !exists(filepath.Join(ho.k8sDir, "pki", k)) {
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
	out, err := ho.ctx.fetchSnapshot(context.Background(), ho.bundle, "", work)
	mustOK(t, err, ho.output())
	if out != filepath.Join(work, "kubehz-handover-snapshot.db") || readFile(t, out) != "snapshot-bytes" {
		t.Fatalf("out=%s", out)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
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
	if !exists(tarball) {
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
	if !exists(ho2.snapshot) {
		t.Fatal("the user-provided snapshot must survive")
	}
}

func TestHandoverWritersRejectInjection(t *testing.T) {
	ho := newHandover(t)
	_ = os.WriteFile(filepath.Join(ho.bundle, "endpoint-dns"), []byte("evil.example.com\"\n  extraArgs:\n    - name: pwn"), 0o644)
	out := filepath.Join(ho.base, "out.yaml")
	mustErr(t, ho.ctx.writeKubeadmConfig(ho.bundle, ho.k8sDir, out, "3.5.21-0"))
	mustContain(t, ho.output(), "not a plain hostname")
	if exists(out) {
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

func TestHandoverPreseedTransfersSixFilesWithImmediateChmod(t *testing.T) {
	ho := newHandover(t)
	ho.mockNodeBinaries(nil)
	mustOK(t, ho.ctx.HandoverPreseed(context.Background(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}), ho.output())
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
	mustErr(t, ho.ctx.HandoverPreseed(context.Background(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
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
	mustErr(t, ho.ctx.HandoverPreseed(context.Background(), PreseedOpts{Bundle: ho.bundle, Node: "203.0.113.7"}))
	mustContain(t, ho.output(), "cannot reach")
	if ho.runner.anyCall("scp ") {
		t.Fatal("scp ran")
	}
}

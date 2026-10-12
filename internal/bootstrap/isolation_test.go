package bootstrap

// isolation_test.go — the whole-project dry-run sandbox primitives (review
// round 2, item A): copyProjectTree's symlink handling, remapUnderBase, and
// newDryRunSandbox's env redirection/unset and cleanup.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestCopyProjectTreeCopiesRegularFiles(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	testutil.WriteFile(t, filepath.Join(src, "a", "b.txt"), "hello\n")
	if err := copyProjectTree(src, dst, src, ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a", "b.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("copy = %q, %v", got, err)
	}
}

func TestCopyProjectTreeSkipsGit(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	testutil.WriteFile(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main\n")
	testutil.WriteFile(t, filepath.Join(src, "keep.txt"), "x\n")
	if err := copyProjectTree(src, dst, src, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git was copied (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Errorf("keep.txt missing: %v", err)
	}
}

// A symlink whose target stays inside root is copied AS ITS TARGET's
// content — a real file in the copy, not a symlink — fixing a
// cross-directory reference the project itself holds.
func TestCopyProjectTreeDereferencesInternalSymlink(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	testutil.WriteFile(t, filepath.Join(src, "real", "file.txt"), "content\n")
	if err := os.Symlink(filepath.Join(src, "real"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyProjectTree(src, dst, src, ""); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("link survived as a symlink in the copy, not dereferenced")
	}
	if !fi.IsDir() {
		t.Error("link's target (a directory) was not copied as a directory")
	}
	got, err := os.ReadFile(filepath.Join(dst, "link", "file.txt"))
	if err != nil || string(got) != "content\n" {
		t.Fatalf("dereferenced content = %q, %v", got, err)
	}
}

// A symlink whose target resolves OUTSIDE root is refused outright — the
// whole copy fails rather than silently following or dropping it.
func TestCopyProjectTreeRefusesEscapingSymlink(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	outside := t.TempDir()
	testutil.WriteFile(t, filepath.Join(outside, "secret.txt"), "nope\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := copyProjectTree(src, dst, src, ""); err == nil {
		t.Fatal("expected a refusal for the escaping symlink")
	}
}

// A RELATIVE symlink that climbs out of root via ../.. is refused the
// same way as an absolute one.
func TestCopyProjectTreeRefusesRelativeEscapingSymlink(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../etc/passwd", filepath.Join(src, "sub", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := copyProjectTree(src, dst, src, ""); err == nil {
		t.Fatal("expected a refusal for the relative escaping symlink")
	}
}

func TestRemapUnderBase(t *testing.T) {
	if got := remapUnderBase("/proj", "/proj/clusters", "/tmp/copy"); got != "/tmp/copy/clusters" {
		t.Errorf("inside base = %q", got)
	}
	if got := remapUnderBase("/proj", "/elsewhere/embedded", "/tmp/copy"); got != "/elsewhere/embedded" {
		t.Errorf("outside base should pass through unchanged, got %q", got)
	}
}

// newDryRunSandbox copies the project, redirects HOME/XDG_*/CAROOT/HELM_*/
// PATH_SECRETS, and unsets KUBECONFIG — all for the duration of restore().
func TestNewDryRunSandboxRedirectsEnvAndUnsetsKubeconfig(t *testing.T) {
	base := t.TempDir()
	testutil.WriteFile(t, filepath.Join(base, "clusters", "x.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	orig := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}

	t.Setenv("HOME", "/real/home")
	t.Setenv("KUBECONFIG", "/real/kubeconfig.yaml")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("HCLOUD_TOKEN", "real-token")

	sb, err := newDryRunSandbox(orig, "x.dev")
	if err != nil {
		t.Fatalf("newDryRunSandbox: %v", err)
	}
	if os.Getenv("HOME") == "/real/home" {
		t.Error("HOME was not redirected")
	}
	if os.Getenv("KUBECONFIG") != "" {
		t.Errorf("KUBECONFIG = %q, want unset", os.Getenv("KUBECONFIG"))
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		t.Errorf("KUBERNETES_SERVICE_HOST = %q, want unset", os.Getenv("KUBERNETES_SERVICE_HOST"))
	}
	if os.Getenv("HCLOUD_TOKEN") != "" {
		t.Errorf("HCLOUD_TOKEN = %q, want unset (matches *_TOKEN)", os.Getenv("HCLOUD_TOKEN"))
	}
	wantSecrets := filepath.Join(sb.paths.Clusters, "x.dev", "secrets")
	if got := os.Getenv("PATH_SECRETS"); got != wantSecrets {
		t.Errorf("PATH_SECRETS = %q, want %q (even though it doesn't exist yet)", got, wantSecrets)
	}
	if _, err := os.Stat(filepath.Join(sb.paths.Base, "clusters", "x.dev", "cluster.lok8s.yaml")); err != nil {
		t.Errorf("the project was not copied into the sandbox: %v", err)
	}
	tmpRoot := filepath.Dir(sb.paths.Base)

	sb.restore()
	if os.Getenv("HOME") != "/real/home" {
		t.Errorf("HOME not restored, got %q", os.Getenv("HOME"))
	}
	if os.Getenv("KUBECONFIG") != "/real/kubeconfig.yaml" {
		t.Errorf("KUBECONFIG not restored, got %q", os.Getenv("KUBECONFIG"))
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "10.0.0.1" {
		t.Errorf("KUBERNETES_SERVICE_HOST not restored, got %q", os.Getenv("KUBERNETES_SERVICE_HOST"))
	}
	if os.Getenv("HCLOUD_TOKEN") != "real-token" {
		t.Errorf("HCLOUD_TOKEN not restored, got %q", os.Getenv("HCLOUD_TOKEN"))
	}

	sb.cleanup()
	if _, err := os.Stat(tmpRoot); !os.IsNotExist(err) {
		t.Errorf("the sandbox temp tree survived cleanup (err=%v)", err)
	}
}

// A project with a symlink escaping it refuses the WHOLE sandbox build,
// not a partial/best-effort copy.
func TestNewDryRunSandboxRefusesEscapingSymlink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	testutil.WriteFile(t, filepath.Join(outside, "secret.txt"), "nope\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(base, "escape")); err != nil {
		t.Fatal(err)
	}
	orig := &config.Paths{Base: base, Clusters: filepath.Join(base, "clusters"), Lok8s: filepath.Join(base, ".lok8s")}
	if _, err := newDryRunSandbox(orig, "x.dev"); err == nil {
		t.Fatal("expected a refusal")
	}
}

// A symlink AT orig.Bin pointing outside the project (the b-managed
// toolchain, commonly a shared install via PATH_BIN) must be TOLERATED,
// not refused — .bin is excluded from the copy entirely. This is the
// exact shape hack/parity-orchestrate.sh's fixture takes (.bin/argsh
// symlinked to the repo's real toolchain install) and it broke the
// sandbox on its first parity run.
func TestNewDryRunSandboxToleratesBinSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	testutil.WriteFile(t, filepath.Join(outside, "argsh"), "#!/bin/sh\n")
	binDir := filepath.Join(base, ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "argsh"), filepath.Join(binDir, "argsh")); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(base, "clusters", "x.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	orig := &config.Paths{Base: base, Bin: binDir, Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	sb, err := newDryRunSandbox(orig, "x.dev")
	if err != nil {
		t.Fatalf("newDryRunSandbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb.paths.Base, ".bin")); !os.IsNotExist(err) {
		t.Errorf(".bin was copied into the sandbox (err=%v)", err)
	}
	if sb.paths.Bin != binDir {
		t.Errorf("Bin = %q, want the original %q (unchanged, read-only)", sb.paths.Bin, binDir)
	}
	sb.restore()
	sb.cleanup()
}

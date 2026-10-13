package addons

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// Review round 6, items 4 and 5: the KRM hint reads the kustomization file
// itself (an inline function in an extensionless Kustomization, in the entry
// or in a base), never reads a file outside the project (a .yaml link below
// the entry is not followed, as grep -r in the bash twin does not), reads
// regular files only, and marks the entry partial when a reference or a
// kustomization file resolves outside the project or a reference is remote:
// the hint cannot rule a function out there. The bash twin is tested on the
// same cases (tests/unit/bootstrap_test.bats, bootstrap::_krm_declared).
func TestKRMFunctionDeclaredRound6(t *testing.T) {
	const fn = "apiVersion: example.test/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: ./fn\n"
	const inline = "transformers:\n- |-\n  apiVersion: example.test/v1\n  kind: Fn\n  metadata:\n    name: fn\n    annotations:\n      config.kubernetes.io/function: |\n        exec:\n          path: ./fn\n"
	for _, tc := range []struct {
		name  string
		files map[string]string // relative to the project; "@<rel>" links to <rel> in the project, "@@OUT/<rel>" to <rel> outside it
		want  bool
	}{
		{"inline function in an extensionless Kustomization", map[string]string{
			"entry/Kustomization": inline,
		}, true},
		{"inline function in a base's extensionless Kustomization", map[string]string{
			"shared/base/Kustomization": inline,
			"entry/kustomization.yaml":  "resources:\n- ../shared/base\n",
		}, true},
		{"a base outside the project", map[string]string{
			"entry/kustomization.yaml": "resources:\n- @@OUT/base\n",
		}, true},
		{"a remote URL", map[string]string{
			"entry/kustomization.yaml": "resources:\n- https://example.test/raw/cm.yaml\n",
		}, true},
		{"a remote GitHub base", map[string]string{
			"entry/kustomization.yaml": "resources:\n- github.com/example/repo/base?ref=v1\n",
		}, true},
		{"a remote git base", map[string]string{
			"entry/kustomization.yaml": "resources:\n- git@github.com:example/repo.git//base\n",
		}, true},
		{"an unreferenced .yaml link to a file outside", map[string]string{
			"entry/kustomization.yaml": "resources:\n- cm.yaml\n",
			"entry/fn.yaml":            "@@OUT/fn.yaml",
		}, false},
		{"a referenced .yaml link to a file outside", map[string]string{
			"entry/kustomization.yaml": "transformers:\n- fn.yaml\n",
			"entry/fn.yaml":            "@@OUT/fn.yaml",
		}, true},
		{"a referenced .yaml link to a file inside", map[string]string{
			"entry/kustomization.yaml": "transformers:\n- fn.yaml\n",
			"entry/fn.yaml":            "@fns/fn.yaml",
			"fns/fn.yaml":              fn,
		}, true},
		{"an unreferenced .yaml link to a file inside", map[string]string{
			"entry/kustomization.yaml": "resources:\n- cm.yaml\n",
			"entry/fn.yaml":            "@fns/fn.yaml",
			"fns/fn.yaml":              fn,
		}, false},
		{"a Kustomization that links outside", map[string]string{
			"entry/kustomization.yaml": "@@OUT/kustomization.yaml",
		}, true},
		{"a missing reference", map[string]string{
			"entry/kustomization.yaml": "resources:\n- ../nowhere\n",
		}, false},
		{"plain", map[string]string{
			"shared/base/kustomization.yaml": "resources:\n- cm.yaml\n",
			"shared/base/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n",
			"entry/kustomization.yaml":       "resources:\n- ../shared/base\n",
		}, false},
	} {
		project, out := t.TempDir(), t.TempDir()
		testutil.WriteFile(t, filepath.Join(out, "fn.yaml"), fn)
		testutil.WriteFile(t, filepath.Join(out, "kustomization.yaml"), "resources:\n- cm.yaml\n")
		testutil.WriteFile(t, filepath.Join(out, "base", "kustomization.yaml"), "resources:\n- cm.yaml\n")
		testutil.WriteFile(t, filepath.Join(project, "entry", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: entry\n")
		for rel, content := range tc.files {
			path := filepath.Join(project, rel)
			switch {
			case strings.HasPrefix(content, "@@OUT/"):
				link(t, filepath.Join(out, strings.TrimPrefix(content, "@@OUT/")), path)
			case strings.HasPrefix(content, "@"):
				link(t, filepath.Join(project, strings.TrimPrefix(content, "@")), path)
			default:
				testutil.WriteFile(t, path, strings.ReplaceAll(content, "@@OUT", out))
			}
		}
		if got := KRMFunctionDeclared(filepath.Join(project, "entry"), project); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An entry outside the roots (a framework addon a dry run peeks lives in a
// temp directory) reads its own tree: a reference into it is inside.
func TestKRMFunctionDeclaredEntryOutsideTheRoots(t *testing.T) {
	entry, project := t.TempDir(), t.TempDir()
	testutil.WriteFile(t, filepath.Join(entry, "kustomization.yaml"), "resources:\n- sub\n")
	testutil.WriteFile(t, filepath.Join(entry, "sub", "kustomization.yaml"), "resources:\n- cm.yaml\n")
	testutil.WriteFile(t, filepath.Join(entry, "sub", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	if KRMFunctionDeclared(entry, project) {
		t.Error("a reference into the entry's own directory counted as outside the project")
	}
}

// A FIFO named like YAML (below the entry, or referenced) is never read:
// the hint must not block on it.
func TestKRMFunctionDeclaredReadsRegularFilesOnly(t *testing.T) {
	project := t.TempDir()
	testutil.WriteFile(t, filepath.Join(project, "entry", "kustomization.yaml"), "resources:\n- cm.yaml\ntransformers:\n- pipe.yaml\n")
	testutil.WriteFile(t, filepath.Join(project, "entry", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	if err := syscall.Mkfifo(filepath.Join(project, "entry", "pipe.yaml"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan bool, 1)
	go func() { done <- KRMFunctionDeclared(filepath.Join(project, "entry"), project) }()
	select {
	case got := <-done:
		if got {
			t.Error("a FIFO marked the entry partial")
		}
	case <-time.After(5 * time.Second):
		// Unblock the reader so the test binary can exit.
		if f, err := os.OpenFile(filepath.Join(project, "entry", "pipe.yaml"), os.O_WRONLY, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("the hint blocked on a FIFO")
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

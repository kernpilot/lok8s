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
// regular files only, and marks the entry when a reference or a
// kustomization file resolves outside the project or a reference is remote:
// the hint cannot rule a function out there.
//
// Review round 7: an unread reference is its own kind (KRMUnread, its own
// label), a function anywhere wins over it (item 3); a referenced DIRECTORY
// link is followed into the project and marks the entry by what the target
// holds, or as unread when it leaves the project (item 5); a reference is
// cleaned lexically before its links resolve, as kustomize does, so
// `l/../fns` is the `fns` next to the link, not the one next to its target
// (item 6). The bash twin runs the same cases (tests/unit/bootstrap_test.bats,
// bootstrap::_krm_declared).
func TestKRMFunctionHint(t *testing.T) {
	const fn = "apiVersion: example.test/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: ./fn\n"
	const inline = "transformers:\n- |-\n  apiVersion: example.test/v1\n  kind: Fn\n  metadata:\n    name: fn\n    annotations:\n      config.kubernetes.io/function: |\n        exec:\n          path: ./fn\n"
	const fnBase = "transformers:\n- fn.yaml\n"
	const plainBase = "resources:\n- cm.yaml\n"
	const cm = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"
	for _, tc := range []struct {
		name  string
		files map[string]string // relative to the project; "@<rel>" links to <rel> in the project, "@@OUT/<rel>" to <rel> outside it; "@@PROJECT" in content is the project path
		want  KRMHint
	}{
		{"inline function in an extensionless Kustomization", map[string]string{
			"entry/Kustomization": inline,
		}, KRMFunction},
		{"inline function in a base's extensionless Kustomization", map[string]string{
			"shared/base/Kustomization": inline,
			"entry/kustomization.yaml":  "resources:\n- ../shared/base\n",
		}, KRMFunction},
		{"a base outside the project", map[string]string{
			"entry/kustomization.yaml": "resources:\n- @@OUT/base\n",
		}, KRMUnread},
		{"a remote URL", map[string]string{
			"entry/kustomization.yaml": "resources:\n- https://example.test/raw/cm.yaml\n",
		}, KRMUnread},
		{"a remote GitHub base", map[string]string{
			"entry/kustomization.yaml": "resources:\n- github.com/example/repo/base?ref=v1\n",
		}, KRMUnread},
		{"a remote git base", map[string]string{
			"entry/kustomization.yaml": "resources:\n- git@github.com:example/repo.git//base\n",
		}, KRMUnread},
		{"a remote base and a function: the function wins", map[string]string{
			"entry/kustomization.yaml":  "resources:\n- https://example.test/raw/cm.yaml\n- ../shared/base\n",
			"shared/base/Kustomization": inline,
		}, KRMFunction},
		{"an unreferenced .yaml link to a file outside", map[string]string{
			"entry/kustomization.yaml": "resources:\n- cm.yaml\n",
			"entry/fn.yaml":            "@@OUT/fn.yaml",
		}, KRMNone},
		{"a referenced .yaml link to a file outside", map[string]string{
			"entry/kustomization.yaml": "transformers:\n- fn.yaml\n",
			"entry/fn.yaml":            "@@OUT/fn.yaml",
		}, KRMUnread},
		{"a referenced .yaml link to a file inside", map[string]string{
			"entry/kustomization.yaml": "transformers:\n- fn.yaml\n",
			"entry/fn.yaml":            "@fns/fn.yaml",
			"fns/fn.yaml":              fn,
		}, KRMFunction},
		{"an unreferenced .yaml link to a file inside", map[string]string{
			"entry/kustomization.yaml": "resources:\n- cm.yaml\n",
			"entry/fn.yaml":            "@fns/fn.yaml",
			"fns/fn.yaml":              fn,
		}, KRMNone},
		{"a Kustomization that links outside", map[string]string{
			"entry/kustomization.yaml": "@@OUT/kustomization.yaml",
		}, KRMUnread},
		{"a referenced directory link to a base with a function", map[string]string{
			"entry/kustomization.yaml":         "resources:\n- linked\n",
			"entry/linked":                     "@shared/fnbase",
			"shared/fnbase/kustomization.yaml": fnBase,
			"shared/fnbase/fn.yaml":            fn,
		}, KRMFunction},
		{"a referenced directory link to a plain base", map[string]string{
			"entry/kustomization.yaml":        "resources:\n- linked\n",
			"entry/linked":                    "@shared/plain",
			"shared/plain/kustomization.yaml": plainBase,
			"shared/plain/cm.yaml":            cm,
		}, KRMNone},
		{"a referenced directory link out of the project", map[string]string{
			"entry/kustomization.yaml": "resources:\n- linked\n",
			"entry/linked":             "@@OUT/base",
		}, KRMUnread},
		{"link/../x is cleaned before the link resolves (relative)", map[string]string{
			"entry/kustomization.yaml":    "resources:\n- ../l/../fns\n",
			"l":                           "@deep/a",
			"deep/a/cm.yaml":              cm,
			"fns/kustomization.yaml":      fnBase,
			"fns/fn.yaml":                 fn,
			"deep/fns/kustomization.yaml": plainBase,
			"deep/fns/cm.yaml":            cm,
		}, KRMFunction},
		{"link/../x is cleaned before the link resolves (absolute)", map[string]string{
			"entry/kustomization.yaml":    "resources:\n- @@PROJECT/l/../fns\n",
			"l":                           "@deep/a",
			"deep/a/cm.yaml":              cm,
			"fns/kustomization.yaml":      fnBase,
			"fns/fn.yaml":                 fn,
			"deep/fns/kustomization.yaml": plainBase,
			"deep/fns/cm.yaml":            cm,
		}, KRMFunction},
		{"link/../x does not reach the target's sibling", map[string]string{
			"entry/kustomization.yaml":    "resources:\n- ../l/../fns\n",
			"l":                           "@deep/a",
			"deep/a/cm.yaml":              cm,
			"fns/kustomization.yaml":      plainBase,
			"fns/cm.yaml":                 cm,
			"deep/fns/kustomization.yaml": fnBase,
			"deep/fns/fn.yaml":            fn,
		}, KRMNone},
		{"a missing reference", map[string]string{
			"entry/kustomization.yaml": "resources:\n- ../nowhere\n",
		}, KRMNone},
		{"plain", map[string]string{
			"shared/base/kustomization.yaml": plainBase,
			"shared/base/cm.yaml":            cm,
			"entry/kustomization.yaml":       "resources:\n- ../shared/base\n",
		}, KRMNone},
	} {
		project, out := t.TempDir(), t.TempDir()
		testutil.WriteFile(t, filepath.Join(out, "fn.yaml"), fn)
		testutil.WriteFile(t, filepath.Join(out, "kustomization.yaml"), plainBase)
		testutil.WriteFile(t, filepath.Join(out, "base", "kustomization.yaml"), plainBase)
		testutil.WriteFile(t, filepath.Join(project, "entry", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: entry\n")
		for rel, content := range tc.files {
			path := filepath.Join(project, rel)
			switch {
			case strings.HasPrefix(content, "@@OUT/"):
				link(t, filepath.Join(out, strings.TrimPrefix(content, "@@OUT/")), path)
			case strings.HasPrefix(content, "@") && !strings.HasPrefix(content, "@@"):
				link(t, filepath.Join(project, strings.TrimPrefix(content, "@")), path)
			default:
				content = strings.ReplaceAll(content, "@@OUT", out)
				testutil.WriteFile(t, path, strings.ReplaceAll(content, "@@PROJECT", project))
			}
		}
		if got := KRMFunctionHint(filepath.Join(project, "entry"), project); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An entry outside the roots (a framework addon a dry run peeks lives in a
// temp directory) reads its own tree: a reference into it is inside.
func TestKRMFunctionHintEntryOutsideTheRoots(t *testing.T) {
	entry, project := t.TempDir(), t.TempDir()
	testutil.WriteFile(t, filepath.Join(entry, "kustomization.yaml"), "resources:\n- sub\n")
	testutil.WriteFile(t, filepath.Join(entry, "sub", "kustomization.yaml"), "resources:\n- cm.yaml\n")
	testutil.WriteFile(t, filepath.Join(entry, "sub", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	if KRMFunctionHint(entry, project) != KRMNone {
		t.Error("a reference into the entry's own directory counted as outside the project")
	}
}

// A FIFO named like YAML (below the entry, or referenced) is never read:
// the hint must not block on it.
func TestKRMFunctionHintReadsRegularFilesOnly(t *testing.T) {
	project := t.TempDir()
	testutil.WriteFile(t, filepath.Join(project, "entry", "kustomization.yaml"), "resources:\n- cm.yaml\ntransformers:\n- pipe.yaml\n")
	testutil.WriteFile(t, filepath.Join(project, "entry", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	if err := syscall.Mkfifo(filepath.Join(project, "entry", "pipe.yaml"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan KRMHint, 1)
	go func() { done <- KRMFunctionHint(filepath.Join(project, "entry"), project) }()
	select {
	case got := <-done:
		if got != KRMNone {
			t.Errorf("a FIFO marked the entry: %v", got)
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

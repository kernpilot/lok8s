package bootstrap

// dryrun_round5_test.go — review round 5 on #268: non-determinism over the
// union of both renders' objects, and the KRM `partial` hint along the
// kustomization graph.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// An object that only one of the two renders has is non-deterministic too:
// the comparison is over the union of the object identities, in the first
// render's order, then the second render's own.
func TestNondeterministicObjectsComparesTheUnion(t *testing.T) {
	first := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stable\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: changes\ndata:\n  k: YQ==\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: first-only\n"
	second := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: second-only\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stable\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: changes\ndata:\n  k: Yg==\n"
	got, err := nondeterministicObjects(first, second)
	want := []string{"v1 Secret changes", "v1 Secret first-only", "v1 Secret second-only"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %q (%v), want %q", got, err, want)
	}
}

// Through the engine: the second render has an object the first does not.
// The entry lists it; --ignore-nondeterministic diffs the first render (it
// does not hold the object).
func TestDryRunListsAnObjectOnlyTheSecondRenderHas(t *testing.T) {
	e, _, spec, kc := dryEngine(t, "testcni")
	renders := 0
	var diffIn []string
	e.Runner = runnerFunc(func(_ context.Context, c execx.Cmd) error {
		switch c.Name {
		case "kustomize":
			renders++
			fmt.Fprint(c.Stdout, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stable\n  namespace: kube-system\n")
			if renders == 2 {
				fmt.Fprint(c.Stdout, "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: late\n  namespace: kube-system\n")
			}
		case "kubectl":
			raw, _ := io.ReadAll(c.Stdin)
			diffIn = append(diffIn, string(raw))
		}
		return nil
	})
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v\n%s", err, stderrOf(e))
	}
	out := stdoutOf(e)
	if !strings.Contains(out, "01-testcni: unchanged (non-deterministic: 1)\n") ||
		!strings.Contains(out, "01-testcni: non-deterministic (regenerated on every apply): v1 Secret kube-system/late\n") ||
		!strings.Contains(out, " nondeterministic=1\n") {
		t.Errorf("stdout:\n%s", out)
	}
	if len(diffIn) != 1 || strings.Contains(diffIn[0], "late") {
		t.Errorf("the diff got %q, want the first render", diffIn)
	}
}

// The KRM hint follows the kustomization graph: a function declared in a
// base the entry references (../base), in a component, or in a transformer
// file outside the entry's directory marks the entry partial. A reference
// outside the project, or a remote one, is not read; since review round 6
// (item 5) it marks the entry partial too, because the hint cannot rule a
// function out there.
func TestDryRunKRMHintFollowsTheKustomizationGraph(t *testing.T) {
	fn := "apiVersion: example.test/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: ./fn\n"
	for _, tc := range []struct {
		name  string
		files map[string]string // relative to the project root
		want  bool
	}{
		{"base", map[string]string{
			"shared/base/kustomization.yaml": "transformers:\n- fn.yaml\n",
			"shared/base/fn.yaml":            fn,
			"entry/kustomization.yaml":       "resources:\n- ../shared/base\n",
		}, true},
		{"component", map[string]string{
			"shared/comp/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\ntransformers:\n- fn.yaml\n",
			"shared/comp/fn.yaml":            fn,
			"entry/kustomization.yaml":       "components:\n- ../shared/comp\n",
		}, true},
		{"transformer file", map[string]string{
			"fns/fn.yaml":              fn,
			"entry/kustomization.yaml": "transformers:\n- ../fns/fn.yaml\n",
		}, true},
		{"nested", map[string]string{
			"a/kustomization.yaml":     "resources:\n- ../b\n",
			"b/kustomization.yaml":     "resources:\n- ../a\nbases:\n- ../c\n", // a cycle, then a base
			"c/kustomization.yaml":     "generators:\n- fn.yaml\n",
			"c/fn.yaml":                fn,
			"entry/kustomization.yaml": "resources:\n- ../a\n",
		}, true},
		{"remote only", map[string]string{
			"entry/kustomization.yaml": "resources:\n- https://github.com/example/repo//base?ref=v1\n- github.com/example/repo/base\n",
		}, true},
		{"inline function in an extensionless Kustomization (round 6, item 4)", map[string]string{
			"entry/Kustomization": "resources:\n- cm.yaml\ntransformers:\n- |-\n  apiVersion: example.test/v1\n  kind: Fn\n  metadata:\n    name: fn\n    annotations:\n      config.kubernetes.io/function: |\n        exec:\n          path: ./fn\n",
		}, true},
		{"plain", map[string]string{
			"shared/base/kustomization.yaml": "resources:\n- cm.yaml\n",
			"shared/base/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n",
			"entry/kustomization.yaml":       "resources:\n- ../shared/base\n",
		}, false},
	} {
		e, f, _, _, p := testEngine(t)
		for rel, content := range tc.files {
			testutil.WriteFile(t, filepath.Join(p.Base, rel), content)
		}
		testutil.WriteFile(t, filepath.Join(p.Base, "entry", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: entry\n")
		spec := writeClusterSpec(t, p, "/entry")
		kc := writeKubeconfig(t, p)
		e.Options.DryRun = true
		diffAnswers(t, f, 0)
		if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
			t.Fatalf("%s: Apply: %v\n%s", tc.name, err, stderrOf(e))
		}
		if marked := strings.Contains(stdoutOf(e), "(partial: KRM function skipped)"); marked != tc.want {
			t.Errorf("%s: marked=%v, want %v\n%s", tc.name, marked, tc.want, stdoutOf(e))
		}
	}

	// Outside the project: not read, and marked partial (round 6, item 5).
	e, f, _, _, p := testEngine(t)
	outside := t.TempDir()
	testutil.WriteFile(t, filepath.Join(outside, "base", "kustomization.yaml"), "transformers:\n- fn.yaml\n")
	testutil.WriteFile(t, filepath.Join(outside, "base", "fn.yaml"), fn)
	testutil.WriteFile(t, filepath.Join(p.Base, "entry", "kustomization.yaml"), "resources:\n- "+filepath.Join(outside, "base")+"\n")
	spec := writeClusterSpec(t, p, "/entry")
	kc := writeKubeconfig(t, p)
	e.Options.DryRun = true
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("outside: Apply: %v", err)
	}
	if !strings.Contains(stdoutOf(e), "01-entry: unchanged (partial: KRM function skipped)\n") {
		t.Errorf("a base outside the project did not mark the entry partial:\n%s", stdoutOf(e))
	}
}

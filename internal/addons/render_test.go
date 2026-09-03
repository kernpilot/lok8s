package addons

// render_test.go — hermetic tests for the addon render pipeline, ported
// from the value-stacking half of tests/unit/bootstrap_test.bats (the bats
// stubbed kustomize/kubectl/envsubst and asserted on the staged merged
// values). khelm/kustomize NEVER runs: the fake runner captures its argv
// and emits a canned manifest — the golden under testdata/ was generated
// ONCE from the bash addons::render over the same canned manifest
// (hack: see the golden header note below), so bash stays the source of
// truth for the pipeline semantics.
//
// DOCUMENTED FORMATTING DEVIATION: the bash pipeline re-emits the stream
// through yq (which preserves the source's sequence-indentation style);
// yaml.v3 normalizes sequence indentation. The golden comparison is
// therefore SEMANTIC (parsed-YAML equality) plus exact-string pins on the
// load-bearing bytes (the coerced env quoting, the envsubst substitution,
// the untouched-`$` guarantee) — not byte equality.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
	"gopkg.in/yaml.v3"
)

// kustomizeManifest is the canned build output the golden was generated
// from (the fake kustomize in the bash golden-gen run emitted exactly this).
const kustomizeManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: testcni
  namespace: kube-system
data:
  host: "${LOK8S_USER_API_HOST}"
  untouched: "$HOME and ${NOT_LISTED} stay"
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: testcni
spec:
  template:
    spec:
      containers:
        - name: agent
          env:
            - name: KUBERNETES_SERVICE_PORT
              value: 6443
            - name: ENABLED
              value: true
            - name: ALREADY_STR
              value: "yes"
            - name: FROM_FIELD
              valueFrom:
                fieldRef:
                  fieldPath: status.hostIP
`

// fakeKustomize records the build invocation, copies the staged
// values.merged.yaml out of the temp build dir (the render deletes it), and
// emits the canned manifest — mirroring the bats kustomize stub. It is the
// exec-pipeline seam: newFakeKustomize pins LO_RENDER=exec so the render
// goes through the runner. TestRenderInProcess covers the default
// in-process pipeline over a real kustomization.
type fakeKustomize struct {
	t         *testing.T
	calls     [][]string
	envs      [][]string
	mergedOut string // last staged values.merged.yaml content
	buildDirs []string
	manifest  string
	fail      bool
}

func newFakeKustomize(t *testing.T, manifest string) *fakeKustomize {
	t.Helper()
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	return &fakeKustomize{t: t, manifest: manifest}
}

func (f *fakeKustomize) Run(ctx context.Context, c execx.Cmd) error {
	if c.Name != "kustomize" {
		f.t.Fatalf("unexpected tool %q", c.Name)
	}
	f.calls = append(f.calls, append([]string{c.Name}, c.Args...))
	f.envs = append(f.envs, c.Env)
	buildDir := c.Args[len(c.Args)-1]
	f.buildDirs = append(f.buildDirs, buildDir)
	if raw, err := os.ReadFile(filepath.Join(buildDir, "values.merged.yaml")); err == nil {
		f.mergedOut = string(raw)
	}
	if f.fail {
		return fmt.Errorf("exit status 1")
	}
	if c.Stdout != nil {
		fmt.Fprint(c.Stdout, f.manifest)
	}
	return nil
}

// writeAddon lays down the bats fixture addon: chart.yaml + the three-layer
// values files (base < driver lo < provider hetzner).
func writeAddon(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "addons", "testcni")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"chart.yaml":          "apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: testcni\nvalueFiles:\n  - values.yaml\n",
		"values.yaml":         "only_base: \"base\"\nshared_all: \"base\"\nnested:\n  overridden: \"base\"\n  from_base: true\n",
		"values.lo.yaml":      "only_driver: \"driver\"\nshared_all: \"driver\"\nnested:\n  overridden: \"driver\"\n",
		"values.hetzner.yaml": "only_provider: \"provider\"\nshared_all: \"provider\"\nnested:\n  overridden: \"provider\"\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const inlineValues = "inline_marker: \"inline\"\nshared_all: \"inline\"\nnested:\n  overridden: \"inline\""

func yqr(t *testing.T, doc, path string) string {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	cur := any(m)
	for _, key := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return "missing"
		}
		cur, ok = mm[key]
		if !ok {
			return "missing"
		}
	}
	return fmt.Sprintf("%v", cur)
}

func TestRenderStacksBaseDriverProviderInline(t *testing.T) {
	dir := writeAddon(t)
	f := newFakeKustomize(t, kustomizeManifest)
	var errBuf strings.Builder
	t.Setenv("LOK8S_USER_API_HOST", "10.0.0.1")

	out, err := Render(context.Background(), f, &errBuf, dir, "lo", "hetzner", inlineValues, nil)
	if err != nil {
		t.Fatalf("Render: %v (stderr: %s)", err, errBuf.String())
	}

	// The staged merge: every layer's unique key survives; precedence is
	// base < driver < provider < inline (bats: the last_merged.yaml asserts).
	merged := f.mergedOut
	for path, want := range map[string]string{
		"only_base": "base", "only_driver": "driver", "only_provider": "provider",
		"shared_all": "inline", "inline_marker": "inline",
		"nested.overridden": "inline", "nested.from_base": "true",
	} {
		if got := yqr(t, merged, path); got != want {
			t.Errorf("merged %s = %q, want %q\n%s", path, got, want, merged)
		}
	}
	// Byte-parity with the yq merge the bash staged (golden generated once
	// from bash — see testdata/merged_values_golden.yaml).
	golden, err := os.ReadFile("testdata/merged_values_golden.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if merged != string(golden) {
		t.Errorf("merged values diverge from the bash golden:\n--- got ---\n%s--- want ---\n%s", merged, golden)
	}

	// khelm/kustomize was invoked exactly once, with the plugin flags, on a
	// STAGED COPY (never the source addon dir), with KHELM_TRUST_ANY_REPO.
	if len(f.calls) != 1 {
		t.Fatalf("kustomize calls = %d, want 1", len(f.calls))
	}
	wantArgs := []string{"kustomize", "build", "--enable-alpha-plugins", "--enable-exec", f.buildDirs[0]}
	if !reflect.DeepEqual(f.calls[0], wantArgs) {
		t.Errorf("argv = %v, want %v", f.calls[0], wantArgs)
	}
	if f.buildDirs[0] == dir {
		t.Errorf("stacking render ran in place — must stage a temp copy")
	}
	found := false
	for _, kv := range f.envs[0] {
		if kv == "KHELM_TRUST_ANY_REPO=true" {
			found = true
		}
	}
	if !found {
		t.Errorf("KHELM_TRUST_ANY_REPO=true missing from env: %v", f.envs[0])
	}

	// The output pipeline: envsubst hit the whitelisted var, left every
	// other `$` alone, and coerced numeric/bool env values to strings.
	for _, pin := range []string{
		`host: "10.0.0.1"`,
		`untouched: "$HOME and ${NOT_LISTED} stay"`,
		`value: "6443"`,
		`value: "true"`,
		`value: "yes"`,
		"fieldPath: status.hostIP",
	} {
		if !strings.Contains(out, pin) {
			t.Errorf("output missing %q:\n%s", pin, out)
		}
	}

	// Semantic equality with the bash golden (formatting normalization is
	// the documented deviation; the MEANING must be identical).
	golden2, err := os.ReadFile("testdata/render_golden.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !yamlStreamEqual(t, out, string(golden2)) {
		t.Errorf("render output semantically diverges from the bash golden:\n--- got ---\n%s\n--- want ---\n%s", out, golden2)
	}
}

func yamlStreamEqual(t *testing.T, a, b string) bool {
	t.Helper()
	return reflect.DeepEqual(parseStream(t, a), parseStream(t, b))
}

func parseStream(t *testing.T, s string) []any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(s))
	var docs []any
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		docs = append(docs, v)
	}
	return docs
}

func TestRenderFallsBackToDriverWithoutProviderValues(t *testing.T) {
	dir := writeAddon(t)
	os.Remove(filepath.Join(dir, "values.hetzner.yaml"))
	f := newFakeKustomize(t, kustomizeManifest)
	var errBuf strings.Builder
	if _, err := Render(context.Background(), f, &errBuf, dir, "lo", "hetzner", "", nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := yqr(t, f.mergedOut, "shared_all"); got != "driver" {
		t.Errorf("shared_all = %q, want driver", got)
	}
	if got := yqr(t, f.mergedOut, "only_provider"); got != "missing" {
		t.Errorf("only_provider leaked: %q", got)
	}
}

func TestRenderBaseOnlyWhenNoOverlays(t *testing.T) {
	dir := writeAddon(t)
	os.Remove(filepath.Join(dir, "values.lo.yaml"))
	os.Remove(filepath.Join(dir, "values.hetzner.yaml"))
	f := newFakeKustomize(t, kustomizeManifest)
	var errBuf strings.Builder
	if _, err := Render(context.Background(), f, &errBuf, dir, "lo", "hetzner", "", nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := yqr(t, f.mergedOut, "shared_all"); got != "base" {
		t.Errorf("shared_all = %q, want base", got)
	}
}

func TestRenderInPlaceWhenNothingToStack(t *testing.T) {
	// A chart pinning its OWN valueFiles (no base values.yaml, no inline)
	// must render IN PLACE: a copy breaks cross-tree relative refs
	// (monitoring, gatus).
	dir := filepath.Join(t.TempDir(), "addon")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "chart.yaml"),
		[]byte("kind: ChartRenderer\nvalueFiles:\n  - ../../../../.lok8s/addons/x/values.yaml\n"), 0o644)
	f := newFakeKustomize(t, kustomizeManifest)
	var errBuf strings.Builder
	if _, err := Render(context.Background(), f, &errBuf, dir, "lo", "", "", nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if f.buildDirs[0] != dir {
		t.Errorf("no-stack render staged a copy (%s) — must run in place", f.buildDirs[0])
	}
	// chart.yaml untouched (its own valueFiles pins survive).
	raw, _ := os.ReadFile(filepath.Join(dir, "chart.yaml"))
	if !strings.Contains(string(raw), "../../../../.lok8s/addons/x/values.yaml") {
		t.Errorf("chart.yaml was rewritten in place:\n%s", raw)
	}
}

func TestRenderDotfilesSurviveTheCopy(t *testing.T) {
	// Dotfiles must ride the staged copy (bash: `cp -r dir/.` — a glob
	// drops them and a chart referencing a dotfile fails to render). The
	// staged copy is deleted after the render, so the probe runner checks
	// mid-flight.
	dir := writeAddon(t)
	os.WriteFile(filepath.Join(dir, ".helmignore"), []byte("*.md\n"), 0o644)
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	var errBuf strings.Builder
	probe := &probeRunner{t: t, check: func(buildDir string) {
		if _, err := os.Stat(filepath.Join(buildDir, ".helmignore")); err != nil {
			t.Errorf(".helmignore missing from staged copy %s", buildDir)
		}
	}, manifest: kustomizeManifest}
	if _, err := Render(context.Background(), probe, &errBuf, dir, "lo", "hetzner", "", nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
}

type probeRunner struct {
	t        *testing.T
	check    func(buildDir string)
	manifest string
}

func (p *probeRunner) Run(ctx context.Context, c execx.Cmd) error {
	p.check(c.Args[len(c.Args)-1])
	if c.Stdout != nil {
		fmt.Fprint(c.Stdout, p.manifest)
	}
	return nil
}

func TestRenderEmptyOutputIsAnError(t *testing.T) {
	// A successful build that yields nothing means the addon rendered no
	// resources — almost always a misconfig. Fail loud rather than report
	// success for an empty apply.
	dir := writeAddon(t)
	f := newFakeKustomize(t, "")
	var errBuf strings.Builder
	_, err := Render(context.Background(), f, &errBuf, dir, "lo", "hetzner", "", nil)
	if err == nil {
		t.Fatal("empty render must fail")
	}
	if !strings.Contains(errBuf.String(), "addons::render: empty output for "+dir+" (no resources rendered)") {
		t.Errorf("missing exact empty-output error: %q", errBuf.String())
	}
}

func TestRenderBuildFailurePropagates(t *testing.T) {
	dir := writeAddon(t)
	f := newFakeKustomize(t, "")
	f.fail = true
	var errBuf strings.Builder
	if _, err := Render(context.Background(), f, &errBuf, dir, "lo", "hetzner", "", nil); err == nil {
		t.Fatal("build failure must propagate")
	}
}

func TestRenderPerEntryEnvReachesProcessAndEnvsubst(t *testing.T) {
	// The per-entry env: overrides join the kustomize process env AND the
	// envsubst lookup — without touching the shared process environment.
	dir := writeAddon(t)
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\ndata:\n  v: \"${LOK8S_USER_TESTVAR}\"\n"
	f := newFakeKustomize(t, manifest)
	var errBuf strings.Builder
	out, err := Render(context.Background(), f, &errBuf, dir, "lo", "hetzner", "",
		map[string]string{"LOK8S_USER_TESTVAR": "hello"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, `v: "hello"`) {
		t.Errorf("env override not substituted:\n%s", out)
	}
	foundEnv := false
	for _, kv := range f.envs[0] {
		if kv == "LOK8S_USER_TESTVAR=hello" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("override missing from kustomize env: %v", f.envs[0])
	}
	if os.Getenv("LOK8S_USER_TESTVAR") != "" {
		t.Errorf("override leaked into the process environment")
	}
}

func TestMergeNodesListsReplace(t *testing.T) {
	// yq `*`: maps deep-merge, LISTS REPLACE (right wins) — the semantics
	// every value stack in the pipeline depends on.
	out, err := MergeYAML("a:\n  - 1\n  - 2\nkeep: x\n", "a:\n  - 3\n")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	yaml.Unmarshal(out, &m)
	list, _ := m["a"].([]any)
	if len(list) != 1 || fmt.Sprint(list[0]) != "3" {
		t.Errorf("list did not replace: %v", m["a"])
	}
	if m["keep"] != "x" {
		t.Errorf("unrelated key lost: %v", m)
	}
}

// TestRenderInProcess: the default pipeline over a real (plugin-free)
// addon — no runner is consulted; the in-process kustomize API renders the
// manifest and the envsubst + env-coercion passes run over its bytes.
func TestRenderInProcess(t *testing.T) {
	t.Setenv(render.ModeEnv, "")
	dir := filepath.Join(t.TempDir(), "addon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources:\n  - manifest.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(kustomizeManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	var errBuf strings.Builder
	got, err := Render(context.Background(), nil, &errBuf, dir, "lo", "", "", map[string]string{"LOK8S_USER_API_HOST": "api.example"})
	if err != nil {
		t.Fatalf("Render: %v\n%s", err, errBuf.String())
	}
	for _, want := range []string{"host: api.example", "untouched: $HOME and ${NOT_LISTED} stay", `value: "6443"`, `value: "true"`, "fieldPath: status.hostIP"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

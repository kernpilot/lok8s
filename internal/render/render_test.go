package render

// render_test.go — the in-process kustomize render against the exec
// pipeline it replaced. Where the pinned binaries are present in the
// repo's .bin/.kustomize (b install), the in-process bytes are compared
// with the binary's bytes for the same fixture: plain kustomizations, the
// secrets.lok8s.dev Secret generator (served by THIS test binary through
// the self-exec plugin home — TestMain dispatches), and a khelm
// ChartRenderer over a local chart (no network). Without the binaries the
// exec comparisons skip; the in-process assertions still run.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/api/types"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

func TestMain(m *testing.M) {
	// The self-exec plugin home points at THIS binary: when kustomize
	// execs …/secret/Secret or …/chartrenderer/ChartRenderer, the test
	// binary must behave as the plugin, exactly like `lo` does.
	if handled, rc := DispatchPlugin(os.Args, os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(rc)
	}
	rc := m.Run()
	Cleanup()
	os.Exit(rc)
}

// repoRoot is the lok8s checkout (three levels up from internal/render).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// pinnedKustomize returns the repo's b-managed kustomize binary, skipping
// the test when it is not installed.
func pinnedKustomize(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(repoRoot(t), ".bin", "kustomize")
	if info, err := os.Stat(bin); err != nil || info.Mode()&0o111 == 0 {
		t.Skip("pinned kustomize not installed under .bin (b install)")
	}
	return bin
}

// execKustomize runs the pinned binary the way the exec pipeline did:
// `kustomize build --enable-alpha-plugins [--enable-exec] dir` with the
// repo's .kustomize as KUSTOMIZE_PLUGIN_HOME (skipping when a needed plugin
// binary is absent) and the overlay in the environment.
func execKustomize(t *testing.T, dir string, enableExec bool, overlay []string, needPlugins ...string) []byte {
	t.Helper()
	bin := pinnedKustomize(t)
	home := filepath.Join(repoRoot(t), ".kustomize")
	for _, p := range needPlugins {
		if info, err := os.Stat(filepath.Join(home, filepath.FromSlash(p))); err != nil || info.Mode()&0o111 == 0 {
			t.Skipf("pinned plugin %s not installed under .kustomize", p)
		}
	}
	args := []string{"build", "--enable-alpha-plugins"}
	if enableExec {
		args = append(args, "--enable-exec")
	}
	args = append(args, dir)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), overlay...)
	cmd.Env = append(cmd.Env, "KUSTOMIZE_PLUGIN_HOME="+home)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pinned kustomize: %v\n%s", err, stderr.String())
	}
	return out
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// plainFixture exercises the pieces of a build whose bytes depend on the
// kustomize version: key sorting, the legacy resource ordering (Namespace
// first), a strategic-merge patch, a commonLabels-free labels transformer,
// a multi-document source, and a nameSuffix.
func plainFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"kustomization.yaml": `resources:
  - manifests.yaml
nameSuffix: -x
labels:
  - pairs:
      app.kubernetes.io/part-of: parity
    includeSelectors: true
patches:
  - path: patch.yaml
`,
		"manifests.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: demo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: web
          image: nginx:1.27
          ports:
            - containerPort: 80
---
apiVersion: v1
kind: Namespace
metadata:
  name: demo
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: demo
data:
  z: "1"
  a: |
    multi
    line
`,
		"patch.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: demo
spec:
  replicas: 3
`,
	})
	return dir
}

func TestCurrentModeRejectsUnknownValue(t *testing.T) {
	t.Setenv(ModeEnv, "sometimes")
	if _, err := CurrentMode(); err == nil || !strings.Contains(err.Error(), "LO_RENDER") {
		t.Fatalf("unknown LO_RENDER accepted: %v", err)
	}
	t.Setenv(ModeEnv, "EXEC")
	if m, err := CurrentMode(); err != nil || m != ModeExec {
		t.Fatalf("LO_RENDER=EXEC → %q, %v", m, err)
	}
	t.Setenv(ModeEnv, "")
	if m, err := CurrentMode(); err != nil || m != ModeInProcess {
		t.Fatalf("unset → %q, %v", m, err)
	}
}

func TestBuildInProcessPlainKustomization(t *testing.T) {
	t.Setenv(ModeEnv, "")
	dir := plainFixture(t)
	var stderr bytes.Buffer
	out, err := Build(context.Background(), dir, Options{Stderr: &stderr})
	if err != nil {
		t.Fatalf("Build: %v\n%s", err, stderr.String())
	}
	// Legacy order: the Namespace first (unsuffixed — kustomize exempts
	// Namespace names); the suffix and label applied; the patch merged;
	// keys sorted.
	want := "apiVersion: v1\nkind: Namespace\nmetadata:\n  labels:\n    app.kubernetes.io/part-of: parity\n  name: demo\n---\n"
	if !strings.HasPrefix(string(out), want) {
		t.Fatalf("output does not start with the Namespace:\n%s", out)
	}
	for _, s := range []string{"replicas: 3", "name: cfg-x", "a: |\n    multi\n    line", "app.kubernetes.io/part-of: parity"} {
		if !strings.Contains(string(out), s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr not empty: %q", stderr.String())
	}
}

func TestBuildInProcessMatchesPinnedBinaryPlain(t *testing.T) {
	t.Setenv(ModeEnv, "")
	dir := plainFixture(t)
	want := execKustomize(t, dir, false, nil)
	got, err := Build(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("in-process render differs from the pinned kustomize binary:\n--- binary\n%s\n--- in-process\n%s", want, got)
	}
}

// secretFixture: a kustomization whose generator is the secrets.lok8s.dev
// Secret plugin, served by the self-exec home (in-process) / the built
// plugin under .kustomize (exec). Deterministic sections only (literals,
// b64, env) so both pipelines produce the same bytes without a store.
func secretFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"kustomization.yaml": "generators:\n  - secret.yaml\n",
		"secret.yaml": `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: app
  namespace: demo
literals:
  USER: alice
env:
  FROM_ENV: RENDER_TEST_VALUE
`,
	})
	return dir
}

func TestBuildInProcessSecretGeneratorViaSelfExec(t *testing.T) {
	t.Setenv(ModeEnv, "")
	t.Setenv("PATH_SECRETS", t.TempDir())
	t.Setenv("RENDER_TEST_VALUE", "from-parent-env")
	dir := secretFixture(t)
	var stderr bytes.Buffer
	out, err := Build(context.Background(), dir, Options{Stderr: &stderr})
	if err != nil {
		t.Fatalf("Build: %v\n%s", err, stderr.String())
	}
	// base64("alice") / base64("from-parent-env")
	for _, s := range []string{"kind: Secret", "name: app", "namespace: demo", "USER: YWxpY2U=", "FROM_ENV: ZnJvbS1wYXJlbnQtZW52"} {
		if !strings.Contains(string(out), s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
}

func TestBuildInProcessMatchesPinnedBinarySecret(t *testing.T) {
	t.Setenv(ModeEnv, "")
	t.Setenv("PATH_SECRETS", t.TempDir())
	t.Setenv("RENDER_TEST_VALUE", "same-in-both")
	dir := secretFixture(t)
	want := execKustomize(t, dir, false, nil, secretPluginRel)
	got, err := Build(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("in-process Secret render differs from the pinned pipeline:\n--- binary\n%s\n--- in-process\n%s", want, got)
	}
}

func TestBuildEnvOverlayReachesPluginAndIsRestored(t *testing.T) {
	t.Setenv(ModeEnv, "")
	t.Setenv("PATH_SECRETS", t.TempDir())
	t.Setenv("RENDER_TEST_VALUE", "parent")
	for _, k := range []string{"LOK8S_SECRETS_DISABLE", "KUSTOMIZE_PLUGIN_HOME"} {
		t.Setenv(k, "") // registers the restore
		os.Unsetenv(k)
	}
	dir := secretFixture(t)

	// The overlay reaches the plugin child: the store-free switch makes
	// the generator emit nothing, and an env override wins over the
	// parent's value.
	out, err := Build(context.Background(), dir, Options{Env: []string{"LOK8S_SECRETS_DISABLE=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("LOK8S_SECRETS_DISABLE=1 did not reach the plugin:\n%s", out)
	}
	if _, set := os.LookupEnv("LOK8S_SECRETS_DISABLE"); set {
		t.Fatal("overlay leaked into the process environment after the run")
	}
	out, err = Build(context.Background(), dir, Options{Env: []string{"RENDER_TEST_VALUE=overlay"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "FROM_ENV: b3ZlcmxheQ==") { // base64("overlay")
		t.Fatalf("overlay value did not win in the plugin:\n%s", out)
	}
	if os.Getenv("RENDER_TEST_VALUE") != "parent" {
		t.Fatalf("overlay not restored: RENDER_TEST_VALUE=%q", os.Getenv("RENDER_TEST_VALUE"))
	}
	if _, set := os.LookupEnv("KUSTOMIZE_PLUGIN_HOME"); set {
		t.Fatal("KUSTOMIZE_PLUGIN_HOME leaked into the process environment")
	}
}

func TestBuildInProcessFailurePrintsCobraErrorLine(t *testing.T) {
	t.Setenv(ModeEnv, "")
	dir := t.TempDir() // no kustomization.yaml
	var stderr bytes.Buffer
	if _, err := Build(context.Background(), dir, Options{Stderr: &stderr}); err == nil {
		t.Fatal("missing kustomization rendered")
	}
	if !strings.HasPrefix(stderr.String(), "Error: unable to find one of 'kustomization.yaml'") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// chartFixture: a khelm ChartRenderer over a LOCAL chart directory — the
// helm inflation runs without a repository or network. The chart uses the
// pieces whose bytes depend on the helm/khelm version: a values file, the
// release name/namespace from metadata, the `toYaml` + `quote` helpers, a
// numeric value (khelm's kyaml re-serialization decides its quoting) and a
// Namespace resource (kustomize's legacy ordering moves it first).
func chartFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"kustomization.yaml": "generators:\n  - chart.yaml\n",
		"chart.yaml": `apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: demo
  namespace: demo-ns
chart: ./chart
valueFiles:
  - values.override.yaml
values:
  inline: "set"
`,
		"values.override.yaml": "replicas: 4\nlabels:\n  tier: web\n",
		"chart/Chart.yaml":     "apiVersion: v2\nname: demo\nversion: 0.1.0\nappVersion: \"1.0\"\n",
		"chart/values.yaml":    "replicas: 1\nport: 8080\nlabels: {}\ninline: unset\n",
		"chart/templates/all.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-cfg
  namespace: {{ .Release.Namespace }}
  labels:
{{ toYaml .Values.labels | indent 4 }}
data:
  port: {{ .Values.port | quote }}
  inline: {{ .Values.inline }}
  kube: {{ .Capabilities.KubeVersion.Version }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
spec:
  replicas: {{ .Values.replicas }}
  selector:
    matchLabels:
      app: {{ .Release.Name }}
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}
    spec:
      containers:
        - name: app
          image: nginx
          env:
            - name: PORT
              value: {{ .Values.port }}
---
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Release.Namespace }}
`,
	})
	return dir
}

func TestBuildInProcessChartRendererViaSelfExec(t *testing.T) {
	t.Setenv(ModeEnv, "")
	dir := chartFixture(t)
	var stderr bytes.Buffer
	out, err := Build(context.Background(), dir, Options{Stderr: &stderr, Env: []string{"KHELM_TRUST_ANY_REPO=true"}})
	if err != nil {
		t.Fatalf("Build: %v\n%s", err, stderr.String())
	}
	for _, s := range []string{"name: demo-cfg", "namespace: demo-ns", "replicas: 4", "tier: web", `port: "8080"`, "inline: set", "kind: Namespace"} {
		if !strings.Contains(string(out), s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
}

func TestBuildInProcessMatchesPinnedBinaryChart(t *testing.T) {
	t.Setenv(ModeEnv, "")
	dir := chartFixture(t)
	overlay := []string{"KHELM_TRUST_ANY_REPO=true"}
	want := execKustomize(t, dir, true, overlay, chartRendererPluginRel)
	got, err := Build(context.Background(), dir, Options{EnableExec: true, Env: overlay})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("in-process khelm render differs from the pinned ChartRenderer binary:\n--- binary\n%s\n--- in-process\n%s", want, got)
	}
}

// recordingRunner is the exec-mode seam: it records the kustomize argv/env
// and answers with a canned stream.
type recordingRunner struct {
	cmd execx.Cmd
}

func (r *recordingRunner) Run(_ context.Context, c execx.Cmd) error {
	r.cmd = c
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte("kind: Canned\n"))
	}
	return nil
}

func TestBuildExecModeUsesRunnerAndDefaultsPluginHome(t *testing.T) {
	t.Setenv(ModeEnv, "exec")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	r := &recordingRunner{}
	out, err := Build(context.Background(), "/some/dir", Options{
		Paths: p, Runner: r, EnableExec: true, Env: []string{"KHELM_TRUST_ANY_REPO=true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "kind: Canned\n" {
		t.Fatalf("out = %q", out)
	}
	if r.cmd.Name != "kustomize" || strings.Join(r.cmd.Args, " ") != "build --enable-alpha-plugins --enable-exec /some/dir" {
		t.Fatalf("argv = %s %v", r.cmd.Name, r.cmd.Args)
	}
	wantEnv := []string{"KHELM_TRUST_ANY_REPO=true", "KUSTOMIZE_PLUGIN_HOME=" + filepath.Join(base, ".kustomize")}
	if strings.Join(r.cmd.Env, "\n") != strings.Join(wantEnv, "\n") {
		t.Fatalf("env = %v, want %v", r.cmd.Env, wantEnv)
	}

	// No Paths (the addon call shape): the plugin home is left to the
	// environment, exactly as before.
	r = &recordingRunner{}
	if _, err := Build(context.Background(), "/d", Options{Runner: r}); err != nil {
		t.Fatal(err)
	}
	if len(r.cmd.Env) != 0 || strings.Join(r.cmd.Args, " ") != "build --enable-alpha-plugins /d" {
		t.Fatalf("addon-shape exec: args=%v env=%v", r.cmd.Args, r.cmd.Env)
	}
}

func TestBuildLoadRestrictionsNoneIsHonoured(t *testing.T) {
	t.Setenv(ModeEnv, "")
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"outside.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: outside\n",
		"kust/kustomization.yaml": "resources:\n  - ../outside.yaml\n",
	})
	dir := filepath.Join(root, "kust")
	var stderr bytes.Buffer
	if _, err := Build(context.Background(), dir, Options{Stderr: &stderr}); err == nil {
		t.Fatal("RootOnly (the default) accepted a file outside the root")
	}
	out, err := Build(context.Background(), dir, Options{LoadRestrictions: types.LoadRestrictionsNone})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "name: outside") {
		t.Fatalf("LoadRestrictionsNone render:\n%s", out)
	}
}

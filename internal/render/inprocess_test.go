//go:build inprocess

package render

// inprocess_test.go — the in-process kustomize render (lo-full)
// against the exec pipeline it replaced. Where the pinned binaries are
// present in the repo's .bin/.kustomize (b install), the in-process bytes
// are compared with the binary's bytes for the same fixture: plain
// kustomizations, the secrets.lok8s.dev Secret generator (served by THIS
// test binary through the self-exec plugin home — TestMain in
// render_test.go dispatches), and a khelm ChartRenderer over a local chart
// (no network). Without the binaries the exec comparisons skip; the
// in-process assertions still run. The whole file is gated on the
// `inprocess` tag: `go test -tags inprocess ./internal/render/`.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// missingToolchain is how the byte-parity tests react to an absent pinned
// binary: a skip on a developer machine (run `b install` to enable them),
// a FAILURE under CI (Actions sets CI=true) — the parity gate once skipped
// silently on every CI run because the toolchain was installed after the
// Go tests, and a skipped gate looks exactly like a green one.
func missingToolchain(t *testing.T, what string) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("%s — on CI the toolchain must be installed before the Go tests (ci.yml: Install toolchain); the byte-parity gate must not skip", what)
	}
	t.Skip(what)
}

// pinnedKustomize returns the repo's b-managed kustomize binary, skipping
// the test when it is not installed (failing on CI).
func pinnedKustomize(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(testutil.RepoRoot(t), ".bin", "kustomize")
	if info, err := os.Stat(bin); err != nil || info.Mode()&0o111 == 0 {
		missingToolchain(t, "pinned kustomize not installed under .bin (b install)")
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
	home := filepath.Join(testutil.RepoRoot(t), ".kustomize")
	for _, p := range needPlugins {
		if info, err := os.Stat(filepath.Join(home, filepath.FromSlash(p))); err != nil || info.Mode()&0o111 == 0 {
			missingToolchain(t, "pinned plugin "+p+" not installed under .kustomize")
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

func TestBuildInProcessPlainKustomization(t *testing.T) {
	t.Setenv(ModeEnv, "")
	dir := plainFixture(t)
	var stderr bytes.Buffer
	out, err := Build(t.Context(), dir, Options{Stderr: &stderr})
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
	got, err := Build(t.Context(), dir, Options{})
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
	out, err := Build(t.Context(), dir, Options{Stderr: &stderr})
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
	got, err := Build(t.Context(), dir, Options{})
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
	t.Setenv("LOK8S_SECRETS_DISABLE", "") // registers the restore
	os.Unsetenv("LOK8S_SECRETS_DISABLE")
	dir := secretFixture(t)

	// The overlay reaches the plugin child: the store-free switch makes
	// the generator emit nothing, and an env override wins over the
	// parent's value.
	out, err := Build(t.Context(), dir, Options{Env: []string{"LOK8S_SECRETS_DISABLE=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("LOK8S_SECRETS_DISABLE=1 did not reach the plugin:\n%s", out)
	}
	if _, set := os.LookupEnv("LOK8S_SECRETS_DISABLE"); set {
		t.Fatal("overlay leaked into the process environment after the run")
	}
	out, err = Build(t.Context(), dir, Options{Env: []string{"RENDER_TEST_VALUE=overlay"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "FROM_ENV: b3ZlcmxheQ==") { // base64("overlay")
		t.Fatalf("overlay value did not win in the plugin:\n%s", out)
	}
	if os.Getenv("RENDER_TEST_VALUE") != "parent" {
		t.Fatalf("overlay not restored: RENDER_TEST_VALUE=%q", os.Getenv("RENDER_TEST_VALUE"))
	}
}

// KUSTOMIZE_PLUGIN_HOME is a per-process constant for the in-process
// renderer: set ONCE to the self-exec home (never per render, never
// restored between renders), and a caller's own value is superseded — the
// symlinks there are the only plugins this build serves. Cleanup puts the
// caller's value back.
func TestBuildPluginHomeIsSetOnceForTheProcess(t *testing.T) {
	t.Setenv(ModeEnv, "")
	t.Setenv("PATH_SECRETS", t.TempDir())
	t.Setenv("RENDER_TEST_VALUE", "x")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "/nowhere/.kustomize")
	dir := secretFixture(t)
	for i := range 2 {
		if _, err := Build(t.Context(), dir, Options{}); err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		home, err := selfExecPluginHome()
		if err != nil {
			t.Fatal(err)
		}
		if got := os.Getenv("KUSTOMIZE_PLUGIN_HOME"); got != home {
			t.Fatalf("after render %d: KUSTOMIZE_PLUGIN_HOME=%q, want the self-exec home %q", i, got, home)
		}
	}
}

// Two sequential renders with different per-render variables: the second
// render's plugin child must not see the first render's variable, and the
// process environment carries neither afterwards. `optional` + `update`
// entries make the generator omit an unset variable instead of erroring or
// serving a cached value.
func TestBuildSequentialRendersDoNotLeakVarsIntoThePluginChild(t *testing.T) {
	t.Setenv(ModeEnv, "")
	for _, k := range []string{"LOK8S_USER_A", "LOK8S_USER_B"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"kustomization.yaml": "generators:\n  - secret.yaml\n",
		"secret.yaml": `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: leak
  namespace: demo
env:
  FROM_A:
    var: LOK8S_USER_A
    optional: true
    update: true
  FROM_B:
    var: LOK8S_USER_B
    optional: true
    update: true
`,
	})
	render := func(overlay ...string) string {
		t.Helper()
		t.Setenv("PATH_SECRETS", t.TempDir())
		var stderr bytes.Buffer
		out, err := Build(t.Context(), dir, Options{Env: overlay, Stderr: &stderr})
		if err != nil {
			t.Fatalf("Build: %v\n%s", err, stderr.String())
		}
		return string(out)
	}
	first := render("LOK8S_USER_A=alpha")
	if !strings.Contains(first, "FROM_A: YWxwaGE=") || strings.Contains(first, "FROM_B") { // base64("alpha")
		t.Fatalf("first render:\n%s", first)
	}
	for _, k := range []string{"LOK8S_USER_A", "LOK8S_USER_B"} {
		if _, set := os.LookupEnv(k); set {
			t.Fatalf("%s leaked into the process environment after the first render", k)
		}
	}
	second := render("LOK8S_USER_B=beta")
	if strings.Contains(second, "FROM_A") {
		t.Fatalf("the first render's variable reached the second render's plugin child:\n%s", second)
	}
	if !strings.Contains(second, "FROM_B: YmV0YQ==") { // base64("beta")
		t.Fatalf("second render:\n%s", second)
	}
	for _, k := range []string{"LOK8S_USER_A", "LOK8S_USER_B"} {
		if _, set := os.LookupEnv(k); set {
			t.Fatalf("%s leaked into the process environment after the second render", k)
		}
	}
}

func TestBuildInProcessFailurePrintsCobraErrorLine(t *testing.T) {
	t.Setenv(ModeEnv, "")
	dir := t.TempDir() // no kustomization.yaml
	var stderr bytes.Buffer
	if _, err := Build(t.Context(), dir, Options{Stderr: &stderr}); err == nil {
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
	out, err := Build(t.Context(), dir, Options{Stderr: &stderr, Env: []string{"KHELM_TRUST_ANY_REPO=true"}})
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
	got, err := Build(t.Context(), dir, Options{EnableExec: true, Env: overlay})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("in-process khelm render differs from the pinned ChartRenderer binary:\n--- binary\n%s\n--- in-process\n%s", want, got)
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
	if _, err := Build(t.Context(), dir, Options{Stderr: &stderr}); err == nil {
		t.Fatal("RootOnly (the default) accepted a file outside the root")
	}
	out, err := Build(t.Context(), dir, Options{LoadRestrictions: LoadRestrictionsNone})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "name: outside") {
		t.Fatalf("LoadRestrictionsNone render:\n%s", out)
	}
}

// Concurrent renders of different directories each reach their own
// plugin child with their own overlay, and nothing of any overlay lands
// in the process environment meanwhile.
func TestBuildConcurrentRendersKeepTheirOwnOverlay(t *testing.T) {
	t.Setenv(ModeEnv, "")
	t.Setenv("PATH_SECRETS", t.TempDir())
	t.Setenv("LOK8S_USER_TAG", "")
	os.Unsetenv("LOK8S_USER_TAG")
	const n = 6
	dirs := make([]string, n)
	for i := range dirs {
		dirs[i] = t.TempDir()
		writeFiles(t, dirs[i], map[string]string{
			"kustomization.yaml": "generators:\n  - secret.yaml\n",
			"secret.yaml": `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: tag
  namespace: demo
env:
  TAG:
    var: LOK8S_USER_TAG
    optional: true
    update: true
`,
		})
	}
	type result struct {
		out string
		err error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	leaked := make(chan string, n*8)
	for i := range dirs {
		wg.Go(func() {
			var stderr bytes.Buffer
			out, err := Build(t.Context(), dirs[i], Options{Env: []string{fmt.Sprintf("LOK8S_USER_TAG=tag-%d", i)}, Stderr: &stderr})
			if err != nil {
				err = fmt.Errorf("%w\n%s", err, stderr.String())
			}
			results[i] = result{string(out), err}
			if v, set := os.LookupEnv("LOK8S_USER_TAG"); set {
				leaked <- v
			}
		})
	}
	wg.Wait()
	close(leaked)
	for v := range leaked {
		t.Errorf("an overlay reached the process environment during a concurrent render: LOK8S_USER_TAG=%q", v)
	}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("render %d: %v", i, r.err)
		}
		want := "TAG: " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("tag-%d", i)))
		if !strings.Contains(r.out, want) {
			t.Errorf("render %d did not get its own overlay:\n%s", i, r.out)
		}
	}
	if _, set := os.LookupEnv("LOK8S_USER_TAG"); set {
		t.Error("overlay left in the process environment")
	}
}

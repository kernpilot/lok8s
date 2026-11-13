package build

// keysonly_test.go — `lo build --keys-only` (KeysOnly), the output guard
// of Artifacts and HasKeysOnlyMarker. The render is the stub kustomize of
// artifacts_test.go (exec pipeline); it logs the Secret plugin's keys-only
// and store-free settings it was given. KeysOnly runs the pinned yq
// (.bin/yq of the repository), linked into the project's .bin.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// A render that holds one Secret of the plugin's keys-only run.
const keysOnlyRender = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\ndata:\n  USER: \"\"\nkind: Secret\nmetadata:\n  annotations:\n    " +
	secret.KeysOnlyAnnotation + ": \"true\"\n  name: app\n  namespace: demo\ntype: Opaque\n"

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmp.") || strings.HasPrefix(e.Name(), ".artifacts-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// pinnedYq is the repository's pinned yq. A missing one skips the test,
// except under CI.
func pinnedYq(t *testing.T) string {
	t.Helper()
	yq := filepath.Join(testutil.RepoRoot(t), ".bin", "yq")
	if _, err := os.Stat(yq); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("the pinned yq is missing under CI: %v", err)
		}
		t.Skip("no .bin/yq (b install)")
	}
	return yq
}

// keysOnlyProject is artifactsProject with the pinned yq in the project's
// .bin (KeysOnly looks there first) and a private TMPDIR (the probe's
// temporary directory goes there).
func keysOnlyProject(t *testing.T) (*config.Paths, string, string) {
	t.Helper()
	p, domainDir := keysOnlyExecProject(t)
	if err := os.Symlink(pinnedYq(t), filepath.Join(p.Bin, "yq")); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(p.Base, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	return p, domainDir, tmp
}

// keysOnlyExecProject is artifactsProject with the plugin home of the
// exec pipeline, the project's .kustomize. An in-process render in the
// same test binary sets KUSTOMIZE_PLUGIN_HOME for the whole process (its
// self-exec home); a real exec-pipeline run of lo never makes one.
func keysOnlyExecProject(t *testing.T) (*config.Paths, string) {
	t.Helper()
	p, domainDir := artifactsProject(t)
	t.Setenv(config.KustomizePluginHomeEnv, filepath.Join(p.Base, ".kustomize"))
	return p, domainDir
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// --keys-only renders with the keys-only run and the store-free switch on,
// writes the render to stdout and nothing to the domain dir: a prior
// artifacts.yaml, the split dir and a pre-domain per-target dir stay as
// they were.
func TestKeysOnlyRendersToStdoutOnly(t *testing.T) {
	p, domainDir, tmp := keysOnlyProject(t)
	stubRender(t, p, keysOnlyRender)
	log := filepath.Join(p.Base, "kustomize-keys.log")
	t.Setenv("KUSTOMIZE_STUB_KEYSLOG", log)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), "kind: Prior\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts", "ConfigMap.demo.a.yaml"), "kind: ConfigMap\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts", "old", "artifacts.yaml"), "kind: Old\n")

	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err != nil {
		t.Fatalf("KeysOnly: %v\n%s", err, stderr.String())
	}
	if stdout.String() != keysOnlyRender {
		t.Errorf("stdout = %q, want the render", stdout.String())
	}
	if !strings.Contains(stderr.String(), "lo build: d.dev rendered 2 document(s) -> stdout (keys only)\n") {
		t.Errorf("stderr = %q", stderr.String())
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "keys-only=1 disable=1" {
		t.Errorf("the kustomize child saw %q, want keys-only=1 disable=1", got)
	}
	for rel, want := range map[string]string{
		"artifacts.yaml":                  "kind: Prior\n",
		"artifacts/ConfigMap.demo.a.yaml": "kind: ConfigMap\n",
		"artifacts/old/artifacts.yaml":    "kind: Old\n",
	} {
		got, err := os.ReadFile(filepath.Join(domainDir, rel))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want it untouched", rel, got, err)
		}
	}
	if left := leftovers(t, domainDir); len(left) != 0 {
		t.Errorf("temp files left in the domain dir: %v", left)
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Errorf("temp files left in TMPDIR: %v", left)
	}
}

// The final stream after the transformations and envsubst: a patch put a
// real value and a stringData key back on the plugin's Secret, a plain
// Secret manifest, a Secret in a List, a SecretList item without a kind,
// an alias into data, and objects that are not Secrets.
const transformedRender = `apiVersion: v1
kind: Secret
metadata:
  annotations:
    secrets.lok8s.dev/keys-only: "true"
  name: patched
data:
  USER: YWxpY2U=
stringData:
  PASSWORD: patched-plain
type: Opaque
---
apiVersion: v1
kind: Secret
metadata:
  name: plain
data:
  PLAIN: cGxhaW4tdmFsdWU=
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: listed
  stringData:
    KEY: listed-plain
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: listed-cm
  data:
    VISIBLE: list-config
---
apiVersion: v1
kind: SecretList
items:
- metadata:
    name: kindless
  data:
    NESTED: a2luZGxlc3M=
---
apiVersion: v1
kind: Secret
metadata:
  name: aliased
  labels: &values
    ALIASED: alias-plain
data: *values
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
data:
  VISIBLE: plain-config
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: gw
spec:
  listeners:
  - name: https
    tls:
      certificateRefs:
      - kind: Secret
        name: tls
`

// The contract: lo build --keys-only prints no Secret value, for every
// Secret in the final stream. The key names stay; the objects that are
// not Secrets keep their values; a reference to a Secret stays as it is.
func TestKeysOnlyBlanksEverySecret(t *testing.T) {
	p, _, _ := keysOnlyProject(t)
	stubRender(t, p, transformedRender)
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err != nil {
		t.Fatalf("KeysOnly: %v\n%s", err, stderr.String())
	}
	out := stdout.String()
	for _, value := range []string{"YWxpY2U=", "patched-plain", "cGxhaW4tdmFsdWU=", "listed-plain", "a2luZGxlc3M="} {
		if strings.Contains(out, value) {
			t.Errorf("stdout holds the Secret value %q:\n%s", value, out)
		}
	}
	for _, key := range []string{"USER: \"\"", "PASSWORD: \"\"", "PLAIN: \"\"", "KEY: \"\"", "NESTED: \"\"", "ALIASED: \"\""} {
		if !strings.Contains(out, key) {
			t.Errorf("stdout lacks the key %q:\n%s", key, out)
		}
	}
	for _, kept := range []string{"VISIBLE: list-config", "VISIBLE: plain-config", "ALIASED: alias-plain", "- kind: Secret\n", "name: tls\n"} {
		if !strings.Contains(out, kept) {
			t.Errorf("stdout lost %q:\n%s", kept, out)
		}
	}
	if strings.Contains(out, "stringData: null") || strings.Contains(out, "data: null") {
		t.Errorf("stdout gained a data or stringData key:\n%s", out)
	}
	if n := len(regexp.MustCompile(`(?m)^\s*(?:- )?(?:data|stringData):`).FindAllString(out, -1)); n != 8 {
		t.Errorf("stdout has %d data/stringData keys, want the 8 of the render:\n%s", n, out)
	}
}

// The Go program is the bash program: the two implementations print the
// same stream.
func TestKeysOnlyBlankExprMatchesBash(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "libs", "build"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "  local expr='"+KeysOnlyBlankExpr+"'\n") {
		t.Error("libs/build (build::keys_only) does not run KeysOnlyBlankExpr")
	}
}

// lastAppliedRender has Secrets with the annotation that kubectl apply
// writes: a Secret document, a Secret without a namespace (flow style, a
// quoted key), a List item, a SecretList item without a kind and a Secret
// that gets the annotation through a merge key. A ConfigMap with the
// annotation and a Secret with another annotation are not refused.
const lastAppliedRender = `apiVersion: v1
kind: Secret
metadata:
  name: app
  namespace: one
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","kind":"Secret","stringData":{"PLAIN":"applied-canary"}}
stringData:
  PLAIN: data-canary
---
apiVersion: v1
kind: Secret
metadata:
  name: nons
  annotations: {"kubectl.kubernetes.io/last-applied-configuration": "{}"}
data:
  PLAIN: ZGF0YS1jYW5hcnk=
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: listed
    namespace: two
    annotations:
      "kubectl.kubernetes.io/last-applied-configuration": "{}"
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cm
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: "{}"
---
apiVersion: v1
kind: SecretList
items:
- metadata:
    name: kindless
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: "{}"
---
apiVersion: v1
kind: Secret
metadata:
  name: merged
  labels: &ann
    kubectl.kubernetes.io/last-applied-configuration: "{}"
  annotations:
    <<: *ann
---
apiVersion: v1
kind: Secret
metadata:
  name: plain
  annotations:
    other: "{}"
`

// The keys-only run does not empty annotations, so a Secret with the
// last-applied annotation is refused: the output must not look complete
// when it is not. The message names each Secret and no value; nothing
// goes to stdout.
func TestKeysOnlyRefusesTheLastAppliedAnnotation(t *testing.T) {
	p, _, _ := keysOnlyProject(t)
	stubRender(t, p, lastAppliedRender)
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); !errors.Is(err, ErrHandled) {
		t.Fatalf("KeysOnly = %v, want ErrHandled\n%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	var refused []string
	for line := range strings.SplitSeq(strings.TrimRight(stderr.String(), "\n"), "\n") {
		refused = append(refused, line)
	}
	var want []string
	for _, name := range []string{"one/app", "nons", "two/listed", "kindless", "merged"} {
		want = append(want, "[error] lo build --keys-only: refusing d.dev's render: Secret "+name+
			" has the annotation kubectl.kubernetes.io/last-applied-configuration, which can hold the Secret's values")
	}
	if !slices.Equal(refused, want) {
		t.Errorf("stderr =\n%s\nwant =\n%s", strings.Join(refused, "\n"), strings.Join(want, "\n"))
	}
	for _, value := range []string{"applied-canary", "data-canary", "ZGF0YS1jYW5hcnk="} {
		if strings.Contains(stderr.String(), value) {
			t.Errorf("stderr holds the value %q", value)
		}
	}
}

// A failed check refuses too: it does not let the render through.
func TestKeysOnlyLastAppliedCheckFails(t *testing.T) {
	p, _, _ := keysOnlyProject(t)
	stubRender(t, p, twoDocRender)
	yq := filepath.Join(p.Bin, "yq")
	if err := os.Remove(yq); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, yq, "#!/bin/sh\n[ \"$1\" = -N ] && exit 1\nexec '"+pinnedYq(t)+"' \"$@\"\n")
	if err := os.Chmod(yq, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); !errors.Is(err, ErrHandled) {
		t.Fatalf("KeysOnly = %v, want ErrHandled\n%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[error] lo build --keys-only: yq could not check d.dev's render for the annotation kubectl.kubernetes.io/last-applied-configuration") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// The Go program is the bash program: the two implementations refuse the
// same Secrets.
func TestKeysOnlyLastAppliedExprMatchesBash(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "libs", "build"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "  local last_applied_expr='"+KeysOnlyLastAppliedExpr+"'\n") {
		t.Error("libs/build (build::keys_only) does not run KeysOnlyLastAppliedExpr")
	}
}

// --keys-only reads no store, so a domain without its own store in a
// project that keeps one per domain renders (like --no-secrets).
func TestKeysOnlyNeedsNoStore(t *testing.T) {
	p, _, _ := keysOnlyProject(t)
	stubRender(t, p, twoDocRender)
	writeFileT(t, filepath.Join(p.Clusters, "other.dev", "secrets", ".gitignore"), "*\n")
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err != nil {
		t.Fatalf("KeysOnly: %v\n%s", err, stderr.String())
	}
	if stdout.String() != twoDocRender {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// A failed render prints nothing on stdout.
func TestKeysOnlyFailedRenderPrintsNothing(t *testing.T) {
	p, _, tmp := keysOnlyProject(t)
	stubRender(t, p, twoDocRender)
	t.Setenv("KUSTOMIZE_STUB_RC", "1")
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err == nil {
		t.Fatal("a failed render succeeded")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[error] kustomize build failed for d.dev") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Errorf("temp files left in TMPDIR: %v", left)
	}
}

// A render that yq cannot read prints nothing on stdout.
func TestKeysOnlyUnparsedRenderPrintsNothing(t *testing.T) {
	p, _, _ := keysOnlyProject(t)
	stubRender(t, p, "a: [b\n")
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err == nil {
		t.Fatal("a render that does not parse succeeded")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[error] lo build --keys-only: yq could not empty the Secret values of d.dev's render") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// Without yq there is nothing to empty the values with: refused before the
// render.
func TestKeysOnlyNeedsYq(t *testing.T) {
	p, _ := keysOnlyExecProject(t)
	stubRender(t, p, twoDocRender)
	log := filepath.Join(p.Base, "kustomize-keys.log")
	t.Setenv("KUSTOMIZE_STUB_KEYSLOG", log)
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err == nil {
		t.Fatal("KeysOnly without yq succeeded")
	}
	if !strings.Contains(stderr.String(), "[error] lo build --keys-only: yq not found — install the pinned toolchain (b install)") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("the render ran")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// A failed write to stdout has its own message.
func TestKeysOnlyStdoutWriteFails(t *testing.T) {
	p, _, _ := keysOnlyProject(t)
	stubRender(t, p, twoDocRender)
	var stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, failingWriter{}); err == nil {
		t.Fatal("a failed write succeeded")
	}
	if !strings.Contains(stderr.String(), "[error] lo build --keys-only: writing d.dev's render to stdout failed") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "kustomize build failed") {
		t.Errorf("stderr = %q, want no render failure", stderr.String())
	}
}

// stubSecretPlugin puts a Secret plugin script into the project's plugin
// home. It logs how it was run, then runs body.
func stubSecretPlugin(t *testing.T, p *config.Paths, body string) string {
	t.Helper()
	log := filepath.Join(p.Base, "plugin-probe.log")
	script := "#!/bin/sh\n" +
		"{ printf 'args=%s\\n' \"$*\"; printf 'cwd=%s\\n' \"$PWD\"; printf 'cwd-entries=%s\\n' \"$(ls -A)\";" +
		" printf 'secrets=%s\\n' \"${PATH_SECRETS-<unset>}\"; printf 'disable=%s\\n' \"${LOK8S_SECRETS_DISABLE-<unset>}\";" +
		" printf 'keys-only=%s\\n' \"${" + secret.KeysOnlyEnv + "-<unset>}\"; printf 'stdin=%s\\n' \"$(cat)\"; } >> \"" + log + "\"\n" +
		body + "\n"
	path := filepath.Join(p.Base, ".kustomize", "secrets.lok8s.dev", "v1", "secret", "Secret")
	writeFileT(t, path, script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

// In the exec pipeline, a Secret plugin without the keys-only run is
// refused before the render: one that knows --capabilities but not
// keys-only, one from v0.2.0 on that stops on the store-free switch and
// prints nothing, one before v0.2.0 that reads the flag as a missing spec
// file and fails. A plugin with the capability renders. The probe runs in
// an empty temporary directory that it removes, with an empty stdin, the
// store-free switch on, the keys-only setting off and PATH_SECRETS at a
// path in that directory that does not exist.
func TestKeysOnlyProbesThePlugin(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"dry-run only", "echo dry-run", false},
		{"no flag, store-free switch", "exit 0", false},
		{"no flag, a missing spec file", "echo 'secret plugin: open spec \"--capabilities\": no such file' >&2; exit 1", false},
		{"keys-only", "printf 'dry-run\\nkeys-only\\n'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, domainDir, tmp := keysOnlyProject(t)
			stubRender(t, p, twoDocRender)
			klog := filepath.Join(p.Base, "kustomize-keys.log")
			t.Setenv("KUSTOMIZE_STUB_KEYSLOG", klog)
			t.Setenv("PATH_SECRETS", filepath.Join(domainDir, "secrets"))
			log := stubSecretPlugin(t, p, tc.body)
			var stdout, stderr bytes.Buffer
			err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout)
			if tc.ok {
				if err != nil {
					t.Fatalf("KeysOnly: %v\n%s", err, stderr.String())
				}
			} else {
				if err == nil {
					t.Fatal("a plugin without the keys-only run was used")
				}
				want := "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the keys-only run (an older plugin would read the secrets store) — run 'lo toolchain install' to install the plugin of this lo\n"
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q\nwant %q", stderr.String(), want)
				}
				if stdout.Len() != 0 {
					t.Errorf("stdout = %q, want nothing", stdout.String())
				}
				if _, err := os.Stat(klog); err == nil {
					t.Error("the render ran")
				}
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("the probe did not run: %v", err)
			}
			got := map[string]string{}
			for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
				k, v, _ := strings.Cut(line, "=")
				got[k] = v
			}
			cwd := got["cwd"]
			if filepath.Dir(cwd) != tmp {
				t.Errorf("the probe ran in %q, want a new directory in TMPDIR %s", cwd, tmp)
			}
			for k, want := range map[string]string{
				"args":        "--capabilities",
				"cwd-entries": "",
				"secrets":     filepath.Join(cwd, "store"),
				"disable":     "1",
				"keys-only":   "0",
				"stdin":       "",
			} {
				if got[k] != want {
					t.Errorf("probe %s = %q, want %q", k, got[k], want)
				}
			}
			if left := dirEntries(t, tmp); len(left) != 0 {
				t.Errorf("the probe left %v in TMPDIR", left)
			}
		})
	}
}

// The output guard: a render that holds an object with the keys-only
// marker is never written into the domain's artifacts. The prior
// artifacts.yaml stays, no split is made, no temp file is left.
func TestArtifactsRefusesTheKeysOnlyMarker(t *testing.T) {
	for _, mode := range []string{"single", "split"} {
		t.Run(mode, func(t *testing.T) {
			p, domainDir := artifactsProject(t)
			stubRender(t, p, keysOnlyRender)
			writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), "kind: Prior\n")
			split := "0"
			if mode == "split" {
				split = "1"
			}
			var stderr bytes.Buffer
			err := Artifacts(t.Context(), Options{Paths: p, Domain: "d.dev", SplitOverride: split, NoSecrets: true, Stderr: &stderr})
			if err == nil {
				t.Fatal("a render with the keys-only marker was written")
			}
			want := "[error] refusing to write d.dev's render to " + filepath.Join(domainDir, "artifacts.yaml") +
				": it holds a keys-only Secret (secrets.lok8s.dev/keys-only); keys-only output goes to stdout only (lo build --keys-only)\n"
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q\nwant it to hold %q", stderr.String(), want)
			}
			if got, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml")); string(got) != "kind: Prior\n" {
				t.Errorf("artifacts.yaml = %q, want the prior content", got)
			}
			if _, err := os.Stat(filepath.Join(domainDir, "artifacts")); err == nil {
				t.Error("a split dir was written")
			}
			if left := leftovers(t, domainDir); len(left) != 0 {
				t.Errorf("temp files left in the domain dir: %v", left)
			}
		})
	}
}

// envsubst can make the marker after the render: an annotation key
// ${LOK8S_USER_MARKER} whose value is the marker in quotes. The guard
// reads the stream after envsubst and parses it, so the quoted key is the
// marker. A render that does not parse is refused too.
func TestArtifactsGuardParsesTheFinalStream(t *testing.T) {
	for _, tc := range []struct {
		name, render, want string
	}{
		{"quoted marker from envsubst", "apiVersion: v1\nkind: Secret\nmetadata:\n  annotations:\n    ${LOK8S_USER_MARKER}: \"true\"\n  name: app\ndata:\n  USER: \"\"\n",
			"it holds a keys-only Secret (secrets.lok8s.dev/keys-only); keys-only output goes to stdout only (lo build --keys-only)\n"},
		{"marker on a List item", "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Secret\n  metadata: {name: app, annotations: {secrets.lok8s.dev/keys-only: \"true\"}}\n",
			"it holds a keys-only Secret (secrets.lok8s.dev/keys-only); keys-only output goes to stdout only (lo build --keys-only)\n"},
		{"no YAML", "kind: ConfigMap\nmetadata: [a\n",
			"it does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, domainDir := artifactsProject(t)
			t.Setenv("LOK8S_USER_MARKER", `"`+secret.KeysOnlyAnnotation+`"`)
			stubRender(t, p, tc.render)
			writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), "kind: Prior\n")
			stderr, err := runArtifacts(t, p)
			if err == nil {
				t.Fatal("the render was written")
			}
			want := "[error] refusing to write d.dev's render to " + filepath.Join(domainDir, "artifacts.yaml") + ": " + tc.want
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q\nwant it to hold %q", stderr, want)
			}
			if got, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml")); string(got) != "kind: Prior\n" {
				t.Errorf("artifacts.yaml = %q, want the prior content", got)
			}
		})
	}
}

// The stale per-target dirs are pruned only by a render that passed every
// check: a refused render (the marker, an empty render over a prior
// artifact) and a failed render keep them.
func TestArtifactsPrunesOnlyAfterTheChecks(t *testing.T) {
	for _, tc := range []struct {
		name, render, rc string
		pruned           bool
	}{
		{"keys-only marker", keysOnlyRender, "0", false},
		{"empty render", "", "0", false},
		{"failed render", twoDocRender, "1", false},
		{"good render", twoDocRender, "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, domainDir := artifactsProject(t)
			stubRender(t, p, tc.render)
			t.Setenv("KUSTOMIZE_STUB_RC", tc.rc)
			writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), "kind: Prior\n")
			stale := filepath.Join(domainDir, "artifacts", "old", "artifacts.yaml")
			writeFileT(t, stale, "kind: Old\n")
			stderr, err := runArtifacts(t, p)
			if tc.pruned != (err == nil) {
				t.Fatalf("Artifacts: %v\n%s", err, stderr)
			}
			_, statErr := os.Stat(stale)
			if tc.pruned && statErr == nil {
				t.Error("a good render kept the stale per-target dir")
			}
			if !tc.pruned && statErr != nil {
				t.Errorf("a refused or failed render removed the stale per-target dir: %v", statErr)
			}
		})
	}
}

// markerCases are streams with and without the keys-only marker. want is
// whether the guard refuses the stream. unparsed marks the one stream that
// Go refuses as "does not parse" while the pinned yq parses it and finds
// the marker: the YAML 1.2 escape \/ (the Go YAML libraries do not know
// it). Both implementations refuse it; only the message differs.
var markerCases = []struct {
	name, in string
	want     bool
	unparsed bool
}{
	{"annotation", "metadata:\n  annotations:\n    secrets.lok8s.dev/keys-only: \"true\"\n", true, false},
	{"quoted key", "metadata:\n  annotations:\n    \"secrets.lok8s.dev/keys-only\": \"true\"\n", true, false},
	{"single-quoted key", "metadata:\n  annotations:\n    'secrets.lok8s.dev/keys-only': x\n", true, false},
	{"escaped key", "metadata: {annotations: {\"secrets.lok8s.dev\\/keys-only\": \"true\"}}\n", true, true},
	{"unicode escape", "metadata: {annotations: {\"secrets.lok8s.dev\\u002Fkeys-only\": \"true\"}}\n", true, false},
	{"flow style", "metadata: {annotations: {secrets.lok8s.dev/keys-only: \"true\"}}\n", true, false},
	{"null value", "metadata:\n  annotations:\n    secrets.lok8s.dev/keys-only:\n", true, false},
	{"List item", "kind: List\nitems:\n- kind: Secret\n  metadata:\n    annotations:\n      secrets.lok8s.dev/keys-only: \"true\"\n", true, false},
	{"second document", "kind: A\n---\nmetadata:\n  annotations:\n    secrets.lok8s.dev/keys-only: \"true\"\n", true, false},
	{"alias", "x: &a {secrets.lok8s.dev/keys-only: \"true\"}\nmetadata:\n  annotations: *a\n", true, false},
	{"merge key", "x: &a {secrets.lok8s.dev/keys-only: \"true\"}\nmetadata:\n  annotations:\n    <<: *a\n", true, false},
	{"merge list", "x: &a {b: c}\ny: &m {secrets.lok8s.dev/keys-only: \"true\"}\nmetadata:\n  annotations:\n    <<: [*a, *m]\n", true, false},
	{"merged metadata", "x: &md {annotations: {secrets.lok8s.dev/keys-only: \"true\"}}\nkind: Secret\n<<: {metadata: *md}\n", true, false},
	{"text in a value", "data:\n  note: \"secrets.lok8s.dev/keys-only: x\"\n", false, false},
	{"top-level key", "secrets.lok8s.dev/keys-only: x\n", false, false},
	{"label, not annotation", "metadata:\n  labels:\n    secrets.lok8s.dev/keys-only: \"true\"\n", false, false},
	{"other key", "metadata:\n  annotations:\n    secrets.lok8s.dev/keys-only-not: x\n", false, false},
	{"block scalar", "data:\n  doc: |\n    metadata:\n      annotations:\n        secrets.lok8s.dev/keys-only: \"true\"\n", false, false},
	{"annotations not a map", "metadata:\n  annotations: [secrets.lok8s.dev/keys-only]\n", false, false},
	{"empty", "", false, false},
	{"comments only", "# a\n---\n# b\n", false, false},
	{"two ConfigMaps", twoDocRender, false, false},
}

// unparsedCases are streams that do not parse as YAML.
var unparsedCases = []string{
	"a: [b\n",
	"kind: A\n---\nb: {c\n",
	"metadata:\n  annotations:\n    secrets.lok8s.dev/keys-only: \"true\"\n---\nkind: [x\n",
}

func TestHasKeysOnlyMarker(t *testing.T) {
	for _, tc := range markerCases {
		got, err := HasKeysOnlyMarker([]byte(tc.in))
		if tc.unparsed {
			if !errors.Is(err, errNoYAML) {
				t.Errorf("%s: HasKeysOnlyMarker error = %v, want errNoYAML", tc.name, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: HasKeysOnlyMarker = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	for _, in := range unparsedCases {
		if _, err := HasKeysOnlyMarker([]byte(in)); !errors.Is(err, errNoYAML) {
			t.Errorf("HasKeysOnlyMarker(%q) error = %v, want errNoYAML", in, err)
		}
	}
}

// The Go guard and the bash guard (keysonly::has_marker: the yq program
// in utils/keysonly.sh, run by the pinned yq) refuse the same streams: the
// marker on every case, and the streams that do not parse.
func TestHasKeysOnlyMarkerMatchesYq(t *testing.T) {
	yq := pinnedYq(t)
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "utils", "keysonly.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`yq --yaml-fix-merge-anchor-to-spec '([^']+)'`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("no yq program in utils/keysonly.sh")
	}
	expr := string(m[1])
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	for i, tc := range markerCases {
		writeFileT(t, path, tc.in)
		out, err := exec.CommandContext(t.Context(), yq, "--yaml-fix-merge-anchor-to-spec", expr, path).Output()
		if err != nil {
			t.Fatalf("case %d (%s): yq: %v", i, tc.name, err)
		}
		if bash := strings.Contains("\n"+string(out), "\ntrue\n"); bash != tc.want {
			t.Errorf("%s: the bash guard says %v, the case says %v", tc.name, bash, tc.want)
		}
		marked, goErr := HasKeysOnlyMarker([]byte(tc.in))
		if goRefuses := marked || goErr != nil; goRefuses != tc.want {
			t.Errorf("%s: the Go guard refuses = %v, the bash guard = %v", tc.name, goRefuses, tc.want)
		}
	}
	for _, in := range unparsedCases {
		writeFileT(t, path, in)
		if err := exec.CommandContext(t.Context(), yq, "--yaml-fix-merge-anchor-to-spec", expr, path).Run(); err == nil {
			t.Errorf("yq accepts %q, which the Go guard rejects", in)
		}
	}
}

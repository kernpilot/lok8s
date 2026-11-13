package main

// main_test.go — the process-start pin of the Secret plugin's keys-only
// setting. The test binary doubles as lo: TestMain hands the process to
// main() when loMainExec is set, so each case runs the real entrypoint
// (the plugin dispatch, the pin, the command tree) in a child process
// against a fixture project whose .bin holds stubs. The stubs write the
// keys-only value they inherited to a log; nothing reaches a real tool,
// a real project or a store.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// loMainExec makes the test binary run main() instead of the tests. A
// self-exec plugin child of an in-process render inherits it, so the
// binary serves the plugin the way lo does.
const loMainExec = "LO_MAIN_TEST_EXEC"

func TestMain(m *testing.M) {
	if os.Getenv(loMainExec) == "1" {
		main()
		return
	}
	if handled, rc := render.DispatchPlugin(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(rc)
	}
	rc := m.Run()
	render.Cleanup()
	os.Exit(rc)
}

// stubTool is the fixture .bin entry for every external tool: it logs
// "<tool> keys-only=<inherited value>" and answers like the tool would
// for `version` and `build` (one ConfigMap).
const stubTool = `#!/bin/sh
tool="$(basename "$0")"
printf '%s keys-only=%s disable=%s\n' "${tool}" "${` + secret.KeysOnlyEnv + `-<unset>}" "${LOK8S_SECRETS_DISABLE-<unset>}" >> "${STUB_LOG}"
case "${tool} ${1:-}" in
  "kustomize build") printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stub\n  namespace: demo\ndata:\n  a: b\n' ;;
  *) echo "${tool} v1.2.3" ;;
esac
`

// mainFixture lays out a Go project with one domain and a .bin of stubs.
func mainFixture(t *testing.T) (proj, log string) {
	t.Helper()
	proj = t.TempDir()
	log = filepath.Join(t.TempDir(), "stub.log")
	files := map[string]string{
		"lok8s.yaml":                          "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: pin\nspec:\n  implementation:\n    default: go\n",
		"clusters/d.dev/cluster.lok8s.yaml":   "kind: Lo\nmetadata:\n  name: d\n",
		"clusters/d.dev/kustomization.yaml":   "resources:\n  - cm.yaml\n",
		"clusters/d.dev/cm.yaml":              "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: demo\ndata:\n  a: b\n",
		"clusters/d.dev/secrets/.gitignore":   "*\n",
		"clusters/gen.dev/cluster.lok8s.yaml": "kind: Lo\nmetadata:\n  name: gen\n",
		"clusters/gen.dev/kustomization.yaml": "resources:\n  - cm.yaml\ngenerators:\n  - secret.yaml\n",
		"clusters/gen.dev/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: demo\ndata:\n  a: b\n",
		"clusters/gen.dev/secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: demo\nliterals:\n  USER: alice\n",
		"clusters/gen.dev/secrets/.gitignore": "*\n",
	}
	for name, content := range files {
		path := filepath.Join(proj, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(proj, ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"argsh", "yq", "kustomize", "kubectl", "kind", "tilt", "docker"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(stubTool), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return proj, log
}

// runLo runs the test binary as lo in proj with a minimal environment
// that inherits the keys-only setting as 1 (and the store-free switch as
// 1). mode is the LO_RENDER value ("" = the build's default).
func runLo(t *testing.T, proj, log, mode string, args ...string) (string, error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), self, args...)
	cmd.Dir = proj
	home := t.TempDir()
	cmd.Env = []string{
		loMainExec + "=1",
		"PATH=" + filepath.Join(proj, ".bin") + ":/usr/bin:/bin",
		"HOME=" + home,
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"TMPDIR=" + t.TempDir(),
		"KUBECONFIG=" + filepath.Join(home, "no-such-kubeconfig"),
		"STUB_LOG=" + log,
		render.ModeEnv + "=" + mode,
		secret.KeysOnlyEnv + "=1",
		secret.DisableEnv + "=1",
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
}

func readLog(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no stub ran: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// `lo version` starts every tool of the toolchain with no render overlay
// of its own: what the stubs see is what the process start left in the
// environment. An inherited 1 is 0 there.
func TestKeysOnlyPinnedAtProcessStart(t *testing.T) {
	proj, log := mainFixture(t)
	if out, err := runLo(t, proj, log, "", "version"); err != nil {
		t.Fatalf("lo version: %v\n%s", err, out)
	}
	lines := readLog(t, log)
	if len(lines) != 7 {
		t.Fatalf("want the 7 toolchain stubs started, got %d: %q", len(lines), lines)
	}
	for _, ln := range lines {
		if !strings.Contains(ln, " keys-only=0 ") {
			t.Errorf("a child inherited the caller's keys-only value: %q", ln)
		}
	}
}

// `lo build` and `lo build --no-secrets` with the setting inherited as 1:
// the kustomize child (exec pipeline) sees 0, and so does the Secret
// plugin under it.
func TestKeysOnlyPinnedForBuildExec(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"build", "--domain", "d.dev"}, "kustomize keys-only=0 disable=0"},
		{[]string{"build", "--domain", "d.dev", "--no-secrets"}, "kustomize keys-only=0 disable=1"},
	} {
		proj, log := mainFixture(t)
		if out, err := runLo(t, proj, log, string(render.ModeExec), tc.args...); err != nil {
			t.Fatalf("lo %v: %v\n%s", tc.args, err, out)
		}
		var builds []string
		for _, ln := range readLog(t, log) {
			if strings.HasPrefix(ln, "kustomize ") {
				builds = append(builds, ln)
			}
		}
		if len(builds) != 1 || builds[0] != tc.want {
			t.Errorf("lo %v: kustomize saw %q, want [%q]", tc.args, builds, tc.want)
		}
	}
}

// lo-full renders in-process and serves the Secret generator by
// re-executing itself. With the setting inherited as 1, the plugin child
// still runs a normal generator: `lo build` writes the real value with no
// keys-only marker, `lo build --no-secrets` writes no Secret at all.
func TestKeysOnlyPinnedForBuildInProcess(t *testing.T) {
	if !render.InProcessAvailable() {
		t.Skip("lo core: the exec pipeline is the only one (TestKeysOnlyPinnedForBuildExec)")
	}
	for _, tc := range []struct {
		args      []string
		secret    bool
		mustNotBe []string
	}{
		{[]string{"build", "--domain", "gen.dev"}, true, []string{secret.KeysOnlyAnnotation}},
		{[]string{"build", "--domain", "gen.dev", "--no-secrets"}, false, []string{secret.KeysOnlyAnnotation, "kind: Secret"}},
	} {
		proj, log := mainFixture(t)
		if out, err := runLo(t, proj, log, "", tc.args...); err != nil {
			t.Fatalf("lo %v: %v\n%s", tc.args, err, out)
		}
		raw, err := os.ReadFile(filepath.Join(proj, "clusters", "gen.dev", "artifacts.yaml"))
		if err != nil {
			t.Fatalf("lo %v: %v", tc.args, err)
		}
		got := string(raw)
		if tc.secret && !strings.Contains(got, "USER: YWxpY2U=") { // base64("alice")
			t.Errorf("lo %v: the generator did not write its real value:\n%s", tc.args, got)
		}
		for _, s := range tc.mustNotBe {
			if strings.Contains(got, s) {
				t.Errorf("lo %v: artifacts.yaml carries %q:\n%s", tc.args, s, got)
			}
		}
	}
}

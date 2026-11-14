package tilt

// tiltfile_pin_test.go — the Tilt extension pins the Secret plugin's
// keys-only setting to 0 for every process Tilt starts. Tilt is never
// run here (see tilt_test.go), so this is a text check of the extension
// as shipped (the embedded mirror; the assets drift test holds .lok8s
// identical to it): the putenv is top-level code, before the first
// definition and before the first child process.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

func TestTiltfilePinsKeysOnlyOff(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "tilt", "Tiltfile"))
	if err != nil {
		t.Fatal(err)
	}
	pin := "os.putenv('" + secret.KeysOnlyEnv + "', '0')"
	lines := strings.Split(string(raw), "\n")
	pinAt, firstDef, firstChild := -1, -1, -1
	for i, ln := range lines {
		switch {
		case ln == pin && pinAt < 0:
			pinAt = i
		case strings.HasPrefix(ln, "def ") && firstDef < 0:
			firstDef = i
		case strings.Contains(ln, "local(") && !strings.HasPrefix(strings.TrimSpace(ln), "#") && firstChild < 0:
			firstChild = i
		}
	}
	if pinAt < 0 {
		t.Fatalf("the Tiltfile has no top-level %q", pin)
	}
	if firstDef >= 0 && pinAt > firstDef {
		t.Errorf("the pin (line %d) comes after the first definition (line %d)", pinAt+1, firstDef+1)
	}
	if firstChild >= 0 && pinAt > firstChild {
		t.Errorf("the pin (line %d) comes after the first child process (line %d)", pinAt+1, firstChild+1)
	}
	for i, ln := range lines {
		if i != pinAt && strings.Contains(ln, secret.KeysOnlyEnv) && !strings.HasPrefix(strings.TrimSpace(ln), "#") {
			t.Errorf("line %d sets or reads the setting again: %q", i+1, ln)
		}
	}
}

// The extension runs `lo tilt preflight` (its apply guard refuses a
// manifest with a keys-only Secret) on every load: not under an if, right
// after the render of the overlay (the domain artifact and the service
// overlays on it), with that render on stdin, and before the first
// k8s_yaml. The preflight setting turns off only the sweep
// (LOK8S_PREFLIGHT=0). Tilt is never run here: a text check of the
// extension as shipped; the guard itself is TestPreflightRefusesAKeysOnlyManifest.
func TestTiltfileGuardsEveryApply(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "tilt", "Tiltfile"))
	if err != nil {
		t.Fatal(err)
	}
	const render = "  artifacts = local('kustomize build --load-restrictor=LoadRestrictionsNone %s' % _artifacts_dir, True, '', True, {})"
	const guard = "  local('%s tilt preflight' % lo_bin, stdin = artifacts, env = {} if preflight else {'LOK8S_PREFLIGHT': '0'})"
	renderAt, guardAt, firstApply := -1, -1, -1
	var code []int // lines that are not comments or blank, after the render
	for i, ln := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		switch {
		case ln == render:
			renderAt = i
		case ln == guard:
			guardAt = i
		case strings.HasPrefix(trimmed, "k8s_yaml(") && firstApply < 0:
			firstApply = i
		}
		if renderAt >= 0 && i > renderAt {
			code = append(code, i)
		}
	}
	if renderAt < 0 || guardAt < 0 {
		t.Fatalf("the Tiltfile has no render (%d) or no guard call (%d)", renderAt+1, guardAt+1)
	}
	if len(code) == 0 || code[0] != guardAt {
		t.Errorf("the guard (line %d) is not the first statement after the render (line %d)", guardAt+1, renderAt+1)
	}
	if firstApply < 0 || firstApply < guardAt {
		t.Errorf("a k8s_yaml (line %d) comes before the guard (line %d)", firstApply+1, guardAt+1)
	}
	if strings.Count(string(raw), "tilt preflight' % lo_bin") != 1 {
		t.Error("the Tiltfile calls lo tilt preflight more than once, or in another form")
	}
}

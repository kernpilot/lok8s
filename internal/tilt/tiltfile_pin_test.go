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

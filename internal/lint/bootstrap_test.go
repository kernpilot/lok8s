package lint

// bootstrap_test.go covers the spec.bootstrap checks.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestBootstrapEntryNotFound(t *testing.T) {
	l, base, _, errOut := newLinter(t)
	dir := filepath.Join(base, "clusters", "a.dev")
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	// Neither name is an addon the binary ships (a shipped one — ccm,
	// cilium — is always found now: the embedded copy serves it).
	testutil.WriteFile(t, spec, "kind: Lo\nspec:\n  bootstrap:\n    - nope\n    - gone:\n        wait: true\n")

	if got := l.bootstrap(dir, spec, "a.dev"); got != 2 {
		t.Fatalf("bootstrap errors = %d, want 2\nstderr:\n%s", got, errOut.String())
	}
	// The scalar entry is reported in its yq-JSON form (quoted), the map
	// entry as compact JSON — both with the verbatim resolved dir.
	for _, want := range []string{
		`spec.bootstrap entry not found: "nope" (resolved to ` + l.Paths.Lok8s + "/addons/nope)",
		`spec.bootstrap entry not found: {"gone":{"wait":true}} (resolved to ` + l.Paths.Lok8s + "/addons/gone)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
		}
	}
}

func TestBootstrapDefaultCilium(t *testing.T) {
	l, base, _, errOut := newLinter(t)
	dir := filepath.Join(base, "clusters", "a.dev")
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	// Absent spec.bootstrap on a Lo cluster → the per-driver default entry
	// "cilium" (BARE, not JSON-quoted — it comes from an echo, not yq).
	testutil.WriteFile(t, spec, "kind: Lo\n")

	// The default resolves to the embedded cilium (the binary ships it), so
	// a project without a local copy lints clean — and lint, being
	// read-only, ejects nothing.
	if got := l.bootstrap(dir, spec, "a.dev"); got != 0 {
		t.Fatalf("bootstrap errors = %d, want 0\nstderr:\n%s", got, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(l.Paths.Lok8s, "addons", "cilium")); err == nil {
		t.Error("lint ejected cilium into the project")
	}

	// Explicit empty list = authoritative opt-out: no default, no error.
	testutil.WriteFile(t, spec, "kind: Lo\nspec:\n  bootstrap: []\n")
	if got := l.bootstrap(dir, spec, "a.dev"); got != 0 {
		t.Fatalf("bootstrap errors with empty list = %d, want 0", got)
	}
}

// TestBootstrapSUCKubectlCases runs the lint cases of the shared fixtures of
// the doctor check (tests/fixtures/doctor-suc-kubectl, the cases with a
// lint-want file; the bats twin is tests/unit/lint_test.bats): a minor out
// of the range of the kubectl image of system-upgrade-controller is a
// warning with the doctor line, never an error, and every other case prints
// nothing.
func TestBootstrapSUCKubectlCases(t *testing.T) {
	const fixtures = "../../tests/fixtures/doctor-suc-kubectl/cases"
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, e := range entries {
		name := e.Name()
		want, err := os.ReadFile(filepath.Join(fixtures, name, "lint-want"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		ran++
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(fixtures, name, "cluster.lok8s.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			l, base, _, errOut := newLinter(t)
			dir := filepath.Join(base, "clusters", "suc.example")
			spec := filepath.Join(dir, "cluster.lok8s.yaml")
			testutil.WriteFile(t, spec, string(raw))
			if got := l.bootstrap(dir, spec, "suc.example"); got != 0 {
				t.Errorf("bootstrap errors = %d, want 0", got)
			}
			if got := errOut.String(); got != string(want) {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
			}
		})
	}
	if ran < 12 {
		t.Fatalf("only %d lint cases under %s", ran, fixtures)
	}
}

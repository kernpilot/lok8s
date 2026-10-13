package domain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// specLayoutFixtures lists tests/fixtures/spec-layout/<verdict>--<case>.yaml
// with the expected answer in <case>.want: "ok" or "<line> <marker>". The
// bats twin (tests/unit/domain_spec_layout_test.bats) reads the same files.
func specLayoutFixtures(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(t), "tests", "fixtures", "spec-layout", "*.yaml"))
	if err != nil || len(files) < 15 {
		t.Fatalf("spec layout fixtures missing (%d found, err %v)", len(files), err)
	}
	out := map[string]string{}
	for _, f := range files {
		want, err := os.ReadFile(strings.TrimSuffix(f, ".yaml") + ".want")
		if err != nil {
			t.Fatal(err)
		}
		out[f] = strings.TrimSuffix(string(want), "\n")
	}
	return out
}

func specLayoutVerdict(path string) string {
	line, marker, ok := SpecLayout(path)
	if ok {
		return "ok"
	}
	return fmt.Sprintf("%d %s", line, marker)
}

func TestSpecLayoutFixtures(t *testing.T) {
	for path, want := range specLayoutFixtures(t) {
		if got := specLayoutVerdict(path); got != want {
			t.Errorf("%s: got %q, want %q", filepath.Base(path), got, want)
		}
		if prefix, _, _ := strings.Cut(filepath.Base(path), "--"); (prefix == "ok") != (want == "ok") {
			t.Errorf("%s: the name says %q, the .want file says %q", filepath.Base(path), prefix, want)
		}
	}
	if _, _, ok := SpecLayout(filepath.Join(t.TempDir(), "missing.yaml")); !ok {
		t.Error("a missing file is not ok: the readers report it themselves")
	}
}

// TestSpecLayoutMatchesBash runs domain::spec_layout on every fixture.
func TestSpecLayoutMatchesBash(t *testing.T) {
	root := testutil.RepoRoot(t)
	script := `import() { :; }; source "$1"
if at=$(domain::spec_layout "$2"); then echo ok; else printf '%s\n' "${at}"; fi`
	for path := range specLayoutFixtures(t) {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", filepath.Join(root, ".lok8s", "utils", "domain.sh"), path)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: bash: %v\n%s", filepath.Base(path), err, stderr.String())
		}
		if bash, goVerdict := strings.TrimSuffix(string(out), "\n"), specLayoutVerdict(path); bash != goVerdict {
			t.Errorf("%s: Go %q, bash %q", filepath.Base(path), goVerdict, bash)
		}
		if stderr.Len() != 0 {
			t.Errorf("%s: bash wrote to stderr:\n%s", filepath.Base(path), stderr.String())
		}
	}
}

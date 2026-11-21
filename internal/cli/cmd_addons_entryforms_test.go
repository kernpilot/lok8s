package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// bareEntryFixtures holds the written forms of a cilium entry that the bats
// twin (tests/unit/addons_detail_test.bats) reads too.
const bareEntryFixtures = "../../tests/fixtures/bare-entry"

// TestAddonsDetailEntryForms runs every shared case through `lo addons
// --detail`: a bare cilium entry with a trailing "/", "/.", "/./" or "//",
// and `a/cilium/.`, get the hint of cilium; a bare `cilium/..` and `.` name
// "addons" and get the default hint; a path entry is a target row.
func TestAddonsDetailEntryForms(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(bareEntryFixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 12 {
		t.Fatalf("only %d cases under %s", len(entries), bareEntryFixtures)
	}
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			p := synthProject(t)
			for _, dst := range []string{
				filepath.Join(p.Lok8s, "addons", "cilium"),
				filepath.Join(p.Clusters, "test.lok8s.dev", "cilium"),
			} {
				for _, f := range []string{"chart.yaml", "values.yaml"} {
					raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "addon", f))
					if err != nil {
						t.Fatal(err)
					}
					testutil.WriteFile(t, filepath.Join(dst, f), string(raw))
				}
			}
			raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "cases", name, "cluster.lok8s.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), string(raw))
			hint, err := os.ReadFile(filepath.Join(bareEntryFixtures, "cases", name, "hint"))
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := runLo(t, NewRoot(p), "addons", "--detail", "--domain", "test.lok8s.dev")
			if err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			if want := "  " + strings.TrimSuffix(string(hint), "\n") + "\n"; !strings.Contains(stdout, want) {
				t.Errorf("no row ends with %q in:\n%s", want, stdout)
			}
		})
	}
}

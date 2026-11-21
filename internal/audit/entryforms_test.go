package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bareEntryFixtures holds the written forms of a cilium entry that the bats
// twin (tests/unit/audit_test.bats) reads too.
const bareEntryFixtures = "../../tests/fixtures/bare-entry"

// bareEntryCase lays down one shared case: the addon at
// .lok8s/addons/cilium, the same dir at clusters/test.lok8s.dev/cilium (a
// `./cilium` path entry), and the case's cluster spec.
func bareEntryCase(t *testing.T, a *Auditor, name string) {
	t.Helper()
	for _, dst := range []string{
		filepath.Join(a.Paths.Lok8s, "addons", "cilium"),
		filepath.Join(a.Paths.Clusters, "test.lok8s.dev", "cilium"),
	} {
		for _, f := range []string{"chart.yaml", "values.yaml"} {
			raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "addon", f))
			if err != nil {
				t.Fatal(err)
			}
			writeFileT(t, filepath.Join(dst, f), string(raw))
		}
	}
	raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "cases", name, "cluster.lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(a.Paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), string(raw))
}

// TestCheckCiliumEntryForms runs every shared case: a bare cilium entry
// with a trailing "/", "/.", "/./" or "//" counts, also `a/cilium/.` (its
// values are absent: unknown); a bare `cilium/..` and `.` name "addons";
// a path entry counts with a trailing "/" and not with a trailing "/." or
// "/./".
func TestCheckCiliumEntryForms(t *testing.T) {
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
			a := newFixtureAuditor(t)
			bareEntryCase(t, a, name)
			raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "cases", name, "audit"))
			if err != nil {
				t.Fatal(err)
			}
			f := findingByID(t, a.RunDomain("test.lok8s.dev"), "cilium-policy-enforcement")
			if want := strings.TrimSuffix(string(raw), "\n"); f.Status != want {
				t.Errorf("status = %s, want %s (%s)", f.Status, want, f.Detail)
			}
		})
	}
}

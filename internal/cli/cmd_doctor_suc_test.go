package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// The kubectl range check of system-upgrade-controller in `lo doctor`
// (cmd_doctor_suc.go). The cases are shared with the bats twin
// (tests/unit/doctor_test.bats) and with the lint tests of both twins
// under tests/fixtures/doctor-suc-kubectl/cases/<case>/:
//
//	cluster.lok8s.yaml   the domain's spec
//	want                 the exact output of the doctor check
//	lint-want            the exact stderr of the lint rule (absent: not a
//	                     lint case)

const (
	sucFixtures   = "../../tests/fixtures/doctor-suc-kubectl"
	sucTestDomain = "suc.example"
)

// TestDoctorSUCKubectlCases runs every shared case: (a) the range edges
// (1.32 and 1.38 warn, 1.33 and 1.37 pass) and 1.31, 1.35 and a digest;
// (b) a Lo cluster without a version gets the line of the kind default, a
// KubeOne cluster without one gets no line; (c) a version that is not vX.Y
// or vX.Y.Z, and one that cannot be read, is not checked; (d) the addon as
// a bare entry or a map entry counts, as a path entry or absent it does not.
func TestDoctorSUCKubectlCases(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(sucFixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 14 {
		t.Fatalf("only %d cases under %s", len(entries), sucFixtures)
	}
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(sucFixtures, "cases", name)
			raw, err := os.ReadFile(filepath.Join(dir, "cluster.lok8s.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(dir, "want"))
			if err != nil {
				t.Fatal(err)
			}
			p := synthProject(t)
			testutil.WriteFile(t, filepath.Join(p.Clusters, sucTestDomain, "cluster.lok8s.yaml"), string(raw))
			var out strings.Builder
			doctorSUCKubectl(p, sucTestDomain, bootstrapEntries(p), &out)
			if got := out.String(); got != string(want) {
				t.Errorf("output:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

// No domain, a deploy domain and a domain without a spec: no line.
func TestDoctorSUCKubectlNeedsAClusterSpec(t *testing.T) {
	p := synthProject(t)
	raw, err := os.ReadFile(filepath.Join(sucFixtures, "cases", "v1.31", "cluster.lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(p.Clusters, "deploy.example", "deploy.lok8s.yaml"), string(raw))
	for _, d := range []string{"", "deploy.example", "none.example"} {
		var out strings.Builder
		doctorSUCKubectl(p, d, bootstrapEntries(p), &out)
		if got := out.String(); got != "" {
			t.Errorf("domain %q: %q, want no line", d, got)
		}
	}
}

// The line comes after the line of the sso-gate check, in the domain
// section.
func TestDoctorSUCKubectlLineInTheDomainSection(t *testing.T) {
	p := synthProject(t)
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	t.Setenv("PATH", t.TempDir())
	raw, err := os.ReadFile(filepath.Join(sucFixtures, "cases", "v1.31", "cluster.lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec := strings.Replace(string(raw), "    - system-upgrade-controller\n", "    - system-upgrade-controller\n    - sso-gate\n", 1)
	testutil.WriteFile(t, filepath.Join(p.Clusters, sucTestDomain, "cluster.lok8s.yaml"), spec)
	stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", sucTestDomain)
	want, err := os.ReadFile(filepath.Join(sucFixtures, "cases", "v1.31", "want"))
	if err != nil {
		t.Fatal(err)
	}
	sso := "  ℹ sso-gate SecurityPolicy CRD check skipped: no kubeconfig at .kubeconfig/suc-test.yaml\n"
	if !strings.Contains(stdout, sso+string(want)) {
		t.Errorf("want %q after %q in:\n%s", want, sso, stdout)
	}
}

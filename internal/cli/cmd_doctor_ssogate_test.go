package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// The sso-gate SecurityPolicy CRD check of `lo doctor`
// (cmd_doctor_ssogate.go). The cases are shared with the bats twin
// (tests/unit/doctor_test.bats) under tests/fixtures/doctor-sso-gate/cases/<case>/:
//
//	cluster.lok8s.yaml   the domain's spec (absent: the domain has none)
//	deploy.lok8s.yaml    the domain's deploy spec (absent: none)
//	kubeconfig-name      the name of the kubeconfig file (absent: sso-test)
//	no-kubeconfig        present: the domain has no kubeconfig
//	crd                  the answer of the CRD read: `json <file in json/>`,
//	                     `none` (no output), `blank` (JSON white space only)
//	                     or `fail <rc> <stderr>`. Absent: the read must not run.
//	want                 the exact output of the check
//
// Every failed read and every invalid answer that has text of its own
// carries the word CANARY: the check must never print it. The runner is a
// fake: nothing runs and no cluster is reached.

const (
	ssoFixtures   = "../../tests/fixtures/doctor-sso-gate"
	ssoTestDomain = "sso.example"
	ssoTestName   = "sso-test" // metadata.name in every case spec
)

// ssoCase sets up one case in a synthetic project and returns the project,
// the PATH that holds the (never executed) kubectl, and the fake runner.
func ssoCase(t *testing.T, name string) (*config.Paths, string, *scriptRunner) {
	t.Helper()
	dir := filepath.Join(ssoFixtures, "cases", name)
	if _, err := os.Stat(filepath.Join(dir, "want")); err != nil {
		t.Fatalf("no case %s: %v", name, err)
	}
	p := synthProject(t)
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	for _, f := range []string{"cluster.lok8s.yaml", "deploy.lok8s.yaml"} {
		if raw, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			testutil.WriteFile(t, filepath.Join(p.Clusters, ssoTestDomain, f), string(raw))
		}
	}
	kcName := ssoTestName
	if raw, err := os.ReadFile(filepath.Join(dir, "kubeconfig-name")); err == nil {
		kcName = strings.TrimSuffix(string(raw), "\n")
	}
	kubeconfig := filepath.Join(p.Base, ".kubeconfig", kcName+".yaml")
	if _, err := os.Stat(filepath.Join(dir, "no-kubeconfig")); err != nil {
		testutil.WriteFile(t, kubeconfig, "apiVersion: v1\nkind: Config\n")
	}
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	testutil.WriteFile(t, kubectl, "#!/bin/sh\nexit 99\n")
	if err := os.Chmod(kubectl, 0o755); err != nil {
		t.Fatal(err)
	}

	r := &scriptRunner{handler: func(c execx.Cmd) error {
		want := []string{"--kubeconfig", kubeconfig, "get", "customresourcedefinitions.apiextensions.k8s.io", "securitypolicies.gateway.envoyproxy.io", "--ignore-not-found", "-o", "json", "--request-timeout=10s"}
		if c.Name != kubectl || !slices.Equal(c.Args, want) {
			t.Errorf("exec %s %q, want %s %q", c.Name, c.Args, kubectl, want)
			return errors.New("unexpected exec")
		}
		if c.WaitDelay != rbacReadGrace || !c.ProcessGroup {
			t.Errorf("the read runs without the grace or its process group: %v %v", c.WaitDelay, c.ProcessGroup)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "crd"))
		if err != nil {
			t.Errorf("unexpected read of the CRD")
			return errors.New("unexpected read")
		}
		kind, rest, _ := strings.Cut(strings.TrimRight(string(raw), "\n"), " ")
		switch kind {
		case "json":
			body, err := os.ReadFile(filepath.Join(ssoFixtures, "json", rest))
			if err != nil {
				t.Fatal(err)
			}
			c.Stdout.Write(body)
			return nil
		case "none":
			return nil
		case "blank":
			c.Stdout.Write([]byte(" \n\t\r\n"))
			return nil
		case "fail":
			code, text, _ := strings.Cut(rest, " ")
			rc, err := strconv.Atoi(code)
			if err != nil {
				t.Fatal(err)
			}
			c.Stderr.Write([]byte(text + "\n"))
			return rbacExit(rc)
		}
		t.Fatalf("case %s: bad answer %q", name, raw)
		return nil
	}}
	return p, bin, r
}

// TestDoctorSSOGateCases runs every shared case: (a) the field in v1alpha1
// is OK, its absence is a FAIL (also without a schema, with a null field,
// with a key in another case, and with the key only at a nested place);
// (b) the version is selected by its name: v1alpha1 after another version
// that lacks the field is OK, and another version first that has the field
// does not hide a v1alpha1 without it; (c) the flags: a v1alpha1 that is
// not served is not checked, also when it lacks the field (the served
// check comes before the field); a storage version other than v1alpha1
// without the field is a FAIL of its own, with the field it is OK, and when
// v1alpha1 lacks the field too, the line of v1alpha1 wins; (d) no CRD (no
// output, white space, an empty List) skips; (e) each failed read prints
// its fixed reason and never kubectl's output; (f) an answer of another
// shape (not JSON, not an object, a List with an item, two documents, a
// byte-order mark, NaN, a number out of range, two v1alpha1, a name that is
// not a string, a step or the field that is not an object, versions that
// are not a list, a version that is not an object, a served flag that is
// not a boolean, no storage flag, no or two storage versions, a field of
// the storage version that is not an object) prints the fixed reason in
// both twins; (g) the addon as a map entry or with a trailing "/" or "/."
// counts, as a path entry or absent it does not, and the default
// spec.bootstrap has no sso-gate; (h)
// no kubeconfig, a kubeconfig path with a control character, a deploy
// domain, no cluster spec and a spec that cannot be read.
func TestDoctorSSOGateCases(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(ssoFixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 30 {
		t.Fatalf("only %d cases under %s", len(entries), ssoFixtures)
	}
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			p, bin, r := ssoCase(t, name)
			var out strings.Builder
			doctorSSOGateMergeType(context.Background(), r, p, ssoTestDomain, bin, bootstrapEntries(p), &out)

			raw, err := os.ReadFile(filepath.Join(ssoFixtures, "cases", name, "want"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := out.String(), string(raw); got != want {
				t.Errorf("output:\n%s\nwant:\n%s", got, want)
			}
			if strings.Contains(out.String(), "CANARY") {
				t.Errorf("the output carries kubectl's output:\n%s", out.String())
			}
			reads := 0
			if _, err := os.Stat(filepath.Join(ssoFixtures, "cases", name, "crd")); err == nil {
				reads = 1
			}
			if len(r.calls) != reads {
				t.Errorf("reads = %q, want %d", r.calls, reads)
			}
		})
	}
}

// No active domain: no line. No kubectl on the PATH: one line. No read in
// either.
func TestDoctorSSOGateSkipsWithoutDomainOrKubectl(t *testing.T) {
	p, _, r := ssoCase(t, "declared")
	var out strings.Builder
	doctorSSOGateMergeType(context.Background(), r, p, "", t.TempDir(), bootstrapEntries(p), &out)
	if got := out.String(); got != "" {
		t.Errorf("no domain: %q, want no line", got)
	}

	out.Reset()
	doctorSSOGateMergeType(context.Background(), r, p, ssoTestDomain, t.TempDir(), bootstrapEntries(p), &out)
	if got, want := out.String(), "  ! sso-gate SecurityPolicy CRD not checked: kubectl not found\n"; got != want {
		t.Errorf("no kubectl: %q, want %q", got, want)
	}
	if len(r.calls) != 0 {
		t.Errorf("reads without a domain or kubectl: %q", r.calls)
	}
}

// The bound (rbacReadBound, through rbacGet) stops a read that does not
// end, and the check prints the fixed reason of the bound. The parent
// context never ends: only the bound can stop the run.
func TestDoctorSSOGateReadBoundStopsAHungRead(t *testing.T) {
	p, bin, _ := ssoCase(t, "declared")
	defer func(d time.Duration) { rbacReadBound = d }(rbacReadBound)
	rbacReadBound = 5 * time.Millisecond
	r := &rbacHangRunner{fallback: 5 * time.Second}
	var out strings.Builder
	doctorSSOGateMergeType(context.Background(), r, p, ssoTestDomain, bin, bootstrapEntries(p), &out)
	want := "  ! sso-gate SecurityPolicy CRD not checked: " + rbacReasonNoAnswer + "\n"
	if got := out.String(); got != want {
		t.Errorf("a hung read: %q, want %q", got, want)
	}
	if r.calls != 1 {
		t.Errorf("reads: %d, want 1", r.calls)
	}
}

// The check reads what the embedded addon applies: sso-gate is an addon,
// its SecurityPolicy has the API version of the check's version name, and
// it sets the field that the check looks for.
func TestDoctorSSOGateConstantsMatchTheAddon(t *testing.T) {
	if !slices.Contains(assets.AddonNames(), ssoAddon) {
		t.Fatalf("no embedded addon %s", ssoAddon)
	}
	raw, err := fs.ReadFile(assets.FS(), "addons/"+ssoAddon+"/securitypolicy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		APIVersion string         `yaml:"apiVersion"`
		Kind       string         `yaml:"kind"`
		Spec       map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	group, version, _ := strings.Cut(doc.APIVersion, "/")
	if doc.Kind != "SecurityPolicy" || version != ssoVersion || "securitypolicies."+group != ssoCRD {
		t.Errorf("the addon applies %s %s; the check reads %s, version %s", doc.APIVersion, doc.Kind, ssoCRD, ssoVersion)
	}
	if got, want := ssoFieldPath[len(ssoFieldPath)-1], "mergeType"; got != want {
		t.Errorf("the check looks for %s, want %s", got, want)
	}
	if _, ok := doc.Spec["mergeType"]; !ok {
		t.Errorf("the addon's SecurityPolicy has no spec.mergeType")
	}
}

// The line closes the domain section, after the line of the RBAC migration
// check.
func TestDoctorSSOGateLineInTheDomainSection(t *testing.T) {
	p, _, _ := ssoCase(t, "no-kubeconfig")
	t.Setenv("PATH", t.TempDir())
	stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", ssoTestDomain)
	want := "--- domain ---\n  ✓ active: sso.example (kind kubeone)\n" +
		"  ℹ RBAC migration check skipped: spec.bootstrap has no system-upgrade-controller and no redis-operator\n" +
		"  ℹ sso-gate SecurityPolicy CRD check skipped: no kubeconfig at .kubeconfig/sso-test.yaml\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("want %q in:\n%s", want, stdout)
	}
}

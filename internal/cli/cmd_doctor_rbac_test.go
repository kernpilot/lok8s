package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// The RBAC migration check of `lo doctor` (cmd_doctor_rbac.go). The cases
// are shared with the bats twin (tests/unit/doctor_rbac_test.bats) under
// tests/fixtures/doctor-rbac/cases/<case>/:
//
//	cluster.lok8s.yaml   the domain's spec (absent: the domain has none)
//	no-kubeconfig        present: the domain has no kubeconfig
//	clusterrolebindings  the answer of that read: `json <file in json/>`, or
//	clusterroles         `fail <rc> <stderr>`. Absent: the read must not run.
//	want                 the exact output of the check
//
// Every failed read carries the word CANARY in its stderr (or its stdout):
// the check must never print it. The runner is a fake: nothing runs and no
// cluster is reached.

const (
	rbacFixtures   = "../../tests/fixtures/doctor-rbac"
	rbacTestDomain = "rbac.example"
	rbacTestName   = "rbac-test" // metadata.name in every case spec
)

// rbacExit is the error of a fake kubectl that exits non-zero.
type rbacExit int

func (e rbacExit) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e rbacExit) ExitCode() int { return int(e) }

// rbacCase sets up one case in a synthetic project and returns the project,
// the PATH that holds the (never executed) kubectl, and the fake runner.
func rbacCase(t *testing.T, name string) (*config.Paths, string, *scriptRunner) {
	t.Helper()
	dir := filepath.Join(rbacFixtures, "cases", name)
	if _, err := os.Stat(filepath.Join(dir, "want")); err != nil {
		t.Fatalf("no case %s: %v", name, err)
	}
	p := synthProject(t)
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	if raw, err := os.ReadFile(filepath.Join(dir, "cluster.lok8s.yaml")); err == nil {
		testutil.WriteFile(t, filepath.Join(p.Clusters, rbacTestDomain, "cluster.lok8s.yaml"), string(raw))
	}
	kubeconfig := filepath.Join(p.Base, ".kubeconfig", rbacTestName+".yaml")
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
		if c.Name != kubectl || len(c.Args) < 4 {
			t.Errorf("unexpected exec: %s %v", c.Name, c.Args)
			return errors.New("unexpected exec")
		}
		res := c.Args[3]
		want := []string{"--kubeconfig", kubeconfig, "get", res, "-o", "json", "--request-timeout=10s"}
		if !slices.Equal(c.Args, want) {
			t.Errorf("kubectl args = %q, want %q", c.Args, want)
		}
		raw, err := os.ReadFile(filepath.Join(dir, res))
		if err != nil {
			t.Errorf("unexpected read of %s", res)
			return errors.New("unexpected read")
		}
		kind, rest, _ := strings.Cut(strings.TrimRight(string(raw), "\n"), " ")
		switch kind {
		case "json":
			body, err := os.ReadFile(filepath.Join(rbacFixtures, "json", rest))
			if err != nil {
				t.Fatal(err)
			}
			c.Stdout.Write(body)
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

func rbacWantReads(t *testing.T, name string) []string {
	t.Helper()
	var reads []string
	for _, res := range []string{"clusterrolebindings", "clusterroles"} {
		if _, err := os.Stat(filepath.Join(rbacFixtures, "cases", name, res)); err == nil {
			reads = append(reads, res)
		}
	}
	return reads
}

// TestDoctorRBACCases runs every shared case: (a) a leftover SUC binding
// under another name than the chart's; (b) a narrowed cluster passes; (c)
// no kubeconfig skips; (d) neither addon in spec.bootstrap skips; (e) each
// failed read prints its fixed reason and never kubectl's output; (f) each
// redis-operator form (the old binding, the old role, its aggregation
// labels, no pods/exec), and the guards around them.
func TestDoctorRBACCases(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(rbacFixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 20 {
		t.Fatalf("only %d cases under %s", len(entries), rbacFixtures)
	}
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			p, bin, r := rbacCase(t, name)
			var out strings.Builder
			doctorRBACLeftovers(context.Background(), r, p, rbacTestDomain, bin, bootstrapEntries(p), &out)

			want, err := os.ReadFile(filepath.Join(rbacFixtures, "cases", name, "want"))
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != string(want) {
				t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
			}
			if strings.Contains(out.String(), "CANARY") {
				t.Errorf("the output carries kubectl's output:\n%s", out.String())
			}
			var reads []string
			for _, call := range r.calls {
				if f := strings.Fields(call); len(f) > 4 {
					reads = append(reads, f[4])
				}
			}
			if want := rbacWantReads(t, name); !slices.Equal(reads, want) {
				t.Errorf("reads = %q, want %q", reads, want)
			}
		})
	}
}

// No active domain, and no kubectl on the PATH: one line each, no read.
func TestDoctorRBACSkipsWithoutDomainOrKubectl(t *testing.T) {
	p, _, r := rbacCase(t, "narrowed")
	var out strings.Builder
	doctorRBACLeftovers(context.Background(), r, p, "", t.TempDir(), bootstrapEntries(p), &out)
	if got, want := out.String(), "  ℹ RBAC migration check skipped: no active domain\n"; got != want {
		t.Errorf("no domain: %q, want %q", got, want)
	}

	out.Reset()
	doctorRBACLeftovers(context.Background(), r, p, rbacTestDomain, t.TempDir(), bootstrapEntries(p), &out)
	if got, want := out.String(), "  ! RBAC migration leftovers not checked: kubectl not found\n"; got != want {
		t.Errorf("no kubectl: %q, want %q", got, want)
	}
	if len(r.calls) != 0 {
		t.Errorf("reads without a domain or kubectl: %q", r.calls)
	}
}

// A kubectl that does not end (an exec credential plugin that waits for a
// login) is stopped at the bound, and the read is unreachable.
func TestDoctorRBACReadBoundIsUnreachable(t *testing.T) {
	r := &scriptRunner{handler: func(c execx.Cmd) error {
		c.Stderr.Write([]byte("error: CANARY\n")) // alone, the class "other"
		return context.DeadlineExceeded
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	var items []rbacBinding
	if got := rbacRead(ctx, r, "kubectl", "kc", "clusterrolebindings", &items); got != rbacReasonUnreachable {
		t.Errorf("an ended context: %q, want %q", got, rbacReasonUnreachable)
	}
}

// The real command: the lines close the domain section, after the active
// line, and before the provider section.
func TestDoctorRBACLinesCloseTheDomainSection(t *testing.T) {
	p, _, _ := rbacCase(t, "no-kubeconfig")
	t.Setenv("PATH", t.TempDir())
	stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", rbacTestDomain)
	want := "--- domain ---\n  ✓ active: rbac.example (kind kubeone)\n  ℹ RBAC migration check skipped: no kubeconfig at .kubeconfig/rbac-test.yaml\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("want %q in:\n%s", want, stdout)
	}
}

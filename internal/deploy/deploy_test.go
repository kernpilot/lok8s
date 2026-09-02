package deploy

// deploy_test.go ports tests/unit/deploy_test.bats: apply/filter logic
// with a recording fake kubectl (no cluster); the scoped wait is answered
// by a ready snapshot so it never loops.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
)

const artifactYAML = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.test.lok8s.dev
---
apiVersion: v1
kind: Namespace
metadata:
  name: networking
  labels:
    lok8s.dev/type: system
    flag: true
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: default
  labels:
    lok8s.dev/type: platform
`

// readySnapshot answers `kubectl get deploy,ds,sts` with test-app available.
const readySnapshot = `{"items":[{"kind":"Deployment","metadata":{"namespace":"default","name":"test-app"},"spec":{"replicas":1},"status":{"availableReplicas":1}}]}`

type rcError struct{ code int }

func (e *rcError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *rcError) ExitCode() int { return e.code }

// fakeRunner mocks kubectl: apply consumes the manifest and records which
// kinds reached it; wait echoes; get returns the ready snapshot.
type fakeRunner struct {
	calls    []string
	applied  []string // manifests handed to apply, in order
	applyRC  int
	waitFail bool
}

func (f *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	f.calls = append(f.calls, c.Name+" "+strings.Join(c.Args, " "))
	verb := ""
	if len(c.Args) > 0 {
		verb = c.Args[0]
	}
	switch verb {
	case "apply":
		raw, _ := io.ReadAll(c.Stdin)
		f.applied = append(f.applied, string(raw))
		if f.applyRC != 0 {
			fmt.Fprintln(c.Stderr, "error: apply refused")
			return &rcError{f.applyRC}
		}
		fmt.Fprintln(c.Stdout, "applied")
		return nil
	case "wait":
		if f.waitFail {
			return &rcError{1}
		}
		fmt.Fprintln(c.Stdout, "waited: "+strings.Join(c.Args, " "))
		return nil
	case "get":
		fmt.Fprint(c.Stdout, readySnapshot)
		return nil
	}
	return nil
}

func newDeployer(t *testing.T, f *fakeRunner, artifact string) (*Deployer, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	base := t.TempDir()
	p := &config.Paths{Base: base, Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	domainDir := filepath.Join(p.Clusters, "test.lok8s.dev")
	if err := os.MkdirAll(domainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if artifact != "" {
		if err := os.WriteFile(filepath.Join(domainDir, "artifacts.yaml"), []byte(artifact), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errBuf bytes.Buffer
	a := kapply.NewApplier(f, &out, &errBuf)
	a.Sleep = func(time.Duration) {}
	return &Deployer{Paths: p, Applier: a, Stderr: &errBuf}, &out, &errBuf, domainDir
}

// bats: "deploy::apply applies the single domain artifact"
// bats: "deploy::apply applies CRDs first (waits for Established)"
func TestApplyAppliesCRDsFirst(t *testing.T) {
	f := &fakeRunner{}
	d, out, _, _ := newDeployer(t, f, artifactYAML)
	if err := d.Apply(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "applied") {
		t.Errorf("stdout = %q", out.String())
	}
	want := []string{
		"kubectl apply --server-side --force-conflicts -f -",
		"kubectl wait --for=condition=Established crd/widgets.test.lok8s.dev --timeout=60s",
		"kubectl apply --server-side --force-conflicts -f -",
		"kubectl get deploy,ds,sts --all-namespaces -o json",
	}
	if got := strings.Join(f.calls, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	// Phase 1 carries ONLY the CRD; phase 2 the whole artifact, verbatim.
	if !strings.Contains(f.applied[0], "kind: CustomResourceDefinition") || strings.Contains(f.applied[0], "kind: Namespace") {
		t.Errorf("CRD phase manifest = %q", f.applied[0])
	}
	if f.applied[1] != artifactYAML {
		t.Errorf("phase-2 manifest must be the artifact bytes")
	}
}

// bats: "deploy::apply errors when the artifact is missing (build not run)"
func TestApplyMissingArtifact(t *testing.T) {
	f := &fakeRunner{}
	d, _, errBuf, domainDir := newDeployer(t, f, "")
	err := d.Apply(context.Background(), "test.lok8s.dev")
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	want := "\033[0;31m[error]\033[0m no artifact for test.lok8s.dev: " + filepath.Join(domainDir, "artifacts.yaml") + " — run 'lo build' first\n"
	if errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
	if len(f.calls) != 0 {
		t.Errorf("kubectl must not run: %v", f.calls)
	}
}

// bats: "deploy::apply is a graceful no-op on an artifact with no objects"
func TestApplyEmptyArtifactNoOp(t *testing.T) {
	f := &fakeRunner{}
	d, out, _, _ := newDeployer(t, f, "# just a comment\n---\n")
	if err := d.Apply(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "applied") || len(f.calls) != 0 {
		t.Errorf("no apply expected: out=%q calls=%v", out.String(), f.calls)
	}
}

// Unfiltered: a failing apply exits with kubectl's status (bash errexit).
func TestApplyFailurePropagatesExitCode(t *testing.T) {
	f := &fakeRunner{applyRC: 3}
	d, _, _, _ := newDeployer(t, f, artifactYAML)
	err := d.Apply(context.Background(), "test.lok8s.dev")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("err = %v, want ExitError{3}", err)
	}
	// Stopped at the CRD phase: no wait, no phase 2.
	if len(f.calls) != 1 {
		t.Errorf("calls = %v", f.calls)
	}
}

// bats: "deploy::apply_filtered applies only the matching subset (full label key)"
func TestApplyFilteredSubset(t *testing.T) {
	f := &fakeRunner{}
	d, _, _, _ := newDeployer(t, f, artifactYAML)
	if err := d.ApplyFiltered(context.Background(), "test.lok8s.dev", "lok8s.dev/type", "platform"); err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != 1 {
		t.Fatalf("applied = %v", f.applied)
	}
	m := f.applied[0]
	if !strings.Contains(m, "kind: Deployment") || strings.Contains(m, "kind: Namespace") || strings.Contains(m, "kind: CustomResourceDefinition") {
		t.Errorf("subset manifest = %q", m)
	}
}

// yq's `==` compares scalar text: an unquoted `flag: true` matches "true".
func TestApplyFilteredUnquotedScalarMatches(t *testing.T) {
	f := &fakeRunner{}
	d, _, _, _ := newDeployer(t, f, artifactYAML)
	if err := d.ApplyFiltered(context.Background(), "test.lok8s.dev", "flag", "true"); err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != 1 || !strings.Contains(f.applied[0], "kind: Namespace") {
		t.Errorf("applied = %v", f.applied)
	}
}

// bats: "deploy::apply_filtered warns and exits 0 when nothing matches"
func TestApplyFilteredNoMatchWarns(t *testing.T) {
	f := &fakeRunner{}
	d, _, errBuf, domainDir := newDeployer(t, f, artifactYAML)
	if err := d.ApplyFiltered(context.Background(), "test.lok8s.dev", "lok8s.dev/type", "nonexistent"); err != nil {
		t.Fatal(err)
	}
	want := "\033[0;33m[warn]\033[0m no objects match lok8s.dev/type=nonexistent in " + filepath.Join(domainDir, "artifacts.yaml") + "\n"
	if errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
	if len(f.calls) != 0 {
		t.Errorf("kubectl must not run: %v", f.calls)
	}
}

// bats: "deploy::apply_filtered errors when the artifact is missing"
func TestApplyFilteredMissingArtifact(t *testing.T) {
	f := &fakeRunner{}
	d, _, errBuf, _ := newDeployer(t, f, "")
	if err := d.ApplyFiltered(context.Background(), "test.lok8s.dev", "lok8s.dev/type", "system"); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errBuf.String(), "run 'lo build' first") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// bats: "deploy::apply_filtered rejects injection in label key" / "… in label value"
func TestApplyFilteredRejectsInjection(t *testing.T) {
	for _, tc := range [][2]string{{"key; rm -rf /", "value"}, {"type", "value; echo pwned"}} {
		f := &fakeRunner{}
		d, _, errBuf, _ := newDeployer(t, f, "")
		if err := d.ApplyFiltered(context.Background(), "test.lok8s.dev", tc[0], tc[1]); !errors.Is(err, ErrHandled) {
			t.Fatalf("%v: err = %v", tc, err)
		}
		want := "\033[0;31m[error]\033[0m Invalid label selector: key and value must be alphanumeric with . _ - (key may also contain /)\n"
		if errBuf.String() != want {
			t.Errorf("stderr = %q", errBuf.String())
		}
	}
}

// Filtered: bash suspends errexit inside the subset apply, so a failing
// kubectl is logged and the sequence continues to a 0 exit.
func TestApplyFilteredSwallowsApplyFailure(t *testing.T) {
	f := &fakeRunner{applyRC: 1, waitFail: true}
	d, _, errBuf, _ := newDeployer(t, f, artifactYAML)
	if err := d.ApplyFiltered(context.Background(), "test.lok8s.dev", "lok8s.dev/type", "platform"); err != nil {
		t.Fatalf("filtered apply must not fail (bash parity), got %v", err)
	}
	// Both phases still ran (no CRD in the subset → one apply + the wait).
	if got := strings.Join(f.calls, "\n"); !strings.Contains(got, "apply") || !strings.Contains(got, "get deploy,ds,sts") {
		t.Errorf("calls = %v", f.calls)
	}
	_ = errBuf
}

// bats: "deploy::wait_crds waits for CRDs to become established"
func TestWaitCRDs(t *testing.T) {
	f := &fakeRunner{}
	d, out, _, _ := newDeployer(t, f, "")
	d.waitCRDs(context.Background(), "apiVersion: v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.test.lok8s.dev\n")
	if !strings.Contains(out.String(), "waited: wait --for=condition=Established crd/widgets.test.lok8s.dev --timeout=60s") {
		t.Errorf("stdout = %q", out.String())
	}
	f2 := &fakeRunner{waitFail: true}
	d2, _, errBuf, _ := newDeployer(t, f2, "")
	d2.waitCRDs(context.Background(), "kind: CustomResourceDefinition\nmetadata:\n  name: x.y\n")
	if want := "\033[0;33m[warn]\033[0m CRD x.y not established within timeout\n"; errBuf.String() != want {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// bats: main::deploy -l parsing (key=value guard) — the three rejects.
func TestParseLabel(t *testing.T) {
	for _, bad := range []string{"=value", "foo", "foo="} {
		var errBuf bytes.Buffer
		if _, _, err := ParseLabel(&errBuf, bad); !errors.Is(err, ErrHandled) {
			t.Errorf("%q: err = %v", bad, err)
		}
		want := "\033[0;31m[error]\033[0m invalid --label '" + bad + "' — expected key=value (e.g. lok8s.dev/name=zitadel)\n"
		if errBuf.String() != want {
			t.Errorf("%q: stderr = %q", bad, errBuf.String())
		}
	}
	k, v, err := ParseLabel(io.Discard, "lok8s.dev/name=zitadel")
	if err != nil || k != "lok8s.dev/name" || v != "zitadel" {
		t.Errorf("got %q %q %v", k, v, err)
	}
	// Only the FIRST '=' splits (bash ${label%%=*} / ${label#*=}).
	k, v, _ = ParseLabel(io.Discard, "a=b=c")
	if k != "a" || v != "b=c" {
		t.Errorf("got %q %q", k, v)
	}
}

func TestHasObjects(t *testing.T) {
	for s, want := range map[string]bool{
		"":                           false,
		"# c\n---\n":                 false,
		"kind: Foo\n":                true,
		"  kind: Foo\n":              false,
		"kind:Foo\n":                 false,
		"a: 1\n---\nkind: Bar\nx: y": true,
	} {
		if got := HasObjects(s); got != want {
			t.Errorf("HasObjects(%q) = %v", s, got)
		}
	}
}

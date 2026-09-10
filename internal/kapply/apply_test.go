package kapply

// apply_test.go — the Go port of tests/unit/kapply_test.bats (apply/heal
// half): server-side apply with bounded, opt-in self-healing for the two
// states a plain apply can't reconcile. HERMETIC: every kubectl runs
// through the fake runner — no live cluster is ever touched.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/clock"
	"github.com/kernpilot/lok8s/internal/execx"
)

// rcError carries a fake subprocess exit code (exitCode() reads it via the
// ExitCode() interface, like a real *exec.ExitError).
type rcError struct{ code int }

func (e *rcError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *rcError) ExitCode() int { return e.code }

// fakeKubectl mirrors the bats kubectl stub: log every call; drive `apply`
// via applyOut/applyRC and `get` via getOut; replace/patch just succeed.
type fakeKubectl struct {
	calls    []string
	applyOut string
	applyRC  int
	getOut   string
}

func (f *fakeKubectl) Run(ctx context.Context, c execx.Cmd) error {
	line := c.Name + " " + strings.Join(c.Args, " ")
	f.calls = append(f.calls, line)
	// Skip a leading --kubeconfig pair, like the bats stub's cmd probe.
	args := c.Args
	if len(args) >= 2 && args[0] == "--kubeconfig" {
		args = args[2:]
	}
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "apply":
		if f.applyOut != "" && c.Stdout != nil {
			fmt.Fprintln(c.Stdout, f.applyOut)
		}
		if f.applyRC != 0 {
			return &rcError{f.applyRC}
		}
		return nil
	case "get":
		if c.Stdout != nil {
			fmt.Fprintln(c.Stdout, f.getOut)
		}
		return nil
	default:
		return nil
	}
}

func (f *fakeKubectl) log() string { return strings.Join(f.calls, "\n") }

const deployManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  selector:
    matchLabels: {app: web}`

const secretManifest = `apiVersion: v1
kind: Secret
metadata:
  name: zitadel-credentials
  namespace: zitadel
immutable: true
data: {}`

const crManifest = `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: db
  namespace: data
spec:
  instances: 1`

func testApplier(f *fakeKubectl) (*Applier, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	a := &Applier{
		Runner: f, Stdout: &out, Stderr: &errOut,
		NonInteractive: true, // the bats setup exports LOK8S_NONINTERACTIVE=1
		NsWait:         0,
		Sleep:          clock.NoSleep,
		PollInterval:   time.Nanosecond,
	}
	return a, &out, &errOut
}

func TestCleanApplyNoHealing(t *testing.T) {
	f := &fakeKubectl{}
	a, _, _ := testApplier(f)
	_, rc := a.Apply(t.Context(), "resources", deployManifest)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if !strings.Contains(f.log(), "apply --server-side") {
		t.Errorf("no server-side apply in log:\n%s", f.log())
	}
	if strings.Contains(f.log(), "replace --force") {
		t.Errorf("unexpected heal in log:\n%s", f.log())
	}
}

func TestImmutableWithoutForceFailsFastWithHint(t *testing.T) {
	f := &fakeKubectl{applyRC: 1,
		applyOut: `Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable`}
	a, _, errOut := testApplier(f)
	_, rc := a.Apply(t.Context(), "resources", deployManifest)
	if rc == 0 {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "force-recreate") {
		t.Errorf("missing force-recreate hint:\n%s", errOut.String())
	}
	if strings.Contains(f.log(), "replace --force") {
		t.Errorf("healed without consent:\n%s", f.log())
	}
}

func TestImmutableWithForceRecreatesAndReapplies(t *testing.T) {
	f := &fakeKubectl{applyRC: 1,
		applyOut: `Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable`}
	a, _, _ := testApplier(f)
	a.ForceRecreate = true
	a.Apply(t.Context(), "resources", deployManifest)
	if !strings.Contains(f.log(), "replace --force") {
		t.Fatalf("no recreate in log:\n%s", f.log())
	}
	// healed → re-applied: two apply calls total
	if n := strings.Count(f.log(), "apply --server-side"); n != 2 {
		t.Errorf("apply count = %d, want 2\n%s", n, f.log())
	}
}

func TestSealedSecretForceRecreateWarnsReKey(t *testing.T) {
	f := &fakeKubectl{applyRC: 1,
		applyOut: "Error from server (Invalid): Secret \"zitadel-credentials\" is invalid: data: Forbidden: field is immutable when `immutable` is set"}
	a, _, errOut := testApplier(f)
	a.ForceRecreate = true
	a.Apply(t.Context(), "resources", secretManifest)
	if !strings.Contains(errOut.String(), "RE-KEYING sealed Secret/zitadel-credentials") {
		t.Errorf("missing RE-KEY warning:\n%s", errOut.String())
	}
	if !strings.Contains(f.log(), "replace --force") {
		t.Errorf("no recreate in log:\n%s", f.log())
	}
}

func TestSealedSecretDeclinedNonInteractiveKept(t *testing.T) {
	// Reaching healImmutable without the flag and without a tty must NOT
	// re-key the Secret — the crown-jewel confirm refuses, object kept.
	f := &fakeKubectl{}
	a, _, errOut := testApplier(f)
	err := `Error from server (Invalid): Secret "zitadel-credentials" is invalid: data: Forbidden: field is immutable`
	a.healImmutable(t.Context(), secretManifest, err, nil)
	if !strings.Contains(errOut.String(), "keeping sealed Secret/zitadel-credentials") {
		t.Errorf("missing keep notice:\n%s", errOut.String())
	}
	if strings.Contains(f.log(), "replace --force") {
		t.Errorf("secret was recreated:\n%s", f.log())
	}
}

func TestUnsealedSecretGenericHealNoReKeyPrompt(t *testing.T) {
	// A plain Secret can hit "field is immutable" too (e.g. a type:
	// change). Only the manifest's immutable: true earns the pointed
	// RE-KEY treatment.
	f := &fakeKubectl{applyRC: 1,
		applyOut: `Error from server (Invalid): Secret "plain-creds" is invalid: type: field is immutable`}
	a, _, errOut := testApplier(f)
	a.ForceRecreate = true
	plain := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: plain-creds\n  namespace: default\ndata: {}"
	a.Apply(t.Context(), "resources", plain)
	if strings.Contains(errOut.String(), "RE-KEYING") {
		t.Errorf("unexpected re-key drama:\n%s", errOut.String())
	}
	if !strings.Contains(f.log(), "replace --force") {
		t.Errorf("no generic heal:\n%s", f.log())
	}
}

func TestSealedSecretInteractiveDeclineKeeps(t *testing.T) {
	// The REACHABLE decline: an interactive operator answered y to the
	// generic heal prompt, then n to the crown-jewel confirm.
	f := &fakeKubectl{}
	a, _, errOut := testApplier(f)
	a.Interactive = func() bool { return true }
	a.Ask = func(string) bool { return false } // operator answers "n"
	err := `Error from server (Invalid): Secret "zitadel-credentials" is invalid: data: Forbidden: field is immutable`
	a.healImmutable(t.Context(), secretManifest, err, nil)
	if !strings.Contains(errOut.String(), "keeping sealed Secret/zitadel-credentials") {
		t.Errorf("missing keep notice:\n%s", errOut.String())
	}
	if strings.Contains(f.log(), "replace --force") {
		t.Errorf("secret was recreated:\n%s", f.log())
	}
}

func TestSealedSecretInteractiveAcceptRecreates(t *testing.T) {
	f := &fakeKubectl{}
	a, _, errOut := testApplier(f)
	a.Interactive = func() bool { return true }
	a.Ask = func(string) bool { return true } // operator answers "y"
	err := `Error from server (Invalid): Secret "zitadel-credentials" is invalid: data: Forbidden: field is immutable`
	a.healImmutable(t.Context(), secretManifest, err, nil)
	if !strings.Contains(errOut.String(), "recreating immutable Secret/zitadel-credentials") {
		t.Errorf("missing recreate notice:\n%s", errOut.String())
	}
	if !strings.Contains(f.log(), "replace --force") {
		t.Errorf("no recreate call:\n%s", f.log())
	}
}

func TestStuckTerminatingForceClearsCRFinalizers(t *testing.T) {
	f := &fakeKubectl{applyRC: 1,
		applyOut: `Error from server: object is being deleted: clusters.postgresql.cnpg.io "db" already exists`,
		getOut:   "2026-01-01T00:00:00Z"} // non-empty deletionTimestamp
	a, _, _ := testApplier(f)
	a.ForceRecreate = true
	a.Apply(t.Context(), "resources", crManifest)
	if !strings.Contains(f.log(), "patch Cluster db") {
		t.Errorf("no CR patch:\n%s", f.log())
	}
	if !strings.Contains(f.log(), "finalizers") {
		t.Errorf("no finalizer clear:\n%s", f.log())
	}
}

func TestStuckTerminatingNamespace403ForceFinalizesAndReapplies(t *testing.T) {
	// The wedged object is the NAMESPACE itself — named only in the 403
	// text, not in the manifest. Heal = drop spec.finalizers via
	// /finalize, then re-apply.
	f := &fakeKubectl{applyRC: 1,
		applyOut: `Error from server (Forbidden): configmaps "ca-bundle" is forbidden: unable to create new content in namespace kubermatic because it is being terminated`,
		getOut:   "2026-01-01T00:00:00Z"}
	a, _, _ := testApplier(f)
	a.ForceRecreate = true
	a.NsWait = 0 // don't poll-wait in the unit test
	a.Apply(t.Context(), "resources", deployManifest)
	if !strings.Contains(f.log(), "replace --raw /api/v1/namespaces/kubermatic/finalize") {
		t.Fatalf("no /finalize call:\n%s", f.log())
	}
	if n := strings.Count(f.log(), "apply --server-side"); n != 2 {
		t.Errorf("apply count = %d, want 2\n%s", n, f.log())
	}
}

func TestFinalizeNamespaceDeclinedNeverCallsFinalize(t *testing.T) {
	// Reaching finalizeNamespace without the flag and without a tty must
	// NOT nuke the namespace — the extra confirm refuses, heal skipped.
	f := &fakeKubectl{getOut: "2026-01-01T00:00:00Z"} // ns is terminating
	a, _, _ := testApplier(f)
	a.finalizeNamespace(t.Context(), "kubermatic", nil)
	if strings.Contains(f.log(), "finalize") {
		t.Errorf("destructive /finalize call happened:\n%s", f.log())
	}
}

func TestUnknownErrorPassedThroughNoHealing(t *testing.T) {
	f := &fakeKubectl{applyRC: 1,
		applyOut: "error: unable to connect to the server: connection refused"}
	a, _, _ := testApplier(f)
	a.ForceRecreate = true
	_, rc := a.Apply(t.Context(), "resources", deployManifest)
	if rc == 0 {
		t.Fatal("expected failure")
	}
	if strings.Contains(f.log(), "replace --force") || strings.Contains(f.log(), "patch") {
		t.Errorf("unexpected healing:\n%s", f.log())
	}
}

package kapply

// wait_test.go — the Go port of the kapply::wait_ready bats cases:
// manifest-scoped readiness, best-effort timeout, no-workload no-op.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

const waitManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  replicas: 1`

func waitApplier(getOut string) (*Applier, *fakeKubectl, *bytes.Buffer) {
	f := &fakeKubectl{getOut: getOut}
	var errOut bytes.Buffer
	a := &Applier{Runner: f, Stdout: &bytes.Buffer{}, Stderr: &errOut,
		NonInteractive: true, Sleep: func(time.Duration) {}, PollInterval: time.Nanosecond}
	return a, f, &errOut
}

func TestWaitReadyReadyDeploymentReturnsPromptly(t *testing.T) {
	a, _, errOut := waitApplier(`{"items":[{"kind":"Deployment","metadata":{"namespace":"default","name":"web"},"spec":{"replicas":1},"status":{"availableReplicas":1}}]}`)
	if err := a.WaitReady(context.Background(), "platform", 30, waitManifest); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if strings.Contains(errOut.String(), "timed out") {
		t.Errorf("unexpected timeout: %s", errOut.String())
	}
}

func TestWaitReadyTimeoutWarnsAndNamesPending(t *testing.T) {
	a, _, errOut := waitApplier(`{"items":[{"kind":"Deployment","metadata":{"namespace":"default","name":"web"},"spec":{"replicas":1},"status":{"availableReplicas":0}}]}`)
	// timeout 0 → immediate ⚠, no sleep; best-effort: never fatal.
	if err := a.WaitReady(context.Background(), "platform", 0, waitManifest); err != nil {
		t.Fatalf("WaitReady must be best-effort, got %v", err)
	}
	if !strings.Contains(errOut.String(), "platform: timed out after 0s; not ready: web") {
		t.Errorf("missing exact timeout warn: %s", errOut.String())
	}
}

func TestWaitReadyNoWorkloadsIsNoop(t *testing.T) {
	a, f, errOut := waitApplier("")
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg, namespace: default}"
	if err := a.WaitReady(context.Background(), "networking", 30, manifest); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("no-op polled the cluster: %v", f.calls)
	}
	if errOut.Len() != 0 {
		t.Errorf("no-op rendered output: %s", errOut.String())
	}
}

func TestWaitReadyDaemonSetSemantics(t *testing.T) {
	// A DaemonSet with zero desired is NOT ready (bash jq: desired > 0
	// gates readiness) — pins the ds-specific readiness branch.
	dsManifest := "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: cilium\n  namespace: kube-system"
	a, _, errOut := waitApplier(`{"items":[{"kind":"DaemonSet","metadata":{"namespace":"kube-system","name":"cilium"},"status":{"desiredNumberScheduled":0,"numberReady":0}}]}`)
	_ = a.WaitReady(context.Background(), "cilium", 0, dsManifest)
	if !strings.Contains(errOut.String(), "timed out") || !strings.Contains(errOut.String(), "cilium") {
		t.Errorf("zero-desired daemonset counted ready: %s", errOut.String())
	}

	a2, _, errOut2 := waitApplier(`{"items":[{"kind":"DaemonSet","metadata":{"namespace":"kube-system","name":"cilium"},"status":{"desiredNumberScheduled":3,"numberReady":3}}]}`)
	if err := a2.WaitReady(context.Background(), "cilium", 30, dsManifest); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if strings.Contains(errOut2.String(), "timed out") {
		t.Errorf("ready daemonset timed out: %s", errOut2.String())
	}
}

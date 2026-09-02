package operator

// capistatus_test.go — the capi-status-sync cases of
// tests/operator/hooks_test.bats (phase mapping, patch JSON, endpoint),
// plus the full trigger flow the bats never exercised.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

type statusFixture struct {
	hook   *CapiStatusSyncHook
	log    *klog
	runner *fakeRunner
	stderr *bytes.Buffer
	stdout *bytes.Buffer
}

func newStatusFixture(t *testing.T) *statusFixture {
	t.Helper()
	env := &Env{HookDir: t.TempDir(), StateDir: t.TempDir()}
	log := &klog{}
	runner := &fakeRunner{log: log}
	var stderr, stdout bytes.Buffer
	hook := &CapiStatusSyncHook{Paths: env.Paths(), Runner: runner, Stdout: &stdout, Stderr: &stderr}
	return &statusFixture{hook: hook, log: log, runner: runner, stderr: &stderr, stdout: &stdout}
}

// bats: "capi-status-sync hook::config watches CAPI Clusters with lok8s label"
func TestCapiStatusConfigPins(t *testing.T) {
	cfg := (&CapiStatusSyncHook{}).Config()
	for _, want := range []string{"cluster.x-k8s.io/v1beta1", "kind: Cluster", "lok8s.dev/managed"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q", want)
		}
	}
}

// bats: "capi-status-sync maps CAPI phases to lok8s phases"
func TestMapPhase(t *testing.T) {
	for in, want := range map[string]string{
		"Provisioned": "Provisioned", "Provisioning": "Provisioning", "Pending": "Provisioning",
		"Failed": "Failed", "Deleting": "Failed", "Unknown": "Provisioning", "": "Provisioning",
	} {
		if got := MapPhase(in); got != want {
			t.Errorf("MapPhase(%q) = %q, want %q", in, got, want)
		}
	}
}

// bats: "capi-status-sync builds correct status patch JSON" + "adds
// controlPlaneEndpoint to patch when available" — as jq prints them.
func TestBuildStatusPatch(t *testing.T) {
	status, _ := decode([]byte(`{"phase":"Provisioned","controlPlaneReady":true}`))
	got, err := BuildStatusPatch(status)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"status\": {\n    \"phase\": \"Provisioned\",\n    \"ready\": true\n  }\n}"
	if got != want {
		t.Errorf("patch:\n%s\nwant:\n%s", got, want)
	}

	status, _ = decode([]byte(`{"phase":"Provisioned","controlPlaneReady":true,"controlPlaneEndpoint":{"host":"10.0.0.1","port":6443}}`))
	got, err = BuildStatusPatch(status)
	if err != nil {
		t.Fatal(err)
	}
	want = "{\n  \"status\": {\n    \"phase\": \"Provisioned\",\n    \"ready\": true,\n    \"controlPlaneEndpoint\": {\n      \"host\": \"10.0.0.1\",\n      \"port\": 6443\n    }\n  }\n}"
	if got != want {
		t.Errorf("patch with endpoint:\n%s\nwant:\n%s", got, want)
	}

	// Defaults: null status → Unknown → Provisioning, ready false; a
	// half-present endpoint is dropped; a string port is --argjson'd to a
	// number, a numeric host --arg'd to a string.
	got, _ = BuildStatusPatch(nil)
	if !strings.Contains(got, `"phase": "Provisioning"`) || !strings.Contains(got, `"ready": false`) || strings.Contains(got, "controlPlaneEndpoint") {
		t.Errorf("null status patch:\n%s", got)
	}
	status, _ = decode([]byte(`{"phase":"Pending","controlPlaneEndpoint":{"host":"h"}}`))
	if got, _ = BuildStatusPatch(status); strings.Contains(got, "controlPlaneEndpoint") {
		t.Errorf("host without port must not add the endpoint:\n%s", got)
	}
	status, _ = decode([]byte(`{"phase":"Pending","controlPlaneEndpoint":{"host":10,"port":"6443"}}`))
	got, _ = BuildStatusPatch(status)
	if !strings.Contains(got, `"host": "10",`) || !strings.Contains(got, `"port": 6443`) {
		t.Errorf("argjson/arg coercion:\n%s", got)
	}
	// A non-JSON port is jq's --argjson failure (the bash `set -e` exit 1).
	status, _ = decode([]byte(`{"controlPlaneEndpoint":{"host":"h","port":"abc"}}`))
	if _, err := BuildStatusPatch(status); err == nil {
		t.Error("non-JSON port must fail like --argjson")
	}
}

const clusterEvent = `[{"type":"Event","object":{"metadata":{"name":"prod","namespace":"clusters"}},"filterResult":{"phase":"%s","controlPlaneReady":true,"controlPlaneEndpoint":{"host":"10.0.0.1","port":6443}}}]`

// Provisioned: patch → kubeconfig Secret → gitops/deploy → conditions.
func TestCapiStatusProvisionedFlow(t *testing.T) {
	f := newStatusFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		a := argv(c)
		switch {
		case strings.HasPrefix(a, "clusterctl get kubeconfig prod -n clusters"):
			writeOut(c, "apiVersion: v1\nkind: Config\n")
		case strings.Contains(a, "jsonpath={.spec.gitops.provider}"):
			writeOut(c, "flux")
		case strings.Contains(a, "jsonpath={.spec.cluster.domain}"):
			writeOut(c, "prod.example.com")
		case strings.HasPrefix(a, "kubectl create secret"):
			writeOut(c, "kind: Secret\n")
		}
		return nil
	}
	var gitops []string
	f.hook.GitopsBootstrap = func(ctx context.Context, domain, provider string) error {
		gitops = append(gitops, domain+" "+provider)
		return nil
	}
	f.hook.DeployApply = func(ctx context.Context, domain string) error {
		t.Error("deploy::apply must not run when gitops is configured")
		return nil
	}
	events := mustEvents(t, strings.Replace(clusterEvent, "%s", "Provisioned", 1))
	if err := f.hook.Trigger(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	patch := "{\n  \"status\": {\n    \"phase\": \"Provisioned\",\n    \"ready\": true,\n    \"controlPlaneEndpoint\": {\n      \"host\": \"10.0.0.1\",\n      \"port\": 6443\n    }\n  }\n}"
	wantSeq := []string{
		"kubectl patch capi prod -n clusters --type merge --subresource status -p " + patch,
		"clusterctl get kubeconfig prod -n clusters",
		"kubectl create secret generic prod-kubeconfig -n clusters --from-file=value=/dev/stdin --dry-run=client -o yaml",
		"kubectl apply -f -",
		`kubectl patch capi prod -n clusters --type merge --subresource status -p {"status":{"kubeconfig":{"secretRef":"prod-kubeconfig"}}}`,
		"kubectl get capi prod -n clusters -o jsonpath={.spec.gitops.provider}",
		"kubectl get capi prod -n clusters -o jsonpath={.spec.cluster.domain}",
		`kubectl patch capi prod -n clusters --type merge --subresource status -p {"status":{"gitops":{"provider":"flux","status":"Bootstrapped"}}}`,
		`kubectl patch capi prod -n clusters --type merge --subresource status -p {"status":{"conditions":[{"type":"InfrastructureReady","status":"True"},{"type":"ControlPlaneReady","status":"True"}]}}`,
	}
	if strings.Join(f.log.lines, "\n") != strings.Join(wantSeq, "\n") {
		t.Errorf("call sequence:\n%s\nwant:\n%s", f.log.text(), strings.Join(wantSeq, "\n"))
	}
	// the kubeconfig went to the create's stdin (trailing newline stripped,
	// as $(…) does), the manifest to the apply's.
	if got := f.log.stdins; len(got) != 2 || got[0] != "apiVersion: v1\nkind: Config" || got[1] != "kind: Secret\n" {
		t.Errorf("stdins = %q", got)
	}
	if len(gitops) != 1 || gitops[0] != "prod.example.com flux" {
		t.Errorf("gitops::bootstrap calls = %q", gitops)
	}
	assertStderr(t, f.stderr,
		"info: syncing CAPI Cluster status for clusters/prod: phase=Provisioned\n",
		"info: Capi cluster prod is Provisioned, running post-provision\n",
		"info: bootstrapping GitOps (flux) for prod.example.com\n",
	)
}

// No gitops → direct deploy; a failed deploy warns; a failed gitops
// bootstrap warns but still stamps the gitops status.
func TestCapiStatusDirectDeployAndWarnings(t *testing.T) {
	f := newStatusFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "jsonpath={.spec.cluster.domain}") {
			writeOut(c, "prod.example.com")
		}
		return nil // clusterctl: empty kubeconfig → no Secret
	}
	f.hook.DeployApply = func(ctx context.Context, domain string) error { return errors.New("no artifact") }
	f.hook.Trigger(context.Background(), mustEvents(t, strings.Replace(clusterEvent, "%s", "Provisioned", 1)))
	refuteHas(t, f.log, "create secret", "secretRef", `"gitops"`)
	assertHas(t, f.log, "InfrastructureReady")
	assertStderr(t, f.stderr,
		"info: no GitOps configured, direct deploy for prod.example.com\n",
		"warn: direct deploy failed for prod.example.com\n",
	)

	f = newStatusFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		a := argv(c)
		if strings.Contains(a, "jsonpath={.spec.gitops.provider}") {
			writeOut(c, "argo")
		}
		if strings.Contains(a, "jsonpath={.spec.cluster.domain}") {
			writeOut(c, "d")
		}
		return nil
	}
	f.hook.GitopsBootstrap = func(ctx context.Context, domain, provider string) error { return errors.New("nope") }
	f.hook.Trigger(context.Background(), mustEvents(t, strings.Replace(clusterEvent, "%s", "Provisioned", 1)))
	assertStderr(t, f.stderr, "warn: GitOps bootstrap failed for d\n")
	assertHas(t, f.log, `{"status":{"gitops":{"provider":"argo","status":"Bootstrapped"}}}`)

	// Neither lib wired (`declare -f` probes fail): nothing beyond the
	// info line + conditions.
	f = newStatusFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "jsonpath=") {
			writeOut(c, "x")
		}
		return nil
	}
	f.hook.Trigger(context.Background(), mustEvents(t, strings.Replace(clusterEvent, "%s", "Provisioned", 1)))
	refuteHas(t, f.log, `"gitops"`)
	assertHas(t, f.log, "InfrastructureReady")
}

// Not-yet-Provisioned phases only sync status; Synchronization is skipped;
// an unpatchable CR warns and moves on.
func TestCapiStatusNonProvisionedAndSkips(t *testing.T) {
	f := newStatusFixture(t)
	f.hook.Trigger(context.Background(), mustEvents(t, strings.Replace(clusterEvent, "%s", "Pending", 1)))
	if len(f.log.lines) != 1 || !strings.Contains(f.log.lines[0], `"phase": "Provisioning"`) {
		t.Errorf("calls = %q, want the single status patch", f.log.lines)
	}
	refuteHas(t, f.log, "clusterctl", "InfrastructureReady")

	f = newStatusFixture(t)
	f.hook.Trigger(context.Background(), mustEvents(t, `[{"type":"Synchronization","objects":[]}]`))
	if len(f.log.lines) != 0 {
		t.Errorf("Synchronization must be skipped, got %q", f.log.lines)
	}

	f = newStatusFixture(t)
	f.runner.handler = func(c execx.Cmd) error { return errors.New("no CR") }
	f.hook.Trigger(context.Background(), mustEvents(t, strings.Replace(clusterEvent, "%s", "Provisioned", 1)))
	if len(f.log.lines) != 1 {
		t.Errorf("a failed patch must `continue`, got %q", f.log.lines)
	}
	assertStderr(t, f.stderr, "warn: could not patch Capi CR prod status (CR may not exist)\n")

	// Deleting → Failed, ready mirrors controlPlaneReady (`// false`).
	f = newStatusFixture(t)
	f.hook.Trigger(context.Background(), mustEvents(t, `[{"object":{"metadata":{"name":"x"}},"filterResult":{"phase":"Deleting"}}]`))
	assertHas(t, f.log, "kubectl patch capi x -n default --type merge --subresource status -p {\n  \"status\": {\n    \"phase\": \"Failed\",\n    \"ready\": false\n  }\n}")
	assertStderr(t, f.stderr, "info: syncing CAPI Cluster status for default/x: phase=Deleting\n")
}

// A non-JSON endpoint port aborts the run (bash: jq --argjson under set -e).
func TestCapiStatusArgjsonAbort(t *testing.T) {
	f := newStatusFixture(t)
	err := f.hook.Trigger(context.Background(), mustEvents(t, `[{"object":{"metadata":{"name":"x"}},"filterResult":{"phase":"Provisioned","controlPlaneEndpoint":{"host":"h","port":"abc"}}}]`))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Errorf("err = %v, want ExitError{2} (jq --argjson usage error)", err)
	}
	assertStderr(t, f.stderr, "jq: invalid JSON text passed to --argjson")
	if len(f.log.lines) != 0 {
		t.Errorf("nothing may be patched after the abort, got %q", f.log.lines)
	}
}

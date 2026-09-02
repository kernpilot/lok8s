package capi

// capi_test.go — the port of the driver-level bats coverage:
//   - capi_provision_guards_test.bats (the #91 silent-success class on the
//     provision path),
//   - kkp_capi_destroy_guards_test.bats (capi half — do not report success
//     you did not have, do not destroy the evidence on your way out),
//   - capi_bootstrap_test.bats (clusterctl init argv, kind reuse,
//     unsupported provider, the provision→bootstrap trigger),
//   - kind_contract_test.bats (capi half — missing mgmt kubeconfig
//     diagnosis, the SaaS-mode refusal, status NotFound).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

// ── provision guards ──────────────────────────────────────

func provisionSetup(t *testing.T) (*Driver, *fakeRunner, *bytes.Buffer) {
	t.Helper()
	t.Setenv("HCLOUD_TOKEN", "test-token")
	d, runner, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", provisionGuardSpecYAML)
	writeKubeconfig(t, d, "mgmt.dev")
	runner.handler = happyHandler
	return d, runner, stderr
}

func TestProvisionHappyPath(t *testing.T) {
	// ANTI-VACUITY for the guards below: without this, 'fixing' them by
	// making provision always fail would look like a pass.
	d, runner, _ := provisionSetup(t)
	if err := d.Provision(context.Background(), "test.dev"); err != nil {
		t.Fatalf("happy path regressed: %v", err)
	}
	// The CAPI resources were applied to the management cluster…
	if !runner.anyCall("kubectl apply --kubeconfig " + d.kubeconfigPath("mgmt.dev") + " -f -") {
		t.Error("no kubectl apply against the management cluster")
	}
	// …and the workload kubeconfig was extracted under metadata.name.
	if !fileNonEmpty(d.kubeconfigPath("provtest")) {
		t.Error("workload kubeconfig missing after a successful provision")
	}
}

func TestProvisionFailedCredentialSetupDoesNotReportSuccess(t *testing.T) {
	// Without the guard the apply still SUCCEEDS (applying CRs is only
	// writing CRs), CAPH then cannot authenticate to Hetzner, and no
	// Machine is ever created — while lo provision reports a provisioned
	// cluster.
	d, runner, _ := provisionSetup(t)
	t.Setenv("HCLOUD_TOKEN", "") // break exactly the credential collaborator
	if err := d.Provision(context.Background(), "test.dev"); err == nil {
		t.Fatal("provision returned success although the credential setup FAILED")
	}
	for i, c := range runner.calls {
		if strings.Contains(argvLine(c), "apply") && strings.Contains(runner.stdins[i], "kind: Cluster") {
			t.Fatal("the CAPI resources were applied after a failed credential setup")
		}
	}
}

func TestProvisionFailedProviderDetectionDoesNotReportSuccess(t *testing.T) {
	// An empty provider flows into ensure_credentials, generate and the
	// local-mgmt bootstrap, each of which would build the wrong thing
	// rather than refuse.
	t.Setenv("HCLOUD_TOKEN", "test-token")
	d, runner, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", `kind: Capi
metadata: {name: provtest}
spec:
  managementCluster: {domain: mgmt.dev, local: false}
  cluster: {namespace: default}
`)
	writeKubeconfig(t, d, "mgmt.dev")
	runner.handler = happyHandler
	if err := d.Provision(context.Background(), "test.dev"); err == nil {
		t.Fatal("provision returned success although provider detection FAILED")
	}
	if !strings.Contains(stderr.String(), "No provider found in cluster spec") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestProvisionFailedNamespaceApplyDoesNotReportSuccess(t *testing.T) {
	// Credentials and CAPI resources would otherwise be applied into a
	// namespace that does not exist. The stub fails ONLY the namespace
	// calls and succeeds for everything after (a blanket failure would trip
	// the readyz loop instead and prove nothing — the bats hit exactly
	// that).
	d, runner, _ := provisionSetup(t)
	runner.handler = func(c execx.Cmd, stdin string) error {
		for _, a := range c.Args {
			if a == "namespace" {
				return errors.New("namespace refused")
			}
		}
		return happyHandler(c, stdin)
	}
	if err := d.Provision(context.Background(), "test.dev"); err == nil {
		t.Fatal("provision returned success although the namespace apply FAILED")
	}
}

func TestProvisionFailedConfigReadDoesNotMisdiagnoseHosted(t *testing.T) {
	// The defect was the DIAGNOSIS: with the read unguarded, the hosting
	// mode stayed unset, the != "hosted" branch was taken, and the operator
	// of a HOSTED cluster was told to set spec.managementCluster.domain —
	// advice that is wrong for their configuration. The driver must surface
	// the read's OWN error instead.
	d, _, stderr := testDriver(t)
	// No spec file at all → mgmt_domain reads empty → the read_config guard.
	d.Hooks.ReadKubehzConfig = func(clusterYAML string) (string, error) {
		return "", errors.New("cannot read cluster spec")
	}
	err := d.Provision(context.Background(), "test.dev")
	if err == nil {
		t.Fatal("provision returned success although the cluster spec could not be read")
	}
	if !strings.Contains(err.Error(), "cannot read cluster spec") {
		t.Fatalf("expected read_config's own diagnosis, got: %v", err)
	}
	if strings.Contains(stderr.String(), "managementCluster.domain is required") {
		t.Fatalf("a failed kubehz read was reported as a missing spec.managementCluster.domain:\n%s", stderr.String())
	}
}

// ── kind_contract (capi half) ─────────────────────────────

func TestProvisionFailsWithoutMgmtKubeconfig(t *testing.T) {
	d, _, stderr := testDriver(t)
	writeSpec(t, d, "prod.dev", `kind: Capi
metadata: {name: work-prod}
spec:
  kubernetes: {version: v1.31.10}
  cluster: {domain: prod.dev}
  managementCluster: {domain: mgmt.dev}
  provider: {name: hetzner}
`)
	if err := d.Provision(context.Background(), "prod.dev"); err == nil {
		t.Fatal("expected error")
	}
	out := stderr.String()
	if !strings.Contains(out, "management cluster kubeconfig not found") {
		t.Fatalf("stderr = %q", out)
	}
	if !strings.Contains(out, "provision it first ('lo provision mgmt.dev'), or set spec.managementCluster.local: true") {
		t.Fatalf("stderr = %q", out)
	}
}

func TestProvisionSaaSModeRefusal(t *testing.T) {
	// No managementCluster, kubehz not wired (nil hook = the bats' no-op
	// stub): the self-hosted refusal with its two advice lines.
	d, _, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", `kind: Capi
metadata: {name: test-saas}
spec:
  kubernetes: {version: v1.31.10}
`)
	if err := d.Provision(context.Background(), "test.dev"); err == nil {
		t.Fatal("expected error")
	}
	out := stderr.String()
	if !strings.Contains(out, "spec.managementCluster.domain is required for self-hosted CAPI") {
		t.Fatalf("stderr = %q", out)
	}
	if !strings.Contains(out, "set spec.kubehz.hosting: hosted to use the kubehz seed cluster") {
		t.Fatalf("stderr = %q", out)
	}
}

func TestProvisionHostedDelegates(t *testing.T) {
	d, runner, _ := testDriver(t)
	spec := writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: hosted}\nspec: {}\n")
	d.Hooks.ReadKubehzConfig = func(string) (string, error) { return "hosted", nil }
	called := false
	d.Hooks.ProvisionHosted = func(ctx context.Context, domain, clusterYAML string) error {
		called = true
		if domain != "test.dev" || clusterYAML != spec {
			t.Errorf("hook args: %s %s", domain, clusterYAML)
		}
		return nil
	}
	if err := d.Provision(context.Background(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("ProvisionHosted not called")
	}
	if len(runner.calls) != 0 {
		t.Fatal("hosted provisioning must not run local commands")
	}
}

// ── destroy guards (capi half of kkp_capi_destroy_guards) ─

func destroySetup(t *testing.T, local string) (*Driver, *fakeRunner, *bytes.Buffer) {
	t.Helper()
	d, runner, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", fmt.Sprintf(destroySpecYAML, local))
	writeKubeconfig(t, d, "mgmt.dev")
	writeKubeconfig(t, d, "destroytest")
	return d, runner, stderr
}

func TestDestroyFailedDeleteDoesNotReportSuccess(t *testing.T) {
	// The real failure is the --wait --timeout=600s delete giving up while
	// CAPH is still deprovisioning Hetzner servers. The driver even prints
	// "KEEPING …" — it must not then report success, or main::down
	// suppresses its orphaned-infra warning (the case the dispatch's rc
	// remap protects).
	d, runner, _ := destroySetup(t, "false")
	runner.handler = func(c execx.Cmd, stdin string) error {
		if c.Name == "kubectl" {
			return errors.New("delete timed out")
		}
		return nil
	}
	if err := d.Destroy(context.Background(), "test.dev"); err == nil {
		t.Fatal("destroy returned success although the workload cluster delete FAILED")
	}
	// Same reasoning as the kubeone guard: the failed teardown leaves live
	// infrastructure, and the kubeconfig is how an operator reaches it.
	if !fileExists(d.kubeconfigPath("destroytest")) {
		t.Fatal("the workload kubeconfig was deleted after a FAILED teardown")
	}
}

func TestDestroyMissingRemoteMgmtKubeconfigFails(t *testing.T) {
	// With the management kubeconfig gone and the management cluster REMOTE
	// (no kind-based recovery possible), the delete cannot even be
	// attempted. The old code skipped it silently, removed the workload
	// kubeconfig, and returned success.
	d, _, stderr := destroySetup(t, "false")
	os.Remove(d.kubeconfigPath("mgmt.dev"))
	if err := d.Destroy(context.Background(), "test.dev"); err == nil {
		t.Fatal("destroy returned success although no delete was ever attempted")
	}
	if !fileExists(d.kubeconfigPath("destroytest")) {
		t.Fatal("the workload kubeconfig was deleted although the destroy never ran")
	}
	if !strings.Contains(stderr.String(), "KEEPING") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDestroyLocalMgmtGoneWorkloadPresentFails(t *testing.T) {
	// The local-mgmt exemption is safe only when the workload kubeconfig is
	// gone too: mgmt kubeconfig missing + kind cluster wiped + workload
	// kubeconfig STILL on disk = a destroy that never completed.
	d, _, _ := destroySetup(t, "true")
	os.Remove(d.kubeconfigPath("mgmt.dev"))
	// kind get clusters (the recovery probe) reports nothing — default
	// handler output is empty.
	if err := d.Destroy(context.Background(), "test.dev"); err == nil {
		t.Fatal("an incomplete destroy was reported as a completed one")
	}
	if !fileExists(d.kubeconfigPath("destroytest")) {
		t.Fatal("the workload kubeconfig was deleted on the incomplete-destroy path")
	}
}

func TestDestroyLocalMgmtBothGoneStaysIdempotent(t *testing.T) {
	// Anti-vacuity companion: both kubeconfigs absent is what a COMPLETED
	// destroy looks like — a re-run of 'lo down' must keep returning 0.
	d, _, _ := destroySetup(t, "true")
	os.Remove(d.kubeconfigPath("mgmt.dev"))
	os.Remove(d.kubeconfigPath("destroytest"))
	if err := d.Destroy(context.Background(), "test.dev"); err != nil {
		t.Fatalf("a re-run of 'lo down' after a completed destroy now FAILS: %v", err)
	}
}

func TestDestroyHappyPathRemovesKubeconfig(t *testing.T) {
	d, runner, _ := destroySetup(t, "false")
	if err := d.Destroy(context.Background(), "test.dev"); err != nil {
		t.Fatalf("capi destroy happy path regressed: %v", err)
	}
	if fileExists(d.kubeconfigPath("destroytest")) {
		t.Fatal("a SUCCESSFUL destroy left the workload kubeconfig behind")
	}
	// The blocking delete argv, exactly as the bash issued it.
	want := "kubectl delete cluster destroytest --namespace default --kubeconfig " +
		d.kubeconfigPath("mgmt.dev") + " --ignore-not-found=true --wait=true --timeout=600s"
	found := false
	for _, c := range runner.calls {
		if argvLine(c) == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("blocking delete argv not issued; calls:\n%s", callDump(runner))
	}
}

func TestDestroyHostedDelegates(t *testing.T) {
	d, _, _ := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: hosted}\nspec: {}\n")
	d.Hooks.ReadKubehzConfig = func(string) (string, error) { return "hosted", nil }
	called := false
	d.Hooks.DestroyHosted = func(ctx context.Context, domain, clusterYAML string) error {
		called = true
		return nil
	}
	if err := d.Destroy(context.Background(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("DestroyHosted not called")
	}
}

func TestDestroySelfHostedWithoutMgmtDomainRefuses(t *testing.T) {
	d, _, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: x}\nspec: {}\n")
	if err := d.Destroy(context.Background(), "test.dev"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "spec.managementCluster.domain is required for self-hosted CAPI destroy") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func callDump(r *fakeRunner) string {
	var b strings.Builder
	for _, c := range r.calls {
		b.WriteString("  " + argvLine(c) + "\n")
	}
	return b.String()
}

// ── status / kubeconfig / export ──────────────────────────

func TestStatusUnknownWithoutMgmtDomain(t *testing.T) {
	d, _, _ := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: x}\nspec: {}\n")
	got, err := d.Status(context.Background(), "test.dev")
	if err != nil || got != "Unknown" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusNotFoundWhenKubectlFails(t *testing.T) {
	d, runner, _ := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: test-prod}\nspec:\n  managementCluster: {domain: mgmt.dev}\n")
	runner.handler = func(c execx.Cmd, stdin string) error { return errors.New("no cluster") }
	got, err := d.Status(context.Background(), "test.dev")
	if err != nil || got != "NotFound" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusPassesPhaseThrough(t *testing.T) {
	d, runner, _ := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: test-prod}\nspec:\n  managementCluster: {domain: mgmt.dev}\n")
	runner.handler = func(c execx.Cmd, stdin string) error {
		c.Stdout.Write([]byte("Provisioned"))
		return nil
	}
	got, err := d.Status(context.Background(), "test.dev")
	if err != nil || got != "Provisioned" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestKubeconfigPathUsesMetadataName(t *testing.T) {
	d, _, _ := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: test-prod}\nspec: {}\n")
	got, err := d.Kubeconfig(context.Background(), "test.dev")
	if err != nil || got != d.kubeconfigPath("test-prod") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestExportSetsCCMNetworkForHetznerNetworking(t *testing.T) {
	t.Setenv("HCLOUD_CCM_NETWORK", "")
	os.Unsetenv("HCLOUD_CCM_NETWORK")
	d, _, _ := testDriver(t)
	writeSpec(t, d, "test.dev", `kind: Capi
metadata: {name: netcluster}
spec:
  provider:
    name: hetzner
    config:
      network: {enabled: true}
`)
	if err := d.Export(context.Background(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("HCLOUD_CCM_NETWORK"); got != "netcluster" {
		t.Fatalf("HCLOUD_CCM_NETWORK = %q", got)
	}
}

func TestExportLeavesEnvAloneWithoutNetworking(t *testing.T) {
	t.Setenv("HCLOUD_CCM_NETWORK", "")
	os.Unsetenv("HCLOUD_CCM_NETWORK")
	d, _, _ := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: x}\nspec:\n  provider: {name: hetzner}\n")
	if err := d.Export(context.Background(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv("HCLOUD_CCM_NETWORK"); set {
		t.Fatal("HCLOUD_CCM_NETWORK exported without networking mode")
	}
}

// ── bootstrap ─────────────────────────────────────────────

func bootstrapSpec(t *testing.T, d *Driver) {
	t.Helper()
	writeSpec(t, d, "mgmt.lok8s.dev", `apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: mgmt-production
spec:
  kubernetes: {version: v1.31.10}
  cluster: {domain: mgmt.lok8s.dev, namespace: capi-system}
  managementCluster: {domain: mgmt.lok8s.dev}
  credentials: {secretName: mgmt-credentials}
  provider:
    name: hetzner
    config: {region: fsn1, sshKeyName: admin-key}
  controlPlane: {replicas: 1, type: cax21}
`)
}

func TestBootstrapRunsClusterctlInitWithProvider(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "test-token")
	d, runner, stderr := testDriver(t)
	bootstrapSpec(t, d)
	runner.handler = happyHandler
	if err := d.Bootstrap(context.Background(), "mgmt.lok8s.dev"); err != nil {
		t.Fatalf("bootstrap failed: %v\nstderr:\n%s", err, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{
		"info: creating bootstrap cluster",
		"info: installing CAPI on bootstrap cluster",
		"info: cleaning up bootstrap cluster",
		"info: management cluster mgmt.lok8s.dev bootstrapped successfully",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q", want)
		}
	}
	// clusterctl init was called with --infrastructure hetzner.
	initSeen := false
	for _, c := range runner.calls {
		line := argvLine(c)
		if strings.HasPrefix(line, "clusterctl init") && strings.Contains(line, "--infrastructure hetzner") {
			initSeen = true
		}
	}
	if !initSeen {
		t.Fatalf("clusterctl init --infrastructure hetzner not issued:\n%s", callDump(runner))
	}
	// The bootstrap kind cluster was created and deleted.
	if !runner.anyCall("kind create cluster --name lok8s-bootstrap") {
		t.Error("bootstrap kind cluster never created")
	}
	if !runner.anyCall("kind delete cluster --name lok8s-bootstrap") {
		t.Error("bootstrap kind cluster never cleaned up")
	}
}

func TestBootstrapReusesExistingKindCluster(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "test-token")
	d, runner, _ := testDriver(t)
	bootstrapSpec(t, d)
	runner.handler = func(c execx.Cmd, stdin string) error {
		if c.Name == "kind" && len(c.Args) > 1 && c.Args[0] == "get" && c.Args[1] == "clusters" {
			fmt.Fprintln(c.Stdout, "lok8s-bootstrap")
			return nil
		}
		if c.Name == "kind" && c.Args[0] == "create" {
			t.Error("kind create called although the bootstrap cluster exists")
		}
		return happyHandler(c, stdin)
	}
	if err := d.Bootstrap(context.Background(), "mgmt.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapFailsForUnsupportedProvider(t *testing.T) {
	d, _, stderr := testDriver(t)
	writeSpec(t, d, "gcp.lok8s.dev", `kind: Capi
metadata: {name: test-gcp}
spec:
  kubernetes: {version: v1.31.10}
  cluster: {domain: gcp.lok8s.dev}
  managementCluster: {domain: gcp.lok8s.dev}
  gcp: {region: us-central1}
`)
	if err := d.Bootstrap(context.Background(), "gcp.lok8s.dev"); err == nil {
		t.Fatal("expected error")
	}
	// The RAW `error:` family (echo >&2), not the colored [error] helper.
	if !strings.Contains(stderr.String(), "error: unsupported provider for bootstrap: ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBootstrapKeepsBootstrapClusterOnFailedMove(t *testing.T) {
	// A failed move MUST keep the bootstrap cluster — it still owns the
	// CAPI resources; deleting it would destroy the only record of the
	// just-provisioned infrastructure.
	t.Setenv("HCLOUD_TOKEN", "test-token")
	d, runner, stderr := testDriver(t)
	bootstrapSpec(t, d)
	runner.handler = func(c execx.Cmd, stdin string) error {
		if c.Name == "clusterctl" && c.Args[0] == "move" {
			return errors.New("move failed")
		}
		return happyHandler(c, stdin)
	}
	if err := d.Bootstrap(context.Background(), "mgmt.lok8s.dev"); err == nil {
		t.Fatal("expected error")
	}
	if runner.anyCall("kind delete cluster --name lok8s-bootstrap") {
		t.Fatal("the bootstrap cluster was deleted after a FAILED clusterctl move")
	}
	if !strings.Contains(stderr.String(), "error: clusterctl move failed — bootstrap cluster lok8s-bootstrap kept (it still owns the CAPI resources); re-run bootstrap after fixing") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestProvisionTriggersBootstrapWhenSelfManaging(t *testing.T) {
	// domain == managementCluster.domain and no kubeconfig on disk → the
	// provision boots the management cluster itself.
	t.Setenv("HCLOUD_TOKEN", "test-token")
	d, runner, stderr := testDriver(t)
	bootstrapSpec(t, d)
	runner.handler = happyHandler
	if err := d.Provision(context.Background(), "mgmt.lok8s.dev"); err != nil {
		t.Fatalf("provision failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "info: bootstrapping management cluster mgmt.lok8s.dev") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !runner.anyCall("clusterctl move") {
		t.Fatal("bootstrap flow never reached clusterctl move")
	}
}

// ── mgmt kind name ────────────────────────────────────────

func TestMgmtKindName(t *testing.T) {
	if got := MgmtKindName("capi-mgmt.lok8s.dev"); got != "capi-mgmt-lok8s-dev" {
		t.Fatalf("got %q", got)
	}
	if got := MgmtKindName("a_b.c"); got != "a-b-c" {
		t.Fatalf("got %q", got)
	}
}

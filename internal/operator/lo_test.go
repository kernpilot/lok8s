package operator

// lo_test.go — the lo-reconcile cases of tests/operator/hooks_test.bats,
// ported one-to-one (same CR fixtures, same KLOG assertions), plus the
// exact call-sequence pins the bats could only spot-check.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

const loCR = `{"metadata":{"name":"test-lo","namespace":"default","finalizers":[]},"spec":{"cluster":{"domain":"test.lok8s.dev"},"runtime":"kind"}}`

const loDeletingCR = `{"metadata":{"name":"test-lo","namespace":"default","deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":["lok8s.dev/lo-teardown"]},"spec":{"cluster":{"domain":"test.lok8s.dev"}}}`

// bats: "lo-reconcile hook::config: events, synchronization, drift schedule"
func TestLoConfigPins(t *testing.T) {
	cfg := (&LoHook{}).Config()
	for _, want := range []string{
		"kind: Lo", `"Added", "Modified"`, "executeHookOnSynchronization: true",
		`crontab: "*/3 * * * *"`, "deletionTimestamp",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q", want)
		}
	}
}

// bats: "lo-reconcile: missing domain marks Failed"
func TestLoMissingDomainMarksFailed(t *testing.T) {
	f := newLoFixture(t)
	if err := f.hook.Reconcile(context.Background(), []byte(`{"metadata":{"name":"bad","namespace":"default"},"spec":{}}`)); err != nil {
		t.Fatal(err)
	}
	assertHas(t, f.log, "MissingDomain")
	refuteHas(t, f.log, "driver::provision")
	// The exact patch line (hook::patch_status argv + body).
	want := `kubectl patch lo bad -n default --type merge --subresource status -p {"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"MissingDomain","message":"spec.cluster.domain is required"}]}}`
	if got := f.log.lines; len(got) != 1 || got[0] != want {
		t.Errorf("calls = %q, want exactly [%q]", got, want)
	}
}

// bats: "lo-reconcile: fresh CR gets finalizer, provision, bootstrap,
// kubeconfig, Provisioned"
func TestLoFreshCRProvisions(t *testing.T) {
	f := newLoFixture(t)
	if err := f.hook.Reconcile(context.Background(), []byte(loCR)); err != nil {
		t.Fatal(err)
	}
	assertHas(t, f.log,
		"/metadata/finalizers",
		"driver::provision test.lok8s.dev",
		"bootstrap::apply test.lok8s.dev",
		`"phase":"Provisioned"`,
		"create secret generic test-lo-kubeconfig",
		`"secretRef":"test-lo-kubeconfig"`,
	)
	// spec materialized where the driver contract expects it
	spec := filepath.Join(f.paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	if !fileExists(spec) {
		t.Fatalf("spec not materialized at %s", spec)
	}
	// … as block YAML (yq -P), key order preserved.
	want := "metadata:\n  name: test-lo\n  namespace: default\n  finalizers: []\nspec:\n  cluster:\n    domain: test.lok8s.dev\n  runtime: kind\n"
	if got := readFileT(t, spec); got != want {
		t.Errorf("materialized spec:\n%s\nwant:\n%s", got, want)
	}
	assertStderr(t, f.stderr,
		"info: reconciling Lo default/test-lo (test.lok8s.dev)\n",
		"info: Lo default/test-lo provisioned\n",
	)

	// The full ordered sequence — every kubectl argv verbatim from the bash.
	kubeconfig := filepath.Join(f.paths.Base, ".kubeconfig", "test-lo.yaml")
	wantSeq := []string{
		`kubectl patch lo test-lo -n default --type json -p [{"op":"add","path":"/metadata/finalizers/-","value":"lok8s.dev/lo-teardown"}]`,
		"driver::status test.lok8s.dev",
		`kubectl patch lo test-lo -n default --type merge --subresource status -p {"status":{"phase":"Provisioning","ready":false}}`,
		"driver::provision test.lok8s.dev",
		"bootstrap::apply test.lok8s.dev " + spec + " " + kubeconfig,
		"driver::kubeconfig test.lok8s.dev",
		"kubectl create secret generic test-lo-kubeconfig -n default --from-file=value=" + kubeconfig + " --dry-run=client -o yaml",
		"kubectl apply -f -",
		`kubectl patch lo test-lo -n default --type merge --subresource status -p {"status":{"kubeconfig":{"secretRef":"test-lo-kubeconfig"}}}`,
		`kubectl patch lo test-lo -n default --type merge --subresource status -p {"status":{"phase":"Provisioned","ready":true,"conditions":[{"type":"Ready","status":"True","reason":"Provisioned","message":"Cluster is ready"}]}}`,
	}
	if !reflect.DeepEqual(f.log.lines, wantSeq) {
		t.Errorf("call sequence:\n%s\nwant:\n%s", f.log.text(), strings.Join(wantSeq, "\n"))
	}
}

// The finalizer append form fails when the array does not exist yet → the
// create-the-array form; both failing → the warn line.
func TestLoEnsureFinalizerFallsBack(t *testing.T) {
	f := newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "/metadata/finalizers/-") {
			return errors.New("no array")
		}
		return loKubectl(c)
	}
	f.hook.Reconcile(context.Background(), []byte(loCR))
	assertHas(t, f.log, `--type json -p [{"op":"add","path":"/metadata/finalizers","value":["lok8s.dev/lo-teardown"]}]`)
	if strings.Contains(f.stderr.String(), "warn: failed to add finalizer") {
		t.Error("fallback succeeded; no warn expected")
	}

	f = newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "--type json") {
			return errors.New("denied")
		}
		return loKubectl(c)
	}
	f.hook.Reconcile(context.Background(), []byte(loCR))
	assertStderr(t, f.stderr, "warn: failed to add finalizer to Lo default/test-lo\n")
}

// A CR that already carries the finalizer is not patched again.
func TestLoFinalizerPresentSkipsPatch(t *testing.T) {
	f := newLoFixture(t)
	cr := `{"metadata":{"name":"test-lo","namespace":"default","finalizers":["lok8s.dev/lo-teardown"]},"spec":{"cluster":{"domain":"test.lok8s.dev"}}}`
	f.hook.Reconcile(context.Background(), []byte(cr))
	refuteHas(t, f.log, "--type json")
}

// bats: "lo-reconcile: running cluster skips provision, syncs status"
func TestLoRunningClusterSkipsProvision(t *testing.T) {
	f := newLoFixture(t)
	f.drv.status = "Running"
	f.hook.Reconcile(context.Background(), []byte(loCR))
	refuteHas(t, f.log, "driver::provision")
	assertHas(t, f.log, `"phase":"Provisioned"`, "create secret generic test-lo-kubeconfig")
}

// A kubeconfig already on disk is published without driver::kubeconfig.
func TestLoPublishReusesExistingKubeconfig(t *testing.T) {
	f := newLoFixture(t)
	f.drv.status = "Running"
	writeFile(t, filepath.Join(f.paths.Base, ".kubeconfig", "test-lo.yaml"), "kc\n")
	f.hook.Reconcile(context.Background(), []byte(loCR))
	refuteHas(t, f.log, "driver::kubeconfig")
	assertHas(t, f.log, "create secret generic test-lo-kubeconfig")
}

// bats: "lo-reconcile: deletion runs teardown and removes finalizer"
func TestLoDeletionTearsDown(t *testing.T) {
	f := newLoFixture(t)
	f.hook.Reconcile(context.Background(), []byte(loDeletingCR))
	assertHas(t, f.log,
		`"phase":"Terminating"`,
		"driver::destroy test.lok8s.dev",
		`{"metadata":{"finalizers":[]}}`,
		"kubectl delete secret test-lo-kubeconfig -n default --ignore-not-found=true",
		"kubectl get lo test-lo -n default -o jsonpath={.metadata.finalizers}",
	)
	if _, err := os.Stat(filepath.Join(f.paths.Clusters, "test.lok8s.dev")); !os.IsNotExist(err) {
		t.Error("materialized spec dir must be removed after teardown")
	}
	assertStderr(t, f.stderr, "info: Lo default/test-lo torn down\n")
	refuteHas(t, f.log, "driver::provision", "driver::status")
}

// bats: "lo-reconcile: failed teardown keeps finalizer for retry"
func TestLoFailedTeardownKeepsFinalizer(t *testing.T) {
	f := newLoFixture(t)
	f.drv.destroy = errors.New("boom")
	f.hook.Reconcile(context.Background(), []byte(loDeletingCR))
	assertHas(t, f.log, "DestroyFailed")
	refuteHas(t, f.log, `{"metadata":{"finalizers":[]}}`, "delete secret")
	assertStderr(t, f.stderr, "error: Lo default/test-lo teardown failed (will retry)\n")
	if !fileExists(filepath.Join(f.paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")) {
		t.Error("spec must survive a failed teardown (the retry needs it)")
	}
}

// Deletion WITHOUT our finalizer is a no-op (nothing to tear down).
func TestLoDeletionWithoutFinalizerIsNoop(t *testing.T) {
	f := newLoFixture(t)
	cr := `{"metadata":{"name":"test-lo","namespace":"default","deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":["other"]},"spec":{"cluster":{"domain":"test.lok8s.dev"}}}`
	f.hook.Reconcile(context.Background(), []byte(cr))
	refuteHas(t, f.log, "kubectl", "driver::")
}

// remove_finalizer's `|| echo '[]'` fallback: a failed jsonpath read still
// patches an empty list; an empty read yields the malformed bash patch.
func TestLoRemoveFinalizerPipelineSemantics(t *testing.T) {
	f := newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "jsonpath=") {
			return errors.New("not found")
		}
		return nil
	}
	f.hook.Reconcile(context.Background(), []byte(loDeletingCR))
	assertHas(t, f.log, `--type merge -p {"metadata":{"finalizers":[]}}`)

	f = newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "jsonpath=") {
			writeOut(c, `["lok8s.dev/lo-teardown","keep"]`)
		}
		return nil
	}
	f.hook.Reconcile(context.Background(), []byte(loDeletingCR))
	assertHas(t, f.log, `--type merge -p {"metadata":{"finalizers":["keep"]}}`)

	f = newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "--type merge -p") {
			return errors.New("malformed")
		}
		return nil // jsonpath: success, EMPTY output
	}
	f.hook.Reconcile(context.Background(), []byte(loDeletingCR))
	assertHas(t, f.log, `--type merge -p {"metadata":{"finalizers":}}`)
	assertStderr(t, f.stderr, "warn: failed to remove finalizer from Lo default/test-lo\n")
}

// driver::provision / bootstrap::apply failures → ProvisionFailed.
func TestLoProvisionFailures(t *testing.T) {
	f := newLoFixture(t)
	f.drv.provision = errors.New("kind exploded")
	f.hook.Reconcile(context.Background(), []byte(loCR))
	assertHas(t, f.log, "ProvisionFailed")
	refuteHas(t, f.log, "bootstrap::apply", `"phase":"Provisioned"`, "create secret")
	assertStderr(t, f.stderr, "error: Lo default/test-lo provisioning failed\n")

	f = newLoFixture(t)
	f.hook.BootstrapApply = func(ctx context.Context, domain, clusterYAML, kubeconfig string) error {
		return errors.New("cilium failed")
	}
	f.hook.Reconcile(context.Background(), []byte(loCR))
	assertHas(t, f.log, "driver::provision test.lok8s.dev", "ProvisionFailed")
	refuteHas(t, f.log, `"phase":"Provisioned"`)

	// No bootstrap lib wired = the bash "command not found" → failed.
	f = newLoFixture(t)
	f.hook.BootstrapApply = nil
	f.hook.Reconcile(context.Background(), []byte(loCR))
	assertHas(t, f.log, "ProvisionFailed")
	assertStderr(t, f.stderr, "bootstrap::apply: command not found")
}

// A failed status patch is logged, never masked (hook::patch_status).
func TestLoPatchStatusWarns(t *testing.T) {
	f := newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.Contains(argv(c), "--subresource status") {
			return errors.New("conflict")
		}
		return loKubectl(c)
	}
	f.hook.Reconcile(context.Background(), []byte(loCR))
	assertStderr(t, f.stderr, "warn: failed to patch lo default/test-lo status\n")
	// … and the flow continues to the end regardless.
	assertHas(t, f.log, `"phase":"Provisioned"`)
}

// bats: "lo-reconcile: schedule event re-lists all Lo resources"
func TestLoScheduleRelists(t *testing.T) {
	f := newLoFixture(t)
	events := mustEvents(t, `[{"type": "Schedule", "binding": "lo-drift"}]`)
	if err := f.hook.Trigger(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	assertHas(t, f.log, "kubectl get lo -A -o json")
	refuteHas(t, f.log, "driver::")
}

// The re-list converges every item; Synchronization behaves like Schedule;
// a plain event reconciles its object.
func TestLoTriggerRoutes(t *testing.T) {
	f := newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.HasPrefix(argv(c), "kubectl get lo -A -o json") {
			writeOut(c, `{"items":[`+loCR+`,{"metadata":{"name":"bad","namespace":"ns"},"spec":{}}]}`)
			return nil
		}
		return loKubectl(c)
	}
	events := mustEvents(t, `[{"type":"Synchronization"},{"type":"Event","object":{"metadata":{"name":"evt","namespace":"default"},"spec":{}}},{"object":`+loCR+`}]`)
	if err := f.hook.Trigger(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	assertHas(t, f.log,
		"kubectl patch lo test-lo -n default --type json",
		"kubectl patch lo bad -n ns --type merge --subresource status",
		"kubectl patch lo evt -n default --type merge --subresource status",
	)
	if n := len(f.log.matching("driver::provision test.lok8s.dev")); n != 2 {
		t.Errorf("driver::provision ran %d times, want 2 (re-list item + typeless event)", n)
	}
}

// A failed / empty re-list runs nothing (jq on empty input → zero items).
func TestLoRelistFailureIsQuiet(t *testing.T) {
	f := newLoFixture(t)
	f.runner.handler = func(c execx.Cmd) error { return errors.New("api down") }
	f.hook.Trigger(context.Background(), mustEvents(t, `[{"type":"Schedule"}]`))
	if len(f.log.lines) != 1 {
		t.Errorf("calls = %q, want only the re-list", f.log.lines)
	}
}

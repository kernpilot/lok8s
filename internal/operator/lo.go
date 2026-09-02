package operator

// lo.go — `lo operator lo-reconcile`: the Lo CRD lifecycle hook
// (.lok8s/legacy/operator/hooks/lo-reconcile.sh). Creation, idempotent
// convergence, kubeconfig publication, drift detection on a schedule, and
// finalizer-guarded teardown — through the same driver contract the lo CLI
// uses (internal/driver/lo instead of the sourced drivers/lo/main).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

// LoFinalizer guards Lo deletion until the driver tore the cluster down.
const LoFinalizer = "lok8s.dev/lo-teardown"

// loConfig is hook::config, verbatim (testdata/lo-reconcile.config.yaml).
const loConfig = `configVersion: v1
kubernetes:
  - apiVersion: cluster.lok8s.dev/v1beta1
    kind: Lo
    executeHookOnEvent: ["Added", "Modified"]
    executeHookOnSynchronization: true
    jqFilter: "{spec: .spec, metadata: {name: .metadata.name, namespace: .metadata.namespace, deletionTimestamp: .metadata.deletionTimestamp, finalizers: .metadata.finalizers}}"
schedule:
  - name: lo-drift
    crontab: "*/3 * * * *"
`

// LoHook reconciles Lo CRs.
type LoHook struct {
	Paths  *config.Paths
	Runner execx.Runner
	Stdout io.Writer
	Stderr io.Writer

	// Drivers overrides the driver lookup (nil → driver.Get). The bash
	// sourced drivers/lo/main unconditionally — the hook is the Lo
	// reconciler, so the kind is fixed: "lo".
	Drivers func(name string) (driver.Factory, bool)

	// BootstrapApply is bootstrap::apply (bootstrap.ApplyHook in the CLI).
	// nil = the lib is not loaded: the bash `driver::provision &&
	// bootstrap::apply` then fails on "command not found" and the CR lands
	// in ProvisionFailed — same here.
	BootstrapApply func(ctx context.Context, domain, clusterYAML, kubeconfig string) error

	drv driver.Driver
}

func (h *LoHook) stdout() io.Writer {
	if h.Stdout != nil {
		return h.Stdout
	}
	return os.Stdout
}

func (h *LoHook) stderr() io.Writer {
	if h.Stderr != nil {
		return h.Stderr
	}
	return os.Stderr
}

func (h *LoHook) kube() *kube {
	return &kube{runner: h.Runner, stdout: h.stdout(), stderr: h.stderr()}
}

// Config implements Hook.
func (h *LoHook) Config() string { return loConfig }

// driver resolves the lo driver once per process (the bash sourced it once
// at the top of the hook).
func (h *LoHook) driver() (driver.Driver, error) {
	if h.drv != nil {
		return h.drv, nil
	}
	lookup := h.Drivers
	if lookup == nil {
		lookup = driver.Get
	}
	factory, ok := lookup("lo")
	if !ok {
		return nil, errorf("driver::provision: command not found (lo driver not registered)")
	}
	drv, err := factory(&driver.Deps{Paths: h.Paths, Runner: h.Runner, Stderr: h.stderr()})
	if err != nil {
		return nil, err
	}
	h.drv = drv
	return drv, nil
}

func (h *LoHook) clusterYAML(domain string) string {
	return filepath.Join(h.Paths.Clusters, domain, "cluster.lok8s.yaml")
}

// materializeSpec writes the CR as a cluster spec where the driver contract
// expects it (lo_hook::materialize_spec): `echo "$json" | yq -P '.' >
// $PATH_CLUSTERS/<domain>/cluster.lok8s.yaml` — JSON re-emitted as block
// YAML, key order preserved.
func (h *LoHook) materializeSpec(domain string, object []byte) error {
	dir := filepath.Join(h.Paths.Clusters, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(object, &node); err != nil {
		return err
	}
	blockStyle(&node)
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cluster.lok8s.yaml"), []byte(buf.String()), 0o644)
}

// blockStyle clears the flow style yaml.v3 records for JSON input, so the
// re-encode is block YAML like `yq -P`. Scalars keep their tag (a JSON
// "123" stays a quoted string).
func blockStyle(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		n.Style = 0
	}
	if n.Kind == yaml.ScalarNode && n.Style == yaml.DoubleQuotedStyle {
		n.Style = 0
	}
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// clusterName reads .metadata.name from the materialized spec with yq -r
// semantics (missing → "null", unreadable → "").
func (h *LoHook) clusterName(domain string) string {
	raw, err := os.ReadFile(h.clusterYAML(domain))
	if err != nil {
		return ""
	}
	var doc struct {
		Metadata struct {
			Name *string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	if doc.Metadata.Name == nil {
		return "null"
	}
	return *doc.Metadata.Name
}

func (h *LoHook) kubeconfigPath(domain string) string {
	return filepath.Join(h.Paths.Base, ".kubeconfig", h.clusterName(domain)+".yaml")
}

// publishKubeconfig publishes the cluster's kubeconfig as Secret
// <name>-kubeconfig and references it from status
// (lo_hook::publish_kubeconfig). The create|apply pipeline's status is
// ignored by the bash (errexit is off at every call site); the status patch
// runs regardless.
func (h *LoHook) publishKubeconfig(ctx context.Context, name, namespace, domain string) error {
	k := h.kube()
	kubeconfig := h.kubeconfigPath(domain)
	if !fileExists(kubeconfig) {
		drv, err := h.driver()
		if err != nil {
			return err
		}
		if _, err := drv.Kubeconfig(ctx, domain); err != nil {
			return err
		}
	}

	var manifest strings.Builder
	_ = k.run(ctx, nil, &manifest, nil, "create", "secret", "generic", name+"-kubeconfig",
		"-n", namespace, "--from-file=value="+kubeconfig, "--dry-run=client", "-o", "yaml")
	_ = k.run(ctx, strings.NewReader(manifest.String()), io.Discard, nil, "apply", "-f", "-")

	k.patchStatus(ctx, "lo", name, namespace, `{"status":{"kubeconfig":{"secretRef":"`+name+`-kubeconfig"}}}`)
	return nil
}

// teardown is lo_hook::teardown: Terminating → driver::destroy → on
// success drop the kubeconfig Secret, the materialized spec dir and the
// finalizer; on failure keep the finalizer so the schedule binding retries.
func (h *LoHook) teardown(ctx context.Context, name, namespace, domain string) {
	k := h.kube()
	stderr := h.stderr()

	k.patchStatus(ctx, "lo", name, namespace, `{"status":{"phase":"Terminating","ready":false}}`)

	destroyed := false
	if drv, err := h.driver(); err == nil {
		destroyed = drv.Destroy(ctx, domain) == nil
	} else {
		fmt.Fprintf(stderr, "error: %v\n", err)
	}

	if destroyed {
		_ = k.run(ctx, nil, io.Discard, io.Discard, "delete", "secret", name+"-kubeconfig", "-n", namespace, "--ignore-not-found=true")
		os.RemoveAll(filepath.Join(h.Paths.Clusters, domain))
		k.removeFinalizer(ctx, "lo", "Lo", name, namespace, LoFinalizer)
		fmt.Fprintf(stderr, "info: Lo %s/%s torn down\n", namespace, name)
		return
	}
	// keep the finalizer: the schedule binding retries
	k.patchStatus(ctx, "lo", name, namespace,
		`{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"DestroyFailed","message":"driver::destroy failed; will retry"}]}}`)
	fmt.Fprintf(stderr, "error: Lo %s/%s teardown failed (will retry)\n", namespace, name)
}

// provision is lo_hook::provision: Provisioning → driver::provision &&
// bootstrap::apply → publish kubeconfig → Provisioned, else Failed.
func (h *LoHook) provision(ctx context.Context, name, namespace, domain string) {
	k := h.kube()
	stderr := h.stderr()

	k.patchStatus(ctx, "lo", name, namespace, `{"status":{"phase":"Provisioning","ready":false}}`)

	clusterYAML := h.clusterYAML(domain)
	kubeconfig := h.kubeconfigPath(domain)

	ok := false
	if drv, err := h.driver(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
	} else if err := drv.Provision(ctx, domain); err == nil {
		if h.BootstrapApply == nil {
			fmt.Fprintln(stderr, "lo-reconcile: bootstrap::apply: command not found")
		} else if err := h.BootstrapApply(ctx, domain, clusterYAML, kubeconfig); err == nil {
			ok = true
		}
	}

	if ok {
		_ = h.publishKubeconfig(ctx, name, namespace, domain)
		k.patchStatus(ctx, "lo", name, namespace,
			`{"status":{"phase":"Provisioned","ready":true,"conditions":[{"type":"Ready","status":"True","reason":"Provisioned","message":"Cluster is ready"}]}}`)
		fmt.Fprintf(stderr, "info: Lo %s/%s provisioned\n", namespace, name)
		return
	}
	k.patchStatus(ctx, "lo", name, namespace,
		`{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"ProvisionFailed","message":"Provisioning failed"}]}}`)
	fmt.Fprintf(stderr, "error: Lo %s/%s provisioning failed\n", namespace, name)
}

// Reconcile converges one Lo object (full JSON) toward its spec
// (lo_hook::reconcile). It never fails: the bash body ran with errexit
// suspended and its last statement always returned 0 — the caller's
// "warn: reconcile failed" line is unreachable there too.
func (h *LoHook) Reconcile(ctx context.Context, object []byte) error {
	k := h.kube()
	stderr := h.stderr()

	obj, err := decode(object)
	if err != nil {
		// jq would have failed on every read; the names come back as the
		// jq -r "null"/default and the domain empty → MissingDomain.
		obj = nil
	}
	name := jqR(get(obj, "metadata", "name"))
	namespace := jqR(alt(get(obj, "metadata", "namespace"), "default"))
	domain := jqEmpty(get(obj, "spec", "cluster", "domain"))
	deletion := jqEmpty(get(obj, "metadata", "deletionTimestamp"))
	finalizers := alt(get(obj, "metadata", "finalizers"), []any{})

	if domain == "" {
		k.patchStatus(ctx, "lo", name, namespace,
			`{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"MissingDomain","message":"spec.cluster.domain is required"}]}}`)
		return nil
	}

	fmt.Fprintf(stderr, "info: reconciling Lo %s/%s (%s)\n", namespace, name, domain)
	if err := h.materializeSpec(domain, object); err != nil {
		// bash: yq's own error on stderr, then carry on (errexit is off).
		fmt.Fprintf(stderr, "error: materializing %s: %v\n", h.clusterYAML(domain), err)
	}

	if deletion != "" {
		if contains(finalizers, LoFinalizer) {
			h.teardown(ctx, name, namespace, domain)
		}
		return nil
	}

	k.ensureFinalizer(ctx, "lo", "Lo", name, namespace, finalizers, LoFinalizer)

	// state=$(driver::status "$domain" 2>/dev/null || echo "Unknown")
	state := "Unknown"
	if drv, err := h.driver(); err == nil {
		if s, err := drv.Status(ctx, domain); err == nil {
			state = s
		}
	}
	if state == "Running" {
		_ = h.publishKubeconfig(ctx, name, namespace, domain)
		k.patchStatus(ctx, "lo", name, namespace,
			`{"status":{"phase":"Provisioned","ready":true,"conditions":[{"type":"Ready","status":"True","reason":"Provisioned","message":"Cluster is ready"}]}}`)
		return nil
	}

	h.provision(ctx, name, namespace, domain)
	return nil
}

// reconcileAll re-lists every Lo CR and converges — drift detection +
// teardown retry (lo_hook::reconcile_all).
func (h *LoHook) reconcileAll(ctx context.Context) {
	for j, item := range h.kube().listAll(ctx, "lo") {
		raw, _ := json.Marshal(item)
		if err := h.Reconcile(ctx, raw); err != nil {
			fmt.Fprintf(h.stderr(), "warn: reconcile failed for item %d\n", j)
		}
	}
}

// Trigger implements Hook (hook::trigger): Schedule/Synchronization events
// re-list everything, any other event reconciles its object.
func (h *LoHook) Trigger(ctx context.Context, events []Event) error {
	for i, ev := range events {
		switch ev.EventType() {
		case "Schedule", "Synchronization":
			h.reconcileAll(ctx)
		default:
			if err := h.Reconcile(ctx, eventObject(ev)); err != nil {
				fmt.Fprintf(h.stderr(), "warn: reconcile failed for event %d\n", i)
			}
		}
	}
	return nil
}

// eventObject is `jq -c ".[i].object"`: a missing object reads as null.
func eventObject(ev Event) []byte {
	if len(ev.Object) == 0 {
		return []byte("null")
	}
	return ev.Object
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

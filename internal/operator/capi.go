package operator

// capi.go — `lo operator capi-reconcile`: the Capi CRD reconciler
// (.lok8s/legacy/operator/hooks/capi-reconcile.sh). Detects the CAPI
// provider from the CR spec, renders the CAPI resources from the
// capi-templates tree with a bare envsubst, applies them to the management
// cluster; finalizer-guarded teardown deletes the CAPI Cluster.
//
// The render is the HOOK's own (not the capi driver's Generate): it reads
// the CR spec JSON, walks core/*.yaml + providers/<p>/*.yaml as globs, and
// substitutes with an unrestricted envsubst — every quirk of that (see the
// per-step comments) is reproduced, bash wins.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// CapiFinalizer intercepts deletion so the workload cluster is torn down
// before the Capi CR vanishes (otherwise the applied CAPI Cluster + machines
// + infra are orphaned and keep the cloud cluster — and its bill — alive).
const CapiFinalizer = "lok8s.dev/capi-teardown"

// capiConfig is hook::config, verbatim (testdata/capi-reconcile.config.yaml).
const capiConfig = `configVersion: v1
kubernetes:
  - apiVersion: cluster.lok8s.dev/v1beta1
    kind: Capi
    executeHookOnEvent: ["Added", "Modified"]
    executeHookOnSynchronization: true
    jqFilter: "{spec: .spec, metadata: {name: .metadata.name, namespace: .metadata.namespace, deletionTimestamp: .metadata.deletionTimestamp, finalizers: .metadata.finalizers}}"
schedule:
  - name: capi-drift
    crontab: "*/3 * * * *"
`

// CapiHook reconciles Capi CRs.
type CapiHook struct {
	Paths  *config.Paths
	Runner execx.Runner
	Stdout io.Writer
	Stderr io.Writer

	// TemplateDir is the CAPI template tree (bash: ${HOOK_DIR}/capi-templates;
	// Env.CapiTemplateDir in the CLI).
	TemplateDir string

	// exported is the `export` set one render builds up. The bash ran the
	// render inside `resources=$(capi::generate_from_spec …)` — a subshell
	// — so the exports never outlived one CR's render (measured: a second
	// CR in the same batch renders with EMPTY POOL_*, not the first CR's).
	// Reset per generate for the same scoping.
	exported map[string]string
}

func (h *CapiHook) stdout() io.Writer {
	if h.Stdout != nil {
		return h.Stdout
	}
	return os.Stdout
}

func (h *CapiHook) stderr() io.Writer {
	if h.Stderr != nil {
		return h.Stderr
	}
	return os.Stderr
}

func (h *CapiHook) kube() *kube {
	return &kube{runner: h.Runner, stdout: h.stdout(), stderr: h.stderr()}
}

// Config implements Hook.
func (h *CapiHook) Config() string { return capiConfig }

func (h *CapiHook) export(name, value string) {
	if h.exported == nil {
		h.exported = map[string]string{}
	}
	h.exported[name] = value
}

// detectProvider is capi::detect_provider_from_spec: `.hcloud` present →
// hetzner, `.aws` present → aws, else an error line.
func (h *CapiHook) detectProvider(spec any) (string, bool) {
	if present(get(spec, "hcloud")) {
		return "hetzner", true
	}
	if present(get(spec, "aws")) {
		return "aws", true
	}
	fmt.Fprintln(h.stderr(), "error: no known CAPI provider found in Capi spec")
	return "", false
}

// renderTemplate is `envsubst < "$tmpl"` appended to the stream; an
// unreadable template is bash's redirect error on stderr, the stream goes
// on (errexit is off inside the captured subshell).
func (h *CapiHook) renderTemplate(b *strings.Builder, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(h.stderr(), "%s: No such file or directory\n", path)
		return
	}
	b.Write(envsubstAll(raw, h.exported))
}

// generate is capi::generate_from_spec: the rendered CAPI manifest stream.
func (h *CapiHook) generate(spec any, provider, name string) (string, bool) {
	stderr := h.stderr()
	tmplDir := h.TemplateDir

	if info, err := os.Stat(tmplDir); err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "error: CAPI template directory not found: %s\n", tmplDir)
		return "", false
	}

	// Extract variables from spec (exported for this render's envsubst).
	h.exported = map[string]string{}
	h.export("CLUSTER_NAME", name)
	h.export("CLUSTER_NAMESPACE", jqR(alt(get(spec, "cluster", "namespace"), "default")))
	h.export("CLUSTER_DOMAIN", jqR(get(spec, "cluster", "domain")))
	h.export("K8S_VERSION", jqR(alt(get(spec, "kubernetes", "version"), "v1.31.10")))
	h.export("CP_REPLICAS", jqR(alt(get(spec, "controlPlane", "replicas"), json.Number("1"))))
	h.export("CREDENTIAL_SECRET_NAME", jqR(alt(get(spec, "credentials", "secretName"), name+"-credentials")))

	switch provider {
	case "hetzner":
		h.export("INFRA_API_VERSION", "infrastructure.cluster.x-k8s.io/v1beta1")
		h.export("INFRA_CLUSTER_KIND", "HetznerCluster")
		h.export("INFRA_MACHINE_TEMPLATE_KIND", "HCloudMachineTemplate")
		h.export("HCLOUD_REGION", jqR(get(spec, "hcloud", "region")))
		h.export("HCLOUD_SSH_KEY_NAME", jqR(get(spec, "hcloud", "sshKeyName")))
	case "aws":
		h.export("INFRA_API_VERSION", "infrastructure.cluster.x-k8s.io/v1beta2")
		h.export("INFRA_CLUSTER_KIND", "AWSCluster")
		h.export("INFRA_MACHINE_TEMPLATE_KIND", "AWSMachineTemplate")
		h.export("AWS_REGION", jqR(get(spec, "aws", "region")))
	default:
		fmt.Fprintf(stderr, "error: unsupported CAPI provider: %s\n", provider)
		return "", false
	}

	var b strings.Builder

	// Core templates: every core/*.yaml in glob order — machine-deployment
	// included, rendered here ONCE with no POOL_* set (blank pool fields),
	// and again per worker pool below.
	core, _ := filepath.Glob(filepath.Join(tmplDir, "core", "*.yaml"))
	first := true
	for _, tmpl := range core {
		if !fileExists(tmpl) {
			continue
		}
		if first {
			first = false
		} else {
			b.WriteString("---\n")
		}
		h.renderTemplate(&b, tmpl)
	}

	// Provider templates.
	providerDir := filepath.Join(tmplDir, "providers", provider)
	if info, err := os.Stat(providerDir); err == nil && info.IsDir() {
		provs, _ := filepath.Glob(filepath.Join(providerDir, "*.yaml"))
		for _, tmpl := range provs {
			if !fileExists(tmpl) {
				continue
			}
			b.WriteString("---\n")
			h.renderTemplate(&b, tmpl)
		}
	}

	// Worker pool machine deployments: `jq -r '.workers // empty'` non-empty
	// and not "null" → iterate `.workers | keys[]` (jq keys: SORTED).
	if workers := jqEmpty(get(spec, "workers")); workers != "" && workers != "null" {
		for _, pool := range h.poolNames(get(spec, "workers")) {
			if pool == "" {
				continue
			}
			h.export("POOL_NAME", pool)
			h.export("POOL_REPLICAS", h.poolField(get(spec, "workers"), pool, "replicas", json.Number("1")))
			h.export("POOL_TYPE", h.poolField(get(spec, "workers"), pool, "type", nil))
			b.WriteString("---\n")
			h.renderTemplate(&b, filepath.Join(tmplDir, "core", "machine-deployment.yaml"))
			if hm := filepath.Join(providerDir, "hcloud-machine-template.yaml"); fileExists(hm) {
				b.WriteString("---\n")
				h.renderTemplate(&b, hm)
			}
		}
	}
	return b.String(), true
}

// poolNames is `.workers | keys[]`: sorted keys of an object; an array's
// indices; anything else has no keys (jq errors, the loop runs nothing).
func (h *CapiHook) poolNames(workers any) []string {
	switch w := workers.(type) {
	case map[string]any:
		names := make([]string, 0, len(w))
		for k := range w {
			names = append(names, k)
		}
		sort.Strings(names)
		return names
	case []any:
		names := make([]string, len(w))
		for i := range w {
			names[i] = strconv.Itoa(i)
		}
		return names
	default:
		fmt.Fprintf(h.stderr(), "jq: error (at <stdin>:0): %s has no keys\n", compact(workers))
		return nil
	}
}

// poolField is `.workers[$p].<field> // <default>` (jq -r). On an array
// `workers` the string index is a jq error: empty output on stdout, the
// error on stderr.
func (h *CapiHook) poolField(workers any, pool, field string, fallback any) string {
	m, ok := workers.(map[string]any)
	if !ok {
		fmt.Fprintf(h.stderr(), "jq: error (at <stdin>:0): Cannot index array with string \"%s\"\n", pool)
		return ""
	}
	return jqR(alt(get(m[pool], field), fallback))
}

// teardown is capi_hook::teardown: delete the CAPI Cluster (its own
// finalizer cascades the control plane, machines and infrastructure;
// --wait=false — async, outlives our CR). The finalizer drops only once the
// delete is accepted; a failed API call keeps it so the capi-drift schedule
// retries rather than orphaning the cluster. The Cluster lives in
// spec.cluster.namespace and is named after the CR.
func (h *CapiHook) teardown(ctx context.Context, name, namespace string, spec any) {
	k := h.kube()
	stderr := h.stderr()
	clusterNS := jqR(alt(get(spec, "cluster", "namespace"), "default"))

	_ = k.patchStatusQuiet(ctx, "capi", name, namespace, `{"status":{"phase":"Terminating","ready":false}}`)

	if err := k.run(ctx, nil, k.out(), k.out(), "delete", "cluster.cluster.x-k8s.io", name, "-n", clusterNS,
		"--wait=false", "--ignore-not-found"); err == nil {
		k.removeFinalizer(ctx, "capi", "Capi", name, namespace, CapiFinalizer)
		fmt.Fprintf(stderr, "info: Capi %s/%s torn down (deleted CAPI Cluster %s/%s)\n", namespace, name, clusterNS, name)
		return
	}
	_ = k.patchStatusQuiet(ctx, "capi", name, namespace,
		`{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"DestroyFailed","message":"failed to delete CAPI Cluster; will retry"}]}}`)
	fmt.Fprintf(stderr, "error: Capi %s/%s teardown failed (will retry)\n", namespace, name)
}

// provision is capi_hook::provision: detect provider, render, apply.
func (h *CapiHook) provision(ctx context.Context, name, namespace string, spec any) {
	k := h.kube()
	stderr := h.stderr()

	_ = k.patchStatusQuiet(ctx, "capi", name, namespace, `{"status":{"phase":"Provisioning","ready":false}}`)

	provider, ok := h.detectProvider(spec)
	if !ok {
		_ = k.patchStatusQuiet(ctx, "capi", name, namespace,
			`{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"UnknownProvider","message":"No known CAPI provider found in spec"}]}}`)
		return
	}

	_ = k.patchStatusQuiet(ctx, "capi", name, namespace, `{"status":{"provider":"`+provider+`"}}`)

	resources, ok := h.generate(spec, provider, name)
	if !ok {
		_ = k.patchStatusQuiet(ctx, "capi", name, namespace,
			`{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"GenerationFailed","message":"Failed to generate CAPI resources from templates"}]}}`)
		return
	}

	// `echo "${resources}" | kubectl apply -f - 2>&1`: the capture stripped
	// the trailing newlines, echo adds one back.
	stream := strings.TrimRight(resources, "\n") + "\n"
	if err := k.run(ctx, strings.NewReader(stream), k.out(), k.out(), "apply", "-f", "-"); err == nil {
		fmt.Fprintf(stderr, "info: CAPI resources applied for %s\n", name)
		// Status reaches Provisioned/Ready via capi-status-sync when the CAPI Cluster is up.
		return
	}
	_ = k.patchStatusQuiet(ctx, "capi", name, namespace,
		`{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"ApplyFailed","message":"Failed to apply CAPI resources"}]}}`)
	fmt.Fprintf(stderr, "error: failed to apply CAPI resources for %s\n", name)
}

// Reconcile converges one Capi object (full JSON) toward its spec
// (capi_hook::reconcile) — provision when live, finalizer-guarded teardown
// when it is being deleted. Never fails (see LoHook.Reconcile).
func (h *CapiHook) Reconcile(ctx context.Context, object []byte) error {
	k := h.kube()

	obj, err := decode(object)
	if err != nil {
		obj = nil
	}
	name := jqR(get(obj, "metadata", "name"))
	namespace := jqR(alt(get(obj, "metadata", "namespace"), "default"))
	spec := get(obj, "spec")
	deletion := jqEmpty(get(obj, "metadata", "deletionTimestamp"))
	finalizers := alt(get(obj, "metadata", "finalizers"), []any{})

	fmt.Fprintf(h.stderr(), "info: reconciling Capi %s/%s\n", namespace, name)

	if deletion != "" {
		if contains(finalizers, CapiFinalizer) {
			h.teardown(ctx, name, namespace, spec)
		}
		return nil
	}

	k.ensureFinalizer(ctx, "capi", "Capi", name, namespace, finalizers, CapiFinalizer)
	h.provision(ctx, name, namespace, spec)
	return nil
}

// reconcileAll re-lists every Capi CR and converges — drift detection +
// teardown retry (capi_hook::reconcile_all).
func (h *CapiHook) reconcileAll(ctx context.Context) {
	for j, item := range h.kube().listAll(ctx, "capi") {
		raw, _ := json.Marshal(item)
		if err := h.Reconcile(ctx, raw); err != nil {
			fmt.Fprintf(h.stderr(), "warn: reconcile failed for item %d\n", j)
		}
	}
}

// Trigger implements Hook (hook::trigger).
func (h *CapiHook) Trigger(ctx context.Context, events []Event) error {
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

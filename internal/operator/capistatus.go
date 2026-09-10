package operator

// capistatus.go — `lo operator capi-status-sync`: mirrors the status of
// lok8s-managed CAPI Cluster objects (label lok8s.dev/managed=true) back to
// the Capi CR, and runs the post-provision actions (kubeconfig Secret,
// GitOps bootstrap or direct deploy) when a cluster becomes Provisioned
// (.lok8s/legacy/operator/hooks/capi-status-sync.sh).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// capiStatusConfig is hook::config, verbatim
// (testdata/capi-status-sync.config.yaml).
const capiStatusConfig = `configVersion: v1
kubernetes:
  - apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    executeHookOnEvent: ["Modified"]
    jqFilter: ".status"
    labelSelector:
      matchLabels:
        lok8s.dev/managed: "true"
`

// CapiStatusSyncHook bridges CAPI Cluster status → Capi CR status.
type CapiStatusSyncHook struct {
	Paths  *config.Paths
	Runner execx.Runner
	Stdout io.Writer
	Stderr io.Writer

	// GitopsBootstrap is gitops::bootstrap (gitops.BootstrapHook in the
	// CLI). nil = the lib is not loaded: the bash `declare -f` probe then
	// skips BOTH the bootstrap and the gitops status patch.
	GitopsBootstrap func(ctx context.Context, domain, provider string) error
	// DeployApply is deploy::apply (deploy.Deployer.Apply in the CLI). nil =
	// not loaded → probed away, like the bash.
	DeployApply func(ctx context.Context, domain string) error
}

func (h *CapiStatusSyncHook) stdout() io.Writer {
	if h.Stdout != nil {
		return h.Stdout
	}
	return os.Stdout
}

func (h *CapiStatusSyncHook) stderr() io.Writer {
	if h.Stderr != nil {
		return h.Stderr
	}
	return os.Stderr
}

func (h *CapiStatusSyncHook) kube() *kube {
	return &kube{runner: h.Runner, stdout: h.stdout(), stderr: h.stderr()}
}

// Config implements Hook.
func (h *CapiStatusSyncHook) Config() string { return capiStatusConfig }

// MapPhase maps a CAPI Cluster phase to the lok8s phase.
func MapPhase(phase string) string {
	switch phase {
	case "Provisioned":
		return "Provisioned"
	case "Provisioning", "Pending":
		return "Provisioning"
	case "Failed", "Deleting":
		return "Failed"
	default:
		return "Provisioning"
	}
}

// The status patch, in the jq construction order (`jq -n '{status: {phase,
// ready}}'`, then `.status.controlPlaneEndpoint = {host, port}` appended).
type statusEndpoint struct {
	Host string          `json:"host"`
	Port json.RawMessage `json:"port"`
}

type statusBody struct {
	Phase                string          `json:"phase"`
	Ready                json.RawMessage `json:"ready"`
	ControlPlaneEndpoint *statusEndpoint `json:"controlPlaneEndpoint,omitempty"`
}

type statusPatch struct {
	Status statusBody `json:"status"`
}

// argjson is `--argjson`: the jq -r text of a value re-parsed as JSON. A
// value that is not JSON (a bare word) fails the jq call — under the hook's
// `set -e` that aborted the whole run with exit 1.
func argjson(text string) (json.RawMessage, error) {
	if !json.Valid([]byte(text)) {
		return nil, errorf("jq: invalid JSON text passed to --argjson: %s", text)
	}
	return json.RawMessage(text), nil
}

// BuildStatusPatch renders the Capi status patch for one CAPI status
// (the jq construction in hook::trigger), pretty-printed as jq prints it.
func BuildStatusPatch(status any) (string, error) {
	phase := jqR(alt(get(status, "phase"), "Unknown"))
	ready, err := argjson(jqR(alt(get(status, "controlPlaneReady"), false)))
	if err != nil {
		return "", err
	}
	patch := statusPatch{Status: statusBody{Phase: MapPhase(phase), Ready: ready}}

	host := jqEmpty(get(status, "controlPlaneEndpoint", "host"))
	portText := jqEmpty(get(status, "controlPlaneEndpoint", "port"))
	if host != "" && portText != "" {
		port, err := argjson(portText)
		if err != nil {
			return "", err
		}
		patch.Status.ControlPlaneEndpoint = &statusEndpoint{Host: host, Port: port}
	}
	return pretty(patch), nil
}

// Trigger implements Hook (hook::trigger).
func (h *CapiStatusSyncHook) Trigger(ctx context.Context, events []Event) error {
	k := h.kube()
	stderr := h.stderr()

	for _, ev := range events {
		if ev.EventType() == "Synchronization" {
			continue
		}

		obj, err := decode(eventObject(ev))
		if err != nil {
			obj = nil
		}
		name := jqR(get(obj, "metadata", "name"))
		namespace := jqR(alt(get(obj, "metadata", "namespace"), "default"))
		var status any
		if len(ev.FilterResult) > 0 {
			status, _ = decode(ev.FilterResult)
		}
		phase := jqR(alt(get(status, "phase"), "Unknown"))

		fmt.Fprintf(stderr, "info: syncing CAPI Cluster status for %s/%s: phase=%s\n", namespace, name, phase)

		patch, err := BuildStatusPatch(status)
		if err != nil {
			// jq's usage error under `set -e`: the run ends with jq's status.
			fmt.Fprintln(stderr, err.Error())
			return &ExitError{Code: 2}
		}

		// Update the lok8s Capi CR status.
		if err := k.patchStatusQuiet(ctx, "capi", name, namespace, patch); err != nil {
			fmt.Fprintf(stderr, "warn: could not patch Capi CR %s status (CR may not exist)\n", name)
			continue
		}

		if phase != "Provisioned" {
			continue
		}
		fmt.Fprintf(stderr, "info: Capi cluster %s is Provisioned, running post-provision\n", name)

		// Extract the work-cluster kubeconfig and pipe it straight into the
		// Secret — never a predictable, world-readable /tmp path.
		kubeconfigSecret := name + "-kubeconfig"
		var kc strings.Builder
		err = h.Runner.Run(ctx, execx.Cmd{
			Name: "clusterctl", Args: []string{"get", "kubeconfig", name, "-n", namespace},
			Stdout: &kc, Stderr: io.Discard,
		})
		if kcText := strings.TrimRight(kc.String(), "\n"); err == nil && kcText != "" {
			var manifest strings.Builder
			_ = k.run(ctx, strings.NewReader(kcText), &manifest, nil, "create", "secret", "generic", kubeconfigSecret,
				"-n", namespace, "--from-file=value=/dev/stdin", "--dry-run=client", "-o", "yaml")
			_ = k.run(ctx, strings.NewReader(manifest.String()), nil, io.Discard, "apply", "-f", "-")

			_ = k.patchStatusQuiet(ctx, "capi", name, namespace,
				`{"status":{"kubeconfig":{"secretRef":"`+kubeconfigSecret+`"}}}`)
		}

		// GitOps configured? (`$(kubectl … 2>/dev/null || true)` — whatever
		// reached stdout, failure or not.)
		gitopsProvider, _ := k.capture(ctx, "get", "capi", name, "-n", namespace, "-o", "jsonpath={.spec.gitops.provider}")
		domain, _ := k.capture(ctx, "get", "capi", name, "-n", namespace, "-o", "jsonpath={.spec.cluster.domain}")
		gitopsProvider = strings.TrimRight(gitopsProvider, "\n")
		domain = strings.TrimRight(domain, "\n")

		switch {
		case gitopsProvider != "" && domain != "":
			fmt.Fprintf(stderr, "info: bootstrapping GitOps (%s) for %s\n", gitopsProvider, domain)
			if h.GitopsBootstrap != nil {
				if err := h.GitopsBootstrap(ctx, domain, gitopsProvider); err != nil {
					fmt.Fprintf(stderr, "warn: GitOps bootstrap failed for %s\n", domain)
				}
				_ = k.patchStatusQuiet(ctx, "capi", name, namespace,
					`{"status":{"gitops":{"provider":"`+gitopsProvider+`","status":"Bootstrapped"}}}`)
			}
		case domain != "":
			// Direct deploy mode (no GitOps)
			fmt.Fprintf(stderr, "info: no GitOps configured, direct deploy for %s\n", domain)
			if h.DeployApply != nil {
				if err := h.DeployApply(ctx, domain); err != nil {
					fmt.Fprintf(stderr, "warn: direct deploy failed for %s\n", domain)
				}
			}
		}

		_ = k.patchStatusQuiet(ctx, "capi", name, namespace,
			`{"status":{"conditions":[{"type":"InfrastructureReady","status":"True"},{"type":"ControlPlaneReady","status":"True"}]}}`)
	}
	return nil
}

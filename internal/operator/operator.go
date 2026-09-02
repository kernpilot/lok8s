// Package operator is the Go port of the shell-operator hooks
// (operator/hooks/*.sh, frozen at .lok8s/legacy/operator/hooks/): the three
// reconcilers behind `lo operator <hook>`, which the hook shims exec.
//
// The shell-operator hook contract, unchanged: `--config` prints the binding
// configuration (byte-identical to the bash heredocs — pinned against
// testdata/*.config.yaml, which were generated ONCE from the bash hooks);
// otherwise the hook reads $BINDING_CONTEXT_PATH, a JSON array of events,
// and reconciles. Every kubectl/clusterctl call runs through the execx.Runner
// seam with the exact argv the bash built, so the recorded call log in a
// test IS the bash KLOG assertion.
//
// Bash wins: where the bash body did something odd (patch bodies, log
// prefixes, fall-through on a failed sub-step), the port does the same and
// the comment says so. The one structural difference is how the hook bodies
// reach the framework: the bash sourced the driver + libs into its shell and
// called driver::provision / bootstrap::apply / gitops::bootstrap /
// deploy::apply; the Go calls the ported packages (internal/driver/lo,
// internal/bootstrap, internal/gitops, internal/deploy) through the same
// injectable seams the dispatch layer uses.
package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/config"
)

// ErrHandled marks a failure whose message was already printed on stderr
// (the CLI exits 1 without printing more).
var ErrHandled = errors.New("operator: handled")

// ExitError carries the process exit status a bash hook would have ended
// with (its message is already on stderr): the `set -u` abort is 1, a jq
// failure is jq's own status — 2 for a usage/system error (file not found,
// a non-JSON --argjson), 5 for invalid JSON input.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("operator: exit status %d", e.Code) }

// ExitCode is the process exit status the error maps to.
func (e *ExitError) ExitCode() int { return e.Code }

// Hook is one shell-operator hook.
type Hook interface {
	// Config is the `--config` output (hook::config).
	Config() string
	// Trigger handles one binding-context batch (hook::trigger).
	Trigger(ctx context.Context, events []Event) error
}

// Defaults of the operator runtime (runtime.sh + operator/Dockerfile).
const (
	// DefaultHookDir is where the image mounts the hook tree; the framework
	// pieces the bash sourced live under it (lib/, drivers/, addons/,
	// capi-templates/ …), so it doubles as PATH_LOK8S.
	DefaultHookDir = "/hooks"
	// DefaultStateDir is LOK8S_STATE_DIR's default: the writable volume laid
	// out like a lok8s project (clusters/<domain>/cluster.lok8s.yaml,
	// .kubeconfig/<cluster>.yaml).
	DefaultStateDir = "/var/lib/lok8s"
	// DefaultKustomizePluginHome is the image's khelm plugin root.
	DefaultKustomizePluginHome = "/usr/local/kustomize-plugins"
)

// Env is the operator runtime layout (runtime.sh's framework-env block).
type Env struct {
	// HookDir is the hook tree (bash: RUNTIME_DIR / HOOK_DIR → PATH_LOK8S).
	HookDir string
	// StateDir is the reconcile state volume (bash: LOK8S_STATE_DIR →
	// PATH_BASE).
	StateDir string
	// KustomizePluginHome is KUSTOMIZE_PLUGIN_HOME (defaulted, never
	// overridden when set).
	KustomizePluginHome string
}

// ResolveEnv reads the runtime layout from the process environment:
// PATH_LOK8S (the hook tree; the bash derived it from the hook file's own
// directory, which the Go binary — installed under /usr/local/bin — cannot),
// LOK8S_STATE_DIR and KUSTOMIZE_PLUGIN_HOME, each with the runtime.sh /
// Dockerfile default.
func ResolveEnv() *Env {
	return &Env{
		HookDir:             envOr("PATH_LOK8S", DefaultHookDir),
		StateDir:            envOr("LOK8S_STATE_DIR", DefaultStateDir),
		KustomizePluginHome: envOr("KUSTOMIZE_PLUGIN_HOME", DefaultKustomizePluginHome),
	}
}

// Paths is the project layout the framework packages read, over the state
// volume (runtime.sh: PATH_BASE=state, PATH_CLUSTERS=state/clusters,
// PATH_LOK8S=hook dir). Bin points below the state dir — the image has no
// b-managed toolchain; execx falls back to PATH, where the image's tools
// live.
func (e *Env) Paths() *config.Paths {
	return &config.Paths{
		Base:          e.StateDir,
		Bin:           filepath.Join(e.StateDir, ".bin"),
		Lok8s:         e.HookDir,
		Clusters:      filepath.Join(e.StateDir, "clusters"),
		SecretsEnv:    filepath.Join(e.StateDir, ".secrets"),
		SecretsEnvSet: true,
	}
}

// CapiTemplateDir is where the image copies the CAPI templates for the
// capi-reconcile hook (Dockerfile: `COPY .lok8s/drivers/capi/cluster/
// /hooks/capi-templates/`; bash: `${HOOK_DIR}/capi-templates`).
func (e *Env) CapiTemplateDir() string {
	return filepath.Join(e.HookDir, "capi-templates")
}

// Export mirrors runtime.sh's exports + mkdirs: the framework env every
// subprocess (kubectl, kustomize plugins, the lo driver's tools) and every
// ported lib reading os.Getenv sees. PATH_SECRETS is the runtime's flat
// store under the state volume (runtime.sh: `${PATH_BASE}/.secrets`).
func (e *Env) Export() error {
	p := e.Paths()
	os.Setenv("PATH_LOK8S", p.Lok8s)
	os.Setenv("PATH_BASE", p.Base)
	os.Setenv("PATH_CLUSTERS", p.Clusters)
	os.Setenv("PATH_SECRETS", p.SecretsEnv)
	os.Setenv("KUSTOMIZE_PLUGIN_HOME", e.KustomizePluginHome)
	for _, dir := range []string{p.Clusters, p.SecretsEnv, filepath.Join(p.Base, ".kubeconfig")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Event is one entry of the shell-operator binding context. Only the fields
// the hooks read are decoded; the rest of the entry is ignored, as the jq
// paths ignored it.
type Event struct {
	// Type is the event type: "Synchronization", "Schedule", "Event" — the
	// bash read `.type // "Event"`, so a missing type is an Event.
	Type string `json:"type"`
	// Binding is the binding name (schedule bindings carry it).
	Binding string `json:"binding"`
	// Object is the watched object (after the jqFilter, for kubernetes
	// bindings).
	Object json.RawMessage `json:"object"`
	// FilterResult is the jqFilter's output (capi-status-sync reads it).
	FilterResult json.RawMessage `json:"filterResult"`
}

// EventType is `.type // "Event"`.
func (e Event) EventType() string {
	if e.Type == "" {
		return "Event"
	}
	return e.Type
}

// ReadBindingContext reads the events file shell-operator hands the hook.
// An unset BINDING_CONTEXT_PATH is the bash `set -u` abort (unbound
// variable, exit 1); an unreadable file is jq's "Could not open file"
// (exit 2); a non-JSON file is jq's parse error (exit 5). The message goes
// to stderr here and the error carries the status (ExitError).
func ReadBindingContext(stderr io.Writer, path string) ([]Event, error) {
	if path == "" {
		fmt.Fprintln(stderr, "error: BINDING_CONTEXT_PATH: unbound variable")
		return nil, &ExitError{Code: 1}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "jq: error: Could not open file %s: %v\n", path, err)
		return nil, &ExitError{Code: 2}
	}
	var events []Event
	if err := json.Unmarshal(raw, &events); err != nil {
		fmt.Fprintf(stderr, "jq: parse error: %s: %v\n", path, err)
		return nil, &ExitError{Code: 5}
	}
	return events, nil
}

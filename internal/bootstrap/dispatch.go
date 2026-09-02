package bootstrap

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"gopkg.in/yaml.v3"
)

// Dispatcher drives the standalone `lo bootstrap` core (bash:
// bootstrap::dispatch — factored out of main::bootstrap so it is testable
// without the argsh :args builtin, mirroring provision::dispatch).
type Dispatcher struct {
	Engine *Engine

	// Drivers overrides the driver lookup (nil → the package registry,
	// driver.Get) — same seam as provision.Dispatcher.
	Drivers func(name string) (driver.Factory, bool)

	// InventoryPublish is inventory::publish, refreshed after a standalone
	// `lo bootstrap` too (same tail as provision::dispatch — fail-soft,
	// probed: bash only calls it when the inventory lib is loaded, so a
	// nil hook is SKIPPED).
	InventoryPublish func(ctx context.Context, domain, clusterYAML, kubeconfig string)
}

// Dispatch runs the standalone `lo bootstrap <domain>` flow.
//
// CRITICAL ordering (bash comment preserved): the domain's driver export
// runs BEFORE Apply — it populates the spec-derived env (LOK8S_SPEC_*) that
// spec.bootstrap addons reference via envsubst; without it they render
// those values empty. Same order provision::dispatch uses.
func (d *Dispatcher) Dispatch(ctx context.Context, domainName string) error {
	e := d.Engine
	stderr := e.stderr()

	// Reject a path-traversal / injected domain before it builds any
	// filesystem path (same guard as provision::resolve_spec).
	if !domain.NameRe.MatchString(domainName) {
		return e.errorf("invalid domain name: %s", domainName)
	}

	clusterYAML := filepath.Join(e.Paths.Clusters, domainName, "cluster.lok8s.yaml")
	if !fileExists(clusterYAML) {
		return e.errorf("cluster spec not found: %s", clusterYAML)
	}

	clusterName := specMetadataName(clusterYAML)
	kubeconfig := filepath.Join(e.Paths.Base, ".kubeconfig", clusterName+".yaml")

	// Resolve the driver (same resolution as provision::dispatch).
	// domain.SpecDriver constrains the value to a bare driver name — it is
	// untrusted YAML about to select executable driver code.
	kind, err := domain.SpecDriver(clusterYAML, "")
	if err != nil {
		return e.errorf("invalid cluster kind in %s", clusterYAML)
	}
	factory, ok := d.driverFactory(kind)
	if !ok {
		return e.errorf("Unknown cluster kind: %s (missing %s)", kind,
			filepath.Join(e.Paths.Lok8s, "drivers", kind, "main"))
	}
	drv, err := factory(&driver.Deps{Paths: e.Paths, Runner: e.Runner, Stderr: stderr})
	if err != nil {
		return err
	}

	// driver::export: idempotent spec-derived env (LOK8S_SPEC_*, …) —
	// optional per the driver contract, so probe before calling.
	if ex, ok := drv.(driver.Exporter); ok {
		if err := ex.Export(ctx, domainName); err != nil {
			return err
		}
	}

	// A standalone `lo bootstrap` NEVER provisions — the KubeOne driver did
	// not run `kubeone apply` this invocation, so bootstrap is the sole
	// applier and MUST reconcile cilium/ccm from spec.bootstrap (identical
	// intent to `lo provision --bootstrap`). Set the gate to reconcile-only
	// so the KubeOne cilium/ccm skip in applyOne does NOT fire.
	os.Setenv("LOK8S_BOOTSTRAP_ONLY", "1")

	if err := e.Apply(ctx, domainName, clusterYAML, kubeconfig); err != nil {
		return err
	}

	// Refresh the in-cluster ClusterInventory after a standalone
	// `lo bootstrap` too (fail-soft; nil hook = the lib isn't loaded).
	if d.InventoryPublish != nil {
		d.InventoryPublish(ctx, domainName, clusterYAML, kubeconfig)
	}
	return nil
}

func (d *Dispatcher) driverFactory(kind string) (driver.Factory, bool) {
	if d.Drivers != nil {
		return d.Drivers(kind)
	}
	return driver.Get(kind)
}

// specMetadataName reads .metadata.name with yq -r semantics: a missing or
// null key reads as the literal "null" (bash: cluster_name=$(yq -r
// '.metadata.name' …) — the kubeconfig path then names null.yaml, exactly
// as the bash did).
func specMetadataName(clusterYAML string) string {
	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		return "null"
	}
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return "null"
	}
	n := lookupMap(lookupMap(derefNode(&root), "metadata"), "name")
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return "null"
	}
	return n.Value
}

// ApplyHook returns the provision dispatch's Hooks.BootstrapApply seam —
// the function provision.Dispatcher calls as bootstrap::apply on both the
// full-provision and --bootstrap paths. The engine is built per call so
// each invocation reads the decision env (LOK8S_FORCE_RECREATE,
// LOK8S_BOOTSTRAP_ONLY, LOK8S_BOOTSTRAP_PARALLEL) at apply time, exactly
// like the sourced bash lib did.
func ApplyHook(p *config.Paths, r execx.Runner, stdout, stderr io.Writer) func(ctx context.Context, domain, clusterYAML, kubeconfig string) error {
	return func(ctx context.Context, domainName, clusterYAML, kubeconfig string) error {
		e := &Engine{Paths: p, Runner: r, Stdout: stdout, Stderr: stderr}
		return e.Apply(ctx, domainName, clusterYAML, kubeconfig)
	}
}

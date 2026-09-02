package provision

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Hooks are the dispatch tail's injectable seams. A nil hook is SKIPPED,
// matching the bash `declare -f` probes: libs/provision only calls
// kubehz::/bootstrap::/inventory:: functions when the lib that defines them
// is loaded, and tests source the lib standalone without them.
type Hooks struct {
	// KubehzRegister covers the bash kubehz tail: kubehz::read_config →
	// kubehz::validate_config → (access != "none") → kubehz::register_cluster.
	// Any error aborts the dispatch (each bash step is `|| return 1`).
	KubehzRegister func(ctx context.Context, domain, clusterYAML string) error

	// BootstrapApply is bootstrap::apply — the spec.bootstrap addon DAG.
	// kubeconfig is .kubeconfig/<metadata.name>.yaml. Errors abort.
	BootstrapApply func(ctx context.Context, domain, clusterYAML, kubeconfig string) error

	// InventoryPublish is inventory::publish — the ClusterInventory
	// singleton record. FAIL-SOFT BY CONTRACT: the bash implementation warns
	// and returns 0 on any cluster/CRD problem so it can never break a
	// provision; the Go signature has no error return to enforce exactly
	// that contract at the type level.
	InventoryPublish func(ctx context.Context, domain, clusterYAML, kubeconfig string)

	// GitopsBootstrap is gitops::bootstrap, called when spec.gitops.provider
	// is set. Errors abort.
	GitopsBootstrap func(ctx context.Context, domain, provider string) error

	// KubehzDeregister covers the destroy path's kubehz tail:
	// kubehz::read_config → (access != "none") → kubehz::deregister_cluster.
	// Best-effort THERE only: an error warns (the platform registration
	// survives) but never blocks the driver teardown.
	KubehzDeregister func(ctx context.Context, domain, clusterYAML string) error
}

// Dispatcher drives the provision/destroy/status lifecycle for a domain
// (bash: provision::dispatch, provision::dispatch_destroy,
// provision::dispatch_status).
type Dispatcher struct {
	Paths  *config.Paths
	Runner execx.Runner
	Stdout io.Writer
	Stderr io.Writer
	// In is the prompt input (bash: the gate reads stdin). Defaults to
	// os.Stdin.
	In io.Reader

	// Force is the global --force|-f: bypasses the real-infrastructure gate
	// (bash: inherited via argsh dynamic scoping).
	Force bool
	// Remote is remote mode (bash: LOK8S_REMOTE=1): the lo driver loses its
	// gate exemption and gains provider loading.
	Remote bool
	// Interactive overrides the tty/CI/LOK8S_NONINTERACTIVE detection
	// (tests stub it exactly like the bats redefine
	// provision::_interactive).
	Interactive func() bool

	// Drivers overrides the driver lookup (nil → the package registry,
	// driver.Get). The lookup answers "does this kind exist" BEFORE the
	// gate, exactly where bash stats drivers/<kind>/main; the factory runs
	// after the gate, where bash sources the file.
	Drivers func(name string) (driver.Factory, bool)

	// Providers loads infrastructure providers. nil = none available: a
	// spec naming one fails with the bash "provider '<name>' not found"
	// error.
	Providers ProviderLoader

	Hooks Hooks

	promptReader *bufio.Reader
}

func (d *Dispatcher) errWriter() io.Writer {
	if d.Stderr != nil {
		return d.Stderr
	}
	return os.Stderr
}

func (d *Dispatcher) outWriter() io.Writer {
	if d.Stdout != nil {
		return d.Stdout
	}
	return os.Stdout
}

func (d *Dispatcher) runner() execx.Runner {
	if d.Runner != nil {
		return d.Runner
	}
	return execx.NewRunner(d.Paths)
}

func (d *Dispatcher) driverFactory(kind string) (driver.Factory, bool) {
	if d.Drivers != nil {
		return d.Drivers(kind)
	}
	return driver.Get(kind)
}

func (d *Dispatcher) newDeps() *driver.Deps {
	return &driver.Deps{Paths: d.Paths, Runner: d.runner(), Stderr: d.errWriter()}
}

// loadProvider resolves + loads a named provider (bash: provider::load).
func (d *Dispatcher) loadProvider(name string) (driver.Provider, error) {
	if d.Providers == nil {
		ui.Errorf(d.errWriter(), "provider '%s' not found at %s", name,
			filepath.Join(d.Paths.Lok8s, "providers", name, "main"))
		return nil, fmt.Errorf("provider %s not found", name)
	}
	return d.Providers.Load(name)
}

// Dispatch provisions a domain (bash: provision::dispatch). bootstrapOnly
// re-applies spec.bootstrap on an ALREADY-provisioned cluster, skipping the
// provider reconcile + driver provision entirely.
//
// Flow, in the bash order: resolve spec FIRST (fail fast, before any driver
// touches infra) → load provider creds → deploy-domain refusal → read kind
// (never defaulted when malformed) → driver-existence check → the
// real-infrastructure gate (a decline propagates driver.ErrDeclined) →
// driver construction → [bootstrap-only guard | provider load+validate →
// driver.Provision (ErrFullLifecycle → success, skip the tail) →
// PostProvision hook] → Export hook → LOK8S_BOOTSTRAP_ONLY export → kubehz
// registration hook → bootstrap hook → inventory hook (fail-soft) → gitops
// hook.
func (d *Dispatcher) Dispatch(ctx context.Context, domainName string, bootstrapOnly bool) error {
	stderr := d.errWriter()

	// Fail fast: a missing/empty/bogus domain must stop here, BEFORE any
	// driver creates a kind cluster or touches infra.
	spec, err := ResolveSpec(d.Paths, domainName, stderr)
	if err != nil {
		return err
	}

	// Auto-load managed provider credentials from the per-domain store.
	_ = LoadProviderCreds(d.Paths, domainName)

	if spec.Kind == SpecKindDeploy {
		ui.Errorf(stderr, "Cannot provision a deployment domain. Use 'lo deploy %s' instead.", domainName)
		ui.Errorf(stderr, "Deployment domains reference a cluster via spec.clusterRef.domain.")
		return fmt.Errorf("cannot provision deploy domain %s", domainName)
	}

	clusterYAML := spec.File
	kind, err := ReadKind(clusterYAML, stderr)
	if err != nil {
		return fmt.Errorf("read kind: %w", err)
	}

	factory, ok := d.driverFactory(kind)
	if !ok {
		ui.Errorf(stderr, "Unknown cluster kind: %s (missing %s)", kind,
			filepath.Join(d.Paths.Lok8s, "drivers", kind, "main"))
		return fmt.Errorf("unknown cluster kind %s", kind)
	}

	// Real-infrastructure gate: summary + confirmation BEFORE the provider
	// or driver touch anything (after the kind check, so a bogus kind stays
	// a plain error). A decline propagates its sentinel.
	gateAction := ActionReconcile
	if bootstrapOnly {
		gateAction = ActionBootstrap
	}
	if err := d.ConfirmInfra(domainName, clusterYAML, kind, gateAction); err != nil {
		return err
	}

	deps := d.newDeps()
	drv, err := factory(deps)
	if err != nil {
		return err
	}

	if bootstrapOnly {
		// Re-apply spec.bootstrap on an ALREADY-provisioned cluster: skip
		// the provider reconcile + driver provision, fall through to the
		// shared bootstrap tail.
		bkc := specMetadataName(clusterYAML)
		if bkc == "" {
			ui.Errorf(stderr, "--bootstrap: cluster spec has no metadata.name (%s)", clusterYAML)
			return fmt.Errorf("bootstrap-only: no metadata.name in %s", clusterYAML)
		}
		if !fileExists(filepath.Join(d.Paths.Base, ".kubeconfig", bkc+".yaml")) {
			ui.Errorf(stderr, "--bootstrap needs an existing cluster (no .kubeconfig/%s.yaml — run a full 'lo provision' first)", bkc)
			return fmt.Errorf("bootstrap-only: cluster %s not provisioned", bkc)
		}
		ui.Debugf(stderr, "Re-applying spec.bootstrap on %s (skipping infra reconcile)", domainName)
	} else {
		// Provider loading rules (review A1: provider loading belongs to
		// the dispatch, not behind --remote):
		//   - lo: only relevant with --remote (provision a cloud VM that
		//     runs kind); plain local runs ignore it.
		//   - every other driver (kubeone, capi, kkp): load whenever
		//     spec.provider.name is set — these target real infrastructure.
		loadProvider := kind != "lo" || d.Remote
		if loadProvider {
			if name := ReadProviderName(clusterYAML); name != "" {
				cfg, cleanup, err := WriteProviderConfig(clusterYAML, stderr)
				if err != nil {
					return err
				}
				if cleanup != nil {
					defer cleanup()
				}
				prov, err := d.loadProvider(name)
				if err != nil {
					return err
				}
				if err := prov.Validate(ctx, cfg); err != nil {
					ui.Errorf(stderr, "Provider '%s' validation failed", name)
					return fmt.Errorf("provider %s validation failed: %w", name, err)
				}
				deps.Provider, deps.ProviderName, deps.ProviderConfigFile = prov, name, cfg
				ui.Debugf(stderr, "Provider '%s' loaded and validated", name)
			}
		}

		ui.Debugf(stderr, "Provisioning %s with kind=%s", domainName, kind)
		if err := drv.Provision(ctx, domainName); err != nil {
			// ErrFullLifecycle (bash rc 100) = driver handled the full
			// lifecycle (remote CI mode) — skip all local post-provision.
			if errors.Is(err, driver.ErrFullLifecycle) {
				ui.Debugf(stderr, "driver handled full lifecycle — skipping post-provision")
				return nil
			}
			return err
		}

		// Post-provision hook: driver SIDE-EFFECTS that need provisioned
		// infra (rare). Bootstrap ENV belongs in Export (below).
		if pp, ok := drv.(driver.PostProvisioner); ok {
			if err := pp.PostProvision(ctx, domainName); err != nil {
				return err
			}
		}
	}

	// Export: spec-derived env for spec.bootstrap addons. Runs on BOTH
	// paths — full provision AND --bootstrap — so a re-applied bootstrap
	// graph renders with the same env.
	if ex, ok := drv.(driver.Exporter); ok {
		if err := ex.Export(ctx, domainName); err != nil {
			return err
		}
	}

	// Tell bootstrap::apply which path this is (the KubeOne field-ownership
	// race guard — see the long comment in libs/provision).
	if bootstrapOnly {
		os.Setenv("LOK8S_BOOTSTRAP_ONLY", "1")
	} else {
		os.Setenv("LOK8S_BOOTSTRAP_ONLY", "0")
	}

	if d.Hooks.KubehzRegister != nil {
		if err := d.Hooks.KubehzRegister(ctx, domainName, clusterYAML); err != nil {
			return err
		}
	}

	bootstrapKubeconfig := filepath.Join(d.Paths.Base, ".kubeconfig", specMetadataName(clusterYAML)+".yaml")
	if d.Hooks.BootstrapApply != nil {
		if err := d.Hooks.BootstrapApply(ctx, domainName, clusterYAML, bootstrapKubeconfig); err != nil {
			return err
		}
	}

	if d.Hooks.InventoryPublish != nil {
		d.Hooks.InventoryPublish(ctx, domainName, clusterYAML, bootstrapKubeconfig)
	}

	info, _ := readSpecInfo(clusterYAML)
	if gp := info.Spec.Gitops.Provider; gp != "" && d.Hooks.GitopsBootstrap != nil {
		ui.Debugf(stderr, "GitOps provider found: %s, bootstrapping", gp)
		if err := d.Hooks.GitopsBootstrap(ctx, domainName, gp); err != nil {
			return err
		}
	}
	return nil
}

// DispatchDestroy destroys a domain's cluster (bash:
// provision::dispatch_destroy). A gate decline propagates
// driver.ErrDeclined (rc 3); a driver error whose OWN exit code is 3 is
// REMAPPED to exit code 1 — that value is the gate's decline sentinel, and
// a subprocess exit 3 inside the driver must not read as "operator said no"
// to the caller (which would suppress its orphaned-infra warning).
func (d *Dispatcher) DispatchDestroy(ctx context.Context, domainName string) error {
	stderr := d.errWriter()

	// Fail fast on a missing/bogus domain — same rule as Dispatch.
	spec, err := ResolveSpec(d.Paths, domainName, stderr)
	if err != nil {
		return err
	}

	if spec.Kind == SpecKindDeploy {
		ui.Errorf(stderr, "Cannot destroy a deployment domain. Destroy the cluster domain instead.")
		return fmt.Errorf("cannot destroy deploy domain %s", domainName)
	}

	clusterYAML := spec.File
	kind, err := ReadKind(clusterYAML, stderr)
	if err != nil {
		return fmt.Errorf("read kind: %w", err)
	}

	factory, ok := d.driverFactory(kind)
	if !ok {
		ui.Errorf(stderr, "Unknown cluster kind: %s", kind)
		return fmt.Errorf("unknown cluster kind %s", kind)
	}

	// Real-infrastructure gate — destroying a cloud cluster deprovisions
	// servers/LBs/volumes; confirm BEFORE deregistration or the driver run.
	if err := d.ConfirmInfra(domainName, clusterYAML, kind, ActionDestroy); err != nil {
		return err
	}

	deps := d.newDeps()
	drv, err := factory(deps)
	if err != nil {
		return err
	}

	// Provider only in remote mode (bash: destroy loads it for --remote;
	// note: load only, no validate — matching the bash destroy path).
	if d.Remote {
		if name := ReadProviderName(clusterYAML); name != "" {
			cfg, cleanup, err := WriteProviderConfig(clusterYAML, stderr)
			if err != nil {
				return err
			}
			if cleanup != nil {
				defer cleanup()
			}
			prov, err := d.loadProvider(name)
			if err != nil {
				return err
			}
			deps.Provider, deps.ProviderName, deps.ProviderConfigFile = prov, name, cfg
		}
	}

	// kubehz deregistration: best-effort HERE only — a dead platform api
	// must not block the driver teardown.
	if d.Hooks.KubehzDeregister != nil {
		if err := d.Hooks.KubehzDeregister(ctx, domainName, clusterYAML); err != nil {
			ui.Warnf(stderr, "kubehz deregistration failed — the platform registration survives this destroy; run 'lo kubehz deregister' afterwards")
		}
	}

	ui.Debugf(stderr, "Destroying %s with kind=%s", domainName, kind)
	if err := drv.Destroy(ctx, domainName); err != nil {
		// Remap a driver rc of 3 → 1 (see the function comment).
		if driver.ExitCode(err) == 3 {
			return &driver.ExitError{Code: 1, Err: err}
		}
		return err
	}
	return nil
}

// DispatchStatus prints a domain's cluster status word to Stdout (bash:
// provision::dispatch_status). Deployment domains follow their
// spec.clusterRef.domain to the referenced cluster. No gate.
func (d *Dispatcher) DispatchStatus(ctx context.Context, domainName string) error {
	stderr := d.errWriter()

	spec, err := ResolveSpec(d.Paths, domainName, stderr)
	if err != nil {
		return err
	}

	if spec.Kind == SpecKindDeploy {
		info, _ := readSpecInfo(spec.File)
		ref := info.Spec.ClusterRef.Domain
		if ref == "" {
			ui.Errorf(stderr, "Deployment domain missing spec.clusterRef.domain")
			return fmt.Errorf("deploy domain %s missing clusterRef", domainName)
		}
		ui.Debugf(stderr, "Deployment domain %s references cluster %s", domainName, ref)
		return d.DispatchStatus(ctx, ref)
	}

	kind, err := ReadKind(spec.File, stderr)
	if err != nil {
		return fmt.Errorf("read kind: %w", err)
	}

	factory, ok := d.driverFactory(kind)
	if !ok {
		ui.Errorf(stderr, "Unknown cluster kind: %s", kind)
		return fmt.Errorf("unknown cluster kind %s", kind)
	}
	drv, err := factory(d.newDeps())
	if err != nil {
		return err
	}
	status, err := drv.Status(ctx, domainName)
	if err != nil {
		return err
	}
	fmt.Fprintln(d.outWriter(), status)
	return nil
}

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
	"github.com/kernpilot/lok8s/internal/fsutil"
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
	// ConfirmCluster is `lo destroy --confirm-cluster <name>`: the answer to
	// the cluster_name guard off a terminal (destroy_guard.go). Force does
	// not replace it.
	ConfirmCluster string
	// Interactive overrides the tty/CI/LOK8S_NONINTERACTIVE detection
	// (tests stub it exactly like the bats redefine
	// provision::_interactive).
	Interactive func() bool

	// BootstrapDryRun is `--bootstrap --dry-run`: the bootstrap hook
	// renders and diffs but applies nothing, so the run is read-only. The
	// real-infrastructure gate is not asked and the tail's writers (the
	// kubehz registration, the inventory, the gitops bootstrap) are
	// skipped. Only honoured with bootstrapOnly.
	BootstrapDryRun bool
	// BootstrapAllowGenerators is `--allow-generators` alongside
	// BootstrapDryRun — passed through to runTail's SetDryRunEnv so a
	// bash: secrets generator or a non-builtin exec plugin can run during
	// Export too, under the same flag that lets the bootstrap hook itself
	// run one. No effect without BootstrapDryRun.
	BootstrapAllowGenerators bool

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
func (d *Dispatcher) loadProvider(ctx context.Context, name string) (driver.Provider, error) {
	if d.Providers == nil {
		ui.ErrorTo(d.errWriter(), "provider '%s' not found at %s", name,
			filepath.Join(d.Paths.Lok8s, "providers", name, "main"))
		return nil, ui.Handled(fmt.Errorf("provider %s not found", name))
	}
	return d.Providers.Load(ctx, name)
}

// Dispatch provisions a domain (bash: provision::dispatch). bootstrapOnly
// re-applies spec.bootstrap on an ALREADY-provisioned cluster, skipping the
// provider reconcile + driver provision entirely.
//
// Flow, in the bash order: resolve spec FIRST (fail fast, before any driver
// touches infra) → load provider creds → deploy-domain refusal → read kind
// (never defaulted when malformed) → driver-existence check → the
// real-infrastructure gate (a decline propagates driver.ErrDeclined) →
// [provider load+validate] → driver construction → [bootstrap-only guard |
// driver.Provision (ErrFullLifecycle → success, skip the tail) →
// PostProvision hook] → Export hook → LOK8S_BOOTSTRAP_ONLY export → kubehz
// registration hook → bootstrap hook → inventory hook (fail-soft) → gitops
// hook. The provider loads before the driver is built (bash sourced the
// driver file first, which prints nothing), so the driver's Deps are
// complete from construction on and never change afterwards.
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
		ui.ErrorTo(stderr, "Cannot provision a deployment domain. Use 'lo deploy %s' instead.", domainName)
		ui.ErrorTo(stderr, "Deployment domains reference a cluster via spec.clusterRef.domain.")
		return ui.Handled(fmt.Errorf("cannot provision deploy domain %s", domainName))
	}

	clusterYAML := spec.File
	kind, err := ReadKind(clusterYAML, stderr)
	if err != nil {
		return fmt.Errorf("read kind: %w", err)
	}

	factory, ok := d.driverFactory(kind)
	if !ok {
		ui.ErrorTo(stderr, "Unknown cluster kind: %s (missing %s)", kind,
			filepath.Join(d.Paths.Lok8s, "drivers", kind, "main"))
		return ui.Handled(fmt.Errorf("unknown cluster kind %s", kind))
	}

	// Real-infrastructure gate: summary + confirmation BEFORE the provider
	// or driver touch anything (after the kind check, so a bogus kind stays
	// a plain error). A decline propagates its sentinel. A bootstrap dry
	// run changes nothing, so it is not gated.
	dryRun := bootstrapOnly && d.BootstrapDryRun
	gateAction := ActionReconcile
	if bootstrapOnly {
		gateAction = ActionBootstrap
	}
	if !dryRun {
		if err := d.ConfirmInfra(domainName, clusterYAML, kind, gateAction); err != nil {
			return err
		}
	}

	deps := d.newDeps()
	if !bootstrapOnly {
		cleanup, err := d.loadSpecProvider(ctx, kind, clusterYAML, deps)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
	}

	// Deps are complete: the driver sees the provider from here on.
	drv, err := factory(deps)
	if err != nil {
		return err
	}

	if bootstrapOnly {
		if err := d.bootstrapOnlyGuard(clusterYAML, domainName); err != nil {
			return err
		}
	} else {
		ui.DebugTo(stderr, "Provisioning %s with kind=%s", domainName, kind)
		if err := drv.Provision(ctx, domainName); err != nil {
			// ErrFullLifecycle (bash rc 100) = driver handled the full
			// lifecycle (remote CI mode) — skip all local post-provision.
			if errors.Is(err, driver.ErrFullLifecycle) {
				ui.DebugTo(stderr, "driver handled full lifecycle — skipping post-provision")
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
	return d.runTail(ctx, drv, domainName, clusterYAML, bootstrapOnly, dryRun)
}

// loadSpecProvider loads and validates the spec's provider into deps for
// the provision path. Provider loading rules (review A1: provider loading
// belongs to the dispatch, not behind --remote):
//   - lo: only relevant with --remote (provision a cloud VM that runs
//     kind); plain local runs ignore it.
//   - every other driver (kubeone, capi, kkp): load whenever
//     spec.provider.name is set — these target real infrastructure.
//
// cleanup removes an inline-config temp file (nil otherwise).
func (d *Dispatcher) loadSpecProvider(ctx context.Context, kind, clusterYAML string, deps *driver.Deps) (cleanup func(), err error) {
	stderr := d.errWriter()
	if kind == "lo" && !d.Remote {
		return nil, nil
	}
	name := ReadProviderName(clusterYAML)
	if name == "" {
		return nil, nil
	}
	cfg, cleanup, err := WriteProviderConfig(clusterYAML, stderr)
	if err != nil {
		return nil, err
	}
	prov, err := d.loadProvider(ctx, name)
	if err != nil {
		return cleanup, err
	}
	if err := prov.Validate(ctx, cfg); err != nil {
		ui.ErrorTo(stderr, "Provider '%s' validation failed", name)
		return cleanup, ui.Handled(fmt.Errorf("provider %s validation failed: %w", name, err))
	}
	deps.Provider, deps.ProviderName, deps.ProviderConfigFile = prov, name, cfg
	ui.DebugTo(stderr, "Provider '%s' loaded and validated", name)
	return cleanup, nil
}

// bootstrapOnlyGuard is the --bootstrap precondition: re-apply
// spec.bootstrap on an ALREADY-provisioned cluster, skipping the provider
// reconcile + driver provision, then fall through to the shared tail.
func (d *Dispatcher) bootstrapOnlyGuard(clusterYAML, domainName string) error {
	stderr := d.errWriter()
	bkc := specMetadataName(clusterYAML)
	if bkc == "" {
		ui.ErrorTo(stderr, "--bootstrap: cluster spec has no metadata.name (%s)", clusterYAML)
		return ui.Handled(fmt.Errorf("bootstrap-only: no metadata.name in %s", clusterYAML))
	}
	if !fsutil.FileExists(filepath.Join(d.Paths.Base, ".kubeconfig", bkc+".yaml")) {
		ui.ErrorTo(stderr, "--bootstrap needs an existing cluster (no .kubeconfig/%s.yaml — run a full 'lo provision' first)", bkc)
		return ui.Handled(fmt.Errorf("bootstrap-only: cluster %s not provisioned", bkc))
	}
	ui.DebugTo(stderr, "Re-applying spec.bootstrap on %s (skipping infra reconcile)", domainName)
	return nil
}

// runTail is the shared post-provision tail: Export → LOK8S_BOOTSTRAP_ONLY
// → kubehz registration → bootstrap → inventory (fail-soft) → gitops. A
// bootstrap dry run keeps only Export and the bootstrap hook: every other
// step writes somewhere.
func (d *Dispatcher) runTail(ctx context.Context, drv driver.Driver, domainName, clusterYAML string, bootstrapOnly, dryRun bool) error {
	stderr := d.errWriter()

	// Item D: live BEFORE Export, not after — Export can write project
	// state of its own (the Lo driver's .registries.json) gated on this
	// exact sentinel, and that write happens here, well before the
	// bootstrap hook (which used to be the only place this was set) ever
	// runs. Duplicated from bootstrap.SetDryRunEnv rather than imported:
	// this package (kubeconfig.go) is imported by internal/build, which
	// internal/bootstrap (via internal/addons) imports — importing
	// bootstrap here would cycle straight back through build.
	defer setDryRunEnv(dryRun, d.BootstrapAllowGenerators)()

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

	if d.Hooks.KubehzRegister != nil && !dryRun {
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
	if dryRun {
		return nil
	}

	if d.Hooks.InventoryPublish != nil {
		d.Hooks.InventoryPublish(ctx, domainName, clusterYAML, bootstrapKubeconfig)
	}

	info, _ := readSpecInfo(clusterYAML)
	if gp := info.Spec.Gitops.Provider; gp != "" && d.Hooks.GitopsBootstrap != nil {
		ui.DebugTo(stderr, "GitOps provider found: %s, bootstrapping", gp)
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
	// Only the guard below sets the confirmed plan: an inherited value
	// (another process, a hand-set variable) must never reach the
	// provider's destroy.
	os.Unsetenv(driver.DestroyPlanEnv)

	// Fail fast on a missing/bogus domain — same rule as Dispatch.
	spec, err := ResolveSpec(d.Paths, domainName, stderr)
	if err != nil {
		return err
	}

	if spec.Kind == SpecKindDeploy {
		ui.ErrorTo(stderr, "Cannot destroy a deployment domain. Destroy the cluster domain instead.")
		return ui.Handled(fmt.Errorf("cannot destroy deploy domain %s", domainName))
	}

	clusterYAML := spec.File
	kind, err := ReadKind(clusterYAML, stderr)
	if err != nil {
		return fmt.Errorf("read kind: %w", err)
	}

	factory, ok := d.driverFactory(kind)
	if !ok {
		ui.ErrorTo(stderr, "Unknown cluster kind: %s", kind)
		return ui.Handled(fmt.Errorf("unknown cluster kind %s", kind))
	}

	// Provider only in remote mode (bash: destroy loads it for --remote;
	// note: load only, no validate — matching the bash destroy path).
	// Loaded before the driver is built, so its Deps are complete, and
	// before the gate, which asks the provider what it would delete.
	deps := d.newDeps()
	if d.Remote {
		if name := ReadProviderName(clusterYAML); name != "" {
			cfg, cleanup, err := WriteProviderConfig(clusterYAML, stderr)
			if err != nil {
				return err
			}
			if cleanup != nil {
				defer cleanup()
			}
			prov, err := d.loadProvider(ctx, name)
			if err != nil {
				return err
			}
			deps.Provider, deps.ProviderName, deps.ProviderConfigFile = prov, name, cfg
		}
	}

	// Real-infrastructure gate — destroying a cloud cluster deprovisions
	// servers/LBs/volumes; confirm BEFORE deregistration or the driver run.
	// A provider that deletes by cluster_name gets the cluster_name guard
	// instead (destroy_guard.go).
	guarded, skip, err := d.guardDestroy(ctx, domainName, kind, clusterYAML, deps)
	defer os.Unsetenv(driver.DestroyPlanEnv)
	if err != nil {
		return err
	}
	if !guarded {
		// --confirm-cluster names a cluster_name that no guard checks: the
		// destroy must not run as if it were confirmed (without -r, a forced
		// kubeone destroy resets the nodes and leaves the servers up).
		if d.ConfirmCluster != "" {
			ui.ErrorTo(stderr, "--confirm-cluster '%s' is set, but the destroy of '%s' checks no cluster_name: %s. Nothing was deleted. Add -r for a provider cluster, or remove --confirm-cluster.", d.ConfirmCluster, domainName, skip)
			return ui.Handled(fmt.Errorf("destroy %s: --confirm-cluster without a cluster_name guard", domainName))
		}
		if err := d.ConfirmInfra(domainName, clusterYAML, kind, ActionDestroy); err != nil {
			return err
		}
	}
	drv, err := factory(deps)
	if err != nil {
		return err
	}

	// kubehz deregistration: best-effort HERE only — a dead platform api
	// must not block the driver teardown.
	if d.Hooks.KubehzDeregister != nil {
		if err := d.Hooks.KubehzDeregister(ctx, domainName, clusterYAML); err != nil {
			ui.WarnTo(stderr, "kubehz deregistration failed — the platform registration survives this destroy; run 'lo kubehz deregister' afterwards")
		}
	}

	ui.DebugTo(stderr, "Destroying %s with kind=%s", domainName, kind)
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
			ui.ErrorTo(stderr, "Deployment domain missing spec.clusterRef.domain")
			return ui.Handled(fmt.Errorf("deploy domain %s missing clusterRef", domainName))
		}
		ui.DebugTo(stderr, "Deployment domain %s references cluster %s", domainName, ref)
		return d.DispatchStatus(ctx, ref)
	}

	kind, err := ReadKind(spec.File, stderr)
	if err != nil {
		return fmt.Errorf("read kind: %w", err)
	}

	factory, ok := d.driverFactory(kind)
	if !ok {
		ui.ErrorTo(stderr, "Unknown cluster kind: %s", kind)
		return ui.Handled(fmt.Errorf("unknown cluster kind %s", kind))
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

// setDryRunEnv scopes LOK8S_BOOTSTRAP_DRYRUN/LOK8S_BOOTSTRAP_ALLOW_GENERATORS
// for the caller's defer — the same contract as bootstrap.SetDryRunEnv,
// duplicated here rather than imported (see runTail's comment on why).
func setDryRunEnv(dryRun, allowGenerators bool) (restore func()) {
	dv := ""
	if dryRun {
		dv = "1"
	}
	av := ""
	if dryRun && allowGenerators {
		av = "1"
	}
	r1 := scopedSetEnv("LOK8S_BOOTSTRAP_DRYRUN", dv)
	r2 := scopedSetEnv("LOK8S_BOOTSTRAP_ALLOW_GENERATORS", av)
	return func() { r2(); r1() }
}

// scopedSetEnv sets key to value (or unsets it, for "") for the duration of
// the call, returning a restore func that puts the PRIOR value back.
func scopedSetEnv(key, value string) (restore func()) {
	prev, had := os.LookupEnv(key)
	if value == "" {
		os.Unsetenv(key)
	} else {
		os.Setenv(key, value)
	}
	return func() {
		if had {
			os.Setenv(key, prev)
		} else {
			os.Unsetenv(key)
		}
	}
}

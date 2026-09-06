package cli

// dispatch.go — the fully-wired provision.Dispatcher behind the
// orchestration commands (up/provision/destroy/down/status/bootstrap), the
// ambient-env helpers they share, and the exit-code mapping.
//
// Hook wiring (bash: the libs `lo` imports, probed with `declare -f` by the
// dispatch tail):
//
//	KubehzRegister/Deregister  internal/kubehz   (kubehz::register/deregister_cluster)
//	BootstrapApply             internal/bootstrap (bootstrap::apply)
//	InventoryPublish           internal/inventory (inventory::publish, fail-soft)
//	GitopsBootstrap            internal/gitops    (gitops::bootstrap, warn-only)
//	kubeone/capi Hooks         kubehz.KubeoneHooks()/CapiHooks() + the bridged
//	                           hetzner-inventory seams (provider/bridge)
//	lo Hooks.KustomizeBuild    kustomizeBuild (kustomize::build)
//	Providers                  provider/bridge.Loader (the bash plugins)

import (
	"context"
	"os"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/driver/capi"
	"github.com/kernpilot/lok8s/internal/driver/kubeone"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/gitops"
	"github.com/kernpilot/lok8s/internal/inventory"
	"github.com/kernpilot/lok8s/internal/kubehz"
	"github.com/kernpilot/lok8s/internal/provider/bridge"
	"github.com/kernpilot/lok8s/internal/provision"
)

// newRunner is the exec seam of the orchestration commands (tests swap in a
// fake; nothing under test may reach docker/kind/tilt).
var newRunner = execx.NewRunner

// osExit is the process-exit seam for the rc passthroughs (a gate decline's
// 3, `tilt ci`'s own status, a subprocess code).
var osExit = os.Exit

// ambientMain replays the entrypoint's pre-dispatch exports (ambientMainEnv)
// and additionally resolves the `cluster` local the bash main derived
// (spec metadata.name > --cluster > LOK8S_CLUSTER_NAME > "local") — the
// name `lo down`/`lo clean` act on.
func ambientMain(cmd *cobra.Command, paths *config.Paths) (domainName, cluster string) {
	d := ambientMainEnv(cmd, paths)
	clusterFlag := ""
	if f := cmd.Flags().Lookup("cluster"); f != nil && f.Changed {
		clusterFlag = f.Value.String()
	}
	return d, build.AmbientClusterName(paths, d, clusterFlag)
}

// newDispatcher builds the dispatcher with every seam wired (see the file
// comment). Streams are the command's; Force/Remote are the global flags.
func newDispatcher(cmd *cobra.Command, paths *config.Paths) *provision.Dispatcher {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	runner := newRunner(paths)
	force, _ := cmd.Flags().GetBool("force")
	remote, _ := cmd.Flags().GetBool("remote")

	kc := kubehzContext(cmd, paths)
	kc.Runner = runner
	loader := &bridge.Loader{Paths: paths, Runner: runner, Stdout: stdout, Stderr: stderr}

	hooks := kc.ProvisionHooks()
	hooks.BootstrapApply = bootstrap.ApplyHook(paths, runner, stdout, stderr)
	hooks.InventoryPublish = inventory.PublishHook(paths, runner, stderr)
	hooks.GitopsBootstrap = gitops.BootstrapHook(stderr)

	return &provision.Dispatcher{
		Paths:     paths,
		Runner:    runner,
		Stdout:    stdout,
		Stderr:    stderr,
		In:        cmd.InOrStdin(),
		Force:     force,
		Remote:    remote,
		Drivers:   wiredDrivers(paths, kc, loader),
		Providers: loader,
		Hooks:     hooks,
	}
}

// wiredDrivers wraps the driver registry so every constructed driver gets
// its seams: the kubehz hosted branches (kubeone/capi), the bridged
// hetzner-inventory seams (kubeone), the on-demand secrets-plugin build
// (lo). The kubehz context's ProviderOutput seam is bound to the SAME
// deps the dispatch fills after construction, so the kubeone fingerprint
// reader sees the provider once it is loaded — the bash `declare -F
// provider::output` probe, evaluated at call time.
func wiredDrivers(paths *config.Paths, kc *kubehz.Context, loader *bridge.Loader) func(string) (driver.Factory, bool) {
	return func(name string) (driver.Factory, bool) {
		f, ok := driver.Get(name)
		if !ok {
			return nil, false
		}
		return func(deps *driver.Deps) (driver.Driver, error) {
			drv, err := f(deps)
			if err != nil {
				return nil, err
			}
			kc.ProviderOutput = func(ctx context.Context) ([]byte, error) {
				if deps.Provider == nil {
					return nil, bridge.ErrNoProvider
				}
				return deps.Provider.Output(ctx, deps.ProviderConfigFile)
			}
			switch d := drv.(type) {
			case *kubeone.Driver:
				d.Hooks = kc.KubeoneHooks()
				d.Hooks.AppendInventory = loader.KubeoneAppendInventory(deps)
				d.Hooks.PrepareApply = loader.KubeonePrepareApply(deps)
			case *capi.Driver:
				d.Hooks = kc.CapiHooks()
			case *lodriver.Driver:
				d.Hooks.KustomizeBuild = func(context.Context) error { return kustomizeBuild(paths) }
			}
			return drv, nil
		}, true
	}
}

// argshFlagErrors makes a cobra flag-parse failure print in the argsh
// parse-error shape (`Error: unknown flag: --x` + the `Run "lo -h"` hint)
// instead of cobra's bare line. The rc stays 1 where argsh exits 2 — the
// documented divergence every ported command shares (cmd_deploy.go).
// Subcommands inherit it (cobra walks parents for the flag-error func).
func argshFlagErrors(cmd *cobra.Command) *cobra.Command {
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return argshErrorf(c.ErrOrStderr(), "%s", argshFlagError(c, err.Error()))
	})
	return cmd
}

var (
	pflagUnknownShorthandRe  = regexp.MustCompile(`^unknown shorthand flag: '(.)' in -\S+$`)
	pflagNeedsArgShorthandRe = regexp.MustCompile(`^flag needs an argument: '(.)' in -\S+$`)
)

// argshFlagError rewrites pflag's SHORTHAND messages into argsh's: `unknown
// shorthand flag: 'x' in -x` → `unknown flag: -x`, `flag needs an argument:
// 'n' in -n` → `missing value for flag: <long name>`. The long-form messages
// already match, so they pass through.
func argshFlagError(c *cobra.Command, msg string) string {
	if m := pflagUnknownShorthandRe.FindStringSubmatch(msg); m != nil {
		return "unknown flag: -" + m[1]
	}
	if m := pflagNeedsArgShorthandRe.FindStringSubmatch(msg); m != nil {
		if f := c.Flags().ShorthandLookup(m[1]); f != nil {
			return "missing value for flag: " + f.Name
		}
	}
	return msg
}

// dispatchExit maps a dispatch/driver error onto the process contract the
// bash produced: every diagnostic was already printed where it happened
// (the dispatcher, the driver, or the failing tool's own stderr — the bash
// never added a line on top), so a plain failure exits 1 silently; a
// carried rc (gate decline 3, remote-CI 100, an explicit ExitError, a
// subprocess status) passes through — the `lo drivers` precedent.
func dispatchExit(err error) error {
	if err == nil {
		return nil
	}
	if rc := driver.ExitCode(err); rc != 1 {
		osExit(rc)
	}
	return ErrHandled
}

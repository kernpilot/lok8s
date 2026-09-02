package cli

// lo provision / lo destroy / lo bootstrap — the cluster-lifecycle
// dispatch entrypoints. Go port of main::provision (.lok8s/libs/provision),
// main::destroy (.lok8s/libs/deploy) and main::bootstrap
// (.lok8s/libs/bootstrap); the dispatch itself is internal/provision, the
// standalone bootstrap core internal/bootstrap.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/inventory"
)

func init() {
	registerPorted("provision", newProvisionCommand)
	registerPorted("destroy", newDestroyCommand)
	registerPorted("bootstrap", newBootstrapCommand)
}

func newProvisionCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var bootstrapOnly bool
	cmd := &cobra.Command{
		Use:          "provision",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			if len(args) > 0 {
				return argshErrorf(stderr, "too many arguments: %s", args[0])
			}
			d := ambientMainEnv(cmd, paths)
			// Same reason as `lo build`: the domain can come from
			// clusters/.active — state a `lo use` persisted hours ago — and
			// provision reconciles THAT domain's infrastructure. Cloud
			// drivers additionally gate on a confirmation, but the local
			// kind path is deliberately frictionless and would otherwise
			// never say which domain it acted on.
			fmt.Fprintf(stderr, "lo provision: domain %s\n", d)
			return dispatchExit(newDispatcher(cmd, paths).Dispatch(cmd.Context(), d, bootstrapOnly))
		},
	}
	cmd.Flags().BoolVarP(&bootstrapOnly, "bootstrap", "b", false, "Re-apply spec.bootstrap only on an existing cluster (skip the infra reconcile)")
	return argshFlagErrors(cmd)
}

func newDestroyCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return argshFlagErrors(&cobra.Command{
		Use:          "destroy",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "too many arguments: %s", args[0])
			}
			d := ambientMainEnv(cmd, paths)
			return dispatchExit(newDispatcher(cmd, paths).DispatchDestroy(cmd.Context(), d))
		},
	})
}

func newBootstrapCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return argshFlagErrors(&cobra.Command{
		Use:          "bootstrap",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if len(args) > 0 {
				return argshErrorf(stderr, "too many arguments: %s", args[0])
			}
			d := ambientMainEnv(cmd, paths)
			runner := newRunner(paths)
			disp := newDispatcher(cmd, paths)
			bd := &bootstrap.Dispatcher{
				Engine:           &bootstrap.Engine{Paths: paths, Runner: runner, Stdout: stdout, Stderr: stderr},
				Drivers:          disp.Drivers,
				InventoryPublish: inventory.PublishHook(paths, runner, stderr),
			}
			return dispatchExit(bd.Dispatch(cmd.Context(), d))
		},
	})
}

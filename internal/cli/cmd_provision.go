package cli

// lo provision / lo destroy / lo bootstrap — the cluster-lifecycle
// dispatch entrypoints. Go port of main::provision (.lok8s/libs/provision),
// main::destroy (.lok8s/libs/deploy) and main::bootstrap
// (.lok8s/libs/bootstrap); the dispatch itself is internal/provision, the
// standalone bootstrap core internal/bootstrap.

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/inventory"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() {
	registerPorted("provision", newProvisionCommand)
	registerPorted("destroy", newDestroyCommand)
	registerPorted("bootstrap", newBootstrapCommand)
}

// bootstrapModeFlags are the run-mode flags `lo bootstrap` and
// `lo provision --bootstrap` share (bash: the same four args in
// main::bootstrap and main::provision).
type bootstrapModeFlags struct {
	dryRun    bool
	renderDir string
	noDiff    bool
	only      []string
}

func (f *bootstrapModeFlags) bind(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Render every entry and apply nothing: diff each one against the cluster (exit 0 unchanged, 1 changes, 2 error)")
	cmd.Flags().StringVar(&f.renderDir, "render-dir", "", "Write the dry run's manifests (NN-<entry>.yaml) and diffs (NN-<entry>.diff) into this directory")
	cmd.Flags().BoolVar(&f.noDiff, "no-diff", false, "Dry run without the kubectl diff: render only, no cluster access")
	cmd.Flags().StringSliceVar(&f.only, "only", nil, "Apply (or dry-run) only these spec.bootstrap entries, by entry name or addon name, comma-separated; dependsOn is not expanded")
}

// options validates the combination and returns the engine's Options. A
// flag that needs another is the bash `error` line + rc 1.
func (f *bootstrapModeFlags) options(stderr io.Writer, bootstrapOnly bool) (bootstrap.Options, error) {
	for _, dep := range []struct {
		set  bool
		flag string
	}{{f.dryRun, "--dry-run"}, {f.renderDir != "", "--render-dir"}, {f.noDiff, "--no-diff"}, {len(f.only) > 0, "--only"}} {
		if dep.set && !bootstrapOnly {
			ui.ErrorTo(stderr, "%s requires --bootstrap", dep.flag)
			return bootstrap.Options{}, ErrHandled
		}
	}
	for _, dep := range []struct {
		set  bool
		flag string
	}{{f.renderDir != "", "--render-dir"}, {f.noDiff, "--no-diff"}} {
		if dep.set && !f.dryRun {
			ui.ErrorTo(stderr, "%s requires --dry-run", dep.flag)
			return bootstrap.Options{}, ErrHandled
		}
	}
	opts := bootstrap.Options{DryRun: f.dryRun, RenderDir: f.renderDir, NoDiff: f.noDiff}
	for _, name := range f.only {
		if name = strings.TrimSpace(name); name != "" {
			opts.Only = append(opts.Only, name)
		}
	}
	return opts, nil
}

func newProvisionCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var bootstrapOnly bool
	var mode bootstrapModeFlags
	cmd := &cobra.Command{
		Use:          "provision",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         argshNoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			opts, err := mode.options(stderr, bootstrapOnly)
			if err != nil {
				return err
			}
			d := ambientMainEnv(cmd, paths)
			// Same reason as `lo build`: the domain can come from
			// clusters/.active — state a `lo use` persisted hours ago — and
			// provision reconciles THAT domain's infrastructure. Cloud
			// drivers additionally gate on a confirmation, but the local
			// kind path is deliberately frictionless and would otherwise
			// never say which domain it acted on.
			fmt.Fprintf(stderr, "lo provision: domain %s\n", d)
			disp := newDispatcher(cmd, paths)
			disp.Hooks.BootstrapApply = bootstrap.ApplyHook(paths, disp.Runner, disp.Stdout, disp.Stderr, opts)
			disp.BootstrapDryRun = opts.DryRun
			return dispatchExit(cmd.ErrOrStderr(), disp.Dispatch(cmd.Context(), d, bootstrapOnly))
		},
	}
	cmd.Flags().BoolVarP(&bootstrapOnly, "bootstrap", "b", false, "Re-apply spec.bootstrap only on an existing cluster (skip the infra reconcile)")
	mode.bind(cmd)
	return argshFlagErrors(cmd)
}

func newDestroyCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return argshFlagErrors(&cobra.Command{
		Use:          "destroy",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         argshNoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			return dispatchExit(cmd.ErrOrStderr(), newDispatcher(cmd, paths).DispatchDestroy(cmd.Context(), d))
		},
	})
}

func newBootstrapCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var mode bootstrapModeFlags
	cmd := &cobra.Command{
		Use:          "bootstrap",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         argshNoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			opts, err := mode.options(stderr, true)
			if err != nil {
				return err
			}
			d := ambientMainEnv(cmd, paths)
			runner := newRunner(paths)
			disp := newDispatcher(cmd, paths)
			bd := &bootstrap.Dispatcher{
				Engine:           &bootstrap.Engine{Paths: paths, Runner: runner, Stdout: stdout, Stderr: stderr, Options: opts},
				Drivers:          disp.Drivers,
				InventoryPublish: inventory.PublishHook(paths, runner, stderr),
			}
			return dispatchExit(cmd.ErrOrStderr(), bd.Dispatch(cmd.Context(), d))
		},
	}
	mode.bind(cmd)
	return argshFlagErrors(cmd)
}

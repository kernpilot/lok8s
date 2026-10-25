package cli

// lo provision / lo destroy / lo bootstrap — the cluster-lifecycle
// dispatch entrypoints. Go port of main::provision (.lok8s/libs/provision),
// main::destroy (.lok8s/libs/deploy) and main::bootstrap
// (.lok8s/libs/bootstrap); the dispatch itself is internal/provision, the
// standalone bootstrap core internal/bootstrap.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
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
	dryRun          bool
	renderDir       string
	noDiff          bool
	only            []string
	allowGenerators bool
	withRestore     bool
	ignoreNondet    bool
}

func (f *bootstrapModeFlags) bind(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Render every entry and apply nothing: diff each one against the cluster (exit 0 unchanged, 1 changes, 2 error)")
	cmd.Flags().StringVar(&f.renderDir, "render-dir", "", "Write the dry run's manifests (NN-<entry>.yaml) and diffs (NN-<entry>.diff) into this directory")
	cmd.Flags().BoolVar(&f.noDiff, "no-diff", false, "Dry run without the kubectl diff: render only, no cluster access")
	cmd.Flags().StringSliceVar(&f.only, "only", nil, "Apply (or dry-run) only these spec.bootstrap entries, comma-separated. A value selects every entry whose name (name:, else the last path element), key as written in spec.bootstrap (./targets/flux) or directory name (of an addon or a target) is equal to it: --only rook-ceph also selects ./targets/rook-ceph named rook-ceph-cluster. dependsOn is not expanded")
	cmd.Flags().BoolVar(&f.allowGenerators, "allow-generators", false, "Run the addon's own executable code in a dry run (exec plugins, KRM functions, bash: generators, remote bases) as trusted, with a real run's side effects (default: refused)")
	cmd.Flags().BoolVar(&f.withRestore, "with-restore", false, "With --only, also apply restore.d first (default: --only skips it and warns when it is non-empty)")
	cmd.Flags().BoolVar(&f.ignoreNondet, "ignore-nondeterministic", false, "Dry run: judge each entry without the objects its chart regenerates on every apply (they are still listed)")
}

// options validates the combination and returns the engine's Options. A
// flag that needs another is the bash `error` line + rc 1. onlyChanged is
// cmd.Flags().Changed("only") (review round 2, item E): pflag's
// StringSliceVar parses an EMPTY value (`--only ""`) through a CSV reader
// that returns ZERO elements for an empty string, so f.only alone cannot
// tell an empty "--only" value apart from "--only never given" — only Changed can.
func (f *bootstrapModeFlags) options(stderr io.Writer, bootstrapOnly, onlyChanged bool) (bootstrap.Options, error) {
	for _, dep := range []struct {
		set  bool
		flag string
	}{{f.dryRun, "--dry-run"}, {f.renderDir != "", "--render-dir"}, {f.noDiff, "--no-diff"}, {onlyChanged, "--only"}, {f.allowGenerators, "--allow-generators"}, {f.withRestore, "--with-restore"}, {f.ignoreNondet, "--ignore-nondeterministic"}} {
		if dep.set && !bootstrapOnly {
			ui.ErrorTo(stderr, "%s requires --bootstrap", dep.flag)
			return bootstrap.Options{}, ErrHandled
		}
	}
	for _, dep := range []struct {
		set  bool
		flag string
	}{{f.renderDir != "", "--render-dir"}, {f.noDiff, "--no-diff"}, {f.allowGenerators, "--allow-generators"}, {f.ignoreNondet, "--ignore-nondeterministic"}} {
		if dep.set && !f.dryRun {
			ui.ErrorTo(stderr, "%s requires --dry-run", dep.flag)
			return bootstrap.Options{}, ErrHandled
		}
	}
	if f.withRestore && !onlyChanged {
		ui.ErrorTo(stderr, "--with-restore requires --only")
		return bootstrap.Options{}, ErrHandled
	}
	opts := bootstrap.Options{DryRun: f.dryRun, RenderDir: f.renderDir, NoDiff: f.noDiff, AllowGenerators: f.allowGenerators, WithRestore: f.withRestore, IgnoreNondeterministic: f.ignoreNondet}
	for _, name := range f.only {
		if name = strings.TrimSpace(name); name != "" {
			opts.Only = append(opts.Only, name)
		}
	}
	// --only was GIVEN (cmd.Flags().Changed — see onlyChanged's doc above)
	// but every value trimmed to nothing (`--only ''`, `--only ' '`,
	// `--only ,`): that is an operator error, never "no filter" — the
	// latter is what an ABSENT --only means, and the two must not silently
	// collapse into the same full, unfiltered apply.
	if onlyChanged && len(opts.Only) == 0 {
		ui.ErrorTo(stderr, "--only: no entry names given")
		return bootstrap.Options{}, ErrHandled
	}
	return opts, nil
}

// exit maps the command's result onto the process exit code. A dry run has
// its own contract at this boundary, for `lo bootstrap` and `lo provision
// --bootstrap` alike: 0 when nothing would change, 1 ONLY when an entry's
// diff found a change (bootstrap.ErrDryRunDiffers), and 2 for every other
// failure, before or outside the per-entry diff (a flag combination, a
// missing spec or kubeconfig, a driver export, the render boundary): a
// caller that gates a change window on rc 1 must never read an error as
// "changes". An interrupt keeps its 128+n code (main maps it).
func (f *bootstrapModeFlags) exit(stderr io.Writer, err error) error {
	if !f.dryRun || err == nil || errors.Is(err, bootstrap.ErrDryRunDiffers) || errors.Is(err, context.Canceled) {
		return dispatchExit(stderr, err)
	}
	if !errors.Is(err, ErrHandled) && !isChildExit(err) {
		ui.ErrorTo(stderr, "%v", err)
	}
	exitNow(2)
	return ErrHandled
}

// parseErrors is argshFlagErrors and argshNoArgs with the dry-run exit: a
// parse error of a command line that asks for --dry-run exits 2 (bash: the
// argsh parser exits 2 on every parse error), never 1.
func (f *bootstrapModeFlags) parseErrors(cmd *cobra.Command) *cobra.Command {
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		herr := argshErrorf(c.ErrOrStderr(), "%s", argshFlagError(c, err.Error()))
		if argvAsksDryRun(os.Args[1:]) {
			exitNow(2)
		}
		return herr
	})
	args := cmd.Args
	cmd.Args = func(c *cobra.Command, a []string) error {
		err := args(c, a)
		if err != nil && f.dryRun {
			exitNow(2)
		}
		return err
	}
	return cmd
}

// argvAsksDryRun reports a --dry-run among the raw arguments (up to `--`).
// A flag parse error stops pflag before it reads every flag, so the parsed
// value cannot answer this.
func argvAsksDryRun(argv []string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		if a == "--dry-run" {
			return true
		}
		if v, ok := strings.CutPrefix(a, "--dry-run="); ok {
			b, err := strconv.ParseBool(v)
			return err != nil || b
		}
	}
	return false
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
			opts, err := mode.options(stderr, bootstrapOnly, cmd.Flags().Changed("only"))
			if err != nil {
				return mode.exit(stderr, err)
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
			disp.BootstrapAllowGenerators = opts.AllowGenerators
			return mode.exit(cmd.ErrOrStderr(), disp.Dispatch(cmd.Context(), d, bootstrapOnly))
		},
	}
	cmd.Flags().BoolVarP(&bootstrapOnly, "bootstrap", "b", false, "Re-apply spec.bootstrap only on an existing cluster (skip the infra reconcile)")
	mode.bind(cmd)
	return mode.parseErrors(cmd)
}

func newDestroyCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var confirmCluster string
	cmd := &cobra.Command{
		Use:          "destroy",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         argshNoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			disp := newDispatcher(cmd, paths)
			disp.ConfirmCluster = confirmCluster
			return dispatchExit(cmd.ErrOrStderr(), disp.DispatchDestroy(cmd.Context(), d))
		},
	}
	cmd.Flags().StringVar(&confirmCluster, "confirm-cluster", "", "The provider's cluster_name, to confirm the destroy off a terminal (--force does not replace it)")
	return argshFlagErrors(cmd)
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
			opts, err := mode.options(stderr, true, cmd.Flags().Changed("only"))
			if err != nil {
				return mode.exit(stderr, err)
			}
			d := ambientMainEnv(cmd, paths)
			runner := newRunner(paths)
			disp := newDispatcher(cmd, paths)
			bd := &bootstrap.Dispatcher{
				Engine:           &bootstrap.Engine{Paths: paths, Runner: runner, Stdout: stdout, Stderr: stderr, Options: opts},
				Drivers:          disp.Drivers,
				InventoryPublish: inventory.PublishHook(paths, runner, stderr),
			}
			return mode.exit(cmd.ErrOrStderr(), bd.Dispatch(cmd.Context(), d))
		},
	}
	mode.bind(cmd)
	return mode.parseErrors(cmd)
}

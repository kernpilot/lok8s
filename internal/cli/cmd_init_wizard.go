package cli

// cmd_init_wizard.go — bare `lo init` (Go-only) and the screens of its
// verbs. On a terminal a bare `lo init` has two modes. Without a project
// here, or in a project without a git repository: the welcome line and
// the bootstrap screen (every value prefilled; Create, Change details,
// Cancel), then project mode. In a project with a repository:
// project mode — the card, one choice from what the state allows, the
// action's screen, the card again — until Exit or Ctrl-C. Off a
// terminal, under CI or with --yes: the help text exactly as before.
// --plan (and --dry-run) prints the mode's screen as text and writes
// nothing.
//
// One executor: every screen's Create and every verb run the same
// functions (scaffold.Project, scaffold.WriteClusterSpec,
// scaffold.Service, scaffold.Tests, runToolchainInstall, useSetActive,
// assetsEject, scaffold.SetImplementation), and the dim `equivalent` row
// of a screen is the verb line a script runs instead.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/initctx"
	"github.com/kernpilot/lok8s/internal/scaffold"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/ui"
)

// initFlags are the flags of bare `lo init` and of its screen verbs.
type initFlags struct {
	yes, plan, dryRun bool
}

// add registers the three flags on a verb.
func (f *initFlags) add(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "Never ask: run with the values given (scripts, CI)")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "Print the screen as text and write nothing (works off a terminal)")
	cmd.Flags().BoolVarP(&f.dryRun, "dry-run", "n", false, "The same as --plan")
}

// The seams the tests replace: the terminal detection (a test has no
// TTY), the form IO (scripted, accessible) and the toolchain step (never
// the network under go test).
var (
	initTerminal = func(yes bool) initctx.Terminal {
		return initctx.DetectTerminal(os.Stdin, os.Stdout, yes)
	}
	initFormIO = func() initctx.IO {
		return initctx.IO{In: os.Stdin, Out: os.Stdout}
	}
	initToolchainInstall = runToolchainInstall
)

// runInitBare is the RunE of bare `lo init`.
func runInitBare(cmd *cobra.Command, args []string, paths *config.Paths, f initFlags) error {
	if len(args) > 0 {
		return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
	}
	setDebugFromVerbose(cmd)
	out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	term := initTerminal(f.yes)
	if !f.plan && !f.dryRun && !term.Interactive() {
		return cmd.Help()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	runner := newRunner(paths)
	ctx := cmd.Context()
	detect := func() (initctx.State, error) {
		s, err := initctx.Detect(ctx, cwd, runner)
		s.Terminal = term
		return s, err
	}
	state, err := detect()
	if err != nil {
		return err
	}
	paint := ui.Paint(term.StdoutTTY)
	if f.plan || f.dryRun {
		if initctx.Bootstrap(state) {
			initctx.Welcome(out, state, paint)
			fmt.Fprintln(out)
			a := initctx.DefaultAnswers(state)
			initctx.WriteScreen(out, initctx.NewProjectScreen(state, &a), paint)
			return nil
		}
		initctx.WritePlan(out, state)
		return nil
	}
	tio := initFormIO()
	loop := initctx.Loop{Out: out, IO: tio, Detect: detect,
		Execute: func(p initctx.Plan) error { return initExecute(ctx, p, runner, out, stderr) }}
	var first *initctx.Plan
	if initctx.Bootstrap(state) {
		initctx.Welcome(out, state, paint)
		fmt.Fprintln(out)
		plan, err := initctx.NewProject(state, out, tio)
		if err != nil {
			return initAbort(err, stderr)
		}
		if err := initExecute(ctx, plan, runner, out, stderr); err != nil {
			return err
		}
		fmt.Fprintln(out)
		first = &plan
		if state, err = detect(); err != nil {
			return err
		}
		if state.Project == nil {
			return nil
		}
	}
	return loop.Run(state, first)
}

// runInitScreen is a verb's screen: with --plan the rows as text (nil
// plan, nothing to do); on a terminal the details, the rows and Create;
// the plan to execute. A cancelled screen is the handled sentinel.
func runInitScreen(cmd *cobra.Command, f initFlags, build func() initctx.Screen) (*initctx.Plan, error) {
	term := initTerminal(f.yes)
	out := cmd.OutOrStdout()
	paint := ui.Paint(term.StdoutTTY)
	if f.plan || f.dryRun {
		initctx.WriteScreen(out, build(), paint)
		return nil, nil
	}
	plan, err := initctx.Run(out, initFormIO(), paint, build)
	if err != nil {
		return nil, initAbort(err, cmd.ErrOrStderr())
	}
	return &plan, nil
}

// initInteractive reports whether a verb opens its screen: --plan
// always prints it; otherwise a terminal without --yes.
func initInteractive(f initFlags) bool {
	return f.plan || f.dryRun || initTerminal(f.yes).Interactive()
}

// projectPaths is the layout of the project at root (no PATH_* override:
// the screens act where the user stands, like `lo init project`).
func projectPaths(root string) *config.Paths {
	return &config.Paths{
		Base:     root,
		Bin:      filepath.Join(root, ".bin"),
		Lok8s:    filepath.Join(root, ".lok8s"),
		Clusters: filepath.Join(root, "clusters"),
	}
}

// initAbort maps a left or cancelled screen onto the handled sentinel.
func initAbort(err error, stderr io.Writer) error {
	if errors.Is(err, initctx.ErrAborted) || errors.Is(err, initctx.ErrCancelled) {
		ui.ErrorTo(stderr, "lo init: cancelled, nothing written")
		return ErrHandled
	}
	return err
}

// initExecute runs the plan's actions through the verbs' functions, from
// the project directory (a service path lands in services.yaml the way
// `lo init service` records it: relative to the root).
func initExecute(ctx context.Context, plan initctx.Plan, runner execx.Runner, out, stderr io.Writer) error {
	if err := os.MkdirAll(plan.Dir, 0o755); err != nil {
		return err
	}
	prev, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(plan.Dir); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(prev) }()
	paths := projectPaths(plan.Dir)
	// A plan of several steps echoes each command; a single step prints
	// what the verb prints, nothing more.
	echo := len(plan.Actions) > 1
	for i, a := range plan.Actions {
		if echo {
			fmt.Fprintf(out, "==> %s\n", a.Command)
		}
		if err := initAction(ctx, plan, a, paths, runner, out, stderr); err != nil {
			// The rest is the user's to run by hand: the failed step
			// again once fixed, then what never started.
			fmt.Fprintf(out, "failed: %s\n", a.Command)
			if rest := plan.Commands()[i+1:]; len(rest) > 0 {
				fmt.Fprintln(out, "not run:")
				for _, c := range rest {
					fmt.Fprintf(out, "  %s\n", c)
				}
			}
			return scaffoldRun(err)
		}
	}
	if echo {
		fmt.Fprintln(out, "Done.")
	}
	return nil
}

// initAction runs one action.
func initAction(ctx context.Context, plan initctx.Plan, a initctx.Action, paths *config.Paths, runner execx.Runner, out, stderr io.Writer) error {
	switch a.Kind {
	case initctx.ActionWriteProjectFiles:
		return scaffold.Project(plan.Dir, scaffold.ProjectOptions{
			Name: a.Name, Env: a.Env, Domain: a.Domain, Driver: a.Driver,
			BVersion: toolchain.BRelease.Version,
		}, out, stderr)
	case initctx.ActionGitInit:
		return runner.Run(ctx, execx.Cmd{Name: "git", Args: []string{"init"}, Dir: plan.Dir, Stdout: out, Stderr: stderr})
	case initctx.ActionWriteClusterSpec:
		return scaffold.WriteClusterSpec(paths.Clusters, a.Domain, a.Driver, plan.Force, out, stderr)
	case initctx.ActionToolchainInstall:
		return initToolchainInstall(ctx, plan.Dir, a.Groups, false, out, stderr)
	case initctx.ActionUse:
		return useSetActive(paths, a.Domain, out, stderr)
	case initctx.ActionAddService:
		return scaffold.Service(plan.Dir, a.Service, a.Path, plan.Force, out, stderr)
	case initctx.ActionAddTests:
		return scaffold.Tests(scaffold.TestTemplate(), plan.Dir, a.Path, plan.Force, out, stderr)
	case initctx.ActionEjectBash:
		return assetsEject(paths, []string{assets.BashRel}, false, false, out, stderr)
	case initctx.ActionSetImplementation:
		return scaffold.SetImplementation(plan.Dir, a.Name, a.Implementation, out)
	}
	return fmt.Errorf("lo init: unknown action %q", a.Kind)
}

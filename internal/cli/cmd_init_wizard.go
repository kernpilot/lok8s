package cli

// cmd_init_wizard.go — bare `lo init` (Go-only): on a terminal, the
// context-aware wizard (internal/initctx); off a terminal, under CI or
// with --yes, the help text exactly as before. --plan prints the state
// card and the commands the defaults would run, writes nothing and works
// anywhere. --dry-run runs the conversation and stops at the summary.
//
// One code path: the wizard fills the same option structs and calls the
// same functions the subcommands call (scaffold.Project, scaffold.Service,
// scaffold.Tests, runToolchainInstall, useSetActive, assetsEject), and
// the summary prints those subcommands with their flags. Nothing is
// written before the summary is confirmed.

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

// initFlags are the bare `lo init` flags.
type initFlags struct {
	yes, plan, dryRun bool
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
	if !f.plan && !term.Interactive() {
		return cmd.Help()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	runner := newRunner(paths)
	state, err := initctx.Detect(cmd.Context(), cwd, runner)
	if err != nil {
		return err
	}
	state.Terminal = term
	if f.plan {
		return initPlan(cmd.Context(), state, out)
	}
	return initWizard(cmd.Context(), state, runner, f.dryRun, out, stderr)
}

// initPlan is `lo init --plan`: the card, then the commands the defaults
// would run (or the menu as hints). rc 0, no writes.
func initPlan(ctx context.Context, s initctx.State, out io.Writer) error {
	initCard(ctx, s, out)
	plan := initctx.Decide(s, initctx.DefaultAnswers(s))
	fmt.Fprintln(out)
	initctx.WriteSummary(out, plan, s.Cwd)
	if len(plan.Actions) == 0 {
		initctx.WriteOptions(out, s)
	}
	return nil
}

// initCard prints the state card; inside a project the doctor's domain
// section and, when `lo toolchain install` wrote .bin/b.yaml, its
// toolchain section follow (the same functions `lo doctor` runs).
func initCard(ctx context.Context, s initctx.State, out io.Writer) {
	initctx.WriteCard(out, s)
	if s.Project == nil {
		return
	}
	p := projectPaths(s.Project.Root)
	doctorDomainSection(p, s.Project.Active, out)
	if s.Project.BYAMLMarker {
		doctorToolchain(ctx, out, p, kustomizePluginHome(p), childPATH(p, bashTreeForPATH(p).Dir))
	}
}

// projectPaths is the layout of the project at root (no PATH_* override:
// the wizard acts where the user stands, like `lo init project`).
func projectPaths(root string) *config.Paths {
	return &config.Paths{
		Base:     root,
		Bin:      filepath.Join(root, ".bin"),
		Lok8s:    filepath.Join(root, ".lok8s"),
		Clusters: filepath.Join(root, "clusters"),
	}
}

// initWizard is the conversation: the card, the questions, the summary,
// the confirmation, then the actions.
func initWizard(ctx context.Context, s initctx.State, runner execx.Runner, dryRun bool, out, stderr io.Writer) error {
	initCard(ctx, s, out)
	fmt.Fprintln(out)
	tio := initFormIO()
	answers, err := initctx.Ask(s, tio)
	if err != nil {
		return initAbort(err, stderr)
	}
	plan := initctx.Decide(s, answers)
	fmt.Fprintln(out)
	initctx.WriteSummary(out, plan, s.Cwd)
	if len(plan.Actions) == 0 {
		return nil
	}
	if dryRun {
		fmt.Fprintln(out, "dry run — nothing was written")
		return nil
	}
	fmt.Fprintln(out)
	ok, err := initctx.Confirm(tio, plan.Network())
	if err != nil {
		return initAbort(err, stderr)
	}
	if !ok {
		fmt.Fprintln(out, "Nothing written.")
		return nil
	}
	return initExecute(ctx, plan, runner, out, stderr)
}

// initAbort maps a left form onto the handled sentinel.
func initAbort(err error, stderr io.Writer) error {
	if errors.Is(err, initctx.ErrAborted) {
		ui.ErrorTo(stderr, "lo init: aborted, nothing written")
		return ErrHandled
	}
	return err
}

// initExecute runs the plan's actions through the subcommands' functions,
// from the project directory (a service path lands in services.yaml the
// way `lo init service` records it: relative to the root).
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
	for i, a := range plan.Actions {
		fmt.Fprintf(out, "==> %s\n", a.Command)
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
	fmt.Fprintln(out, "Done.")
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
	case initctx.ActionWriteEnvFile:
		return scaffold.EnvFile(plan.Dir, a.Env, a.Name, toolchain.BRelease.Version, false, out)
	case initctx.ActionWriteClusterSpec:
		return scaffold.WriteClusterSpec(paths.Clusters, a.Domain, a.Driver, false, out, stderr)
	case initctx.ActionEjectBash:
		return assetsEject(paths, []string{assets.BashRel}, false, false, out, stderr)
	case initctx.ActionSetImplementation:
		return scaffold.SetImplementation(plan.Dir, a.Name, a.Implementation, out)
	case initctx.ActionToolchainInstall:
		return initToolchainInstall(ctx, plan.Dir, a.Groups, false, out, stderr)
	case initctx.ActionUse:
		return useSetActive(paths, a.Domain, out, stderr)
	case initctx.ActionAddService:
		return scaffold.Service(plan.Dir, a.Service, "", false, out, stderr)
	case initctx.ActionAddTests:
		return scaffold.Tests(scaffold.TestTemplate(), plan.Dir, "", false, out, stderr)
	case initctx.ActionRegisterService:
		// The registration half of `lo init service <name> --path <dir>`:
		// the service file exists, so only the catalog entry and the
		// Tiltfile are ensured (the same functions Service calls).
		if err := scaffold.MergeServices(filepath.Join(plan.Dir, "services.yaml"), a.Service, a.ServicePath, out, stderr); err != nil {
			return err
		}
		return scaffold.EnsureTiltfile(filepath.Join(plan.Dir, "Tiltfile"), out, stderr)
	}
	return fmt.Errorf("lo init: unknown action %q", a.Kind)
}

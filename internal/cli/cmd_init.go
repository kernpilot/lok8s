package cli

// lo init — scaffold lok8s project/service config from a correct template.
// Go port of .lok8s/libs/init (main::init); the scaffolding lives in
// internal/scaffold. Output and emitted bytes are identical to the bash
// implementation off a terminal. On a terminal without --yes, `service`,
// `test` and the Go-only `cluster` open their screen first
// (cmd_init_wizard.go). A value given on the command line is a fixed
// row; the screen asks for the rest. Create runs the same functions.

import (
	"errors"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/initctx"
	"github.com/kernpilot/lok8s/internal/scaffold"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("init", newInitCommand) }

// scaffoldRun maps the scaffold package's already-printed sentinel onto the
// cli one.
func scaffoldRun(err error) error {
	if errors.Is(err, scaffold.ErrHandled) {
		return ErrHandled
	}
	return err
}

func newInitCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	// Bare `lo init` (Go-only, cmd_init_wizard.go): the screens on a
	// terminal, the help text (argshGroupRunE) off one, under CI or with
	// --yes; --plan prints the mode's screen as text and writes nothing.
	var flags initFlags
	cmd := &cobra.Command{
		Use:          "init",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitBare(cmd, args, paths, flags)
		},
	}
	cmd.Flags().BoolVarP(&flags.yes, "yes", "y", false, "Never ask: print the help instead of the screens (scripts, CI)")
	cmd.Flags().BoolVar(&flags.plan, "plan", false, "Print what lo init sees here and offers, as text; write nothing (works off a terminal)")
	cmd.Flags().BoolVarP(&flags.dryRun, "dry-run", "n", false, "The same as --plan")

	var svcPath string
	var svcFlags initFlags
	service := &cobra.Command{
		Use:   "service [name]",
		Short: "Scaffold a bare service (lok8s.yaml + services.yaml + Tiltfile)",
		// argsh collects positionals into an array; extras are ignored.
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			setDebugFromVerbose(cmd)
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			// --force|-f is the inherited global flag (bash: the subcommand
			// re-declares it, same name, same shorthand — one value).
			force, _ := cmd.Flags().GetBool("force")
			if !initInteractive(svcFlags) {
				return scaffoldRun(scaffold.Service(paths.Base, name, svcPath, force, cmd.OutOrStdout(), cmd.ErrOrStderr()))
			}
			in := &initctx.ServiceInput{Name: name, Path: svcPath, NameGiven: len(args) > 0, PathGiven: cmd.Flags().Changed("path")}
			plan, err := runInitScreen(cmd, svcFlags, func() initctx.Screen { return initctx.ServiceScreen(paths.Base, in) })
			if err != nil || plan == nil {
				return err
			}
			plan.Force = force
			return initReport(initExecute(cmd.Context(), *plan, newRunner(paths), cmd.OutOrStdout(), cmd.ErrOrStderr()), cmd.OutOrStdout())
		},
	}
	service.Flags().StringVarP(&svcPath, "path", "p", "", "Directory for the service (default: ./<name>)")
	svcFlags.add(service)

	var testPath string
	var testFlags initFlags
	test := &cobra.Command{
		Use:          "test",
		Short:        "Scaffold a Playwright integration suite (tests/)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			setDebugFromVerbose(cmd)
			force, _ := cmd.Flags().GetBool("force")
			if !initInteractive(testFlags) {
				return scaffoldRun(scaffold.Tests(scaffold.TestTemplate(), paths.Base, testPath, force, cmd.OutOrStdout(), cmd.ErrOrStderr()))
			}
			in := &initctx.TestsInput{Path: testPath, PathGiven: cmd.Flags().Changed("path")}
			plan, err := runInitScreen(cmd, testFlags, func() initctx.Screen { return initctx.TestsScreen(paths.Base, in) })
			if err != nil || plan == nil {
				return err
			}
			plan.Force = force
			return initReport(initExecute(cmd.Context(), *plan, newRunner(paths), cmd.OutOrStdout(), cmd.ErrOrStderr()), cmd.OutOrStdout())
		},
	}
	test.Flags().StringVarP(&testPath, "path", "p", "", "Directory for the suite (default: ./tests)")
	testFlags.add(test)

	// Go-only: a cluster spec into the project, made the active domain
	// unless --no-active. The screen twin of the project list's "Add a
	// cluster"; off a terminal the domain is required.
	var clusterDriver string
	var clusterNoActive bool
	var clusterFlags initFlags
	cluster := &cobra.Command{
		Use:          "cluster [domain]",
		Short:        "Scaffold a cluster spec, clusters/<domain>/cluster.lok8s.yaml, and make it the active domain",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		Annotations:  map[string]string{AnnotationIdempotent: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			setDebugFromVerbose(cmd)
			force, _ := cmd.Flags().GetBool("force")
			in := &initctx.ClusterInput{Driver: clusterDriver, Active: !clusterNoActive,
				DomainGiven: len(args) > 0, DriverGiven: cmd.Flags().Changed("driver"), ActiveGiven: cmd.Flags().Changed("no-active")}
			if len(args) > 0 {
				in.Domain = args[0]
			}
			var plan *initctx.Plan
			if initInteractive(clusterFlags) {
				p, err := runInitScreen(cmd, clusterFlags, func() initctx.Screen { return initctx.ClusterScreen(paths.Base, in) })
				if err != nil || p == nil {
					return err
				}
				plan = p
			} else {
				if in.Domain == "" {
					ui.ErrorTo(cmd.ErrOrStderr(), "%s", initctx.MissingDomain)
					return ErrHandled
				}
				p := initctx.ClusterPlan(paths.Base, in.Domain, in.Driver, in.Active)
				plan = &p
			}
			plan.Force = force
			return initReport(initExecute(cmd.Context(), *plan, newRunner(paths), cmd.OutOrStdout(), cmd.ErrOrStderr()), cmd.OutOrStdout())
		},
	}
	cluster.Flags().StringVar(&clusterDriver, "driver", "lo", "Driver of the spec: "+strings.Join(scaffold.DriverNames(), ", "))
	cluster.Flags().BoolVar(&clusterNoActive, "no-active", false, "Write the spec only; keep the active domain as it is")
	clusterFlags.add(cluster)

	// Go-only (no twin in .lok8s/libs/init): the eject model's project
	// scaffold — files only. No .lok8s/ tree (assets are ejected on first
	// use), no network (the toolchain is `lo toolchain install`).
	var projectPath, projectEnv, projectDomain, projectDriver, projectImpl string
	project := &cobra.Command{
		Use:          "project [name]",
		Short:        "Scaffold a project (clusters/, lok8s.yaml, .gitignore entries, one env file, optionally the first cluster spec) — files only, no network",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			setDebugFromVerbose(cmd)
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			force, _ := cmd.Flags().GetBool("force")
			// A NEW project is scaffolded where the user stands, never into
			// the ambient project: with PATH_BASE exported (direnv/mise
			// shells), the resolved base is whatever project that variable
			// points at — a smoke run once wrote mise.toml into a live repo
			// this way. Kept while PATH_BASE keeps first place in
			// config.ResolvePaths (WP9 step 1, parked).
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return scaffoldRun(scaffold.Project(cwd, scaffold.ProjectOptions{
				Dir: projectPath, Name: name, Env: projectEnv, Force: force,
				BVersion: toolchain.BRelease.Version,
				Domain:   projectDomain, Driver: projectDriver,
				Implementation: projectImpl,
			}, cmd.OutOrStdout(), cmd.ErrOrStderr()))
		},
	}
	project.Flags().StringVarP(&projectPath, "path", "p", "", "Directory for the project (default: the working directory)")
	project.Flags().StringVar(&projectEnv, "env", "mise", "Shell environment file to scaffold: mise (mise.toml), direnv (.envrc) or none — PATH only, no PATH_* pins")
	project.Flags().StringVar(&projectDomain, "cluster", "", "Also write the first cluster spec, clusters/<domain>/cluster.lok8s.yaml, for this domain")
	project.Flags().StringVar(&projectDriver, "driver", "lo", "Driver of the --cluster spec: "+strings.Join(scaffold.DriverNames(), ", "))
	project.Flags().StringVar(&projectImpl, "implementation", "", "Set spec.implementation.default in lok8s.yaml: go or bash (the file is created when missing; comments and other keys are kept)")

	// `lo init toolchain` is the hidden alias of `lo toolchain install`
	// for one release (WP9): same flags, same run, a deprecation hint on
	// stderr first.
	cmd.AddCommand(service, test, cluster, project, newToolchainInstallCommand(paths, "toolchain", true))
	return cmd
}

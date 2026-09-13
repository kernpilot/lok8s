package cli

// lo init — scaffold lok8s project/service config from a correct template.
// Go port of .lok8s/libs/init (main::init); the scaffolding lives in
// internal/scaffold. Output and emitted bytes are identical to the bash
// implementation.

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/scaffold"
	"github.com/kernpilot/lok8s/internal/toolchain"
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
	cmd := &cobra.Command{
		Use:          "init",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE:         argshGroupRunE,
	}

	var svcPath string
	service := &cobra.Command{
		Use:   "service <name>",
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
			return scaffoldRun(scaffold.Service(paths.Base, name, svcPath, force, cmd.OutOrStdout(), cmd.ErrOrStderr()))
		},
	}
	service.Flags().StringVarP(&svcPath, "path", "p", "", "Directory for the service (default: ./<name>)")

	var testPath string
	test := &cobra.Command{
		Use:          "test",
		Short:        "Scaffold a Playwright integration suite (tests/)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			setDebugFromVerbose(cmd)
			force, _ := cmd.Flags().GetBool("force")
			return scaffoldRun(scaffold.Tests(scaffold.TestTemplate(), paths.Base, testPath, force, cmd.OutOrStdout(), cmd.ErrOrStderr()))
		},
	}
	test.Flags().StringVarP(&testPath, "path", "p", "", "Directory for the suite (default: ./tests)")

	// Go-only (no twin in .lok8s/libs/init): the eject model's project
	// scaffold — files only. No .lok8s/ tree (assets are ejected on first
	// use), no network (the toolchain is `lo toolchain install`).
	var projectPath, projectEnv string
	project := &cobra.Command{
		Use:          "project [name]",
		Short:        "Scaffold a project (clusters/, lok8s.yaml, .gitignore entries, one env file) — files only, no network",
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
			}, cmd.OutOrStdout(), cmd.ErrOrStderr()))
		},
	}
	project.Flags().StringVarP(&projectPath, "path", "p", "", "Directory for the project (default: the working directory)")
	project.Flags().StringVar(&projectEnv, "env", "mise", "Shell environment file to scaffold: mise (mise.toml), direnv (.envrc) or none — PATH only, no PATH_* pins")

	// `lo init toolchain` is the hidden alias of `lo toolchain install`
	// for one release (WP9): same flags, same run, a deprecation hint on
	// stderr first.
	cmd.AddCommand(service, test, project, newToolchainInstallCommand(paths, "toolchain", true))
	return cmd
}

package cli

// lo init — scaffold lok8s project/service config from a correct template.
// Go port of .lok8s/libs/init (main::init); the scaffolding lives in
// internal/scaffold. Output and emitted bytes are identical to the bash
// implementation.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/scaffold"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

func init() { registerPorted("init", newInitCommand) }

// scaffoldRun maps the scaffold package's already-printed sentinel onto the
// cli one.
func scaffoldRun(err error) error {
	if err == scaffold.ErrHandled {
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
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
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
	// scaffold — no .lok8s/ tree, assets are ejected on first use.
	var projectPath, projectGroups, projectEnv string
	var noToolchain bool
	project := &cobra.Command{
		Use:          "project [name]",
		Short:        "Scaffold a project (clusters/, lok8s.yaml, .gitignore entries, .bin/b.yaml) — no .lok8s/ — and install the toolchain",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			setDebugFromVerbose(cmd)
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			force, _ := cmd.Flags().GetBool("force")
			groups, err := toolchain.NormalizeGroups(strings.Split(projectGroups, ","))
			if err != nil {
				return err
			}
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			tc := scaffold.ProjectToolchain{
				Template: func(n string) string { return toolchainTemplate(n, groups) },
				Env:      projectEnv,
				BVersion: toolchain.BRelease.Version,
			}
			if !noToolchain {
				tc.Bootstrap = func(dir string) error {
					return toolchain.Bootstrap(cmd.Context(), toolchain.BootstrapOptions{
						Base: dir, Bin: filepath.Join(dir, ".bin"), Out: out, Stderr: stderr,
					})
				}
			}
			// A NEW project is scaffolded where the user stands, never into the
			// ambient project: with PATH_BASE exported (direnv/mise shells), the
			// resolved base is whatever project that variable points at — a
			// smoke run once wrote mise.toml into a live repo this way.
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return scaffoldRun(scaffold.Project(cwd, name, projectPath, force, out, stderr, tc))
		},
	}
	project.Flags().StringVarP(&projectPath, "path", "p", "", "Directory for the project (default: the current project root)")
	project.Flags().StringVar(&projectGroups, "groups", strings.Join(toolchain.DefaultGroups, ","), "Toolchain groups to activate in .bin/b.yaml (core,local,cloud; core is implied)")
	project.Flags().BoolVar(&noToolchain, "no-toolchain", false, "Write .bin/b.yaml but do not install b / run b install (no network)")
	project.Flags().StringVar(&projectEnv, "env", "mise", "Shell environment file(s) to scaffold: mise (mise.toml), direnv (.envrc), both, none — PATH only, no PATH_* pins")

	cmd.AddCommand(service, test, project, newInitToolchainCommand(paths))
	return cmd
}

// setDebugFromVerbose exports DEBUG=1 for -v/--verbose, like the argsh
// entrypoint (the debug() lines depend on it).
func setDebugFromVerbose(cmd *cobra.Command) {
	if v, _ := cmd.Flags().GetCount("verbose"); v > 0 {
		os.Setenv("DEBUG", "1")
	}
}

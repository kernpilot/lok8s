package cli

// lo env — service/env config rendering (internal; driven by the Tilt
// extension). Go port of .lok8s/libs/env (main::env); the rendering lives in
// internal/env.
//
// `services` disables cobra flag parsing and hand-parses argsh-style: the
// bash spec gives -s to --only-services and -r to --only-registry inside the
// subcommand, shadowing the global --cluster/-s and --remote/-r shorthands —
// cobra cannot express a shadowed inherited shorthand (the merged flag set
// panics on the redefinition), the hand parser can.

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/env"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/image"
)

func init() { registerPorted("env", newEnvCommand) }

// envRun maps the env package's already-printed sentinel onto the cli one.
func envRun(err error) error {
	if err == env.ErrHandled {
		return ErrHandled
	}
	return err
}

func envContext(cmd *cobra.Command, paths *config.Paths, d string) *env.Context {
	return &env.Context{
		Paths:  paths,
		Runner: execx.NewRunner(paths),
		Out:    cmd.OutOrStdout(),
		ErrOut: cmd.ErrOrStderr(),
		Domain: d,
	}
}

func newEnvCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "env",
		Aliases:      spec.aliases,
		Short:        spec.short,
		Hidden:       spec.hidden,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newEnvServices(paths),
		newEnvKustomization(paths),
	)
	return cmd
}

// envServicesFlags is the hand-parse result for `env services`.
type envServicesFlags struct {
	onlyServices, onlyRegistry bool
	verbose, force             bool
	domain, cluster            string
	help                       bool
	positionals                []string
}

// parseEnvServices hand-parses argv for `env services`: the subcommand's own
// spec (--only-services, --only-registry) plus everything it inherits in
// bash (the parent's domain flag and main's global flags). NOTE the
// shorthand resolution, verified against the argsh runtime: the INHERITED
// globals win the -s/-r collision here (unlike `secrets set`, where the
// subcommand's own -s wins) — `env services -s <v>` consumes a --cluster
// VALUE and `-r` is the --remote boolean; only the long spellings reach
// --only-services/--only-registry.
func parseEnvServices(errOut io.Writer, args []string) (*envServicesFlags, error) {
	p := &envServicesFlags{}
	valueFlag := func(long string) bool {
		switch long {
		case "domain", "kubernetes", "cluster", "config", "domain-sans":
			return true
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-" || !strings.HasPrefix(a, "-"):
			p.positionals = append(p.positionals, a)
		case strings.HasPrefix(a, "--"):
			long, val, hasVal := strings.Cut(a[2:], "=")
			switch {
			case valueFlag(long):
				if !hasVal {
					i++
					if i >= len(args) {
						return nil, argshErrorf(errOut, "flag needs an argument: %s", a)
					}
					val = args[i]
				}
				switch long {
				case "domain":
					p.domain = val
				case "cluster":
					p.cluster = val
				}
			case long == "only-services":
				p.onlyServices = true
			case long == "only-registry":
				p.onlyRegistry = true
			case long == "verbose":
				p.verbose = true
			case long == "force" || long == "force-recreate":
				p.force = true
			case long == "remote":
				// consumed, no effect at subcommand level
			case long == "help":
				p.help = true
			default:
				return nil, argshErrorf(errOut, "unknown flag: %s", a)
			}
		default:
			if len(a) != 2 {
				return nil, argshErrorf(errOut, "unknown flag: %s", a)
			}
			switch a[1] {
			case 's':
				// argsh resolves -s to the inherited --cluster (value flag).
				i++
				if i >= len(args) {
					return nil, argshErrorf(errOut, "missing value for flag: cluster")
				}
				p.cluster = args[i]
			case 'r':
				// argsh resolves -r to the inherited --remote boolean.
			case 'v':
				p.verbose = true
			case 'f':
				p.force = true
			case 'h':
				p.help = true
			default:
				return nil, argshErrorf(errOut, "unknown flag: %s", a)
			}
		}
	}
	return p, nil
}

func newEnvServices(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:                "services",
		Aliases:            []string{"v"},
		Short:              "Print services",
		Annotations:        commandSpec{readonly: true}.annotations(),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := parseEnvServices(cmd.ErrOrStderr(), args)
			if err != nil {
				return err
			}
			if p.help {
				return cmd.Help()
			}
			if len(p.positionals) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "unexpected argument: %s", p.positionals[0])
			}
			// Replay main's env prep by hand (flag parsing was disabled).
			d := applyMainEnv(paths, cmd.ErrOrStderr(), p.verbose, p.force, false,
				p.domain, p.domain != "", p.cluster)
			ec := envContext(cmd, paths, d)
			return envRun(ec.Services(cmd.Context(), p.onlyServices, p.onlyRegistry))
		},
	}
}

func newEnvKustomization(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "kustomization",
		Aliases:      []string{"k"},
		Short:        "Generate kustomization.yaml",
		Annotations:  commandSpec{idempotent: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			noBuild, _ := cmd.Flags().GetBool("no-build")
			pull, _ := cmd.Flags().GetBool("pull")
			ec := envContext(cmd, paths, d)
			// --pull drains the queue via the image port (bash:
			// image::cache --all with the lib's own unset force/all locals).
			ec.Pull = func() error {
				ic := &image.Context{
					Paths:  paths,
					Runner: execx.NewRunner(paths),
					Out:    cmd.OutOrStdout(),
					ErrOut: cmd.ErrOrStderr(),
					Domain: d,
				}
				return ic.Cache(cmd.Context(), "", false, true)
			}
			return envRun(ec.Kustomization(cmd.Context(), noBuild, pull))
		},
	}
	c.Flags().BoolP("no-build", "n", false, "Do not build artifacts")
	c.Flags().BoolP("pull", "p", false, "After writing the kustomization, drain the cache queue (lo image cache --all)")
	return c
}

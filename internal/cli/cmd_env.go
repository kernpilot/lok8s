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
	"errors"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/env"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/image"
)

func init() { registerPorted("env", newEnvCommand) }

// envRun maps the env package's already-printed sentinel onto the cli one.
func envRun(err error) error {
	if errors.Is(err, env.ErrHandled) {
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

func newEnvServices(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "services",
		Aliases:      []string{"v"},
		Short:        "Print services",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			onlyServices, _ := f.GetBool("only-services")
			onlyRegistry, _ := f.GetBool("only-registry")
			verbose, _ := f.GetCount("verbose")
			force, _ := f.GetBool("force")
			forceRecreate, _ := f.GetBool("force-recreate")
			cluster, _ := f.GetString("cluster")
			domainFlag, domainChanged := "", false
			if d := f.Lookup("domain"); d != nil && d.Changed {
				domainFlag, domainChanged = d.Value.String(), true
			}
			// Replay main's env prep. Remote stays false: in bash the
			// inherited -r/--remote is consumed here without effect.
			d := applyMainEnv(paths, cmd.ErrOrStderr(), verbose > 0, force || forceRecreate, false,
				domainFlag, domainChanged, cluster)
			ec := envContext(cmd, paths, d)
			return envRun(ec.Services(cmd.Context(), onlyServices, onlyRegistry))
		},
	}
	// No shorthands: in bash the inherited globals win -s/-r here (verified
	// against the argsh runtime), so only the long spellings exist.
	c.Flags().Bool("only-services", false, "Print the services block only")
	c.Flags().Bool("only-registry", false, "Print the registry block only")
	return argshFlagErrors(c)
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

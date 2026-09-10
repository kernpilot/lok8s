package cli

// lo recover — rebuild a cluster from bare metal (disaster recovery). Go
// port of .lok8s/libs/recover main::recover; the phase orchestrator lives
// in internal/recover.

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/recover"
)

func init() { registerPorted("recover", newRecoverCommand) }

func newRecoverCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var skipRebuild, dryRun bool
	cmd := &cobra.Command{
		Use:          "recover [recover-domain]",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			// The entrypoint's pre-dispatch exports; returns the --domain /
			// active fallback the positional outranks.
			fallback := ambientMainEnv(cmd, paths)
			// `lo recover <domain>` — the documented form. A command that
			// reimages a whole fleet should NAME its target rather than inherit
			// whatever .active happens to point at mid-incident; the argsh
			// array positional is greedy, so a second token is a hard error
			// (never swallowed).
			d, err := recover.PickDomain(stderr, fallback, args)
			if err != nil {
				return ErrHandled
			}
			force, _ := cmd.Flags().GetBool("force")
			r := &recover.Runner{
				Paths:  paths,
				Exec:   execx.NewRunner(paths),
				Stdout: cmd.OutOrStdout(),
				Stderr: stderr,
				In:     cmd.InOrStdin(),
				Force:  force,
			}
			if err := r.Run(cmd.Context(), d, skipRebuild, dryRun); err != nil {
				if errors.Is(err, recover.ErrHandled) {
					return ErrHandled
				}
				return err
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&skipRebuild, "skip-rebuild", false, "Skip the bare-metal node rebuild — re-run provision + verify only")
	f.BoolVar(&dryRun, "dry-run", false, "Preview the rebuild plan (reimages nothing) and stop before provision")
	return cmd
}

package cli

// lo gitops — GitOps integration (DEFERRED post-refactor; both subcommands
// print the deferral error, exactly like .lok8s/libs/gitops). Go port of
// main::gitops / gitops::flux / gitops::argo; the stub itself lives in
// internal/gitops. Flipped because the surface is fully hermetic (no exec,
// no cluster): the only difference to the argsh tree is the bare `lo
// gitops` / `-h` usage text, which is cobra's here and argsh's there (the
// same documented divergence every ported command group carries).

import (
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/gitops"
)

func init() { registerPorted("gitops", newGitopsCommand) }

func newGitopsCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "gitops",
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
	cmd.AddCommand(
		newGitopsSub(paths, "flux", "f", "Generate Flux manifests (deferred)", gitops.Flux),
		newGitopsSub(paths, "argo", "a", "Annotate artifacts with Argo sync-wave (deferred)", gitops.Argo),
	)
	return cmd
}

func newGitopsSub(paths *config.Paths, use, alias, short string, run func(stderr io.Writer) error) *cobra.Command {
	return &cobra.Command{
		Use:          use,
		Aliases:      []string{alias},
		Short:        short,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The argsh spec declares no positional: a stray one is a parse
			// error there ("too many arguments", rc 2); same message, exit 1.
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "too many arguments: %s", args[0])
			}
			// Replay the entrypoint's pre-dispatch exports (DEBUG, DOMAIN_NAME,
			// KUBECONFIG …) so the deferred stub sees what its argsh twin saw.
			ambientMainEnv(cmd, paths)
			if err := run(cmd.ErrOrStderr()); err != nil {
				if errors.Is(err, gitops.ErrDeferred) {
					return ErrHandled
				}
				return err
			}
			return nil
		},
	}
}

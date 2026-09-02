package cli

// lo lint — structure and spec validation. Go port of .lok8s/libs/lint
// main::lint; the checks live in internal/lint.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/lint"
)

func init() { registerPorted("lint", newLintCommand) }

func newLintCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return &cobra.Command{
		Use:          "lint",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			// No positionals in the argsh spec (the domain rides --domain);
			// same message as the argsh parser, exit 1 (version/use precedent).
			if len(args) > 0 {
				return argshErrorf(stderr, "too many arguments: %s", args[0])
			}
			if v, _ := cmd.Flags().GetCount("verbose"); v > 0 {
				os.Setenv("DEBUG", "1")
			}

			// Domain: the canonical precedence chain (--domain flag >
			// DOMAIN_NAME env > clusters/.active > lok8s.dev). Always
			// non-empty, so — exactly like the bash, whose main pre-sets the
			// lib's `domain` local — the "validate all domains" branch inside
			// the lib is unreachable through the CLI.
			domainFlag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(domainFlag, paths.Clusters, stderr)

			l := &lint.Linter{Paths: paths, Out: cmd.OutOrStdout(), ErrOut: stderr}
			if err := l.Run(d); err != nil {
				return ErrHandled
			}
			return nil
		},
	}
}

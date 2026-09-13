package cli

// lo lint — structure and spec validation. Go port of .lok8s/libs/lint
// main::lint; the checks live in internal/lint.

import (
	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/lint"
)

func init() { registerPorted("lint", newLintCommand) }

func newLintCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var notes bool
	cmd := &cobra.Command{
		Use:          "lint",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         argshNoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			// No positionals in the argsh spec (the domain rides --domain);
			// same message as the argsh parser, exit 1 (version/use precedent).
			setDebugFromVerbose(cmd)

			// Domain: the canonical precedence chain (--domain flag >
			// DOMAIN_NAME env > clusters/.active > lok8s.dev). Always
			// non-empty, so — exactly like the bash, whose main pre-sets the
			// lib's `domain` local — the "validate all domains" branch inside
			// the lib is unreachable through the CLI.
			domainFlag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(domainFlag, paths.Clusters, stderr)

			// Go-only: the spec.implementation block of lok8s.yaml (every
			// other command refuses to start on an invalid block; lint
			// reports it, and a missing routed tree, as a finding, and
			// unknown keys as warnings). A valid or absent block prints
			// nothing, so the output stays byte-identical to bash.
			l := &lint.Linter{Paths: paths, Out: cmd.OutOrStdout(), ErrOut: stderr, Notes: notes,
				Implementation: func() ([]string, error) {
					r := newRouting(paths)
					return r.impl.Warnings, r.problem()
				}}
			if err := l.Run(d); err != nil {
				return ErrHandled
			}
			return nil
		},
	}
	// Go-only: the advisory for keys equal to a documented default
	// (internal/lint/defaults.go). Opt-in so every `check - lint` parity
	// case stays byte-identical (the bash lint prints no such line).
	cmd.Flags().BoolVar(&notes, "notes", false, "Also print a [note] per spec key that equals its documented default (advisory; the exit code is unchanged)")
	return cmd
}

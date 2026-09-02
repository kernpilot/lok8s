package cli

// lo audit — static security-posture audit (read-only, cluster-free).
// Go port of .lok8s/libs/audit main::audit; the checks + renderers live in
// internal/audit. Output (human, --json, --sarif) is byte-identical.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/audit"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("audit", newAuditCommand) }

func newAuditCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var (
		jsonFlag  bool
		sarifFlag bool
	)
	cmd := &cobra.Command{
		Use:          "audit [domain]",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			stderr := cmd.ErrOrStderr()

			// -v/--verbose → DEBUG, like the argsh entrypoint.
			if v, _ := cmd.Flags().GetCount("verbose"); v > 0 {
				os.Setenv("DEBUG", "1")
			}

			if jsonFlag && sarifFlag {
				ui.Errorf(stderr, "--json and --sarif are mutually exclusive")
				return ErrHandled
			}

			// Domain precedence: positional > --domain flag > DOMAIN_NAME env
			// > clusters/.active > lok8s.dev. The bash entrypoint always
			// resolves a default, so a bare `lo audit` audits THAT active
			// domain — it never sweeps the fleet (the bash multi-domain
			// fallback is reachable only from a direct programmatic call the
			// CLI never makes). Extra positionals are collected and ignored
			// beyond the first, exactly like the argsh array parameter.
			domainFlag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(domainFlag, paths.Clusters, stderr)
			if len(args) > 0 {
				d = args[0]
			}

			a := audit.New(paths)
			findings := a.RunDomain(d)
			switch {
			case sarifFlag:
				audit.RenderSarif(out, a.SarifFindings(d, findings))
			case jsonFlag:
				audit.RenderJSON(out, d, findings)
			default:
				audit.RenderHuman(out, d, findings)
			}

			// Exit-code contract: ONLY a fail-level finding turns the rc
			// non-zero (warn/unknown do not); the report itself is the
			// message.
			if audit.HasFail(findings) {
				return ErrHandled
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&jsonFlag, "json", false, "Emit machine-readable JSON (stable schema for tooling)")
	f.BoolVar(&sarifFlag, "sarif", false, "Emit SARIF 2.1.0 (GitHub code-scanning upload)")
	return cmd
}

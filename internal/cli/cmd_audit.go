package cli

// lo audit — static security-posture audit (read-only, cluster-free).
// Go port of .lok8s/libs/audit main::audit; the checks + renderers live in
// internal/audit. Output (human, --json, --sarif) is byte-identical.

import (
	"bytes"
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
		format    func() (string, error)
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
			setDebugFromVerbose(cmd)

			if jsonFlag && sarifFlag {
				ui.ErrorTo(stderr, "--json and --sarif are mutually exclusive")
				return ErrHandled
			}
			// Go-only: -o json is --json, -o yaml the same document as yaml;
			// --sarif is its own document and takes no -o.
			f, err := outputWithJSONFlag(cmd, format, jsonFlag, "lo audit")
			if err != nil {
				return err
			}
			if sarifFlag && f != outputText {
				return argshErrorf(stderr, "lo audit: --sarif conflicts with --output %s", f)
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
			case f == outputJSON:
				audit.RenderJSON(out, d, findings)
			case f == outputYAML:
				var raw bytes.Buffer
				audit.RenderJSON(&raw, d, findings)
				if err := writeJSONAsYAML(out, raw.Bytes()); err != nil {
					return err
				}
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
	f.BoolVar(&jsonFlag, "json", false, "Emit machine-readable JSON (stable schema for tooling; the same as -o json)")
	f.BoolVar(&sarifFlag, "sarif", false, "Emit SARIF 2.1.0 (GitHub code-scanning upload)")
	format = addOutputFlag(cmd)
	return cmd
}

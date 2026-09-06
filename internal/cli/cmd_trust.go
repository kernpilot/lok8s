package cli

// lo trust — install the local development CA into the OS / browser trust
// stores. Port of .lok8s/libs/trust.
//
// The CA is the one the secrets `cert:` generator uses (mkcert's CAROOT). The
// generator deliberately does NOT install it (that needs root); this wraps
// `mkcert -install` so you don't have to fumble $CAROOT. One-time, per
// machine, idempotent. mkcert is needed ONLY for this trust step — never for
// `lo build`.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("trust", newTrustCommand) }

func newTrustCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return &cobra.Command{
		Use:          spec.use,
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mkcert, ok := execx.Look(paths, "mkcert")
			if !ok {
				ui.Error("mkcert not found — install it once for trust:  b install mkcert")
				ui.Error("(mkcert is needed only for this trust step, never for the build.)")
				return ErrHandled
			}

			out := cmd.OutOrStdout()
			caroot := ""
			if raw, err := exec.Command(mkcert, "-CAROOT").Output(); err == nil {
				caroot = strings.TrimRight(string(raw), "\n")
			}
			fmt.Fprintln(out, "Installing the local development CA into your system + browser trust stores.")
			if caroot != "" {
				fmt.Fprintf(out, "  CAROOT: %s\n", caroot)
			}
			fmt.Fprintln(out, "  (you may be prompted for your password)")
			fmt.Fprintln(out)

			// `mkcert -install` creates the CA at CAROOT if absent, then installs
			// it — the SAME CAROOT the cert: generator load-or-creates, so the two
			// share one CA.
			install := exec.Command(mkcert, "-install")
			install.Stdout = out
			install.Stderr = cmd.ErrOrStderr()
			install.Stdin = os.Stdin
			if err := install.Run(); err != nil {
				ui.Error("mkcert -install failed")
				return ErrHandled
			}

			flagDomain, _ := cmd.Flags().GetString("domain")
			resolved := domain.Resolve(flagDomain, paths.Clusters, cmd.ErrOrStderr())
			fmt.Fprintln(out)
			fmt.Fprintf(out, "Done. cert: leaves signed by this CA (e.g. *.%s) are now trusted\n", resolved)
			fmt.Fprintln(out, "by your browser and curl. Re-running is safe.")
			return nil
		},
	}
}

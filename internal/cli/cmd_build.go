package cli

// lo build — build the domain kustomization into one artifacts.yaml (plus
// the spec-declared split emit). Go port of .lok8s/libs/build main::build;
// the pipeline itself lives in internal/build.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("build", newBuildCommand) }

func newBuildCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var (
		clusterOverride string
		splitFlag       bool
		singleFlag      bool
		noSecretsFlag   bool
	)
	cmd := &cobra.Command{
		Use:          "build",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()

			// -v/--verbose → DEBUG, like the argsh entrypoint.
			if v, _ := cmd.Flags().GetCount("verbose"); v > 0 {
				os.Setenv("DEBUG", "1")
			}

			// Domain: the canonical precedence chain (--domain flag >
			// DOMAIN_NAME env > clusters/.active > lok8s.dev). NOTE the
			// argsh `~domain` validator does NOT run here — argsh skips
			// type validation for pre-set locals, and main's resolved
			// domain pre-sets it — so an unknown --domain value flows
			// through to the banner and fails at the kustomization guard,
			// exactly like bash (verified live against the argsh
			// implementation).
			domainFlag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(domainFlag, paths.Clusters, stderr)

			if splitFlag && singleFlag {
				ui.Errorf(stderr, "--split and --single are mutually exclusive")
				return ErrHandled
			}
			// --no-secrets is a split-time modifier: it only makes sense
			// when a split is actually emitted. With --single there is no
			// split dir to shape, so the flag would be a silent no-op —
			// reject the contradiction loudly.
			if noSecretsFlag && singleFlag {
				ui.Errorf(stderr, "--no-secrets and --single are mutually exclusive (--no-secrets shapes the split emit)")
				return ErrHandled
			}

			// The split trigger itself lives in build.Artifacts (so EVERY
			// build path honors the spec; a flag-only trigger would let dev
			// builds silently stale the committed GitOps dir). The flags
			// are debug overrides communicated via Options.SplitOverride
			// (bash: LOK8S_BUILD_SPLIT) and WARN when they contradict the
			// spec.
			mode := build.ArtifactsMode(filepath.Join(paths.Clusters, d))
			override := ""
			if singleFlag {
				if mode == "split" {
					ui.Warnf(stderr, "--single overrides spec.build.artifacts=split — the committed artifacts/ dir is now STALE for %s", d)
				}
				override = "0"
			} else if splitFlag {
				if mode != "split" {
					ui.Warnf(stderr, "--split without spec.build.artifacts=split — one-off output; declare it in the spec so every build (CI, recovery) matches")
				}
				override = "1"
			}

			// Say which domain this is, BEFORE touching anything. `lo
			// build` writes clusters/<domain>/artifacts* and reads
			// clusters/<domain>/secrets — a run against the wrong domain
			// renders one cluster's manifests from another cluster's secret
			// store (one such run re-keyed a live database's encryption
			// secrets and it could no longer decrypt itself). The domain
			// comes from --domain > DOMAIN_NAME > clusters/.active, and
			// that last one is state a `lo use` persisted possibly hours
			// ago; domain.Resolve warns on a DISAGREEMENT, but not when
			// nothing disagrees and the answer is simply not what the
			// operator assumed. Unconditional, on stderr, so piping the
			// artifacts is unaffected.
			fmt.Fprintf(stderr, "lo build: domain %s\n", d)

			// Ambient KUBECONFIG default, exactly what the argsh
			// entrypoint exported before dispatching (lo main): spec
			// metadata.name > --cluster flag > LOK8S_CLUSTER_NAME >
			// "local".
			clusterFlag, _ := cmd.Flags().GetString("cluster")
			os.Setenv("KUBECONFIG", build.AmbientKubeconfig(paths, d, clusterFlag))

			// Kubeconfig pass A: deploy domains follow their clusterRef.
			if err := build.ResolveKubeconfigForDomain(paths, d, clusterOverride, stderr); err != nil {
				return ErrHandled
			}

			// --no-secrets rides through independent of the split trigger —
			// it only shapes WHAT a split emits.
			err := build.Artifacts(build.Options{
				Paths:         paths,
				Domain:        d,
				SplitOverride: override,
				NoSecrets:     build.NoSecretsEffective(noSecretsFlag),
				Stderr:        stderr,
			})
			if err != nil {
				return ErrHandled
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&clusterOverride, "cluster-override", "", "Override cluster domain for kubeconfig resolution")
	f.BoolVar(&splitFlag, "split", false, "Also emit per-resource files under artifacts/ (debug override; declare it in spec.build.artifacts instead)")
	f.BoolVar(&singleFlag, "single", false, "Skip the split emit even when the spec declares it (debug override)")
	f.BoolVar(&noSecretsFlag, "no-secrets", false, "Split ONLY non-Secret resources; never render, re-encrypt, prune, or even read committed Secret.*.sops.yaml (CI render path — no store/key needed)")
	return cmd
}

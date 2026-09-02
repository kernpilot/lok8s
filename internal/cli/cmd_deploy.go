package cli

// lo deploy — apply the domain artifact (clusters/<domain>/artifacts.yaml).
// Go port of .lok8s/libs/deploy main::deploy; the apply phases live in
// internal/deploy over the ported kapply.Applier.

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/deploy"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
)

func init() { registerPorted("deploy", newDeployCommand) }

// deployer is the routing seam main::deploy selects on (bash: the bats stub
// deploy::apply / deploy::apply_filtered to assert the -l routing).
type deployer interface {
	Apply(ctx context.Context, domain string) error
	ApplyFiltered(ctx context.Context, domain, labelKey, labelValue string) error
}

func newDeployCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var (
		label           string
		clusterOverride string
	)
	cmd := &cobra.Command{
		Use:          "deploy",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			// No positional in the argsh spec: a stray one is a parse error
			// there ("too many arguments", rc 2); same message, exit 1.
			if len(args) > 0 {
				return argshErrorf(stderr, "too many arguments: %s", args[0])
			}
			// The entrypoint's pre-dispatch exports (DEBUG, LOK8S_FORCE_RECREATE,
			// DOMAIN_NAME, the ambient KUBECONFIG …) — kapply reads
			// LOK8S_FORCE_RECREATE at construction, so this runs first.
			d := ambientMainEnv(cmd, paths)

			// Kubeconfig pass A: a deploy domain follows its clusterRef (or the
			// --cluster-override) BEFORE the label is even looked at, like bash.
			if err := build.ResolveKubeconfigForDomain(paths, d, clusterOverride, stderr); err != nil {
				return ErrHandled
			}

			dep := &deploy.Deployer{
				Paths:   paths,
				Applier: kapply.NewApplier(execx.NewRunner(paths), cmd.OutOrStdout(), stderr),
				Stderr:  stderr,
			}
			return runDeploy(cmd.Context(), dep, stderr, d, label)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&label, "label", "l", "", "Only deploy resources with this label (key=value; key may be lok8s.dev/<x> or any label key)")
	f.StringVar(&clusterOverride, "cluster-override", "", "Override cluster domain for kubeconfig resolution")
	return cmd
}

// runDeploy is the -l routing (bash: main::deploy after the kubeconfig
// resolution): a label routes to the filtered apply after the key=value
// guard; none routes to the full artifact apply.
func runDeploy(ctx context.Context, dep deployer, stderr io.Writer, domain, label string) error {
	if label != "" {
		key, value, err := deploy.ParseLabel(stderr, label)
		if err != nil {
			return deployRun(err)
		}
		return deployRun(dep.ApplyFiltered(ctx, domain, key, value))
	}
	return deployRun(dep.Apply(ctx, domain))
}

// deployRun maps the deploy package's outcomes onto the process contract:
// an already-printed error exits 1 silently; a kubectl apply failure exits
// with kubectl's own status (bash: set -e propagated it).
func deployRun(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, deploy.ErrHandled) {
		return ErrHandled
	}
	var ee *deploy.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}
	return err
}

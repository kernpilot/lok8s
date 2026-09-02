package cli

// lo hooks — dev hook actions on rendered artifacts (internal; driven by the
// Tilt hooks: wrapper). Go port of .lok8s/libs/hooks (main::hooks); the
// actions live in internal/hooks.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/hooks"
)

func init() { registerPorted("hooks", newHooksCommand) }

// hooksRun maps the hooks package's already-printed sentinel onto the cli one.
func hooksRun(err error) error {
	if err == hooks.ErrHandled {
		return ErrHandled
	}
	return err
}

func newHooksCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "hooks",
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
		newHooksAction(paths, "recreate", "Delete + apply selected objects (immutable Jobs re-run)",
			commandSpec{destructive: true},
			func(c *hooks.Context, cmd *cobra.Command, selector string) error {
				return c.Recreate(cmd.Context(), selector)
			}),
		newHooksAction(paths, "restart", "Rollout restart selected workloads",
			commandSpec{destructive: true},
			func(c *hooks.Context, cmd *cobra.Command, selector string) error {
				return c.Restart(cmd.Context(), selector)
			}),
		newHooksAction(paths, "apply", "Re-apply selected objects",
			commandSpec{destructive: true, idempotent: true},
			func(c *hooks.Context, cmd *cobra.Command, selector string) error {
				return c.Apply(cmd.Context(), selector)
			}),
	)
	return cmd
}

func newHooksAction(paths *config.Paths, name, short string, marks commandSpec,
	run func(*hooks.Context, *cobra.Command, string) error) *cobra.Command {
	c := &cobra.Command{
		Use:          name,
		Short:        short,
		Annotations:  marks.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Driven by Tilt's local_resource — there is no controlling
			// terminal. Force non-interactive so kapply (and anything else)
			// skip the /dev/tty progress UI + confirm prompts, which fail
			// with "No such device or address" here (bash: main::hooks).
			os.Setenv("LOK8S_NONINTERACTIVE", "1")
			d := ambientMainEnv(cmd, paths)
			// bash: _resolve_kubeconfig_for_domain — deploy domains follow
			// their clusterRef before any kubectl runs.
			if err := build.ResolveKubeconfigForDomain(paths, d, "", cmd.ErrOrStderr()); err != nil {
				return ErrHandled
			}
			selector, _ := cmd.Flags().GetString("selector")
			hc := &hooks.Context{
				Paths:  paths,
				Runner: execx.NewRunner(paths),
				Out:    cmd.OutOrStdout(),
				ErrOut: cmd.ErrOrStderr(),
				Domain: d,
			}
			return hooksRun(run(hc, cmd, selector))
		},
	}
	c.Flags().String("selector", "", "Label selector (k=v,k2=v2)")
	return c
}

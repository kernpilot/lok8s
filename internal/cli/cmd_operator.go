package cli

// lo operator — the shell-operator hook implementations (internal; exec'd
// by the operator/hooks/*.sh shims inside the operator image). Go port of
// the bash hooks frozen at .lok8s/legacy/operator/hooks/; the bodies live
// in internal/operator.
//
// Go-only: shell-operator discovers hooks by executable path under /hooks,
// so the shims keep their names and exec `lo operator <hook> "$@"`; there
// was never an argsh usage entry. Argument handling follows the hook
// contract, not cobra's: `--config` (first argument) prints the binding
// configuration; anything else runs the trigger against
// $BINDING_CONTEXT_PATH (extra arguments are ignored, as the bash `${1:-}`
// test ignored them).

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/deploy"
	_ "github.com/kernpilot/lok8s/internal/driver/lo" // registers the lo driver
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/gitops"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/operator"
)

// operatorHooks are the hook names, one per shim in operator/hooks/.
var operatorHooks = []struct {
	name  string
	short string
}{
	{"lo-reconcile", "Lo CRD lifecycle: provision, converge, publish kubeconfig, finalizer-guarded teardown"},
	{"capi-reconcile", "Capi CRD reconciler: render + apply CAPI manifests, finalizer-guarded teardown"},
	{"capi-status-sync", "CAPI Cluster status → Capi CR status, post-provision actions"},
}

func newOperatorCommand(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "operator",
		Short:        "shell-operator hook implementations (internal; exec'd by operator/hooks/*.sh)",
		Hidden:       true,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}
	for _, h := range operatorHooks {
		name := h.name
		cmd.AddCommand(&cobra.Command{
			Use:                name + " [--config]",
			Short:              h.short,
			DisableFlagParsing: true,
			SilenceUsage:       true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runOperatorHook(cmd, name, args)
			},
		})
	}
	return cmd
}

// runOperatorHook is the hook's main: `--config` or trigger. The runtime
// layout comes from the operator env (LOK8S_STATE_DIR, PATH_LOK8S, …), NOT
// from the project resolution `lo` did at startup — the image has no
// project; runtime.sh built the same layout from its own location.
//
// `--config` is answered BEFORE the state dir is materialized: the binding
// configuration is static, and shell-operator asks for it at startup —
// the capi hooks answered it without touching the state dir; lo-reconcile
// sourced runtime.sh (mkdir) first, which is the one ordering this port
// does not keep (a --config that fails on an unwritable /var/lib/lok8s
// serves nobody).
func runOperatorHook(cmd *cobra.Command, name string, args []string) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()

	env := operator.ResolveEnv()
	p := env.Paths()
	runner := execx.NewRunner(p)

	var hook operator.Hook
	switch name {
	case "lo-reconcile":
		hook = &operator.LoHook{
			Paths: p, Runner: runner, Stdout: stdout, Stderr: stderr,
			BootstrapApply: bootstrap.ApplyHook(p, runner, stdout, stderr),
		}
	case "capi-reconcile":
		hook = &operator.CapiHook{
			Paths: p, Runner: runner, Stdout: stdout, Stderr: stderr,
			TemplateDir: env.CapiTemplateDir(),
		}
	case "capi-status-sync":
		hook = &operator.CapiStatusSyncHook{
			Paths: p, Runner: runner, Stdout: stdout, Stderr: stderr,
			GitopsBootstrap: gitops.BootstrapHook(stderr),
			DeployApply: func(ctx context.Context, domain string) error {
				dep := &deploy.Deployer{
					Paths:   p,
					Applier: kapply.NewApplier(runner, stdout, stderr),
					Stderr:  stderr,
				}
				return dep.Apply(ctx, domain)
			},
		}
	default:
		return fmt.Errorf("unknown operator hook: %s", name)
	}

	if len(args) > 0 && args[0] == "--config" {
		fmt.Fprint(stdout, hook.Config())
		return nil
	}

	if err := env.Export(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ErrHandled
	}
	// A hook has no controlling terminal: never prompt, never draw (kapply,
	// the bootstrap engine and the drivers all read this).
	os.Setenv("LOK8S_NONINTERACTIVE", "1")

	events, err := operator.ReadBindingContext(stderr, os.Getenv("BINDING_CONTEXT_PATH"))
	if err != nil {
		return operatorExit(err)
	}
	return operatorExit(hook.Trigger(cmd.Context(), events))
}

// operatorExitProcess is os.Exit behind a seam (tests).
var operatorExitProcess = os.Exit

// operatorExit maps a hook error to the process outcome: an ExitError
// whose status is not 1 ends the process with that status (the bash hook's
// own jq status — shell-operator logs it), everything else is the plain
// exit 1 (message already printed).
func operatorExit(err error) error {
	if err == nil {
		return nil
	}
	var ee *operator.ExitError
	if errors.As(err, &ee) && ee.Code != 1 {
		operatorExitProcess(ee.Code)
		return ErrHandled
	}
	if errors.As(err, &ee) || errors.Is(err, operator.ErrHandled) {
		return ErrHandled
	}
	return err
}

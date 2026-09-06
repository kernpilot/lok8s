package cli

// lo registry — container registry management. Go port of
// .lok8s/drivers/lo/libs/registry (main::registry + the registry::* CLI
// wrappers); the bodies live in internal/driver/lo (registrycli.go).
//
// Domain resolution stays here (flag > env > .active, via ambientMainEnv);
// the driver gate (_registry_init → domain::require_driver lo) runs BEFORE
// any docker/network command — a non-Lo domain gets the actionable
// mismatch message instead of dying three layers down on a spec field the
// other driver never has.

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/driver"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/execx"
)

func init() { registerPorted("registry", newRegistryCommand) }

// registryDeps are the command's seams.
type registryDeps struct {
	runner execx.Runner
	// isTTY is `[[ -t 1 ]]` for the status markers (nil = stdout terminal).
	isTTY func() bool
}

func newRegistryCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var shared bool
	cmd := &cobra.Command{
		Use:          "registry",
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
	cmd.PersistentFlags().BoolVarP(&shared, "shared", "S", false, "Include shared mirrors (for clean/status)")
	argshFlagErrors(cmd)

	// The leaves mirror the argsh usage verbatim — which carries NO
	// @destructive/@readonly markers on the registry commands (so neither
	// did the MCP tools the bash server exposed).
	sub := func(use, alias, short string, run func(ctx context.Context, drv *lodriver.Driver, d string, out, errOut io.Writer, deps registryDeps) error) *cobra.Command {
		return &cobra.Command{
			Use:     use,
			Aliases: []string{alias},
			Short:   short,
			Args:    secretsArgs(0, 0),
			// The registry::* bodies have no :args of their own: an unknown
			// flag after the subcommand reaches them unparsed and is ignored.
			FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
			SilenceUsage:       true,
			RunE: func(cmd *cobra.Command, args []string) error {
				out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
				d := ambientMainEnv(cmd, paths)
				deps := registryDeps{runner: newRunner(paths)}
				if err := registryGate(paths, d, stderr); err != nil {
					return ErrHandled
				}
				drv := lodriver.New(&driver.Deps{Paths: paths, Runner: deps.runner, Stderr: stderr})
				drv.SetOutput(out)
				if err := run(cmd.Context(), drv, d, out, stderr, deps); err != nil {
					// Every diagnostic is printed where it happens (the lib
					// mirrors the bash); rc 1 without a second line.
					return ErrHandled
				}
				return nil
			},
		}
	}

	cmd.AddCommand(
		sub("up", "u", "Spin up registries", func(ctx context.Context, drv *lodriver.Driver, d string, out, errOut io.Writer, _ registryDeps) error {
			return drv.RegistryUp(ctx, d, out, errOut)
		}),
		sub("down", "d", "Spin down registries", func(ctx context.Context, drv *lodriver.Driver, d string, _, errOut io.Writer, _ registryDeps) error {
			return drv.RegistryDown(ctx, d, errOut)
		}),
		sub("status", "s", "Check registry status", func(ctx context.Context, drv *lodriver.Driver, d string, out, errOut io.Writer, deps registryDeps) error {
			isTTY := deps.isTTY
			if isTTY == nil {
				isTTY = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
			}
			return drv.RegistryStatus(ctx, d, shared, isTTY(), out, errOut)
		}),
		sub("clean", "c", "Clean up registries", func(ctx context.Context, drv *lodriver.Driver, d string, _, errOut io.Writer, _ registryDeps) error {
			return drv.RegistryClean(ctx, d, shared, errOut)
		}),
	)
	return cmd
}

// registryGate is the driver check of _registry_init: skipped when a
// registry JSON is already loaded (LOK8S_REGISTRY_JSON — Tilt subshells),
// otherwise the domain MUST be a Lo cluster.
func registryGate(paths *config.Paths, domainName string, stderr io.Writer) error {
	if path := os.Getenv("LOK8S_REGISTRY_JSON"); path != "" && fileExists(path) {
		return nil
	}
	if err := domain.RequireDriver("lo", paths.Clusters, domainName, "registry management", stderr); err != nil {
		return errors.New("registry: driver gate refused")
	}
	return nil
}

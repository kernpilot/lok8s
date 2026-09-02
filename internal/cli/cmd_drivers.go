package cli

// lo drivers — direct access to a driver's own CLI (`lo drivers <name>
// <cmd>`). Go port of .lok8s/libs/drivers (main::drivers).
//
// Mostly a debugging/ops door — the normal path is `lo provision`, which
// routes through the dispatch (provision.Dispatcher: spec resolution,
// provider loading, the real-infrastructure gate, the bootstrap tail). This
// command does what the bash did: call the driver contract function
// DIRECTLY, with no gate and no provider — `lo drivers lo provision <domain>`
// is driver::provision alone. provision/destroy stay marked destructive.
//
// Dispatch: a name in the Go driver registry gets the Go driver
// (provision/destroy/status/kubeconfig, positional <domain>, exactly the
// bash main::driver grammar). A name that only exists as a bash driver
// (.lok8s/drivers/<name>/main — e.g. kubehz until it ports) falls back to
// the argsh implementation via Shim, argv verbatim. `--list` prints the
// UNION of both worlds so nothing disappears mid-migration.
//
// Note on help: the argsh :args intercepted -h/--help at the `drivers` level
// (a `lo drivers lo status --help` printed the drivers usage, never the
// driver's). Cobra resolves the nested command first, so here --help reaches
// the per-driver subcommand — a documented improvement, not a parity goal.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"

	// The Go drivers register themselves in init(); the binary must link
	// them for `lo drivers` to find them (the provision port will import
	// them too — the registry dedups nothing, so keep the set here in sync).
	_ "github.com/kernpilot/lok8s/internal/driver/capi"
	_ "github.com/kernpilot/lok8s/internal/driver/kkp"
	_ "github.com/kernpilot/lok8s/internal/driver/kubeone"
	_ "github.com/kernpilot/lok8s/internal/driver/lo"
)

func init() {
	registerPorted("drivers", func(paths *config.Paths, spec commandSpec) *cobra.Command {
		return newDriversCommand(paths, spec, defaultDriversDeps(paths))
	})
}

// driversDeps are the command's injectable seams (tests register fake
// drivers in a test registry and capture the shim/exit calls).
type driversDeps struct {
	// names lists the Go driver registry (driver.Names).
	names func() []string
	// lookup resolves a Go driver factory (driver.Get).
	lookup func(name string) (driver.Factory, bool)
	// runner is handed to the driver (execx.NewRunner; a fake under test).
	runner execx.Runner
	// shim passes argv to the argsh implementation (Shim).
	shim func(argv []string) error
	// exit ends the process with a driver's own exit code (os.Exit).
	exit func(code int)
}

func defaultDriversDeps(paths *config.Paths) driversDeps {
	return driversDeps{
		names:  driver.Names,
		lookup: driver.Get,
		runner: execx.NewRunner(paths),
		shim:   func(argv []string) error { return Shim(paths, argv) },
		exit:   os.Exit,
	}
}

// driverUsage carries each bash driver's main::driver usage table verbatim
// (title + per-command short text). Unknown Go drivers get a generic one.
type driverUsage struct {
	title                                  string
	provision, destroy, status, kubeconfig string
	kubeconfigReadonly                     bool
}

var driverUsages = map[string]driverUsage{
	"lo": {
		title: "Lo driver", provision: "Provision a cluster", destroy: "Destroy a cluster",
		status: "Check cluster status", kubeconfig: "Extract kubeconfig (writes .kubeconfig/, may open an SSH tunnel)",
	},
	"capi": {
		title: "CAPI driver", provision: "Provision a CAPI cluster", destroy: "Destroy a CAPI cluster",
		status: "Check cluster status", kubeconfig: "Extract kubeconfig path", kubeconfigReadonly: true,
	},
	"kubeone": {
		title: "KubeOne driver", provision: "Provision a KubeOne cluster", destroy: "Destroy a KubeOne cluster",
		status: "Check cluster status", kubeconfig: "Extract kubeconfig path", kubeconfigReadonly: true,
	},
	"kkp": {
		title: "KKP driver", provision: "Provision a KKP cluster", destroy: "Destroy a KKP cluster",
		status: "Check cluster status", kubeconfig: "Extract kubeconfig path", kubeconfigReadonly: true,
	},
	"kubehz": {
		title: "kubehz space driver", provision: "Create/adopt the space + mint node join tickets", destroy: "Remove the space from kubehz",
		status: "Show space + node status", kubeconfig: "Explain how space access works (no kubeconfig download)", kubeconfigReadonly: true,
	},
}

func usageFor(name string) driverUsage {
	if u, ok := driverUsages[name]; ok {
		return u
	}
	return driverUsage{
		title: name + " driver", provision: "Provision a cluster", destroy: "Destroy a cluster",
		status: "Check cluster status", kubeconfig: "Extract kubeconfig path", kubeconfigReadonly: true,
	}
}

func newDriversCommand(paths *config.Paths, spec commandSpec, deps driversDeps) *cobra.Command {
	var list int
	cmd := &cobra.Command{
		Use:          "drivers [name] [args...]",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if list > 0 {
				fmt.Fprint(out, "Available drivers:\n\n")
				for _, d := range driversList(paths, deps) {
					fmt.Fprintf(out, "- %s\n", d)
				}
				return nil
			}
			// No driver named: a bare call gets a pointer to --list instead
			// of the misleading "Driver '' not found".
			if len(args) == 0 || args[0] == "" {
				ui.Errorf(stderr, "Driver name required — try: lo drivers --list")
				return ErrHandled
			}
			name := args[0]
			// The name lands in a path the bash sourced — same allowlist.
			if !driver.NameRe.MatchString(name) {
				ui.Errorf(stderr, "Invalid driver name: %s", name)
				return ErrHandled
			}
			// A Go driver never reaches here (its subcommand resolves first);
			// this is the bash-only fallback.
			if fileExists(filepath.Join(paths.Lok8s, "drivers", name, "main")) {
				return deps.shim(os.Args[1:])
			}
			ui.Errorf(stderr, "Driver '%s' not found", name)
			return ErrHandled
		},
	}
	cmd.Flags().CountVarP(&list, "list", "l", "List available drivers")

	for _, name := range deps.names() {
		cmd.AddCommand(newDriverSubcommand(paths, deps, name))
	}
	return cmd
}

// driversList is the union of the Go registry and the bash driver
// directories (bash: for dir in "${PATH_LOK8S}/drivers"/*/), sorted.
func driversList(paths *config.Paths, deps driversDeps) []string {
	seen := map[string]bool{}
	for _, n := range deps.names() {
		seen[n] = true
	}
	entries, _ := os.ReadDir(filepath.Join(paths.Lok8s, "drivers"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, err := os.Stat(filepath.Join(paths.Lok8s, "drivers", e.Name())); err == nil && info.IsDir() {
			seen[e.Name()] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	// bash `*/` glob order (the trailing slash takes part in the comparison).
	sort.Slice(names, func(i, j int) bool { return names[i]+"/" < names[j]+"/" })
	return names
}

// newDriverSubcommand builds `lo drivers <name>` over a Go driver: the four
// contract commands with the driver's own positional-domain grammar.
func newDriverSubcommand(paths *config.Paths, deps driversDeps, name string) *cobra.Command {
	u := usageFor(name)
	cmd := &cobra.Command{
		Use:          name,
		Short:        u.title,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}
	op := func(use, alias, short string, annotations map[string]string, run func(ctx context.Context, drv driver.Driver, domain string, cmd *cobra.Command) error) *cobra.Command {
		return &cobra.Command{
			Use: use + " <domain>", Aliases: []string{alias},
			Short:        short,
			Annotations:  annotations,
			Args:         cobra.ArbitraryArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				stderr := cmd.ErrOrStderr()
				if len(args) == 0 {
					return argshErrorf(stderr, "missing required argument: domain")
				}
				if len(args) > 1 {
					return argshErrorf(stderr, "too many arguments: %s", args[1])
				}
				setDebugFromVerbose(cmd)
				factory, ok := deps.lookup(name)
				if !ok {
					ui.Errorf(stderr, "Driver '%s' not found", name)
					return ErrHandled
				}
				drv, err := factory(&driver.Deps{Paths: paths, Runner: deps.runner, Stderr: stderr})
				if err != nil {
					return err
				}
				return driverExit(deps, run(cmd.Context(), drv, args[0], cmd))
			},
		}
	}
	destructive := map[string]string{AnnotationDestructive: "true"}
	readonly := map[string]string{AnnotationReadonly: "true"}
	var kubeconfigAnn map[string]string
	if u.kubeconfigReadonly {
		kubeconfigAnn = readonly
	}
	cmd.AddCommand(
		op("provision", "p", u.provision, destructive, func(ctx context.Context, drv driver.Driver, d string, _ *cobra.Command) error {
			return drv.Provision(ctx, d)
		}),
		op("destroy", "d", u.destroy, destructive, func(ctx context.Context, drv driver.Driver, d string, _ *cobra.Command) error {
			return drv.Destroy(ctx, d)
		}),
		op("status", "s", u.status, readonly, func(ctx context.Context, drv driver.Driver, d string, cmd *cobra.Command) error {
			status, err := drv.Status(ctx, d)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), status)
			return nil
		}),
		op("kubeconfig", "k", u.kubeconfig, kubeconfigAnn, func(ctx context.Context, drv driver.Driver, d string, cmd *cobra.Command) error {
			path, err := drv.Kubeconfig(ctx, d)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		}),
	)
	return cmd
}

// driverExit maps a driver error onto the process exit the bash produced:
// the driver's own rc passes through (a gate-decline sentinel 3, the
// remote-CI 100, a subprocess status — all already reported by whoever
// produced them); a plain error exits 1 with its message.
func driverExit(deps driversDeps, err error) error {
	if err == nil {
		return nil
	}
	if rc := driver.ExitCode(err); rc != 1 {
		deps.exit(rc)
		return ErrHandled
	}
	return err
}

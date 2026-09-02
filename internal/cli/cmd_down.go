package cli

// lo down / lo clean — teardown. Go port of main::down + main::clean
// (.lok8s/lo).
//
// main::down is driver-aware: cloud drivers (capi/kkp/kubeone) provision
// real infrastructure and must be deprovisioned via the driver's destroy
// contract; only the local kind driver (lo) is torn down here by deleting
// the kind cluster + Tilt + registries. The routing reads `.kind` through
// provision.ReadKind — the ONE validating reader — so a spec WITHOUT one
// can never reach driver-destroy (the bats in
// tests/unit/lo_down_routing_test.bats pin this; the Go twin is
// cmd_down_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/driver"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/tilt"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() {
	registerPorted("down", newDownCommand)
	registerPorted("clean", newCleanCommand)
}

// downDeps are main::down's side effects, injectable so the routing is
// testable with every one of them recorded instead of performed.
type downDeps struct {
	paths  *config.Paths
	runner execx.Runner
	out    io.Writer
	stderr io.Writer
	// dispatchDestroy is provision::dispatch_destroy.
	dispatchDestroy func(ctx context.Context, domainName string) error
	// tiltDown is tilt::down (force = the inherited global --force).
	tiltDown func(ctx context.Context) error
}

func defaultDownDeps(cmd *cobra.Command, paths *config.Paths) downDeps {
	disp := newDispatcher(cmd, paths)
	force, _ := cmd.Flags().GetBool("force")
	tc := &tilt.Context{Paths: paths, Runner: disp.Runner, Out: cmd.OutOrStdout(), ErrOut: cmd.ErrOrStderr(), Stdin: cmd.InOrStdin()}
	return downDeps{
		paths:           paths,
		runner:          disp.Runner,
		out:             cmd.OutOrStdout(),
		stderr:          cmd.ErrOrStderr(),
		dispatchDestroy: disp.DispatchDestroy,
		tiltDown:        func(ctx context.Context) error { return tc.Down(ctx, force) },
	}
}

func newDownCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return argshFlagErrors(&cobra.Command{
		Use:         "down",
		Aliases:     spec.aliases,
		Short:       spec.short,
		GroupID:     spec.group,
		Annotations: spec.annotations(),
		Args:        cobra.ArbitraryArgs, // bash: main::down ignores positionals
		// … and unknown flags: main::down has no :args, so whatever argsh's
		// global parse did not own reaches it unparsed and is dropped.
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, cluster := ambientMain(cmd, paths)
			return runDown(cmd.Context(), defaultDownDeps(cmd, paths), d, cluster)
		},
	})
}

// runDown is main::down. Errors are already reported (ErrHandled); a gate
// decline is a SILENT rc 1 (nothing ran, nothing is orphaned), any other
// driver-destroy failure prints the orphaned-infra warning.
func runDown(ctx context.Context, deps downDeps, domainName, cluster string) error {
	out := deps.out
	fmt.Fprintf(out, "\n  \033[1;36m%s\033[0m  \033[2mtearing down\033[0m\n", cluster)

	// No spec file at all still means the local path: an unprovisioned or
	// deploy-only domain has nothing but a kind cluster to remove. That is
	// why this reads the kind rather than domain.Driver, which answers
	// "deploy" there and would send those domains down the driver branch.
	spec := filepath.Join(deps.paths.Clusters, domainName, "cluster.lok8s.yaml")
	kind := ""
	if fileExists(spec) {
		k, err := provision.ReadKind(spec, deps.stderr)
		if err != nil {
			// ReadKind has already said WHICH way it is wrong (absent vs
			// malformed). This frame says what to do about it.
			fmt.Fprintf(out, "  \033[31m✗ cannot read the driver from %s — refusing to tear down\033[0m\n", spec)
			fmt.Fprintf(out, "  \033[2m  set .kind to a bare driver name (a directory under .lok8s/drivers), then run lo down again\033[0m\n")
			return ErrHandled
		}
		kind = k
	}
	if kind != "" && kind != "lo" {
		fmt.Fprintf(out, "  \033[2m• destroying %s cluster via its driver (deprovisions infrastructure)\033[0m\n", kind)
		// A failed driver destroy means infrastructure may be ORPHANED (and
		// billed) — a visible non-zero exit, never a silent success. A gate
		// DECLINE (rc 3) is different: nothing ran, nothing is orphaned —
		// exit non-zero without the scary orphan warning.
		if err := deps.dispatchDestroy(ctx, domainName); err != nil {
			if driver.ExitCode(err) == 3 {
				return ErrHandled
			}
			fmt.Fprintf(out, "  \033[31m✗ driver destroy failed — infrastructure may still exist; inspect and re-run\033[0m\n")
			return ErrHandled
		}
		fmt.Fprintln(out)
		return nil
	}

	fmt.Fprintf(out, "  \033[2m• stopping Tilt\033[0m\n")
	_ = deps.tiltDown(ctx)

	if kindClusterListed(ctx, deps.runner, cluster) {
		fmt.Fprintf(out, "  \033[2m• deleting kind cluster (node teardown can take ~10–30s)\033[0m\n")
		_ = deps.runner.Run(ctx, execx.Cmd{Name: "kind", Args: []string{"delete", "cluster", "--name", cluster}, Stdout: out, Stderr: deps.stderr})
	} else {
		fmt.Fprintf(out, "  \033[2m• kind cluster not running\033[0m\n")
	}

	// Registries: a SHARED setup is left up — the mirrors are reused across
	// clusters and build/cache stay warm for a fast next `lo up`. A
	// non-shared setup is project-local with nothing to reuse, so tear its
	// containers down (the named volumes stay, so build cache survives a
	// later recreate).
	if raw, err := os.ReadFile(filepath.Join(deps.paths.Clusters, domainName, ".registries.json")); err == nil {
		var doc struct {
			Shared         bool   `json:"shared"`
			ProjectNetwork string `json:"project_network"`
			Registries     []struct {
				Name string `json:"name"`
			} `json:"registries"`
		}
		// jq on a non-JSON file yields nothing for every field — the
		// non-shared branch with no containers to remove.
		_ = json.Unmarshal(raw, &doc)
		if doc.Shared {
			fmt.Fprintf(out, "  \033[2mℹ registries left up: %d containers (shared mirrors reused; build/cache kept warm)\033[0m\n", len(doc.Registries))
			fmt.Fprintf(out, "  \033[2m  remove:     lo registry down\033[0m\n")
			fmt.Fprintf(out, "  \033[2m  + volumes:  lo registry clean --shared\033[0m\n")
		} else {
			removed := 0
			if doc.ProjectNetwork != "" {
				for _, r := range doc.Registries {
					if r.Name == "" {
						continue
					}
					if err := deps.runner.Run(ctx, execx.Cmd{Name: "docker", Args: []string{"rm", "-f", doc.ProjectNetwork + "-registry-" + r.Name}, Stdout: io.Discard, Stderr: io.Discard}); err == nil {
						removed++
					}
				}
			}
			fmt.Fprintf(out, "  \033[2m• shut down %d registry containers (not shared — nothing to reuse; volumes kept)\033[0m\n", removed)
		}
	}
	fmt.Fprintln(out)
	return nil
}

// kindClusterListed is `kind get clusters 2>/dev/null | grep -qx <name>`.
func kindClusterListed(ctx context.Context, runner execx.Runner, cluster string) bool {
	var buf strings.Builder
	if err := runner.Run(ctx, execx.Cmd{Name: "kind", Args: []string{"get", "clusters"}, Stdout: &buf, Stderr: io.Discard}); err != nil {
		// grep over an empty pipe — a failed `kind` lists nothing.
		if buf.Len() == 0 {
			return false
		}
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == cluster {
			return true
		}
	}
	return false
}

// cleanDeps extend downDeps with main::clean's own seams.
type cleanDeps struct {
	downDeps
	// driverOf is domain::driver (nil → domain.Driver over paths.Clusters).
	driverOf func(domainName string) (string, error)
	// registryClean is registry::clean (project registries only — the
	// `shared` flag belongs to `lo registry clean`, unset here).
	registryClean func(ctx context.Context, domainName string) error
}

func newCleanCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:          "clean",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "too many arguments: %s", args[0])
			}
			d, cluster := ambientMain(cmd, paths)
			dd := defaultDownDeps(cmd, paths)
			deps := cleanDeps{
				downDeps: dd,
				registryClean: func(ctx context.Context, domainName string) error {
					drv := lodriver.New(&driver.Deps{Paths: paths, Runner: dd.runner, Stderr: dd.stderr})
					return drv.RegistryClean(ctx, domainName, false, dd.stderr)
				},
			}
			return runClean(cmd.Context(), deps, d, cluster, all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Clean up all volumes")
	return argshFlagErrors(cmd)
}

// runClean is main::clean: down, then `docker system prune -f` (--all) or
// the cluster's named volumes + the project registries (Lo domains only —
// registries are a Lo-driver artifact, and the driver gate in
// _registry_init would rightly refuse elsewhere).
func runClean(ctx context.Context, deps cleanDeps, domainName, cluster string, all bool) error {
	if err := runDown(ctx, deps.downDeps, domainName, cluster); err != nil {
		return err
	}
	if all {
		if err := deps.runner.Run(ctx, execx.Cmd{Name: "docker", Args: []string{"system", "prune", "-f"}, Stdout: deps.out, Stderr: deps.stderr}); err != nil {
			return dispatchExit(err)
		}
		return nil
	}
	var vols strings.Builder
	_ = deps.runner.Run(ctx, execx.Cmd{Name: "docker", Args: []string{"volume", "ls", "--filter", "name=^" + cluster + "-", "-q"}, Stdout: &vols, Stderr: deps.stderr})
	for _, v := range strings.Fields(vols.String()) {
		if err := deps.runner.Run(ctx, execx.Cmd{Name: "docker", Args: []string{"volume", "rm", "-f", v}, Stdout: deps.out, Stderr: deps.stderr}); err != nil {
			return dispatchExit(err)
		}
	}
	driverOf := deps.driverOf
	if driverOf == nil {
		driverOf = func(d string) (string, error) { return domain.Driver(deps.paths.Clusters, d) }
	}
	if got, err := driverOf(domainName); err == nil && got == "lo" {
		if err := deps.registryClean(ctx, domainName); err != nil {
			return dispatchExit(err)
		}
		return nil
	}
	// Visible, not debug: on a migrated/deleted domain this leaves any
	// lok8s-registry-* containers behind — say so instead of hiding it.
	ui.Warnf(deps.stderr, "skipping registry cleanup — domain '%s' is not a Lo cluster (leftover registries: docker rm -f $(docker ps -aq --filter name=-registry-))", domainName)
	return nil
}

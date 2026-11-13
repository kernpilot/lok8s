// Command lo is the lok8s CLI.
//
// The Go binary is the single entrypoint. Every command runs natively; a
// project routes commands to the frozen bash tree through its lok8s.yaml
// (spec.implementation, internal/cli/routing.go), and the exec shim
// (internal/cli/shim.go) hands them to `<tree>/lo` with the process
// environment prepared. No environment variable selects the
// implementation.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/cli"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/interrupt"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

func main() {
	// Self-exec plugin mode: an in-process `lo build` renders through the
	// kustomize API and serves its exec generators (secrets.lok8s.dev
	// Secret, khelm ChartRenderer) by re-executing THIS binary under the
	// plugin's name from a temp plugin home. Dispatched before anything
	// else — no project paths, no cobra — because the plugin runs in the
	// kustomization directory, not in a project. The same interrupt watch
	// as the command tree cancels a chart render and maps the exit code.
	ctx, interrupted := cli.WatchInterrupt(context.Background())
	if handled, rc := render.DispatchPlugin(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr); handled {
		if irc := interrupted(); irc != 0 {
			rc = irc
		}
		os.Exit(rc)
	}
	// After a SIGHUP that a driver caught (interrupt.CatchHangup), lo ends
	// with that SIGHUP here, when every deferred cleanup of run ran.
	interrupt.Exit(run(ctx, interrupted))
}

// run is main without os.Exit, so the deferred cleanup of the per-run
// assets temp dir (embedded assets served under --no-eject) and of the
// self-exec plugin home runs on every exit path. ctx is cancelled by the
// interrupt watch; interrupted reports its exit code (128+n) once one
// arrived.
func run(ctx context.Context, interrupted func() int) int {
	defer assets.Cleanup()
	defer render.Cleanup()
	// The confirmed destroy plan is set only by the destroy guard of this
	// run (internal/provision/destroy_guard.go). A value inherited from the
	// caller's environment must not reach a provider through any path,
	// a command routed to the bash tree included.
	os.Unsetenv(driver.DestroyPlanEnv)
	// The keys-only setting of the Secret plugin is internal to lo: only
	// `lo build --keys-only` turns it on, for its own render. A value
	// inherited from the caller's environment must not reach any child
	// (kustomize, a plugin, Tilt, a command routed to the bash tree, the
	// registry TLS mint), so it is 0 from here on. This runs AFTER the
	// plugin dispatch in main (a self-exec plugin child reads the value
	// its render set) and BEFORE anything else reads the environment or
	// starts a child.
	os.Setenv(secret.KeysOnlyEnv, "0")
	paths, err := config.ResolvePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lo: %v\n", err)
		return 1
	}

	// SIGINT/SIGTERM (and a SIGHUP that a driver catches) cancel the
	// command context (every Runner child and wait loop reads it) and map
	// to the exit code the bash entrypoint died with, 128+n, with nothing
	// printed on top.
	root := cli.NewRoot(paths)
	err = root.ExecuteContext(ctx)
	if rc := interrupted(); rc != 0 {
		return rc
	}
	if err != nil {
		// cli.ErrHandled means the command already printed its own message in
		// the bash implementation's format; everything else prints here.
		if !errors.Is(err, cli.ErrHandled) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

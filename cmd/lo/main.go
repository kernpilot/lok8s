// Command lo is the lok8s CLI.
//
// The Go binary is the single entrypoint. Commands not yet ported from the
// argsh implementation are passed through to `.lok8s/lo` via the exec shim
// (internal/cli/shim.go) with the process environment prepared, so both
// implementations behave identically during the migration.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/cli"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
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
	os.Exit(run(ctx, interrupted))
}

// run is main without os.Exit, so the deferred cleanup of the per-run
// assets temp dir (embedded assets served under --no-eject) and of the
// self-exec plugin home runs on every exit path. ctx is cancelled by the
// interrupt watch; interrupted reports its exit code (128+n) once one
// arrived.
func run(ctx context.Context, interrupted func() int) int {
	defer assets.Cleanup()
	defer render.Cleanup()
	paths, err := config.ResolvePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lo: %v\n", err)
		return 1
	}

	// LO_IMPL=bash bypasses the Go implementation entirely — the per-command
	// escape hatch while ports stabilize.
	if os.Getenv("LO_IMPL") == "bash" {
		if err := cli.Shim(paths, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "lo: %v\n", err)
			return 1
		}
		return 0
	}

	// SIGINT/SIGTERM cancel the command context (every Runner child and
	// wait loop reads it) and map to the exit code the bash entrypoint
	// died with, 128+n, with nothing printed on top.
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

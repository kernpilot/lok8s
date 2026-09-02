// Command lo is the lok8s CLI.
//
// The Go binary is the single entrypoint. Commands not yet ported from the
// argsh implementation are passed through to `.lok8s/lo` via the exec shim
// (internal/cli/shim.go) with the process environment prepared, so both
// implementations behave identically during the migration.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/kernpilot/lok8s/internal/cli"
	"github.com/kernpilot/lok8s/internal/config"
)

func main() {
	paths, err := config.ResolvePaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lo: %v\n", err)
		os.Exit(1)
	}

	// LO_IMPL=bash bypasses the Go implementation entirely — the per-command
	// escape hatch while ports stabilize.
	if os.Getenv("LO_IMPL") == "bash" {
		if err := cli.Shim(paths, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "lo: %v\n", err)
			os.Exit(1)
		}
		return
	}

	root := cli.NewRoot(paths)
	if err := root.Execute(); err != nil {
		// cli.ErrHandled means the command already printed its own message in
		// the bash implementation's format; everything else prints here.
		if !errors.Is(err, cli.ErrHandled) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(1)
	}
}

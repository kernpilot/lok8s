//go:build !inprocess

package render

// core.go — the `lo` (core) build: no in-process renderer. The render is
// the subprocess pipeline in render.go (the pinned kustomize binary + the
// b-managed exec plugins under .kustomize/), and the binary never serves
// a kustomize plugin itself. `make build-full` / `-tags inprocess` swaps
// this file for inprocess.go.

import (
	"context"
	"errors"
	"io"
)

const (
	inProcessAvailable = false
	variantName        = "core"
)

// buildInProcess is unreachable on core: CurrentMode never yields
// ModeInProcess here. Kept as a guard so a future call site fails loudly
// instead of silently rendering nothing.
func buildInProcess(_ context.Context, _ string, _ Options) ([]byte, error) {
	return nil, errors.New("render: the in-process renderer is not linked into lo core. Install lo-full")
}

// DispatchPlugin on core serves one plugin only: the ChartRenderer gate of
// a dry run (chartgate.go), which kustomize starts from the dry run's plugin
// home. Every other render execs the plugin BINARIES under
// KUSTOMIZE_PLUGIN_HOME, never this executable. main calls it first so both
// builds share one entry.
func DispatchPlugin(_ context.Context, args []string, _ io.Reader, _, stderr io.Writer) (handled bool, rc int) {
	return dispatchChartGate(args, stderr)
}

// Cleanup is a no-op on core (no self-exec plugin home is ever created).
func Cleanup() {}

// Prepare is a no-op on core (no self-exec plugin home).
func Prepare() error { return nil }

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
	return nil, errors.New("render: in-process renderer not linked (this is lo core; install lo-full)")
}

// DispatchPlugin is a no-op on core: kustomize runs as a child process
// and execs the plugin BINARIES under KUSTOMIZE_PLUGIN_HOME, never this
// executable. main still calls it first so both builds share one entry.
func DispatchPlugin(_ []string, _ io.Reader, _, _ io.Writer) (handled bool, rc int) {
	return false, 0
}

// Cleanup is a no-op on core (no self-exec plugin home is ever created).
func Cleanup() {}

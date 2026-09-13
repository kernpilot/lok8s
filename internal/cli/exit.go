package cli

import (
	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/render"
)

// exitNow ends the process with rc — the rc passthroughs (`tilt ci`'s own
// status, `tilt doctor`, the image-list curl status, kubectl apply's code,
// a bash driver's exit) — AFTER the per-run temp dirs are dropped. A bare
// os.Exit skips main's deferred assets.Cleanup/render.Cleanup and leaked a
// /tmp/lo-assets-* (and lo-full's plugin home) on every non-zero
// passthrough. Both Cleanups are idempotent, so main's defers running
// again on a test-swapped osExit are harmless.
func exitNow(rc int) {
	assets.Cleanup()
	render.Cleanup()
	osExit(rc)
}

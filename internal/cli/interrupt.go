package cli

import (
	"context"

	"github.com/kernpilot/lok8s/internal/interrupt"
)

// WatchInterrupt cancels the returned context on the first SIGINT or
// SIGTERM and reports the exit code that signal maps to (128+n), as
// interrupt.Watch does: see there. A SIGHUP that a driver catches
// (interrupt.CatchHangup) cancels this context too, and maps to 129.
func WatchInterrupt(parent context.Context) (ctx context.Context, exitCode func() int) {
	return interrupt.Watch(parent)
}

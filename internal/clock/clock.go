// Package clock holds the wait seam every retry and poll loop sleeps
// through. Production code sleeps with Sleep, which ends early when the
// context ends (a SIGINT cancels the command context, see cli.WatchInterrupt);
// tests install NoSleep so waits are instant.
package clock

import (
	"context"
	"time"
)

// SleepFunc waits d or until ctx ends. It returns nil after a full wait
// and ctx.Err() when the context ended first, so a loop that sleeps
// through it stops on a cancel instead of running its remaining retries.
type SleepFunc func(ctx context.Context, d time.Duration) error

// Sleep is the production SleepFunc.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NoSleep is the test fake: it returns at once. It still reports an ended
// context, so a loop under test stops on a cancel the way it does in
// production.
func NoSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

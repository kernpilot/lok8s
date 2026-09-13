package cli

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// A SIGINT cancels the command context and maps to the exit code the bash
// entrypoint died with (128+2); no signal maps to 0.
func TestWatchInterruptCancelsAndMapsTheExitCode(t *testing.T) {
	ctx, exitCode := WatchInterrupt(t.Context())
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the context")
	}
	if got := exitCode(); got != 130 {
		t.Errorf("exit code = %d, want 130", got)
	}

	quiet, exitCode := WatchInterrupt(t.Context())
	if quiet.Err() != nil {
		t.Errorf("context cancelled before any signal: %v", quiet.Err())
	}
	if got := exitCode(); got != 0 {
		t.Errorf("exit code without a signal = %d, want 0", got)
	}
	if quiet.Err() == nil {
		t.Error("exitCode must release the context")
	}
}

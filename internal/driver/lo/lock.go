package lo

// lock.go — the Go equivalent of the bash `flock -w 60` best-effort
// serialization. Same lock FILES as the bash (the .lock/.netlock paths under
// LO_REGISTRY_STATE_DIR), so a Go `lo` and a bash `lo` running concurrently
// on one host still exclude each other.

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/kernpilot/lok8s/internal/clock"
)

// flockTimeout mirrors `flock -w 60`.
const flockTimeout = 60 * time.Second

// acquireLock opens (creating) path and takes an exclusive flock, waiting up
// to flockTimeout. Best-effort like the bash: returns a nil release func
// (proceed unlocked) when the file cannot be opened, and proceeds unlocked
// after the wait times out — the bash debug'd and continued too. A
// cancelled context ends the wait the same way.
func acquireLock(ctx context.Context, path string, sleep clock.SleepFunc) (release func(), locked bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, false
	}
	deadline := time.Now().Add(flockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, true
		}
		if time.Now().After(deadline) {
			// Lock wait timed out — proceed unlocked (bash: debug + continue).
			_ = f.Close()
			return nil, false
		}
		if sleep(ctx, 200*time.Millisecond) != nil {
			_ = f.Close()
			return nil, false
		}
	}
}

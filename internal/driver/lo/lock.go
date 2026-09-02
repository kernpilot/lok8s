package lo

// lock.go — the Go equivalent of the bash `flock -w 60` best-effort
// serialization. Same lock FILES as the bash (the .lock/.netlock paths under
// LO_REGISTRY_STATE_DIR), so a Go `lo` and a bash `lo` running concurrently
// on one host still exclude each other.

import (
	"os"
	"syscall"
	"time"
)

// flockTimeout mirrors `flock -w 60`.
const flockTimeout = 60 * time.Second

// acquireLock opens (creating) path and takes an exclusive flock, waiting up
// to flockTimeout. Best-effort like the bash: returns a nil release func
// (proceed unlocked) when the file cannot be opened, and proceeds unlocked
// after the wait times out — the bash debug'd and continued too.
func acquireLock(path string, sleep func(time.Duration)) (release func(), locked bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, false
	}
	deadline := time.Now().Add(flockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, true
		}
		if time.Now().After(deadline) {
			// Lock wait timed out — proceed unlocked (bash: debug + continue).
			f.Close()
			return nil, false
		}
		sleep(200 * time.Millisecond)
	}
}

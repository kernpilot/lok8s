package toolchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// writeB writes an executable .bin/b with the given sh body.
func writeB(t *testing.T, bin, body string) {
	t.Helper()
	b := filepath.Join(bin, "b")
	testutil.WriteFile(t, b, "#!/bin/sh\n"+body)
	if err := os.Chmod(b, 0o700); err != nil {
		t.Fatal(err)
	}
}

// stuckB writes a .bin/b that ignores SIGINT, then writes its pid to
// dir/ready, then sleeps for 30 s: a broken or foreign b that the
// interrupt does not stop. The pid in dir/ready marks that the trap is
// set. exec keeps the pid, so a kill reaches the sleep itself.
func stuckB(t *testing.T, bin, dir string) {
	t.Helper()
	writeB(t, bin, "trap '' INT\necho $$ > \""+filepath.Join(dir, "ready")+"\"\nexec sleep 30\n")
}

// waitReady waits up to 5 s for the pid that stuckB writes after its
// trap, and returns it. A line without its newline is not complete yet.
func waitReady(t *testing.T, dir string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "ready"))
		if err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatalf("the b wrote %q, not a pid", raw)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the b did not set its trap in 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// versionUnknown reports whether the first check is the warning for a
// failed probe of b.
func versionUnknown(checks []Check) bool {
	return len(checks) > 0 && checks[0].Status == Warn && strings.Contains(checks[0].Msg, "b at .bin/b (version unknown:")
}

// doctorOptions returns options for a project whose .bin holds only b. An
// empty plugin home and PATH dir: b is the only tool probed.
func doctorOptions(t *testing.T) DoctorOptions {
	t.Helper()
	base := t.TempDir()
	return DoctorOptions{Base: base, Bin: filepath.Join(base, ".bin"), PluginHome: t.TempDir(), PATH: t.TempDir()}
}

// A b that ignores the interrupt does not hold the doctor: it gets the
// interrupt, is killed at the end of probeGrace, and the doctor reports
// its version as unknown. Without the grace (no WaitDelay), the doctor
// waits for the b to end: here 30 s. The test cancels the doctor's
// context only after the b has set its trap. An interrupt before the trap
// ends the b at once and proves nothing. The time limit is far away, so
// only the cancel sends the interrupt.
func TestDoctorBoundsAProbeThatIgnoresTheInterrupt(t *testing.T) {
	timeout, grace := probeTimeout, probeGrace
	probeTimeout, probeGrace = time.Minute, 500*time.Millisecond
	t.Cleanup(func() { probeTimeout, probeGrace = timeout, grace })

	o := doctorOptions(t)
	marks := t.TempDir()
	stuckB(t, o.Bin, marks)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan []Check, 1)
	go func() { done <- Doctor(ctx, o) }()

	pid := waitReady(t, marks)
	// The doctor's wait reaps the b. After that, the pid can belong to
	// another process. So the cleanup kills the b only while the doctor
	// has not returned.
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	start := time.Now()
	cancel()
	var checks []Check
	select {
	case checks = <-done:
		reaped = true
	case <-time.After(10 * time.Second):
		t.Fatal("the doctor did not return in 10 s after the cancel: the probe of b has no bound after its interrupt")
	}
	took := time.Since(start)

	if took < probeGrace || took > probeGrace+time.Second {
		t.Errorf("the doctor returned %s after the cancel, want the grace (%s) and at most 1 s more", took, probeGrace)
	}
	if !versionUnknown(checks) {
		t.Errorf("the b check = %+v, want a warning: version unknown", checks)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the b (pid %d) still runs after the doctor returned (kill -0: %v)", pid, err)
	}
}

// At probeTimeout the probe gets the interrupt. A b that obeys it ends at
// once, and the doctor does not wait for the grace. An interrupt that
// comes before the sleep also ends this b, so the test needs no ready
// mark. If the doctor does not return, the cancel of the test context
// interrupts the b at the end of the test.
func TestDoctorInterruptsAProbeAtItsTimeLimit(t *testing.T) {
	timeout, grace := probeTimeout, probeGrace
	probeTimeout, probeGrace = 300*time.Millisecond, time.Minute
	t.Cleanup(func() { probeTimeout, probeGrace = timeout, grace })

	o := doctorOptions(t)
	writeB(t, o.Bin, "exec sleep 30\n")

	done := make(chan []Check, 1)
	start := time.Now()
	go func() { done <- Doctor(t.Context(), o) }()
	var checks []Check
	select {
	case checks = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the doctor did not return in 5 s: the probe of b got no interrupt at its time limit")
	}

	if took := time.Since(start); took < probeTimeout {
		t.Errorf("the doctor returned after %s, before the time limit (%s)", took, probeTimeout)
	}
	if !versionUnknown(checks) {
		t.Errorf("the b check = %+v, want a warning: version unknown", checks)
	}
}

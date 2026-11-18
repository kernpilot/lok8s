package toolchain

import (
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

// stuckB writes a .bin/b that records its pid in dir/pid, ignores SIGINT
// and sleeps for 30 s: a broken or foreign b that the interrupt at the
// time limit does not stop. exec keeps the pid, so the kill reaches the
// sleep itself.
func stuckB(t *testing.T, bin, dir string) {
	t.Helper()
	b := filepath.Join(bin, "b")
	testutil.WriteFile(t, b, "#!/bin/sh\ntrap '' INT\necho $$ > \""+dir+"/pid\"\nexec sleep 30\n")
	if err := os.Chmod(b, 0o700); err != nil {
		t.Fatal(err)
	}
}

// stuckPid returns the pid that stuckB recorded, and kills it at the end
// of the test, so a failed run leaves no process behind.
func stuckPid(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatalf("the b probe did not start: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// A b that ignores the interrupt at the time limit does not hold the
// doctor: it gets the interrupt at probeTimeout, is killed at the end of
// probeGrace, and the doctor reports its version as unknown. Without the
// grace (no WaitDelay), the doctor waits for the b to end: here 30 s.
func TestDoctorBoundsAProbeThatIgnoresTheInterrupt(t *testing.T) {
	timeout, grace := probeTimeout, probeGrace
	probeTimeout, probeGrace = 300*time.Millisecond, 700*time.Millisecond
	t.Cleanup(func() { probeTimeout, probeGrace = timeout, grace })

	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	marks := t.TempDir()
	stuckB(t, bin, marks)

	// An empty plugin home and PATH dir: b is the only tool probed.
	o := DoctorOptions{Base: base, Bin: bin, PluginHome: t.TempDir(), PATH: t.TempDir()}
	done := make(chan []Check, 1)
	start := time.Now()
	go func() { done <- Doctor(t.Context(), o) }()
	var checks []Check
	select {
	case checks = <-done:
	case <-time.After(10 * time.Second):
		stuckPid(t, marks)
		t.Fatal("the doctor did not return in 10 s: the probe of b has no bound after its interrupt")
	}
	took := time.Since(start)

	if took < 900*time.Millisecond || took > 2200*time.Millisecond {
		t.Errorf("the doctor took %s, want about 1 s (the limit, then the whole grace)", took)
	}
	if len(checks) == 0 || checks[0].Status != Warn || !strings.Contains(checks[0].Msg, "b at .bin/b (version unknown:") {
		t.Errorf("the b check = %+v, want a warning: version unknown", checks)
	}
	pid := stuckPid(t, marks)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the b (pid %d) still runs after the doctor returned (kill -0: %v)", pid, err)
	}
}

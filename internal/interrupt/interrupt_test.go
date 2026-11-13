package interrupt

import (
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A SIGHUP while CatchHangup catches it cancels the context of CatchHangup
// and the command context of Watch, and maps to the exit code 129. stop
// reports it. The test keeps a SIGHUP handler of its own, so the signal
// never ends the test binary.
func TestCatchHangupCancelsTheCommandContext(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored")
	}
	keep := make(chan os.Signal, 1)
	signal.Notify(keep, syscall.SIGHUP)
	defer signal.Stop(keep)
	root, exitCode := Watch(t.Context())
	ctx, stop := CatchHangup(root)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-root.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not cancel the command context")
	}
	if ctx.Err() == nil {
		t.Error("SIGHUP did not cancel the context of CatchHangup")
	}
	if !stop() {
		t.Error("stop did not report the SIGHUP")
	}
	if got := exitCode(); got != 129 {
		t.Errorf("exit code = %d, want 129", got)
	}
	<-keep

	// After the catch, a SIGHUP is not caught.
	root, exitCode = Watch(t.Context())
	_, stop = CatchHangup(root)
	if stop() {
		t.Error("stop reported a SIGHUP that did not come")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-keep:
	case <-time.After(5 * time.Second):
		t.Fatal("the SIGHUP did not come")
	}
	if root.Err() != nil {
		t.Errorf("a SIGHUP after the catch cancelled the command context: %v", root.Err())
	}
	if got := exitCode(); got != 0 {
		t.Errorf("exit code after the catch = %d, want 0", got)
	}
}

// With SIGHUP ignored (nohup lo ...), CatchHangup leaves the ignore in
// place: it does not call signal.Notify, which would undo it, and the
// context is the caller's. The test runs again in a child that starts
// with SIGHUP ignored, as under nohup: an ignore made in this process
// could not be undone for the tests after it.
func TestCatchHangupKeepsAnIgnoredSIGHUP(t *testing.T) {
	if os.Getenv("LOK8S_TEST_SIGHUP_IGNORED") != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", `trap "" HUP; exec "$0" -test.run='^TestCatchHangupKeepsAnIgnoredSIGHUP$' -test.count=1 -test.v`, exe)
		cmd.Env = append(os.Environ(), "LOK8S_TEST_SIGHUP_IGNORED=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestCatchHangupKeepsAnIgnoredSIGHUP") {
			t.Fatalf("the child with SIGHUP ignored: %v\n%s", err, out)
		}
		return
	}
	if !signal.Ignored(syscall.SIGHUP) {
		t.Fatal("the child does not start with SIGHUP ignored")
	}
	parent, exitCode := Watch(t.Context())
	ctx, stop := CatchHangup(parent)
	if !signal.Ignored(syscall.SIGHUP) {
		t.Error("CatchHangup undid the ignore of SIGHUP")
	}
	if ctx != parent {
		t.Error("CatchHangup made a new context, want the caller's")
	}
	if stop() {
		t.Error("stop reported a SIGHUP")
	}
	if !signal.Ignored(syscall.SIGHUP) {
		t.Error("the ignore of SIGHUP is gone after the catch")
	}
	if got := exitCode(); got != 0 {
		t.Errorf("exit code = %d, want 0", got)
	}
}

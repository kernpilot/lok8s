package interrupt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// childEnv names the test that runs as a child of the test binary.
const childEnv = "LOK8S_TEST_SIGHUP_CHILD"

// runChild runs the test name again in a child of the test binary, with
// SIGHUP at its default action and no handler of the test: a SIGHUP that
// no catch holds ends the child, not the test binary. It returns the
// output and the wait status of the child.
func runChild(t *testing.T, name string) (string, syscall.WaitStatus) {
	t.Helper()
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^"+name+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"="+name)
	out, err := cmd.CombinedOutput()
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		t.Fatalf("the child did not run: %v\n%s", err, out)
	}
	status, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return string(out), status
}

// A SIGHUP while CatchHangup catches it cancels the context of CatchHangup
// and the command context of Watch, and maps to the exit code 129. stop
// reports it. Without a Watch in the context, the SIGHUP cancels the
// context of CatchHangup. After stop, a SIGHUP takes the default action again: it ends
// the child that the test runs in, as no handler of the test holds it.
func TestCatchHangupCancelsTheCommandContext(t *testing.T) {
	const name = "TestCatchHangupCancelsTheCommandContext"
	if os.Getenv(childEnv) != name {
		out, status := runChild(t, name)
		if !strings.Contains(out, "the catch held the first SIGHUP") {
			t.Fatalf("the child failed before the second SIGHUP (%v):\n%s", status, out)
		}
		if !status.Signaled() || status.Signal() != syscall.SIGHUP {
			t.Fatalf("the SIGHUP after stop did not end the child (%v):\n%s", status, out)
		}
		return
	}
	root, exitCode := Watch(t.Context())
	ctx, stop := CatchHangup(root)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// The catch cancels the command context first and its own context
	// after it, so the test waits for each of the two.
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{{"the command context", root}, {"the context of CatchHangup", ctx}} {
		select {
		case <-c.ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("SIGHUP did not cancel %s", c.name)
		}
	}
	if !stop() {
		t.Fatal("stop did not report the SIGHUP")
	}
	if got := exitCode(); got != 129 {
		t.Fatalf("exit code = %d, want 129", got)
	}

	// Without a Watch, the catch cancels its own context.
	ctx, stop = CatchHangup(t.Context())
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("without a Watch, SIGHUP did not cancel the context of CatchHangup")
	}
	if !stop() {
		t.Fatal("without a Watch, stop did not report the SIGHUP")
	}

	// A catch without a SIGHUP reports none, and leaves the default action.
	root, exitCode = Watch(t.Context())
	_, stop = CatchHangup(root)
	if stop() {
		t.Fatal("stop reported a SIGHUP that did not come")
	}
	if got := exitCode(); got != 0 {
		t.Fatalf("exit code after the catch = %d, want 0", got)
	}
	fmt.Println("the catch held the first SIGHUP")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	t.Fatal("a SIGHUP after stop did not end the process")
}

// A SIGHUP that came before stop counts, also when it is still in the
// channel as stop runs. The hook holds the goroutine of the catch until
// stop runs: the SIGHUP is then in the channel, and the goroutine sees
// both the SIGHUP and stop. Its select picks one of the two at random, so
// the test runs 64 rounds: stop must report the SIGHUP and cancel the
// contexts in each round. The kernel can give the SIGHUP to another
// thread, after the kill returns: so the test waits for the SIGHUP on a
// channel of its own before it calls stop. signal sends a SIGHUP to each
// channel in one step, under the lock that signal.Stop takes, so the
// SIGHUP is then in the channel of the catch too. stop must also wait
// for the goroutine: the hook marks the end of its wait a little after
// stop runs, and the mark must be set when stop returns. The test runs in
// a child: a SIGHUP that no catch holds ends the child, not the test
// binary.
func TestCatchHangupActsOnASIGHUPBeforeStop(t *testing.T) {
	const name = "TestCatchHangupActsOnASIGHUPBeforeStop"
	if os.Getenv(childEnv) != name {
		out, status := runChild(t, name)
		if status.Signaled() || status.ExitStatus() != 0 || !strings.Contains(out, "--- PASS: "+name) {
			t.Fatalf("the child failed (%v):\n%s", status, out)
		}
		return
	}
	var held atomic.Bool
	beforeCatchWait = func(stopped <-chan struct{}) {
		<-stopped
		time.Sleep(time.Millisecond)
		held.Store(true)
	}
	defer func() { beforeCatchWait = func(<-chan struct{}) {} }()
	seen := make(chan os.Signal, 1)
	signal.Notify(seen, syscall.SIGHUP)
	defer signal.Stop(seen)
	for round := range 64 {
		held.Store(false)
		root, exitCode := Watch(t.Context())
		_, stop := CatchHangup(root)
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the SIGHUP did not come", round)
		}
		if !stop() {
			t.Fatalf("round %d: stop did not report the SIGHUP that came before it", round)
		}
		if !held.Load() {
			t.Fatalf("round %d: stop returned before the goroutine of the catch ended", round)
		}
		// stop cancels the context of the catch itself, so only the
		// command context shows that the SIGHUP was acted on.
		if root.Err() == nil {
			t.Fatalf("round %d: the SIGHUP before stop did not cancel the command context", round)
		}
		if got := exitCode(); got != 129 {
			t.Fatalf("round %d: exit code = %d, want 129", round, got)
		}
	}
	if !hungUp.Load() {
		t.Error("Exit would not end lo with the SIGHUP")
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

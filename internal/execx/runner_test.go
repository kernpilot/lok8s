package execx

// runner_test.go covers the default Runner: env merge, Dir, a name with a
// path separator, an unresolvable tool, the interrupt on cancel, the wait
// after it (WaitDelay), and the state of the child (Exited).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
)

// Run: Env entries are APPENDED to the inherited environment (the parent's
// variables survive), Dir is the child's working directory, and a Name
// carrying a path separator is used as-is instead of resolved.
func TestRunMergesEnvAndSetsDirAndUsesPathsAsIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	t.Setenv("LO_EXECX_INHERITED", "kept")
	var out bytes.Buffer
	err := NewRunner(&config.Paths{Bin: t.TempDir()}).Run(t.Context(), Cmd{
		Name:   "/bin/sh",
		Args:   []string{"-c", `pwd; printf '%s %s\n' "$LO_EXECX_INHERITED" "$LO_EXECX_ADDED"`},
		Dir:    dir,
		Env:    []string{"LO_EXECX_ADDED=added"},
		Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("output: %q", out.String())
	}
	if got, _ := filepath.EvalSymlinks(lines[0]); got != mustEval(t, dir) {
		t.Errorf("pwd = %q, want %q", lines[0], dir)
	}
	if lines[1] != "kept added" {
		t.Errorf("env line = %q, want %q", lines[1], "kept added")
	}
}

// A bare tool name is resolved through Look; a miss is an error naming the
// tool, before anything runs.
func TestRunReportsAnUnresolvableTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := NewRunner(&config.Paths{Bin: t.TempDir()}).Run(t.Context(), Cmd{Name: "lo-execx-absent"})
	if err == nil || !strings.Contains(err.Error(), "lo-execx-absent: executable not found") {
		t.Fatalf("err = %v", err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A cancelled context stops the child with SIGINT so it can clean up and
// report its own exit code; the default SIGKILL would end it at once.
// lockedBuffer is a bytes.Buffer the test may read while the child's
// output copier still writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunCancelSendsInterruptToTheChild(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r := NewRunner(nil)
	var out lockedBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx, Cmd{
			Name:   "/bin/sh",
			Args:   []string{"-c", `trap 'echo caught; exit 3' INT; echo up; while :; do sleep 0.05; done`},
			Stdout: &out,
			Stderr: &out,
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "up") {
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if ExitCode(err) != 3 || !strings.Contains(out.String(), "caught") {
			t.Errorf("child was not interrupted gracefully: rc=%d err=%v out=%q", ExitCode(err), err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not stop after the cancel")
	}
}

// startAndCancel runs script with the given WaitDelay, cancels the context
// once the child printed "up", and returns the output, the time from the
// cancel to the return, and the result.
func startAndCancel(t *testing.T, script string, delay time.Duration) (string, time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out lockedBuffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- NewRunner(nil).Run(ctx, Cmd{
			Name:      "/bin/sh",
			Args:      []string{"-c", script},
			Stdout:    &out,
			Stderr:    &out,
			WaitDelay: delay,
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "up") {
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	start := time.Now()
	select {
	case err := <-errCh:
		return out.String(), time.Since(start), err
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return in 20 s after the cancel")
	}
	return "", 0, nil
}

// A Cmd without a WaitDelay keeps the default: after the interrupt the
// runner waits for the child's own cleanup, however long, and reports the
// child's exit code. Every caller except the dry run's plugin check sets
// none.
func TestRunWithoutWaitDelayWaitsForTheChildsCleanup(t *testing.T) {
	out, took, err := startAndCancel(t,
		`trap 'echo caught; i=0; while [ "$i" -lt 30 ]; do sleep 0.05; i=$((i + 1)); done; echo done; exit 3' INT; echo up; while :; do sleep 0.05; done`, 0)
	if ExitCode(err) != 3 || !strings.Contains(out, "done") {
		t.Errorf("the child's cleanup did not finish: rc=%d err=%v out=%q", ExitCode(err), err, out)
	}
	if took < time.Second {
		t.Errorf("Run returned %s after the cancel, before the child's cleanup (about 1.5 s) ended", took)
	}
}

// A Cmd with a WaitDelay bounds the wait: a child that ignores the
// interrupt is killed after the delay.
func TestRunWithWaitDelayKillsAChildThatIgnoresTheInterrupt(t *testing.T) {
	out, took, err := startAndCancel(t,
		`trap '' INT; echo up; i=0; while [ "$i" -lt 100 ]; do sleep 0.05; i=$((i + 1)); done; echo done`, 300*time.Millisecond)
	if err == nil || strings.Contains(out, "done") {
		t.Errorf("the child was not stopped: err=%v out=%q", err, out)
	}
	if took > 3*time.Second {
		t.Errorf("Run returned %s after the cancel, want about the delay (300ms)", took)
	}
}

// After the cancel, Run returns the error of the context also when the
// child ended with 0 on the interrupt: Exited gets the state, which says
// so. A child that did not start (the context ended before) gives no
// state: Run returns the same error.
func TestRunExitedGetsTheStateOfTheChild(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var out lockedBuffer
	var state *os.ProcessState
	errCh := make(chan error, 1)
	go func() {
		errCh <- NewRunner(nil).Run(ctx, Cmd{
			Name:   "/bin/sh",
			Args:   []string{"-c", `trap 'exit 0' INT; echo up; while :; do sleep 0.05; done`},
			Stdout: &out,
			Stderr: &out,
			Exited: func(s *os.ProcessState) { state = s },
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "up") {
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	var err error
	select {
	case err = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not stop after the cancel")
	}
	if err == nil || state == nil || !state.Success() {
		t.Errorf("err = %v, state = %v; want the error of the context and a state with 0", err, state)
	}

	called := false
	err = NewRunner(nil).Run(ctx, Cmd{
		Name:   "/bin/sh",
		Args:   []string{"-c", "exit 0"},
		Exited: func(*os.ProcessState) { called = true },
	})
	if err == nil || called {
		t.Errorf("a done context: err = %v, Exited called = %v; want an error and no call", err, called)
	}
}

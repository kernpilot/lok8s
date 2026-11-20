//go:build linux

package execx

// group_linux_test.go covers the order of the group stop on Linux: the
// child ends (or the context ends), the group is stopped, and then the
// child is reaped. It also covers the stop after the child ends on its own,
// and the Cmd without ProcessGroup, which gets no group signal.

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readPid reads the pid that a test process wrote to path.
func readPid(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a process did not start: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitGone waits up to limit for pid to be gone (processGone).
func waitGone(pid int, limit time.Duration) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if processGone(pid) {
			return true
		}
	}
	return processGone(pid)
}

// The child ends on its own and leaves a member of its group that keeps
// the output pipe of the child. Run stops that member before it returns:
// a member that ends on SIGTERM goes at once, and Run returns long before
// WaitDelay (5 s), without ErrWaitDelay; a member that ignores SIGTERM gets
// SIGKILL after WaitDelay (300 ms), or after groupGrace (2 s) when the Cmd
// has no WaitDelay. Without the stop, Run would wait WaitDelay for the
// pipe, return ErrWaitDelay, and leave the member.
func TestRunProcessGroupStopsTheGroupAfterTheChildEnds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trap     string
		delay    time.Duration
		min, max time.Duration
	}{
		{"a member that ends on TERM", "", 5 * time.Second, 0, 4 * time.Second},
		{"a member that ignores TERM", `trap "" TERM; `, 300 * time.Millisecond, 300 * time.Millisecond, 3 * time.Second},
		{"a member that ignores TERM, no WaitDelay", `trap "" TERM; `, 0, groupGrace, groupGrace + 3*time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "member.pid")
			var out lockedBuffer
			start := time.Now()
			err := NewRunner(nil).Run(t.Context(), Cmd{
				Name: "/bin/sh",
				Args: []string{"-c", `sh -c '` + tc.trap + `echo $$ > "$0"; exec sleep 10' "$0" &
while [ ! -s "$0" ]; do sleep 0.01; done
echo up`, pidFile},
				Stdout:       &out,
				Stderr:       &out,
				WaitDelay:    tc.delay,
				ProcessGroup: true,
			})
			took := time.Since(start)
			if err != nil {
				t.Errorf("err = %v, want nil (the child ended with 0); output %q", err, out.String())
			}
			if took < tc.min || took > tc.max {
				t.Errorf("Run took %s, want between %s and %s", took, tc.min, tc.max)
			}
			if pid := readPid(t, pidFile); !processGone(pid) {
				t.Errorf("the member (pid %d) still runs after Run returned", pid)
			}
		})
	}
}

// The group gets its signal while the child has ended and is not reaped:
// the member reads /proc/<child>/stat in its SIGTERM trap, and the state
// there is Z (a zombie), in the group of the child. When Run reaps the
// child before the signal, /proc has no entry for it, and the member
// records nothing. The member does not keep the pipes, so Run does not
// wait for them.
func TestRunProcessGroupSignalsTheGroupBeforeTheReap(t *testing.T) {
	dir := t.TempDir()
	member := filepath.Join(dir, "member.sh")
	stateFile, readyFile := filepath.Join(dir, "state"), filepath.Join(dir, "ready")
	if err := os.WriteFile(member, []byte(`trap 'cat "/proc/$1/stat" > "$2.tmp" 2> /dev/null; mv "$2.tmp" "$2"; exit 0' TERM
: > "$3"
i=0
while [ "$i" -lt 200 ]; do sleep 0.05; i=$((i + 1)); done
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out lockedBuffer
	var state *os.ProcessState
	err := NewRunner(nil).Run(t.Context(), Cmd{
		Name: "/bin/sh",
		Args: []string{"-c", `sh "$0" $$ "$1" "$2" > /dev/null 2>&1 &
while [ ! -e "$2" ]; do sleep 0.01; done`, member, stateFile, readyFile},
		Stdout:       &out,
		Stderr:       &out,
		WaitDelay:    5 * time.Second,
		ProcessGroup: true,
		Exited:       func(s *os.ProcessState) { state = s },
	})
	if err != nil || state == nil || !state.Success() {
		t.Fatalf("err = %v, state = %v; want nil and a state with 0; output %q", err, state, out.String())
	}
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("the member got no SIGTERM: %v", err)
	}
	f := statFields(raw)
	if len(f) < 3 || f[0] != "Z" || f[2] != strconv.Itoa(state.Pid()) {
		t.Errorf("the child at the group signal: %q, want state Z in group %d (not reaped)", raw, state.Pid())
	}
}

// One scan of /proc can miss a member: a member can start a child after
// the scan read the list of /proc, and end before the scan reads its
// entry. The first list here is such a list: it holds the leader only, and
// not the member that runs. othersInGroup reports false only after two
// scans in a row found no member, so the second scan, of the real list,
// finds the member. The test makes the group itself (Setpgid), and the
// member ends when the test creates its stop file.
func TestOthersInGroupNeedsTwoScansWithoutAMember(t *testing.T) {
	stopFile := filepath.Join(t.TempDir(), "stop")
	cmd := exec.Command("/bin/sh", "-c", `sh -c 'while [ ! -e "$0" ]; do sleep 0.02; done' "$0" &
echo $!
wait`, stopFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(stopFile, nil, 0o600)
		_ = cmd.Wait()
	})
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("the member did not start: %v", err)
	}
	g := newGroup(cmd.Process.Pid)
	real := procNames
	t.Cleanup(func() { procNames = real })
	calls := 0
	procNames = func() ([]string, error) {
		calls++
		if calls == 1 {
			return []string{strconv.Itoa(g.pgid)}, nil
		}
		return real()
	}
	if !othersInGroup(g) {
		t.Error("othersInGroup = false: a first list without the member hid it")
	}
	if calls != 2 {
		t.Errorf("othersInGroup read the list of /proc %d times, want 2", calls)
	}
}

// A cancel of a ProcessGroup child that ends with 0 on the SIGTERM: Run
// returns the error of the context, and Exited gets the state with 0. A
// context that ended before the start gives the same error, no child and
// no call of Exited.
func TestRunProcessGroupExitedGetsTheStateOfTheChild(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var out lockedBuffer
	var state *os.ProcessState
	errCh := make(chan error, 1)
	go func() {
		errCh <- NewRunner(nil).Run(ctx, Cmd{
			Name:         "/bin/sh",
			Args:         []string{"-c", `trap 'exit 0' TERM; echo up; while :; do sleep 0.05; done`},
			Stdout:       &out,
			Stderr:       &out,
			WaitDelay:    5 * time.Second,
			ProcessGroup: true,
			Exited:       func(s *os.ProcessState) { state = s },
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
	if !errors.Is(err, context.Canceled) || state == nil || !state.Success() {
		t.Errorf("err = %v, state = %v; want the error of the context and a state with 0", err, state)
	}

	called := false
	err = NewRunner(nil).Run(ctx, Cmd{
		Name:         "/bin/sh",
		Args:         []string{"-c", "exit 0"},
		ProcessGroup: true,
		Exited:       func(*os.ProcessState) { called = true },
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Errorf("a done context: err = %v, Exited called = %v; want the error of the context and no call", err, called)
	}
}

// A Cmd without ProcessGroup keeps the default: no group signal, after a
// cancel (the child alone gets SIGINT) and after the child ends on its own.
// The child of the child traps SIGTERM, and it still runs, with no SIGTERM,
// when Run returns. It ends when the test creates its stop file (or after
// about 30 s).
func TestRunWithoutProcessGroupSendsNoGroupSignal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		cancel bool
	}{
		{"the child ends on its own", `while [ ! -s "$1" ]; do sleep 0.01; done; echo up`, false},
		{"a cancel", `trap 'exit 0' INT; while [ ! -s "$1" ]; do sleep 0.01; done; echo up; while :; do sleep 0.05; done`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			member := filepath.Join(dir, "member.sh")
			pidFile, termFile, stopFile := filepath.Join(dir, "member.pid"), filepath.Join(dir, "term"), filepath.Join(dir, "stop")
			if err := os.WriteFile(member, []byte(`trap ': > "$2"; exit 0' TERM
echo $$ > "$1.tmp"; mv "$1.tmp" "$1"
i=0
while [ ! -e "$3" ] && [ "$i" -lt 600 ]; do sleep 0.05; i=$((i + 1)); done
`), 0o600); err != nil {
				t.Fatal(err)
			}
			stop := func() { _ = os.WriteFile(stopFile, nil, 0o600) }
			t.Cleanup(stop)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out lockedBuffer
			errCh := make(chan error, 1)
			go func() {
				errCh <- NewRunner(nil).Run(ctx, Cmd{
					Name:      "/bin/sh",
					Args:      []string{"-c", `sh "$0" "$1" "$2" "$3" > /dev/null 2>&1 & ` + tc.script, member, pidFile, termFile, stopFile},
					Stdout:    &out,
					Stderr:    &out,
					WaitDelay: 5 * time.Second,
				})
			}()
			if tc.cancel {
				deadline := time.Now().Add(5 * time.Second)
				for !strings.Contains(out.String(), "up") {
					if time.Now().After(deadline) {
						t.Fatal("child did not start")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			select {
			case err := <-errCh:
				if err != nil && !tc.cancel {
					t.Errorf("err = %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return in 5 s")
			}
			pid := readPid(t, pidFile)
			if processGone(pid) {
				t.Errorf("the child of the child (pid %d) is gone when Run returned", pid)
			}
			if _, err := os.Stat(termFile); err == nil {
				t.Error("the child of the child got a SIGTERM")
			}
			stop()
			if !waitGone(pid, 20*time.Second) {
				t.Errorf("the child of the child (pid %d) did not end after its stop file", pid)
			}
		})
	}
}

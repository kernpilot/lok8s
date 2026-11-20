//go:build linux

package execx

// group_linux_test.go covers the order of the group stop on Linux: the
// child ends (or the context ends), the group is stopped, and then the
// child is reaped. Before each signal to the group the leader must be
// there, not reaped, with the start time that Run recorded; else the group
// gets no more signals. It also covers the stop after the child ends on
// its own, the errors of waitid, a panic after the start, and the Cmd
// without ProcessGroup, which gets no group signal.

import (
	"bufio"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// procEntry is /proc/<pid>/stat of a process as a test reads it, with its
// own parser (statFields), not the one of the code under test.
type procEntry struct {
	ok    bool // the file was there
	state string
	ppid  int
	pgrp  int
	start uint64
}

func readEntry(pid int) procEntry {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procEntry{}
	}
	f := statFields(raw)
	if len(f) < 20 {
		return procEntry{}
	}
	ppid, _ := strconv.Atoi(f[1])
	pgrp, _ := strconv.Atoi(f[2])
	start, _ := strconv.ParseUint(f[19], 10, 64)
	return procEntry{ok: true, state: f[0], ppid: ppid, pgrp: pgrp, start: start}
}

// signalEvent is one call of groupSignalHook: the signal, its time, the
// start time that Run recorded, and the leader in /proc right then.
type signalEvent struct {
	sig    syscall.Signal
	at     time.Time
	pgid   int
	start  uint64
	leader procEntry
}

// recordSignals sets groupSignalHook for the test. The function it returns
// gives the events so far.
func recordSignals(t *testing.T) func() []signalEvent {
	t.Helper()
	var mu sync.Mutex
	var events []signalEvent
	groupSignalHook = func(pgid int, start uint64, sig syscall.Signal) {
		e := signalEvent{sig: sig, at: time.Now(), pgid: pgid, start: start, leader: readEntry(pgid)}
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	t.Cleanup(func() { groupSignalHook = nil })
	return func() []signalEvent {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
}

// replaceWaitid sets the waitid seam for the test.
func replaceWaitid(t *testing.T, f func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error) {
	t.Helper()
	real := waitid
	waitid = f
	t.Cleanup(func() { waitid = real })
}

// replacePidfdOpen sets the pidfdOpen seam for the test.
func replacePidfdOpen(t *testing.T, f func(pid, flags int) (int, error)) {
	t.Helper()
	real := pidfdOpen
	pidfdOpen = f
	t.Cleanup(func() { pidfdOpen = real })
}

// noPidfd is a pidfdOpen of a kernel without pidfds.
func noPidfd(int, int) (int, error) { return -1, unix.ENOSYS }

// waitFile waits up to 5 s for path to exist.
func waitFile(t *testing.T, path string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s did not appear", path)
}

// killOwnChild sends SIGKILL to pid when /proc shows it as a child of the
// test that is not reaped (a test that failed must not leave it running).
func killOwnChild(pid int) {
	if e := readEntry(pid); pid > 0 && e.ok && e.ppid == os.Getpid() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// sigs returns the signals of events, in order.
func sigs(events []signalEvent) []syscall.Signal {
	out := make([]syscall.Signal, len(events))
	for i, e := range events {
		out[i] = e.sig
	}
	return out
}

// checkHeld fails the test unless the leader was not reaped at the signal
// e: its /proc entry is there, with the start time that Run recorded, in
// the group pgid. With zombie, the state must also be Z (the leader has
// ended and waits for its reap).
func checkHeld(t *testing.T, e signalEvent, zombie bool) {
	t.Helper()
	l := e.leader
	if !l.ok || l.start != e.start || l.pgrp != e.pgid || (zombie && l.state != "Z") {
		t.Errorf("at %v the leader %d was %+v, want the start time %d and the group %d (zombie: %v)", e.sig, e.pgid, l, e.start, e.pgid, zombie)
	}
}

var termContKill = []syscall.Signal{syscall.SIGTERM, syscall.SIGCONT, syscall.SIGKILL}

// The child ends on its own and leaves a member of its group that keeps
// the output pipe of the child. Run stops that member before it returns:
// a member that ends on SIGTERM goes at once, and Run returns long before
// WaitDelay (5 s), without ErrWaitDelay; a member that ignores SIGTERM gets
// SIGKILL after WaitDelay (300 ms), or after groupGrace (2 s) when the Cmd
// has no WaitDelay. The times are measured from the SIGTERM (the seam
// groupSignalHook), not from the start of Run, so a slow start cannot hide
// an early SIGKILL. Without the stop, Run would wait WaitDelay for the
// pipe, return ErrWaitDelay, and leave the member.
func TestRunProcessGroupStopsTheGroupAfterTheChildEnds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trap  string
		delay time.Duration
		grace time.Duration // from the SIGTERM to the SIGKILL; 0: no SIGKILL
	}{
		{"a member that ends on TERM", "", 5 * time.Second, 0},
		{"a member that ignores TERM", `trap "" TERM; `, 300 * time.Millisecond, 300 * time.Millisecond},
		{"a member that ignores TERM, no WaitDelay", `trap "" TERM; `, 0, groupGrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := recordSignals(t)
			pidFile := filepath.Join(t.TempDir(), "member.pid")
			var out lockedBuffer
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
			returned := time.Now()
			if err != nil {
				t.Errorf("err = %v, want nil (the child ended with 0); output %q", err, out.String())
			}
			ev := events()
			want := termContKill[:2]
			if tc.grace > 0 {
				want = termContKill
			}
			if !slices.Equal(sigs(ev), want) {
				t.Fatalf("the group got %v, want %v", sigs(ev), want)
			}
			term := ev[0].at
			if tc.grace == 0 {
				if took := returned.Sub(term); took > 4*time.Second {
					t.Errorf("Run returned %s after the SIGTERM, want long before WaitDelay (5 s)", took)
				}
			} else if took := ev[2].at.Sub(term); took < tc.grace || took > tc.grace+2*time.Second {
				t.Errorf("the SIGKILL came %s after the SIGTERM, want between %s and %s", took, tc.grace, tc.grace+2*time.Second)
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

// At each signal to the group (SIGTERM, SIGCONT and SIGKILL) the leader is
// not reaped: the seam groupSignalHook reads /proc/<leader>/stat right
// before the kill(2), and the entry has the start time that Run recorded
// after the start, in the group of the leader. The member ignores SIGTERM,
// so the group also gets the SIGKILL. When the child ends first, the
// leader is a zombie at each signal. On a cancel, the leader still runs at
// the SIGTERM (it ends on it), and it is a zombie at the SIGKILL, which
// the member gets after WaitDelay. A stop that sends the SIGKILL after the
// reap fails here: /proc has no entry for the leader then.
func TestRunProcessGroupHoldsTheLeaderAtEachSignal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		delay  time.Duration
		cancel bool
	}{
		{"the child ends first", `while [ ! -s "$0" ]; do sleep 0.01; done; echo up`, 300 * time.Millisecond, false},
		{"a cancel", `trap 'exit 0' TERM; while [ ! -s "$0" ]; do sleep 0.01; done; echo up; while :; do sleep 0.05; done`, time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := recordSignals(t)
			pidFile := filepath.Join(t.TempDir(), "member.pid")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out lockedBuffer
			errCh := make(chan error, 1)
			go func() {
				errCh <- NewRunner(nil).Run(ctx, Cmd{
					Name:         "/bin/sh",
					Args:         []string{"-c", `sh -c 'trap "" TERM; echo $$ > "$0"; exec sleep 10' "$0" > /dev/null 2>&1 & ` + tc.script, pidFile},
					Stdout:       &out,
					Stderr:       &out,
					WaitDelay:    tc.delay,
					ProcessGroup: true,
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
				if err != nil && (!tc.cancel || !errors.Is(err, context.Canceled)) {
					t.Errorf("err = %v; output %q", err, out.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return in 10 s")
			}
			ev := events()
			if !slices.Equal(sigs(ev), termContKill) {
				t.Fatalf("the group got %v, want %v", sigs(ev), termContKill)
			}
			if tc.cancel {
				if ev[0].leader.state == "Z" {
					t.Errorf("at the SIGTERM of the cancel the leader was a zombie: %+v", ev[0].leader)
				}
				checkHeld(t, ev[0], false)
				checkHeld(t, ev[1], false)
				checkHeld(t, ev[2], true)
			} else {
				for _, e := range ev {
					checkHeld(t, e, true)
				}
			}
			if pid := readPid(t, pidFile); !processGone(pid) {
				t.Errorf("the member (pid %d) still runs after Run returned", pid)
			}
		})
	}
}

// writeMember writes a member script to dir: it writes its pid to $1,
// writes $2 on SIGTERM and goes on, and ends when $3 exists (or after
// about 30 s). The test ends it with its stop file, not with a signal. As
// it goes on after a SIGTERM, it keeps the id of its group until the
// stop file, also when a broken stop signals the group.
func writeMember(t *testing.T, dir string) (script, pidFile, termFile, stopFile string) {
	t.Helper()
	script = filepath.Join(dir, "member.sh")
	pidFile, termFile, stopFile = filepath.Join(dir, "member.pid"), filepath.Join(dir, "term"), filepath.Join(dir, "stop")
	if err := os.WriteFile(script, []byte(`trap ': > "$2"' TERM
echo $$ > "$1.tmp"; mv "$1.tmp" "$1"
i=0
while [ ! -e "$3" ] && [ "$i" -lt 600 ]; do sleep 0.05; i=$((i + 1)); done
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(stopFile, nil, 0o600) })
	return script, pidFile, termFile, stopFile
}

// The errors of waitid, through the seam waitid. EINTR: waitid runs again,
// and the stop is the normal one. ECHILD after another waiter reaped the
// child, and a reap by another waiter after a good waitid: the leader has
// gone, so the group gets no signal at all (its id can be another
// group's), Run returns at once with errGroupNotStopped, and the member,
// which still runs, got no SIGTERM. ENOSYS while the leader runs: the end
// of the child is not known, and the group gets the checked stop: SIGTERM,
// SIGCONT, and SIGKILL after the grace, each with the leader not reaped.
func TestRunProcessGroupWaitidErrors(t *testing.T) {
	// reapFirst is another waiter: it reaps the child before this waitid.
	reapFirst := func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error {
		if err := unix.Waitid(idType, id, info, options&^unix.WNOWAIT, rusage); err != nil {
			return err
		}
		return unix.ECHILD
	}
	// reapAfter is another waiter that reaps the child after a good
	// waitid with WNOWAIT.
	reapAfter := func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error {
		if err := unix.Waitid(idType, id, info, options, rusage); err != nil {
			return err
		}
		var other unix.Siginfo
		return unix.Waitid(idType, id, &other, unix.WEXITED, nil)
	}

	t.Run("EINTR", func(t *testing.T) {
		var calls atomic.Int32
		replaceWaitid(t, func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error {
			if calls.Add(1) == 1 {
				return unix.EINTR
			}
			return unix.Waitid(idType, id, info, options, rusage)
		})
		events := recordSignals(t)
		pidFile := filepath.Join(t.TempDir(), "member.pid")
		err := NewRunner(nil).Run(t.Context(), Cmd{
			Name:         "/bin/sh",
			Args:         []string{"-c", `sh -c 'echo $$ > "$0"; exec sleep 10' "$0" > /dev/null 2>&1 & while [ ! -s "$0" ]; do sleep 0.01; done`, pidFile},
			WaitDelay:    5 * time.Second,
			ProcessGroup: true,
		})
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("waitid ran %d times, want 2 (EINTR, then again)", n)
		}
		ev := events()
		if !slices.Equal(sigs(ev), termContKill[:2]) {
			t.Fatalf("the group got %v, want %v", sigs(ev), termContKill[:2])
		}
		for _, e := range ev {
			checkHeld(t, e, true)
		}
		if pid := readPid(t, pidFile); !processGone(pid) {
			t.Errorf("the member (pid %d) still runs after Run returned", pid)
		}
	})

	for _, tc := range []struct {
		name   string
		waitid func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error
	}{
		{"ECHILD, the leader gone", reapFirst},
		{"a reap by another waiter after a good waitid", reapAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replaceWaitid(t, tc.waitid)
			events := recordSignals(t)
			member, pidFile, termFile, stopFile := writeMember(t, t.TempDir())
			start := time.Now()
			err := NewRunner(nil).Run(t.Context(), Cmd{
				Name:         "/bin/sh",
				Args:         []string{"-c", `sh "$0" "$1" "$2" "$3" > /dev/null 2>&1 & while [ ! -s "$1" ]; do sleep 0.01; done`, member, pidFile, termFile, stopFile},
				WaitDelay:    5 * time.Second,
				ProcessGroup: true,
			})
			took := time.Since(start)
			if !errors.Is(err, errGroupNotStopped) || !strings.Contains(err.Error(), "was not stopped") {
				t.Errorf("err = %v, want errGroupNotStopped", err)
			}
			if !errors.Is(err, syscall.ECHILD) {
				t.Errorf("err = %v, want the ECHILD of the reap", err)
			}
			if ev := events(); len(ev) != 0 {
				t.Errorf("the group got %v after the leader had gone, want no signal", sigs(ev))
			}
			if took > 3*time.Second {
				t.Errorf("Run took %s, want a return at once (no grace, no wait for the members)", took)
			}
			pid := readPid(t, pidFile)
			if processGone(pid) {
				t.Errorf("the member (pid %d) is gone, want it untouched", pid)
			}
			if _, err := os.Stat(termFile); err == nil {
				t.Error("the member got a SIGTERM")
			}
			_ = os.WriteFile(stopFile, nil, 0o600)
			if !waitGone(pid, 20*time.Second) {
				t.Errorf("the member (pid %d) did not end after its stop file", pid)
			}
		})
	}

	t.Run("ENOSYS, the leader there", func(t *testing.T) {
		replaceWaitid(t, func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return unix.ENOSYS })
		events := recordSignals(t)
		delay := 200 * time.Millisecond
		err := NewRunner(nil).Run(t.Context(), Cmd{
			Name:         "/bin/sh",
			Args:         []string{"-c", "exec sleep 10"},
			WaitDelay:    delay,
			ProcessGroup: true,
		})
		if !errors.Is(err, unix.ENOSYS) || errors.Is(err, errGroupNotStopped) {
			t.Errorf("err = %v, want the waitid error ENOSYS alone", err)
		}
		ev := events()
		if !slices.Equal(sigs(ev), termContKill) {
			t.Fatalf("the group got %v, want %v", sigs(ev), termContKill)
		}
		for _, e := range ev {
			checkHeld(t, e, false)
		}
		if took := ev[2].at.Sub(ev[0].at); took < delay {
			t.Errorf("the SIGKILL came %s after the SIGTERM, want at least %s", took, delay)
		}
	})
}

// owned: true for the leader of a group that is not reaped; false for a
// start time that is not the recorded one, for a child that does not lead
// a group, and for a leader that is reaped. A false stays, also when the
// identity matches again.
func TestGroupOwnedChecksTheLeader(t *testing.T) {
	start := func(setpgid bool) (*exec.Cmd, func()) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", "read x")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: setpgid}
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		end := func() {
			_ = in.Close()
			_ = cmd.Wait()
		}
		t.Cleanup(end)
		return cmd, end
	}
	cmd, end := start(true)
	g := newGroup(cmd.Process.Pid)
	if g.lost || !g.owned() {
		t.Fatalf("a leader that runs: owned = false (%+v, /proc %+v)", g, readEntry(g.pgid))
	}
	other := &group{pgid: g.pgid, start: g.start + 1}
	if other.owned() || !other.lost {
		t.Error("another start time: owned = true")
	}
	// A false stays: with the identity that matches again, the lost group
	// still reports false, and sends no signal (0: no effect, also if sent).
	other.start = g.start
	events := recordSignals(t)
	if other.owned() {
		t.Error("a lost group with a matching identity again: owned = true")
	}
	if sent, _ := other.signal(0); sent {
		t.Error("a lost group sent a signal")
	}
	if ev := events(); len(ev) != 0 {
		t.Errorf("a lost group got %v, want no signal", sigs(ev))
	}
	if !g.owned() {
		t.Error("owned of the real group changed")
	}
	noGroup, _ := start(false)
	if ng := newGroup(noGroup.Process.Pid); !ng.lost || ng.owned() {
		t.Error("a child that does not lead a group: owned = true")
	}
	end()
	if g.owned() {
		t.Error("a reaped leader: owned = true")
	}
	if !g.lost {
		t.Error("a reaped leader: lost = false")
	}
}

// A group that is lost from the start: the Cmd here has no Setpgid, so
// /proc does not show the child as the leader of a group, and newGroup
// records a lost group. The group gets no signal, and Run says so
// (errGroupNotStopped). With a pidfd, the child alone gets the signals of
// the stop through it: on a cancel it ends on its SIGTERM (its trap writes
// $0), and after a waitid error (ENOSYS) it gets SIGTERM and SIGKILL.
// Without a pidfd (the seam pidfdOpen fails), the child gets no signal at
// all: it ends on its own after about 1 s, with 0. The child is in the
// process group of the test, so a group signal would go to -pid of a
// group that does not exist.
func TestRunGroupLostAtTheStartStopsTheChildAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enosys bool
		pidfd  bool
		want   error
	}{
		{"a cancel", false, true, context.Canceled},
		{"ENOSYS", true, true, unix.ENOSYS},
		{"a cancel, no pidfd", false, false, context.Canceled},
		{"ENOSYS, no pidfd", true, false, unix.ENOSYS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.enosys {
				replaceWaitid(t, func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return unix.ENOSYS })
			}
			if !tc.pidfd {
				replacePidfdOpen(t, noPidfd)
			}
			events := recordSignals(t)
			termFile := filepath.Join(t.TempDir(), "term")
			var out lockedBuffer
			cmd := exec.Command("/bin/sh", "-c", `trap ': > "$0"; exit 0' TERM; echo up; i=0; while [ "$i" -lt 20 ]; do sleep 0.05; i=$((i + 1)); done`, termFile)
			cmd.Stdout, cmd.Stderr = &out, &out
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- runGroup(ctx, cmd, 300*time.Millisecond) }()
			if !tc.enosys {
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
				if !errors.Is(err, tc.want) || !errors.Is(err, errGroupNotStopped) {
					t.Errorf("err = %v, want %v and errGroupNotStopped", err, tc.want)
				}
			case <-time.After(10 * time.Second):
				// The test started this child: end it, and let Run reap it.
				_ = cmd.Process.Kill()
				<-errCh
				t.Fatal("Run did not return in 10 s")
			}
			if ev := events(); len(ev) != 0 {
				t.Errorf("a lost group got %v, want no group signal", sigs(ev))
			}
			_, termErr := os.Stat(termFile)
			ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
			switch {
			case !tc.pidfd && (termErr == nil || !cmd.ProcessState.Success()):
				t.Errorf("without a pidfd the child got a signal: state %v, SIGTERM trap ran: %v", cmd.ProcessState, termErr == nil)
			case tc.pidfd && !tc.enosys && termErr != nil:
				t.Error("with a pidfd the child got no SIGTERM on the cancel")
			case tc.pidfd && tc.enosys && !ws.Signaled() && termErr != nil:
				t.Errorf("with a pidfd the child got no signal after ENOSYS: state %v", cmd.ProcessState)
			}
		})
	}
}

// A leader that ignores SIGTERM: the stop must send SIGKILL. On a cancel,
// the group gets SIGKILL after WaitDelay; after a panic in ctx.Done, the
// guard sends SIGKILL after groupGrace, also with a zero WaitDelay. In
// both, the leader still runs at the SIGKILL (not reaped, not a zombie),
// the waitid goroutine ends, and the leader is reaped before Run returns
// (or before the panic reaches the caller). The leader sets its trap
// before it writes its ready file, and the cancel (or the panic) comes
// after that file. Without the SIGKILL, Run would wait for the leader
// without end.
func TestRunProcessGroupKillsALeaderThatIgnoresTerm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		panic bool
		delay time.Duration
		grace time.Duration
	}{
		{"a cancel", false, 300 * time.Millisecond, 300 * time.Millisecond},
		{"a panic, no WaitDelay", true, 0, groupGrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pid atomic.Int64
			var waiterEnded atomic.Bool
			replaceWaitid(t, func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error {
				pid.CompareAndSwap(0, int64(id))
				err := unix.Waitid(idType, id, info, options, rusage)
				if !errors.Is(err, unix.EINTR) {
					waiterEnded.Store(true)
				}
				return err
			})
			events := recordSignals(t)
			ready := filepath.Join(t.TempDir(), "ready")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runCtx := ctx
			if tc.panic {
				runCtx = readyPanicContext{Context: ctx, ready: ready}
			}
			type outcome struct {
				err   error
				panic any
			}
			done := make(chan outcome, 1)
			go func() {
				var o outcome
				defer func() {
					o.panic = recover()
					done <- o
				}()
				o.err = NewRunner(nil).Run(runCtx, Cmd{
					Name:         "/bin/sh",
					Args:         []string{"-c", `trap '' TERM; : > "$0"; while :; do sleep 0.05; done`, ready},
					WaitDelay:    tc.delay,
					ProcessGroup: true,
				})
			}()
			if !tc.panic {
				waitFile(t, ready)
				cancel()
			}
			var o outcome
			select {
			case o = <-done:
			case <-time.After(15 * time.Second):
				// The test started the leader: end it, so that Run reaps it.
				killOwnChild(int(pid.Load()))
				<-done
				t.Fatal("Run did not end: the leader that ignores SIGTERM got no SIGKILL")
			}
			if tc.panic {
				if err, ok := o.panic.(error); !ok || !errors.Is(err, errDonePanic) {
					t.Errorf("the caller got the panic %v, want %v", o.panic, errDonePanic)
				}
			} else if o.panic != nil || ExitCode(o.err) != 128+int(syscall.SIGKILL) {
				t.Errorf("err = %v, panic = %v; want the exit of a SIGKILL", o.err, o.panic)
			}
			ev := events()
			if len(ev) < 3 || !slices.Equal(sigs(ev[:3]), termContKill) {
				t.Fatalf("the group got %v, want %v first", sigs(ev), termContKill)
			}
			for _, e := range ev[3:] {
				if e.sig != syscall.SIGKILL {
					t.Errorf("after the SIGKILL the group got %v", e.sig)
				}
			}
			kill := ev[2]
			checkHeld(t, kill, false)
			if kill.leader.state == "Z" {
				t.Errorf("at the SIGKILL the leader was a zombie, want a leader that runs: %+v", kill.leader)
			}
			if took := kill.at.Sub(ev[0].at); took < tc.grace {
				t.Errorf("the SIGKILL came %s after the SIGTERM, want at least %s", took, tc.grace)
			}
			if !waiterEnded.Load() {
				t.Error("the waitid goroutine did not end before Run returned")
			}
			if p := int(pid.Load()); p == 0 {
				t.Error("waitid did not run")
			} else if e := readEntry(p); e.ok && e.ppid == os.Getpid() {
				t.Errorf("the leader (pid %d) is not reaped: %+v", p, e)
				killOwnChild(p)
			}
		})
	}
}

// Run closes the pidfd of the child on each path: after a normal end and
// after a panic in ctx.Done (the guard). The fd number comes from the seam
// pidfdOpen; after Run, /proc/self/fd has no pidfd under that number.
func TestRunProcessGroupClosesThePidfd(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func(context.Context) context.Context
	}{
		{"a normal end", func(c context.Context) context.Context { return c }},
		{"a panic", func(c context.Context) context.Context { return panicDoneContext{c} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fd atomic.Int64
			fd.Store(-1)
			replacePidfdOpen(t, func(pid, flags int) (int, error) {
				n, err := unix.PidfdOpen(pid, flags)
				if err == nil {
					fd.Store(int64(n))
				}
				return n, err
			})
			func() {
				defer func() { _ = recover() }()
				_ = NewRunner(nil).Run(tc.ctx(t.Context()), Cmd{
					Name:         "/bin/sh",
					Args:         []string{"-c", "exec sleep 0.1"},
					WaitDelay:    300 * time.Millisecond,
					ProcessGroup: true,
				})
			}()
			n := int(fd.Load())
			if n < 0 {
				t.Fatal("Run opened no pidfd")
			}
			if link, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(n)); err == nil && strings.Contains(link, "pidfd") {
				t.Errorf("the pidfd %d is still open after Run (%s)", n, link)
			}
		})
	}
}

// When the leader has gone (another waiter reaped it), scanGroup reports
// no member and does not read the list of /proc: the id of the group can
// be another group's now, so the stop sends nothing more. A scan that
// goes on would let the stop send SIGTERM and SIGKILL to that id.
func TestScanGroupStopsWhenTheLeaderIsGone(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := newGroup(cmd.Process.Pid)
	if g.lost {
		t.Fatal("newGroup: lost at the start")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	real := procNames
	t.Cleanup(func() { procNames = real })
	calls := 0
	procNames = func() ([]string, error) {
		calls++
		return real()
	}
	if scanGroup(g) {
		t.Error("scanGroup = true for a leader that has gone, want false (stop)")
	}
	if !g.lost {
		t.Error("the group is not lost after the leader has gone")
	}
	if othersInGroup(g) {
		t.Error("othersInGroup = true for a leader that has gone")
	}
	if calls != 0 {
		t.Errorf("the scan read the list of /proc %d times for a leader that has gone, want 0", calls)
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

// errDonePanic is the value of the panic of panicDoneContext.
var errDonePanic = errors.New("a Done that panics")

// panicDoneContext is a context of a caller whose Done panics. Err works,
// so Run starts the child before the panic.
type panicDoneContext struct{ context.Context }

func (panicDoneContext) Done() <-chan struct{} { panic(errDonePanic) }

// readyPanicContext is a context of a caller whose Done panics after the
// file ready exists (up to 5 s): the child is ready then.
type readyPanicContext struct {
	context.Context
	ready string
}

func (c readyPanicContext) Done() <-chan struct{} {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(c.ready); err == nil {
			break
		}
	}
	panic(errDonePanic)
}

// A panic after the start (here in ctx.Done) reaches the caller with its
// own value, and Run first stops the group, waits for the waitid goroutine
// and reaps the child: no child and no zombie is left. The pid comes from
// the seam waitid.
func TestRunProcessGroupPanicAfterStartReapsTheChild(t *testing.T) {
	var pid atomic.Int64
	replaceWaitid(t, func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error {
		pid.CompareAndSwap(0, int64(id))
		return unix.Waitid(idType, id, info, options, rusage)
	})
	events := recordSignals(t)
	var got any
	func() {
		defer func() { got = recover() }()
		_ = NewRunner(nil).Run(panicDoneContext{t.Context()}, Cmd{
			Name:         "/bin/sh",
			Args:         []string{"-c", "exec sleep 10"},
			WaitDelay:    300 * time.Millisecond,
			ProcessGroup: true,
		})
	}()
	if err, ok := got.(error); !ok || !errors.Is(err, errDonePanic) {
		t.Errorf("the caller got the panic %v, want %v", got, errDonePanic)
	}
	var p int
	for end := time.Now().Add(5 * time.Second); p == 0 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		p = int(pid.Load())
	}
	if p == 0 {
		t.Fatal("waitid did not run")
	}
	if e := readEntry(p); e.ok && e.ppid == os.Getpid() {
		t.Errorf("the child (pid %d) is not reaped after the panic: %+v", p, e)
		// The test started this child: end and reap it.
		_ = syscall.Kill(p, syscall.SIGKILL)
		var ws unix.WaitStatus
		_, _ = unix.Wait4(p, &ws, 0, nil)
	}
	ev := events()
	if len(ev) == 0 || ev[0].sig != syscall.SIGTERM {
		t.Fatalf("the group got %v, want a SIGTERM first", sigs(ev))
	}
	checkHeld(t, ev[0], false)
}

// On the other systems the group gets no signal (group_other.go): Run
// cannot see the end of the child there without a reap, and the Cancel of
// os/exec can run after the reap. That file does not build on Linux, so
// this test reads its source: no kill(2) call, and no Cancel of its own.
func TestGroupOtherSendsNoGroupSignal(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "group_other.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Kill" {
				if x, ok := sel.X.(*ast.Ident); ok && (x.Name == "syscall" || x.Name == "unix") {
					t.Errorf("group_other.go calls %s.Kill", x.Name)
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cancel" {
					t.Error("group_other.go sets a Cancel of its own")
				}
			}
		}
		return true
	})
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
			member, pidFile, termFile, stopFile := writeMember(t, t.TempDir())
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
			_ = os.WriteFile(stopFile, nil, 0o600)
			if !waitGone(pid, 20*time.Second) {
				t.Errorf("the child of the child (pid %d) did not end after its stop file", pid)
			}
		})
	}
}

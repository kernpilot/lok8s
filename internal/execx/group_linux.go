//go:build linux

package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// groupPoll is the interval of the checks for the members of a group.
const groupPoll = 20 * time.Millisecond

// errGroupNotStopped is in the error of Run when a check before a signal
// to the group failed (group.owned): the group did not get that signal or
// a later one, so members of the group can still run.
var errGroupNotStopped = errors.New("exec: the process group was not stopped")

// The seams of the tests. Production code does not change them.
var (
	// waitid is unix.Waitid. A test replaces it to inject an error, or a
	// reap by another waiter.
	waitid = unix.Waitid
	// groupSignalHook, when not nil, is called right before each signal
	// to the group, after the check (group.signal), with the start time
	// of the leader that Run recorded.
	groupSignalHook func(pgid int, start uint64, sig syscall.Signal)
)

// groupCmd builds the command of a ProcessGroup child and the function
// that runs it. The function keeps this order: the child ends, or the
// context ends and the group gets SIGTERM; then the stop of the other
// members ends (stopMembers); then cmd.Wait reaps the child and ends the
// copies of the pipes. Run watches the context itself (exec.Command, not
// exec.CommandContext): the context watch of os/exec would kill the child
// and close the pipes on its own timer.
//
// A signal to -pid is safe only while the child is not reaped: until the
// reap no other process can get its pid, so no other group can get the id
// of this group. This rests on two invariants: lo never sets SIGCHLD to
// SIG_IGN (with it, the kernel reaps the child itself), and only this
// runner waits on the child (no context watch of os/exec, and no other
// wait in lo). Run also checks the result of the two. After the start it
// records the start time of the child from /proc/<pid>/stat. Before each
// signal to the group (SIGTERM, SIGCONT, SIGKILL, on each path) it reads
// that file again (group.owned): the file must be there, with the same
// start time, and with the pid as its process group. When a check fails,
// the group gets no more signals: the stop ends, cmd.Wait reaps, and Run
// returns errGroupNotStopped in its error. A child that still runs then
// gets the SIGTERM and SIGKILL of a cancel alone, through os.Process
// (groupRun.signal). A window stays open: the time between the check and
// the kill(2). The child is not reaped in that window only while the two
// invariants hold.
func groupCmd(ctx context.Context, path string, args []string, waitDelay time.Duration) (*exec.Cmd, func() error) {
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, func() error { return runGroup(ctx, cmd, waitDelay) }
}

// group is the process group of a ProcessGroup child, the leader: its id
// is the pid of the child.
type group struct {
	pgid  int
	start uint64 // the start time of the leader, read after the start
	// lost is true after a check failed (owned): the group gets no more
	// signals.
	lost bool
}

// newGroup records the leader pid and its start time. When /proc does not
// show pid as the leader of its group, the group is lost from the start.
func newGroup(pid int) *group {
	g := &group{pgid: pid}
	st, ok := readStat(pid)
	if !ok || st.pgrp != pid {
		g.lost = true
	}
	g.start = st.start
	return g
}

// owned reports whether the id of g is still the group of the child:
// /proc/<pgid>/stat is there, with the start time that newGroup recorded,
// and with pgid as its process group. Then the child is not reaped yet, so
// no other group can have the id: a process that got the pid after a reap
// has another start time. After a false, owned stays false.
func (g *group) owned() bool {
	if g.lost {
		return false
	}
	st, ok := readStat(g.pgid)
	if !ok || st.start != g.start || st.pgrp != g.pgid {
		g.lost = true
	}
	return !g.lost
}

// signal sends sig to the group when owned reports true, and reports
// whether it sent it.
func (g *group) signal(sig syscall.Signal) (bool, error) {
	if !g.owned() {
		return false, nil
	}
	if groupSignalHook != nil {
		groupSignalHook(g.pgid, g.start, sig)
	}
	return true, syscall.Kill(-g.pgid, sig)
}

// groupRun is the state of one runGroup, which the panic guard (abort)
// shares.
type groupRun struct {
	cmd       *exec.Cmd
	g         *group
	waitDelay time.Duration
	exited    chan error // the result of waitExited
	exitSeen  bool       // exited was read, into waitErr
	waitErr   error
	termAt    time.Time // the time of the SIGTERM of the stop; zero: not sent
}

// runGroup starts cmd, waits for the end of the child or of ctx, stops the
// group, and reaps the child. After a cancel it returns the error of the
// context when the child ended with 0, as exec.Cmd does after its Cancel.
func runGroup(ctx context.Context, cmd *exec.Cmd, waitDelay time.Duration) error {
	// As exec.CommandContext: a context that ended before the start gives
	// its error, and no child.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := &groupRun{cmd: cmd, g: newGroup(cmd.Process.Pid), waitDelay: waitDelay, exited: make(chan error, 1)}
	go func() { r.exited <- waitExited(r.g.pgid) }()
	// The guard of a panic after the start, for example in ctx.Done (a
	// method of the context of the caller): abort stops the group with
	// the same checks, waits for the waitid goroutine, and reaps the
	// child. It does not call recover, so the panic goes on to the caller
	// with its own value and stack.
	finished := false
	defer func() {
		if !finished {
			r.abort()
		}
	}()
	cancelErr := r.wait(ctx)
	r.stop()
	err := cmd.Wait()
	finished = true
	return r.result(err, cancelErr)
}

// wait waits for the end of the child or of ctx. When ctx ends first, the
// group gets SIGTERM and SIGCONT (term), and SIGKILL after waitDelay when
// it is positive; wait then goes on until the child ends. It returns the
// error of the cancel, or nil when the child ended before ctx.
func (r *groupRun) wait(ctx context.Context) error {
	var cancelErr error
	var killAt <-chan time.Time
	done := ctx.Done()
	for {
		select {
		case r.waitErr = <-r.exited:
			r.exitSeen = true
			return cancelErr
		case <-done:
			done = nil
			select {
			case r.waitErr = <-r.exited:
				// The child ended before the cancel: no error of the
				// context.
				r.exitSeen = true
				return nil
			default:
			}
			cancelErr = ctx.Err()
			if err := r.term(); err != nil {
				cancelErr = fmt.Errorf("exec: canceling Cmd: %w", err)
			}
			if r.waitDelay > 0 {
				timer := time.NewTimer(r.waitDelay)
				defer timer.Stop()
				killAt = timer.C
			}
		case <-killAt:
			killAt = nil
			_ = r.signal(syscall.SIGKILL)
		}
	}
}

// stop ends the group after the end of the child, before the reap: the
// members that are left get SIGTERM, and SIGKILL after the grace
// (stopMembers). After a waitid error the end of the child is not known
// without a reap: the group gets SIGTERM now (when it did not get it), and
// SIGKILL after the grace (stopGrace). A lost group gets no signal (owned):
// the child alone gets them then, unless it has gone already.
func (r *groupRun) stop() {
	if r.waitErr != nil {
		if r.termAt.IsZero() {
			_ = r.term()
		}
		if !r.g.lost || !r.childGone() {
			time.Sleep(time.Until(r.termAt.Add(stopGrace(r.waitDelay))))
			_ = r.signal(syscall.SIGKILL)
		}
	}
	stopMembers(r.g, r.termAt, r.waitDelay)
}

// abort runs when a panic leaves runGroup after the start. It stops as a
// cancel does: SIGTERM and SIGCONT (when not sent yet), and SIGKILL after
// the grace also when waitDelay is zero, so that the panic does not wait
// for a child that ignores SIGTERM. Then it waits for the waitid
// goroutine, stops the members (stop), and reaps the child.
func (r *groupRun) abort() {
	if !r.exitSeen {
		if r.termAt.IsZero() {
			_ = r.term()
		}
		timer := time.NewTimer(time.Until(r.termAt.Add(stopGrace(r.waitDelay))))
		select {
		case r.waitErr = <-r.exited:
		case <-timer.C:
			_ = r.signal(syscall.SIGKILL)
			r.waitErr = <-r.exited
		}
		timer.Stop()
		r.exitSeen = true
	}
	r.stop()
	_ = r.cmd.Wait()
}

// result returns the error of Run: the waitid error, when there is one;
// the error of the cancel, when the child ended with 0 after it (as
// exec.Cmd after its Cancel); else the error of cmd.Wait. When a check
// stopped the signals to the group (owned), errGroupNotStopped is joined
// to it, also to a nil error.
func (r *groupRun) result(reapErr, cancelErr error) error {
	err := reapErr
	switch {
	case r.waitErr != nil:
		err = os.NewSyscallError("waitid", r.waitErr)
	case cancelErr != nil && r.cmd.ProcessState != nil && r.cmd.ProcessState.Success():
		err = cancelErr
	}
	if r.g.lost {
		err = errors.Join(err, fmt.Errorf("%w: its leader (pid %d) was gone, or /proc did not show it", errGroupNotStopped, r.g.pgid))
	}
	return err
}

// term sends SIGTERM and SIGCONT (SIGCONT: a member that a read of the
// terminal stopped gets the SIGTERM too), and records the time of the
// SIGTERM. It returns the error of the SIGTERM.
func (r *groupRun) term() error {
	r.termAt = time.Now()
	err := r.signal(syscall.SIGTERM)
	_ = r.signal(syscall.SIGCONT)
	return err
}

// signal sends sig to the group (group.signal). When the group is lost,
// the child alone gets sig, through cmd.Process: os.Process sends through
// a pidfd when the kernel has one, and sends nothing after its own Wait.
// A child that has gone gives no error.
func (r *groupRun) signal(sig syscall.Signal) error {
	if sent, err := r.g.signal(sig); sent {
		return err
	}
	if err := r.cmd.Process.Signal(sig); !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// childGone reports whether the child has gone (another waiter reaped it):
// a signal 0 through cmd.Process gives os.ErrProcessDone.
func (r *groupRun) childGone() bool {
	return errors.Is(r.cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// waitExited waits until the child pid has ended, and does not reap it:
// with WNOWAIT, waitid leaves the child a zombie, and os.Process.Wait
// reaps it later.
func waitExited(pid int) error {
	for {
		var info unix.Siginfo
		err := waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// stopMembers ends the members of the group g other than its leader,
// which has ended and is not reaped. When no other member is left, or the
// group is lost, it sends nothing. Else it sends SIGTERM and SIGCONT if
// the group did not get them yet (termAt is zero), waits until the grace
// after the SIGTERM (stopGrace) for the members to end, sends SIGKILL to
// the group, and waits up to 1 s for the members to go. Each signal has
// the check of group.signal.
func stopMembers(g *group, termAt time.Time, waitDelay time.Duration) {
	if !othersInGroup(g) {
		return
	}
	if termAt.IsZero() {
		termAt = time.Now()
		_, _ = g.signal(syscall.SIGTERM)
		_, _ = g.signal(syscall.SIGCONT)
	}
	killAt := termAt.Add(stopGrace(waitDelay))
	for othersInGroup(g) {
		if time.Now().After(killAt) {
			_, _ = g.signal(syscall.SIGKILL)
			for end := time.Now().Add(time.Second); othersInGroup(g) && time.Now().Before(end); {
				time.Sleep(groupPoll)
			}
			return
		}
		time.Sleep(groupPoll)
	}
}

// othersInGroup reports whether a process other than the leader is in the
// group g. While the leader is not reaped, kill(-pgid, 0) cannot tell: the
// zombie leader is a member. So it reads the process group of each process
// in /proc (scanGroup). One scan can miss a member: a member can start a
// child after the scan read the list of /proc, and end before the scan
// reads its own entry. So it reports false only after two scans in a row
// found no member.
func othersInGroup(g *group) bool {
	for range 2 {
		if scanGroup(g) {
			return true
		}
	}
	return false
}

// procNames reads the names in /proc. A test replaces it.
var procNames = func() ([]string, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close() // a read-only dir: a close error changes nothing here
	return names, err
}

// scanGroup reads the list of /proc once, and reports whether a process in
// it other than the leader is in the group g. A member that its new parent
// has not reaped yet (a zombie) still counts. First it checks the leader
// (owned): when the leader has gone, the id can be another group's, so it
// reports false, and the stop sends nothing more. When the list cannot be
// read, it reports true: the stop then sends SIGKILL after the grace, and
// each signal still has the check of the leader.
func scanGroup(g *group) bool {
	if !g.owned() {
		return false
	}
	names, err := procNames()
	if err != nil {
		return true
	}
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid == g.pgid {
			continue
		}
		if st, ok := readStat(pid); ok && st.pgrp == g.pgid {
			return true
		}
	}
	return false
}

// procStat holds the fields of /proc/<pid>/stat that the stop reads.
type procStat struct {
	pgrp  int    // field 5: the process group
	start uint64 // field 22: the start time, in clock ticks after the boot
}

// readStat reads /proc/<pid>/stat. The fields after the name are read
// after its last ")": the name itself can hold ") ". It reports false when
// the file cannot be read (the process has gone, or /proc is not usable)
// or parsed.
func readStat(pid int) (procStat, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return procStat{}, false
	}
	// f[0] is field 3 (the state), so field n is f[n-3].
	f := bytes.Fields(raw[i+1:])
	if len(f) < 20 {
		return procStat{}, false
	}
	pgrp, err1 := strconv.Atoi(string(f[2]))
	start, err2 := strconv.ParseUint(string(f[19]), 10, 64)
	if err1 != nil || err2 != nil {
		return procStat{}, false
	}
	return procStat{pgrp: pgrp, start: start}, true
}

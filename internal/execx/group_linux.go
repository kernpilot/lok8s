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

// groupCmd builds the command of a ProcessGroup child and the function
// that runs it. The function keeps this order: the child ends, or the
// context ends and the group gets SIGTERM; then the stop of the other
// members ends (stopMembers); then cmd.Wait reaps the child and ends the
// copies of the pipes. Until the reap the child is a zombie: no other
// process can get its pid, so no other group can get the id of this group,
// and each signal to -pid reaches this group only. Run watches the context
// itself (exec.Command, not exec.CommandContext): the context watch of
// os/exec would kill the child and close the pipes on its own timer.
func groupCmd(ctx context.Context, path string, args []string, waitDelay time.Duration) (*exec.Cmd, func() error) {
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, func() error { return runGroup(ctx, cmd, waitDelay) }
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
	pgid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- waitExited(pgid) }()

	var termAt time.Time // the time of the group SIGTERM; zero: not sent
	var cancelErr error
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	var killAt <-chan time.Time
	done := ctx.Done()
	var waitErr error
wait:
	for {
		select {
		case waitErr = <-exited:
			break wait
		case <-done:
			done = nil
			select {
			case waitErr = <-exited:
				// The child ended before the cancel: no error of the
				// context.
				break wait
			default:
			}
			termAt = time.Now()
			cancelErr = ctx.Err()
			if err := termGroup(pgid); err != nil {
				cancelErr = fmt.Errorf("exec: canceling Cmd: %w", err)
			}
			if waitDelay > 0 {
				timer = time.NewTimer(waitDelay)
				killAt = timer.C
			}
		case <-killAt:
			killAt = nil
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	if waitErr != nil {
		// The end of the child is not known without a reap. The child is
		// not reaped, so the id is still this group's: stop the group
		// now, and send SIGKILL after the grace (stopGrace), before the
		// reap.
		if termAt.IsZero() {
			termAt = time.Now()
			_ = termGroup(pgid)
		}
		time.Sleep(time.Until(termAt.Add(stopGrace(waitDelay))))
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	stopMembers(pgid, termAt, waitDelay)
	err := cmd.Wait()
	if waitErr != nil {
		return os.NewSyscallError("waitid", waitErr)
	}
	if cancelErr != nil && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return cancelErr
	}
	return err
}

// waitExited waits until the child pid has ended, and does not reap it:
// with WNOWAIT, waitid leaves the child a zombie, and os.Process.Wait
// reaps it later.
func waitExited(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// termGroup sends SIGTERM and SIGCONT to the group pgid (SIGCONT: a member
// that a read of the terminal stopped gets the SIGTERM too). It returns the
// error of the SIGTERM.
func termGroup(pgid int) error {
	err := syscall.Kill(-pgid, syscall.SIGTERM)
	_ = syscall.Kill(-pgid, syscall.SIGCONT)
	return err
}

// stopMembers ends the members of the group pgid other than its leader,
// which has ended and is not reaped. When no other member is left, it
// sends nothing. Else it sends SIGTERM and SIGCONT if the group did not get
// them yet (termAt is zero), waits until the grace after the SIGTERM
// (stopGrace) for the members to end, sends SIGKILL to the group, and
// waits up to 1 s for the members to go.
func stopMembers(pgid int, termAt time.Time, waitDelay time.Duration) {
	if !othersInGroup(pgid) {
		return
	}
	if termAt.IsZero() {
		termAt = time.Now()
		_ = termGroup(pgid)
	}
	killAt := termAt.Add(stopGrace(waitDelay))
	for othersInGroup(pgid) {
		if time.Now().After(killAt) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			for end := time.Now().Add(time.Second); othersInGroup(pgid) && time.Now().Before(end); {
				time.Sleep(groupPoll)
			}
			return
		}
		time.Sleep(groupPoll)
	}
}

// othersInGroup reports whether a process other than the leader pgid is in
// the process group pgid. While the leader is not reaped, kill(-pgid, 0)
// cannot tell: the zombie leader is a member. So it reads the process group
// of each process in /proc (scanGroup). One scan can miss a member: a
// member can start a child after the scan read the list of /proc, and end
// before the scan reads its own entry. So it reports false only after two
// scans in a row found no member.
func othersInGroup(pgid int) bool {
	for range 2 {
		if scanGroup(pgid) {
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
// it other than the leader pgid is in the process group pgid. A member that
// its new parent has not reaped yet (a zombie) still counts. When /proc
// does not show the leader in its group, /proc is not usable, and it
// reports true: the stop then sends SIGKILL after the grace.
func scanGroup(pgid int) bool {
	if g, ok := procGroup(pgid); !ok || g != pgid {
		return true
	}
	names, err := procNames()
	if err != nil {
		return true
	}
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid == pgid {
			continue
		}
		if g, ok := procGroup(pid); ok && g == pgid {
			return true
		}
	}
	return false
}

// procGroup returns the process group of pid from /proc/<pid>/stat: the
// third field after the name, which ends at the last ")". It reports false
// when the file cannot be read (the process has gone) or parsed.
func procGroup(pid int) (int, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, false
	}
	f := bytes.Fields(raw[i+1:])
	if len(f) < 3 {
		return 0, false
	}
	g, err := strconv.Atoi(string(f[2]))
	return g, err == nil
}

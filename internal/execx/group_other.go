//go:build !linux

package execx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// groupCmd builds the command of a ProcessGroup child and the function
// that runs it. golang.org/x/sys/unix has waitid only on Linux (and z/OS),
// so this path cannot see the end of the child without a reap: Run reaps
// the child first, and stops the group only after a cancelled context
// (stopGroup). A member that is left after the child ended on its own is
// not stopped.
func groupCmd(ctx context.Context, path string, args []string, waitDelay time.Duration) (*exec.Cmd, func() error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stopped atomic.Int64 // the time of the group SIGTERM
	cmd.Cancel = func() error {
		stopped.Store(time.Now().UnixNano())
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGCONT)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd, func() error {
		err := cmd.Run()
		if t := stopped.Load(); t != 0 && cmd.Process != nil {
			stopGroup(cmd.Process.Pid, time.Unix(0, t).Add(waitDelay))
		}
		return err
	}
}

// stopGroup ends the process group pgid of a cancelled ProcessGroup child,
// after Run reaped its leader: it waits until killAt for the group to end
// after the SIGTERM, sends SIGKILL to the members that are left, and waits
// up to 1 s for them to go. A member that its new parent has not reaped
// yet (a zombie) still counts. kill(-pgid, 0) fails with ESRCH when no
// member is left.
func stopGroup(pgid int, killAt time.Time) {
	gone := func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }
	for !gone() {
		if time.Now().After(killAt) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			for end := time.Now().Add(time.Second); !gone() && time.Now().Before(end); {
				time.Sleep(10 * time.Millisecond)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

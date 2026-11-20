//go:build !linux

package execx

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// groupCmd builds the command of a ProcessGroup child and the function
// that runs it. On this path the child leads a new process group, but the
// group gets no signal. golang.org/x/sys/unix has waitid only on Linux
// (and z/OS), so Run cannot see the end of the child without a reap, and
// after the reap the kernel can give the id of the group to another group.
// The Cancel of exec.CommandContext can also run after the reap: Cmd.Wait
// reaps the child before it reads the result of the context watch
// (watchCtx), and the watch calls Cancel when the context ends in that
// time. So a cancel stops the child alone, with the default Cancel of
// exec.CommandContext (SIGKILL through os.Process, which sends no signal
// after its own Wait). The other members of the group are not stopped:
// not after a cancel, and not after the child ends on its own. A port
// could see the end of the child without a reap through kqueue
// (EVFILT_PROC, NOTE_EXIT).
func groupCmd(ctx context.Context, path string, args []string, _ time.Duration) (*exec.Cmd, func() error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, cmd.Run
}

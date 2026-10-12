package execx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
)

// Output runs c through r and returns its stdout — the `$(cmd 2>/dev/null)`
// shape of the bash ports (and of exec.Cmd.Output, whose stderr capture
// none of the call sites ever read). Stdin is closed and stderr is
// discarded unless c sets them. Stdout must be unset.
func Output(ctx context.Context, r Runner, c Cmd) ([]byte, error) {
	var out bytes.Buffer
	c.Stdout = &out
	if c.Stdin == nil {
		c.Stdin = strings.NewReader("")
	}
	if c.Stderr == nil {
		c.Stderr = io.Discard
	}
	err := r.Run(ctx, c)
	return out.Bytes(), err
}

// TrimNewlines drops the trailing newlines of s, as a bash command
// substitution (`$(…)`) does with a tool's output or a file's content.
func TrimNewlines(s string) string { return strings.TrimRight(s, "\n") }

// ExitCode maps a Runner error to the subprocess exit code: nil → 0, an
// *exec.ExitError or anything carrying ExitCode() → its code, anything else
// (the tool was not found, the context ended) → 1. A child that died of a
// signal reports 128+n, which is what bash's `$?` shows for it (130 for
// SIGINT, 143 for SIGTERM); exec.ExitError.ExitCode alone reports -1 there,
// and -1 reaches os.Exit as 255.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if xe, ok := errors.AsType[*exec.ExitError](err); ok {
		if ws, ok := xe.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return xe.ExitCode()
	}
	var ce interface{ ExitCode() int }
	if errors.As(err, &ce) {
		return ce.ExitCode()
	}
	return 1
}

// LaunchFailed reports whether err means the process never produced an
// exit code at all (the binary was not found, the context ended before it
// started, …) rather than ran and exited non-zero. ExitCode folds this
// case into a bare 1 for the common "$?" caller; a classifier that needs
// to tell "launched and differed/erred" apart from "never ran" (dry-run
// diff classification, review round 2 item F) checks this FIRST.
func LaunchFailed(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return false
	}
	var ce interface{ ExitCode() int }
	return !errors.As(err, &ce)
}

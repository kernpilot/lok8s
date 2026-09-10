package execx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
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

// ExitCode maps a Runner error to the subprocess exit code: nil → 0, an
// *exec.ExitError or anything carrying ExitCode() → its code, anything else
// (the tool was not found, the context ended) → 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if xe, ok := errors.AsType[*exec.ExitError](err); ok {
		return xe.ExitCode()
	}
	var ce interface{ ExitCode() int }
	if errors.As(err, &ce) {
		return ce.ExitCode()
	}
	return 1
}

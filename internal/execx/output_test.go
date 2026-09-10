package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

type codeErr struct{ code int }

func (e codeErr) Error() string { return fmt.Sprintf("rc %d", e.code) }
func (e codeErr) ExitCode() int { return e.code }

func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("nil = %d", got)
	}
	if got := ExitCode(errors.New("plain")); got != 1 {
		t.Errorf("plain error = %d", got)
	}
	if got := ExitCode(fmt.Errorf("wrapped: %w", codeErr{7})); got != 7 {
		t.Errorf("wrapped ExitCode() = %d", got)
	}
}

// recordingRunner captures the Cmd handed to Run and answers scripted stdout.
type recordingRunner struct {
	got    Cmd
	stdout string
	err    error
}

func (r *recordingRunner) Run(_ context.Context, c Cmd) error {
	r.got = c
	if c.Stdout != nil {
		io.WriteString(c.Stdout, r.stdout)
	}
	return r.err
}

// Output captures stdout, closes stdin and discards stderr unless set — the
// `$(cmd 2>/dev/null)` contract every capture site relies on.
func TestOutputShape(t *testing.T) {
	r := &recordingRunner{stdout: "v1.2.3\n", err: errors.New("rc")}
	out, err := Output(context.Background(), r, Cmd{Name: "tool", Args: []string{"--version"}})
	if string(out) != "v1.2.3\n" || err == nil {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if r.got.Stderr != io.Discard {
		t.Error("stderr must be discarded by default")
	}
	if r.got.Stdin == nil {
		t.Error("stdin must be closed (an empty reader), never the terminal")
	}
	if b, _ := io.ReadAll(r.got.Stdin); len(b) != 0 {
		t.Error("stdin must read as EOF")
	}
	own := &recordingRunner{}
	sink := io.Discard
	if _, err := Output(context.Background(), own, Cmd{Name: "t", Stderr: sink}); err != nil || own.got.Stderr != sink {
		t.Error("an explicit stderr is kept")
	}
}

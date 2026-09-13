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
	t.Parallel()
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

// A child that died of a signal exits 128+n, as bash's `$?` reports it.
// exec.ExitError.ExitCode alone gives -1 there, which os.Exit turns into
// 255, and main's 128+n mapping never sees a child's signal.
func TestExitCodeOfASignaledChildIs128PlusN(t *testing.T) {
	t.Parallel()
	r := NewRunner(nil)
	for sig, want := range map[string]int{"INT": 130, "TERM": 143} {
		err := r.Run(t.Context(), Cmd{Name: "/bin/sh", Args: []string{"-c", "kill -" + sig + " $$"}, Stdout: io.Discard, Stderr: io.Discard})
		if err == nil {
			t.Fatalf("SIG%s: the child did not die of the signal", sig)
		}
		if got := ExitCode(err); got != want {
			t.Errorf("SIG%s: ExitCode = %d, want %d", sig, got, want)
		}
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
	out, err := Output(t.Context(), r, Cmd{Name: "tool", Args: []string{"--version"}})
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
	if _, err := Output(t.Context(), own, Cmd{Name: "t", Stderr: sink}); err != nil || own.got.Stderr != sink {
		t.Error("an explicit stderr is kept")
	}
}

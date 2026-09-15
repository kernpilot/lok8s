package execx

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

func TestDebugNamesTheFailedCommand(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}
	var out bytes.Buffer
	prevDebug, prevOut := Debug, DebugOut
	Debug, DebugOut = true, &out
	t.Cleanup(func() { Debug, DebugOut = prevDebug, prevOut })

	r := NewRunner(&config.Paths{Base: t.TempDir()})
	err = r.Run(context.Background(), Cmd{Name: sh, Args: []string{"-c", "exit 3"}, Stdout: io.Discard, Stderr: io.Discard})
	if err == nil {
		t.Fatal("exit 3 reported no error")
	}
	if want := "[debug] exec: " + sh + " -c 'exit 3': exit 3\n"; out.String() != want {
		t.Errorf("debug line = %q, want %q", out.String(), want)
	}

	// A success prints nothing; off --debug a failure prints nothing.
	out.Reset()
	_ = r.Run(context.Background(), Cmd{Name: sh, Args: []string{"-c", "exit 0"}, Stdout: io.Discard, Stderr: io.Discard})
	Debug = false
	_ = r.Run(context.Background(), Cmd{Name: sh, Args: []string{"-c", "exit 1"}, Stdout: io.Discard, Stderr: io.Discard})
	if out.Len() != 0 {
		t.Errorf("unexpected debug output: %q", out.String())
	}
}

func TestShellWords(t *testing.T) {
	got := shellWords([]string{"docker", "run", "--name", "a b", "it's", "", "plain-1_2.3"})
	if want := `docker run --name 'a b' 'it'\''s' '' plain-1_2.3`; got != want {
		t.Errorf("shellWords = %s, want %s", got, want)
	}
	if !strings.Contains(shellWords([]string{"x", "$HOME"}), "'$HOME'") {
		t.Errorf("a $ word must be quoted")
	}
}

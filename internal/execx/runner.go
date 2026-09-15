package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
)

// Cmd describes one external process invocation.
type Cmd struct {
	// Name is the tool name, resolved via Look (b-managed .bin first, then
	// PATH). A value containing a path separator is used as-is.
	Name string
	Args []string
	// Dir is the working directory ("" = inherit). The kubeone driver
	// depends on this: `kubeone apply` writes <name>-kubeconfig into its
	// CWD, so the apply MUST run inside the work dir.
	Dir string
	// Env entries are appended to the inherited environment.
	Env []string
	// Stdin/Stdout/Stderr default to the process's own when nil.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Runner runs external commands. The single seam that lets driver code run
// hermetically under test (a fake Runner records the Cmd instead of
// executing).
type Runner interface {
	Run(ctx context.Context, c Cmd) error
}

// Debug, when set (the root --debug flag; no environment variable sets
// it), prints every failed external command with its exit code on
// stderr, so a failure names the docker/kind/kubectl line behind it.
var Debug bool

// DebugOut receives the --debug lines; tests redirect it.
var DebugOut io.Writer = os.Stderr

// ErrNotFound is the Runner's error when a tool name resolves nowhere
// (wrapped as `<name>: executable not found`); errors.Is matches it.
var ErrNotFound = errors.New("executable not found")

// NewRunner builds the default Runner over the resolved project paths.
func NewRunner(p *config.Paths) Runner {
	return &osRunner{paths: p}
}

type osRunner struct {
	paths *config.Paths
}

func (r *osRunner) Run(ctx context.Context, c Cmd) error {
	path := c.Name
	if !strings.ContainsRune(c.Name, os.PathSeparator) {
		resolved, ok := Look(r.paths, c.Name)
		if !ok {
			return fmt.Errorf("%s: %w", c.Name, ErrNotFound)
		}
		path = resolved
	}
	cmd := exec.CommandContext(ctx, path, c.Args...)
	// On a cancelled context the child gets SIGINT, not the SIGKILL
	// exec.CommandContext sends by default: a Ctrl-C already delivered it
	// to the whole foreground process group, and the child (kubeone,
	// terraform, kubectl) finishes its own cleanup the way it did under
	// the bash entrypoint, which waited for it. No WaitDelay: like bash,
	// the parent waits for the child to end. On SIGTERM this is a
	// deliberate deviation: the bash shell died and left the child
	// running, the binary interrupts it and waits (catalogue D26).
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	if c.Stdin != nil {
		cmd.Stdin = c.Stdin
	} else {
		cmd.Stdin = os.Stdin
	}
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		cmd.Stdout = os.Stdout
	}
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	err := cmd.Run()
	if err != nil && Debug {
		fmt.Fprintf(DebugOut, "[debug] exec: %s: exit %d\n", shellWords(append([]string{path}, c.Args...)), ExitCode(err))
	}
	return err
}

// shellWords joins argv for a human, quoting the words a shell would need
// quoted.
func shellWords(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$`\\|&;<>()*?[]#~") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
			continue
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

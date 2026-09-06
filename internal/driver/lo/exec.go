package lo

// exec.go — thin helpers over the injectable execx.Runner seam. EVERY
// external invocation (docker, kind, kubectl, ssh, rsync, scp, git, curl,
// the secrets plugin binary) goes through these, so unit tests run against a
// fake runner and never touch a live daemon.

import (
	"context"
	"io"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
)

// run executes a tool, stdout/stderr inherited (bash: a bare command).
func (d *Driver) run(ctx context.Context, name string, args ...string) error {
	return d.deps.Runner.Run(ctx, execx.Cmd{Name: name, Args: args})
}

// runQuiet executes a tool with all output discarded (bash: cmd &>/dev/null
// / 2>/dev/null >/dev/null).
func (d *Driver) runQuiet(ctx context.Context, name string, args ...string) error {
	return d.deps.Runner.Run(ctx, execx.Cmd{Name: name, Args: args, Stdout: io.Discard, Stderr: io.Discard})
}

// output captures a tool's stdout, stderr discarded (bash: $(cmd
// 2>/dev/null)). The captured text is returned with trailing newlines
// trimmed, matching $() semantics.
func (d *Driver) output(ctx context.Context, name string, args ...string) (string, error) {
	var out strings.Builder
	err := d.deps.Runner.Run(ctx, execx.Cmd{Name: name, Args: args, Stdout: &out, Stderr: io.Discard})
	return strings.TrimRight(out.String(), "\n"), err
}

// errOutput captures a tool's STDERR, stdout discarded (bash:
// $(cmd 2>&1 >/dev/null) — the registry-run error capture).
func (d *Driver) errOutput(ctx context.Context, name string, args ...string) (string, error) {
	var errBuf strings.Builder
	err := d.deps.Runner.Run(ctx, execx.Cmd{Name: name, Args: args, Stdout: io.Discard, Stderr: &errBuf})
	return strings.TrimRight(errBuf.String(), "\n"), err
}

// runInput executes a tool with the given stdin, output inherited-by-writer
// (bash: cmd <<EOF … EOF / printf … | cmd).
func (d *Driver) runInput(ctx context.Context, stdin string, stdout, stderr io.Writer, name string, args ...string) error {
	return d.deps.Runner.Run(ctx, execx.Cmd{
		Name: name, Args: args,
		Stdin:  strings.NewReader(stdin),
		Stdout: stdout, Stderr: stderr,
	})
}

// cmdWith builds an execx.Cmd bound to the given writers.
func cmdWith(name string, args []string, stdout, stderr io.Writer) execx.Cmd {
	return execx.Cmd{Name: name, Args: args, Stdout: stdout, Stderr: stderr}
}

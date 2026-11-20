package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

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
	// Unset names variables that the child does not inherit: the child's
	// environment is the inherited one without these names, then Env. A
	// name in both is set to its Env value. An empty value is not an
	// unset (some TLS backends test only that a variable exists), so the
	// name must be removed (see ChildEnv).
	Unset []string
	// Stdin/Stdout/Stderr default to the process's own when nil.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// WaitDelay bounds the wait for a child after its context ends, and
	// for its output pipes after it exits (exec.Cmd.WaitDelay): a child
	// that still runs then is killed, and the pipes close. Zero keeps the
	// default: the parent waits for the child to end. For the grace of a
	// group stop, see ProcessGroup.
	WaitDelay time.Duration
	// ProcessGroup starts the child as the leader of a new process group,
	// so that a stop also reaches the children of the child (the exec
	// credential plugin of a kubectl). On Linux: on a cancelled context
	// the group gets SIGTERM and SIGCONT, not the SIGINT of the child
	// alone. When the child ends first and other members of its group are
	// left, they get the same SIGTERM and SIGCONT, also after an exit with
	// 0. This also stops a process that a member started in the group to
	// outlive the child, for example a browser that a credential plugin
	// opened for a login. When the grace has passed after that signal, the
	// group gets SIGKILL, and Run then waits up to 1 s more for the group
	// to be gone. The grace is WaitDelay, or groupGrace when WaitDelay is
	// zero. With zero, Run still waits for the child itself to end after a
	// cancel, as the default does. Run reaps the child only after this
	// stop: while the child is not reaped, no other process can get its
	// pid, so no other group can get the id of its group. Before each
	// signal to the group, Run checks in /proc that the child is still
	// there and not reaped (groupCmd). When a check fails, the group gets
	// no more signals, the child alone gets the signals of the stop through
	// a pidfd (no signal without one), and Run returns an error that says
	// the group was not stopped. Then Run
	// waits up to WaitDelay for the output pipes, which a process outside
	// the group can keep open. When the child ends first, a cancel during
	// the stop does not change the result (see osRunner.Run). On other
	// systems, the group gets no signal: Run cannot see the end of the
	// child there without a reap. A cancel stops the child alone (SIGKILL,
	// the default of exec.CommandContext), and the other members of the
	// group are not stopped. The group is not the foreground group of a
	// terminal: a member that reads the terminal stops there until the
	// stop, or on other systems until it ends. False keeps the default:
	// the child stays in the process group of lo.
	ProcessGroup bool
	// Exited, when set, gets the state of the child after it ended (not
	// when it did not start). After the context ends, Run returns the
	// error of the context also for a child that ended with 0
	// (exec.Cmd.Cancel), and Start returns the same error when the child
	// did not start: the state tells the two apart.
	Exited func(*os.ProcessState)
}

// groupGrace is the grace of a group stop (ProcessGroup) for a Cmd
// without a WaitDelay: the time from the SIGTERM to the SIGKILL.
const groupGrace = 2 * time.Second

// stopGrace returns the grace of a group stop: waitDelay, or groupGrace
// when waitDelay is zero.
func stopGrace(waitDelay time.Duration) time.Duration {
	if waitDelay > 0 {
		return waitDelay
	}
	return groupGrace
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

// Run runs c and returns its error (see Cmd). With ProcessGroup on Linux,
// the first of two events fixes the result: the end of the child, or the
// end of ctx. When the child ends first, the stop of the group and the
// reap that come after it do not watch ctx. A cancel in that time does not
// change the result, so Run can return nil (the child ended with 0) after
// ctx ended. A caller that needs the deadline to win checks ctx.Err()
// itself after Run.
func (r *osRunner) Run(ctx context.Context, c Cmd) error {
	path := c.Name
	if !strings.ContainsRune(c.Name, os.PathSeparator) {
		resolved, ok := Look(r.paths, c.Name)
		if !ok {
			return fmt.Errorf("%s: %w", c.Name, ErrNotFound)
		}
		path = resolved
	}
	var cmd *exec.Cmd
	var run func() error
	if c.ProcessGroup {
		// A ProcessGroup child is not in the foreground process group: a
		// Ctrl-C reaches lo alone, and its interrupt watch cancels the
		// context. The cancel then stops the whole group of the child
		// (groupCmd).
		cmd, run = groupCmd(ctx, path, c.Args, c.WaitDelay)
	} else {
		cmd = exec.CommandContext(ctx, path, c.Args...)
		// On a cancelled context the child gets SIGINT, not the SIGKILL
		// exec.CommandContext sends by default: a Ctrl-C already delivered
		// it to the whole foreground process group, and the child (kubeone,
		// terraform, kubectl) finishes its own cleanup the way it did under
		// the bash entrypoint, which waited for it. No WaitDelay unless the
		// Cmd sets one: like bash, the parent waits for the child to end. A
		// Cmd with a WaitDelay gets a SIGKILL after it. On SIGTERM this is a
		// deliberate deviation: the bash shell died and left the child
		// running, the binary interrupts it and waits (catalogue D26).
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		run = cmd.Run
	}
	cmd.WaitDelay = c.WaitDelay
	cmd.Dir = c.Dir
	cmd.Env = ChildEnv(c)
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
	err := run()
	if c.Exited != nil && cmd.ProcessState != nil {
		c.Exited(cmd.ProcessState)
	}
	if err != nil && Debug {
		fmt.Fprintf(DebugOut, "[debug] exec: %s: exit %d\n", shellWords(append([]string{path}, c.Args...)), ExitCode(err))
	}
	return err
}

// ChildEnv is the environment the default Runner gives the child of c: nil
// (inherit the process environment as it is) when c has no Env and no
// Unset; else os.Environ() without each entry whose name is in c.Unset,
// then c.Env. A fake Runner can call it to see what the child would get.
func ChildEnv(c Cmd) []string {
	if len(c.Env) == 0 && len(c.Unset) == 0 {
		return nil
	}
	env := os.Environ()
	if len(c.Unset) > 0 {
		kept := make([]string, 0, len(env))
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			if !slices.Contains(c.Unset, name) {
				kept = append(kept, kv)
			}
		}
		env = kept
	}
	return append(env, c.Env...)
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

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
	"sync/atomic"
	"syscall"
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
	// default: the parent waits for the child to end.
	WaitDelay time.Duration
	// ProcessGroup starts the child as the leader of a new process group,
	// so that a stop also reaches the children of the child (the exec
	// credential plugin of a kubectl). On a cancelled context the group
	// gets SIGTERM and SIGCONT, not the SIGINT of the child alone. When
	// WaitDelay has passed after that signal, the group gets SIGKILL, and
	// Run then waits up to 1 s more for the group to be gone. The group is
	// not the foreground group of a terminal: a member that reads the
	// terminal stops there until the stop. False keeps the default: the
	// child stays in the process group of lo.
	ProcessGroup bool
	// Exited, when set, gets the state of the child after it ended (not
	// when it did not start). After the context ends, Run returns the
	// error of the context also for a child that ended with 0
	// (exec.Cmd.Cancel), and Start returns the same error when the child
	// did not start: the state tells the two apart.
	Exited func(*os.ProcessState)
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
	// the bash entrypoint, which waited for it. No WaitDelay unless the
	// Cmd sets one: like bash, the parent waits for the child to end. A
	// Cmd with a WaitDelay gets a SIGKILL after it. On SIGTERM this is a
	// deliberate deviation: the bash shell died and left the child
	// running, the binary interrupts it and waits (catalogue D26).
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	// A ProcessGroup child is not in the foreground process group: a
	// Ctrl-C reaches lo alone, and its interrupt watch cancels the context.
	// The cancel then stops the whole group of the child (stopGroup).
	var stopped atomic.Int64 // the time of the group SIGTERM (ProcessGroup)
	if c.ProcessGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			stopped.Store(time.Now().UnixNano())
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGCONT)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
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
	err := cmd.Run()
	if t := stopped.Load(); t != 0 && cmd.Process != nil {
		stopGroup(cmd.Process.Pid, time.Unix(0, t).Add(c.WaitDelay))
	}
	if c.Exited != nil && cmd.ProcessState != nil {
		c.Exited(cmd.ProcessState)
	}
	if err != nil && Debug {
		fmt.Fprintf(DebugOut, "[debug] exec: %s: exit %d\n", shellWords(append([]string{path}, c.Args...)), ExitCode(err))
	}
	return err
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

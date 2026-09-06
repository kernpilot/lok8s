package execx

import (
	"context"
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
			return fmt.Errorf("%s: executable not found", c.Name)
		}
		path = resolved
	}
	cmd := exec.CommandContext(ctx, path, c.Args...)
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
	return cmd.Run()
}

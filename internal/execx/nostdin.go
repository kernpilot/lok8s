package execx

import (
	"context"
	"io"
	"strings"
)

// WithoutStdin wraps r for a command that prompts: a child with no Stdin
// gets an empty one instead of the process stdin. The prompt reads the
// process stdin, and a child that inherits it can read the answer first: an
// ssh probe without -n reads its stdin to the end, so a piped `yes` never
// reaches the prompt. A child that must read the terminal sets Stdin
// itself (see TerminalOrEmpty).
func WithoutStdin(r Runner) Runner {
	if _, ok := r.(noStdinRunner); ok {
		return r
	}
	return noStdinRunner{r}
}

type noStdinRunner struct{ next Runner }

func (n noStdinRunner) Run(ctx context.Context, c Cmd) error {
	if c.Stdin == nil {
		c.Stdin = strings.NewReader("")
	}
	return n.next.Run(ctx, c)
}

// TerminalOrEmpty is the stdin for a child that may ask the operator
// something: in when it is a terminal, else an empty reader. Piped input
// belongs to the prompt of the command, never to its children.
func TerminalOrEmpty(in io.Reader, isTerminal bool) io.Reader {
	if isTerminal && in != nil {
		return in
	}
	return strings.NewReader("")
}

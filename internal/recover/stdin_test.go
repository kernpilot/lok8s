package recover

// stdin_test.go: the consent prompt reads the process stdin, and no child
// reads it first. On the kubehz.in.net recreate (2026-10-23) a piped `yes`
// never reached the prompt: the provider children inherited stdin (execx:
// a nil Stdin is the process stdin), and the ssh probes of provider::doctor
// read it to the end.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

// drainRunner stands in for the bash children of recover. Like the real
// runner, a child with a nil Stdin reads the process stdin. Every child
// reads its stdin to the end, like an ssh probe without -n, and records
// what it read and whether it got the prompt's own reader.
type drainRunner struct {
	mu      sync.Mutex
	prompt  io.Reader // the Runner's In, to tell it apart
	read    []string  // "<child>: <bytes read>"
	gotIn   []string  // the children that got In itself
	ran     []string
	nodeOut string
}

func (d *drainRunner) Run(ctx context.Context, c execx.Cmd) error {
	child := c.Name
	if c.Name == "bash" && len(c.Args) > 2 {
		child = c.Args[2] // lo-recover-provider / lo-recover-provision
		if c.Args[2] == "lo-recover-provider" && len(c.Args) > 4 {
			child = c.Args[4] // the contract function
		}
		if c.Args[1] == providerProbeScript {
			child = "probe"
		}
	}
	in := c.Stdin
	if in == nil {
		in = os.Stdin
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ran = append(d.ran, child)
	if c.Stdin != nil && c.Stdin == d.prompt {
		d.gotIn = append(d.gotIn, child)
	} else if b, _ := io.ReadAll(in); len(b) > 0 {
		d.read = append(d.read, fmt.Sprintf("%s: %q", child, b))
	}
	switch child {
	case "probe":
		fmt.Fprint(c.Stdout, "rebuild\ndoctor\n")
	case "provider::doctor":
		fmt.Fprint(c.Stdout, "ok\tssh reachable on 3 nodes\n")
	case "provider::output":
		fmt.Fprint(c.Stdout, `{"nodes":[{},{},{}]}`)
	case "kubectl":
		fmt.Fprint(c.Stdout, d.nodeOut)
	}
	return nil
}

// stdinHarness runs the real bash bridge (probe, doctor, output, rebuild,
// provision) over drainRunner.
func stdinHarness(t *testing.T) (*harness, *drainRunner) {
	t.Helper()
	h := newHarness(t)
	d := &drainRunner{nodeOut: "cp1  Ready  control-plane  1d  v1.31.0\ncp2  Ready  control-plane  1d  v1.31.0\nw1  Ready  <none>  1d  v1.31.0\n"}
	h.r.Exec = d
	h.r.Provision = nil // the bash bridge: provision::dispatch in a child
	h.r.LoadProvider = func(ctx context.Context, spec string) (string, string, func(), Provider, error) {
		prov, err := h.r.bashProvider(ctx, "hetzner")
		return "hetzner", h.cfg, nil, prov, err
	}
	return h, d
}

// pipeStdin makes os.Stdin a pipe that holds input, like `yes | lo recover`.
func pipeStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

// A piped answer reaches the prompt: no child reads the pipe, before or
// after the consent, and recover runs every phase.
func TestPipedConsentReachesThePromptPastTheChildren(t *testing.T) {
	h, d := stdinHarness(t)
	pipeStdin(t, "yes\nyes\nyes\n")
	h.r.In = nil // the process stdin, as `lo recover` gets it from cobra
	if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatalf("Run: %v\n%s", err, h.all())
	}
	if strings.Contains(h.all(), "aborted by operator") {
		t.Fatalf("the piped yes did not reach the prompt:\n%s", h.all())
	}
	if len(d.read) > 0 {
		t.Errorf("a child read the prompt's input: %v", d.read)
	}
	want := "probe provider::doctor provider::output provider::rebuild lo-recover-provision provider::output kubectl"
	if got := strings.Join(d.ran, " "); got != want {
		t.Errorf("children = %q, want %q", got, want)
	}
}

// On a terminal, the rebuild and the provision (after the consent) get the
// terminal, so they can still ask the operator something. No child before
// the consent gets it.
func TestOnlyTheChildrenAfterTheConsentGetTheTerminal(t *testing.T) {
	h, d := stdinHarness(t)
	in := strings.NewReader("yes\n")
	h.r.In = in
	d.prompt = in
	h.r.InIsTerminal = func() bool { return true }
	if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatalf("Run: %v\n%s", err, h.all())
	}
	if got := strings.Join(d.gotIn, " "); got != "provider::rebuild lo-recover-provision" {
		t.Errorf("children with the terminal = %q, want the rebuild and the provision", got)
	}
	if len(d.read) > 0 {
		t.Errorf("a child read the prompt's input: %v", d.read)
	}

	// Not a terminal: no child gets In at all.
	h2, d2 := stdinHarness(t)
	in2 := strings.NewReader("yes\n")
	h2.r.In, d2.prompt = in2, in2
	h2.r.InIsTerminal = func() bool { return false }
	if err := h2.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatalf("Run: %v\n%s", err, h2.all())
	}
	if len(d2.gotIn) > 0 || len(d2.read) > 0 {
		t.Errorf("off a terminal a child got the input: got In %v, read %v", d2.gotIn, d2.read)
	}
}

func TestWithoutStdinGivesAChildAnEmptyStdin(t *testing.T) {
	var got []io.Reader
	rec := runnerFunc(func(ctx context.Context, c execx.Cmd) error { got = append(got, c.Stdin); return nil })
	own := strings.NewReader("manifest")
	r := execx.WithoutStdin(execx.WithoutStdin(rec))
	_ = r.Run(t.Context(), execx.Cmd{Name: "x"})
	_ = r.Run(t.Context(), execx.Cmd{Name: "y", Stdin: own})
	if got[0] == nil {
		t.Fatal("a child with no Stdin got the process stdin")
	}
	if b, _ := io.ReadAll(got[0]); len(b) != 0 {
		t.Errorf("the empty stdin holds %q", b)
	}
	if got[1] != own {
		t.Error("a child's own Stdin was replaced")
	}
}

type runnerFunc func(ctx context.Context, c execx.Cmd) error

func (f runnerFunc) Run(ctx context.Context, c execx.Cmd) error { return f(ctx, c) }

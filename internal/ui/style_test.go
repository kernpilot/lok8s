package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// tty renders f into a buffer in the terminal form (colour on) and
// returns the bytes. Plain renders the piped form.
func tty(t *testing.T, f func(w *bytes.Buffer)) string {
	t.Helper()
	defer ForceTTY(true)()
	defer ForceColor(true)()
	var b bytes.Buffer
	f(&b)
	return b.String()
}

func plain(t *testing.T, f func(w *bytes.Buffer)) string {
	t.Helper()
	var b bytes.Buffer
	f(&b)
	return b.String()
}

func TestPipedIsPlain(t *testing.T) {
	// A buffer is not a terminal: no decoration change, no escapes.
	cases := map[string]struct {
		f    func(w *bytes.Buffer)
		want string
	}{
		"title":       {func(w *bytes.Buffer) { Title(w, "=== lok8s doctor ===") }, "=== lok8s doctor ===\n"},
		"section":     {func(w *bytes.Buffer) { Section(w, "tools") }, "--- tools ---\n"},
		"ok":          {func(w *bytes.Buffer) { MarkOK(w, "bash 5.3") }, "  ✓ bash 5.3\n"},
		"warn":        {func(w *bytes.Buffer) { MarkWarn(w, "x") }, "  ! x\n"},
		"bad":         {func(w *bytes.Buffer) { MarkBad(w, "x") }, "  ✗ x\n"},
		"next":        {func(w *bytes.Buffer) { Next(w, "toolchain doctor", "why") }, "next: lo toolchain doctor   # why\n"},
		"next-no-why": {func(w *bytes.Buffer) { Next(w, "init", "") }, "next: lo init\n"},
		"error":       {func(w *bytes.Buffer) { ErrorTo(w, "boom %d", 1) }, "[error] boom 1\n"},
		"warnpfx":     {func(w *bytes.Buffer) { WarnTo(w, "careful") }, "[warn] careful\n"},
		"raw-err":     {func(w *bytes.Buffer) { RawErrorTo(w, "invalid domain name: %s", "x") }, "error: invalid domain name: x\n"},
		"raw-warn":    {func(w *bytes.Buffer) { RawWarningTo(w, "ignoring") }, "warning: ignoring\n"},
	}
	for name, c := range cases {
		got := plain(t, c.f)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
		if strings.Contains(got, "\033") {
			t.Errorf("%s: escape sequence in piped output: %q", name, got)
		}
	}
}

func TestTerminalIsStyled(t *testing.T) {
	cases := map[string]struct {
		f    func(w *bytes.Buffer)
		want string
	}{
		"title":    {func(w *bytes.Buffer) { Title(w, "=== Domain: x.dev ===") }, "\033[1mDomain: x.dev\033[0m\n"},
		"section":  {func(w *bytes.Buffer) { Section(w, "tools") }, "\033[1mtools\033[0m\n"},
		"ok":       {func(w *bytes.Buffer) { MarkOK(w, "bash 5.3") }, "  \033[32m✓\033[0m bash 5.3\n"},
		"warn":     {func(w *bytes.Buffer) { MarkWarn(w, "x") }, "  \033[33m!\033[0m x\n"},
		"bad":      {func(w *bytes.Buffer) { MarkBad(w, "x") }, "  \033[31m✗\033[0m x\n"},
		"next":     {func(w *bytes.Buffer) { Next(w, "init", "why") }, "\033[2mnext: lo init   # why\033[0m\n"},
		"error":    {func(w *bytes.Buffer) { ErrorTo(w, "boom") }, "\033[31m[error]\033[0m boom\n"},
		"warnpfx":  {func(w *bytes.Buffer) { WarnTo(w, "careful") }, "\033[33m[warn]\033[0m careful\n"},
		"raw-err":  {func(w *bytes.Buffer) { RawErrorTo(w, "x") }, "\033[31merror:\033[0m x\n"},
		"raw-warn": {func(w *bytes.Buffer) { RawWarningTo(w, "x") }, "\033[33mwarning:\033[0m x\n"},
	}
	for name, c := range cases {
		if got := tty(t, c.f); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestDebugPrefixNeedsDEBUG(t *testing.T) {
	t.Setenv("DEBUG", "")
	if got := plain(t, func(w *bytes.Buffer) { DebugTo(w, "x") }); got != "" {
		t.Errorf("DEBUG unset printed %q", got)
	}
	t.Setenv("DEBUG", "1")
	if got := plain(t, func(w *bytes.Buffer) { DebugTo(w, "x") }); got != "[debug] x\n" {
		t.Errorf("piped debug: %q", got)
	}
	if got := tty(t, func(w *bytes.Buffer) { DebugTo(w, "x") }); got != "\033[32m[debug]\033[0m x\n" {
		t.Errorf("tty debug: %q", got)
	}
}

// A terminal without colour (NO_COLOR, --no-color) keeps the terminal
// shape (no === / --- decoration) and drops every escape.
func TestTerminalWithoutColor(t *testing.T) {
	defer ForceTTY(true)()
	t.Setenv("NO_COLOR", "1")
	var b bytes.Buffer
	Title(&b, "=== lok8s doctor ===")
	Section(&b, "tools")
	MarkOK(&b, "yq")
	ErrorTo(&b, "boom")
	want := "lok8s doctor\ntools\n  ✓ yq\n[error] boom\n"
	if b.String() != want {
		t.Errorf("NO_COLOR: got %q, want %q", b.String(), want)
	}

	os.Unsetenv("NO_COLOR")
	SetNoColor(true)
	t.Cleanup(func() { SetNoColor(false); os.Unsetenv("NO_COLOR") })
	if os.Getenv("NO_COLOR") != "1" {
		t.Error("--no-color did not export NO_COLOR for the children")
	}
	b.Reset()
	MarkBad(&b, "x")
	if b.String() != "  ✗ x\n" {
		t.Errorf("--no-color: got %q", b.String())
	}
	if s := For(&b); !s.TTY || s.Color {
		t.Errorf("--no-color style: %+v", s)
	}
}

func TestStyledWriterCarriesItsStyle(t *testing.T) {
	var b bytes.Buffer
	w := Styled(&b, Style{TTY: true, Color: true})
	if s := For(w); !s.TTY || !s.Color {
		t.Errorf("styled: %+v", s)
	}
	Section(w, "x")
	if b.String() != "\033[1mx\033[0m\n" {
		t.Errorf("styled section: %q", b.String())
	}
	// A plain buffer next to it stays plain.
	b.Reset()
	Section(&b, "x")
	if b.String() != "--- x ---\n" {
		t.Errorf("buffer section: %q", b.String())
	}
	// The whole style is honoured: a terminal without colour never
	// colours, even under the colour override, and NO_COLOR gates a
	// coloured wrapper like every stream.
	defer ForceColor(true)()
	b.Reset()
	Section(Styled(&b, Style{TTY: true, Color: false}), "x")
	MarkOK(Styled(&b, Style{TTY: true, Color: false}), "y")
	if b.String() != "x\n  ✓ y\n" {
		t.Errorf("styled without colour: %q", b.String())
	}
	t.Setenv("NO_COLOR", "1")
	if s := For(Styled(&b, Style{TTY: true, Color: true})); !s.TTY || s.Color {
		t.Errorf("NO_COLOR on a styled writer: %+v", s)
	}
	if s := For(Styled(&b, Style{TTY: false, Color: true})); s.TTY || s.Color {
		t.Errorf("a piped styled writer never colours: %+v", s)
	}
}

// no-color.org: NO_COLOR disables colour only when it is present and
// non-empty. An empty value keeps the colour on. The colour override is
// test only and applies on a terminal only.
func TestNoColorEmptyKeepsColor(t *testing.T) {
	restoreTTY := ForceTTY(true)
	t.Setenv("NO_COLOR", "")
	var b bytes.Buffer
	MarkOK(&b, "x")
	if b.String() != "  \033[32m✓\033[0m x\n" {
		t.Errorf("empty NO_COLOR turned colour off: %q", b.String())
	}
	t.Setenv("NO_COLOR", "0")
	b.Reset()
	MarkOK(&b, "x")
	if b.String() != "  ✓ x\n" {
		t.Errorf("NO_COLOR=0 kept colour: %q", b.String())
	}
	restoreTTY()
	// ForceColor(true) off a terminal changes nothing.
	defer ForceTTY(false)()
	defer ForceColor(true)()
	t.Setenv("NO_COLOR", "")
	b.Reset()
	MarkOK(&b, "x")
	if b.String() != "  ✓ x\n" {
		t.Errorf("ForceColor coloured a pipe: %q", b.String())
	}
}

func TestForceColorOffKeepsTTYShape(t *testing.T) {
	defer ForceTTY(true)()
	defer ForceColor(false)()
	var b bytes.Buffer
	Title(&b, "=== x ===")
	MarkOK(&b, "y")
	if b.String() != "x\n  ✓ y\n" {
		t.Errorf("got %q", b.String())
	}
}

func TestForResolvesProcessStreams(t *testing.T) {
	// Under go test neither stream is a terminal.
	if For(os.Stdout).TTY || For(os.Stderr).TTY {
		t.Skip("go test on a terminal")
	}
	if Stdout().Color || Stderr().Color {
		t.Error("colour off a terminal")
	}
}

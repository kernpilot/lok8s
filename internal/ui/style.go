package ui

// style.go is the presentation layer. Piped output (a stream that is not a
// terminal) is the CONTRACT: byte for byte what the bash implementation
// prints, which the parity harnesses diff through pipes. Terminal output
// is PRESENTATION: one house style, colour only on a TTY, and the
// NO_COLOR convention (https://no-color.org) plus the global --no-color
// flag honoured. Each stream is gated on its own: `lo doctor >out.txt`
// prints a plain report and coloured [error] lines on the terminal.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Style is what a stream can show.
type Style struct {
	// TTY is true when the stream is a terminal: titles and sections drop
	// their === / --- decoration, tables are measured, hints are dimmed.
	TTY bool
	// Color is true when ANSI colour is allowed: a TTY, and neither
	// NO_COLOR nor --no-color is set. Never true off a TTY.
	Color bool
}

// The palette: one accent (the doctor green), warn, bad, dim and bold.
// The bash prefixes used "\033[0;31m" and the doctor markers "\033[31m", the
// same colour, so the terminal rendering unifies on the short form.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
)

// Paint wraps text in code and a reset when the style allows colour.
func (s Style) Paint(code, text string) string {
	if !s.Color {
		return text
	}
	return code + text + ansiReset
}

// Bold and Dim are the two weights of the palette.
func (s Style) Bold(text string) string { return s.Paint(ansiBold, text) }
func (s Style) Dim(text string) string  { return s.Paint(ansiDim, text) }

var (
	styleMu     sync.Mutex
	noColorFlag bool
	// forceTTY and forceColor are the test overrides (ForceTTY, ForceColor).
	forceTTY, forceColor   *bool
	stdoutOnce, stderrOnce sync.Once
	stdoutTTY, stderrTTY   bool
)

// SetNoColor records the global --no-color flag (the env form is NO_COLOR).
// It also exports NO_COLOR=1 so every child (the provider plugins, a
// routed bash command) sees the same choice.
func SetNoColor(on bool) {
	styleMu.Lock()
	noColorFlag = on
	styleMu.Unlock()
	if on {
		os.Setenv("NO_COLOR", "1")
	}
}

// ForceTTY is a test-only override of the terminal detection: every
// writer resolves as a terminal (true) or as a pipe (false), so a
// bytes.Buffer renders the terminal form under go test without a pty.
// The returned function restores the previous state. Production code
// never calls it.
func ForceTTY(on bool) (restore func()) {
	styleMu.Lock()
	prev := forceTTY
	forceTTY = &on
	styleMu.Unlock()
	return func() { styleMu.Lock(); forceTTY = prev; styleMu.Unlock() }
}

// ForceColor is a test-only override of the colour decision. It applies
// only where TTY is true: ForceColor(true) with ForceTTY(true) renders
// the coloured terminal form into a buffer, ForceColor(false) keeps a
// forced terminal plain. Off a terminal the output stays plain whatever
// the override says. Production code never calls it.
func ForceColor(on bool) (restore func()) {
	styleMu.Lock()
	prev := forceColor
	forceColor = &on
	styleMu.Unlock()
	return func() { styleMu.Lock(); forceColor = prev; styleMu.Unlock() }
}

// colorAllowed is the NO_COLOR / --no-color gate (no lock: callers hold it).
func colorAllowed() bool {
	if noColorFlag {
		return false
	}
	// no-color.org: NO_COLOR disables colour when it is present AND
	// non-empty. An empty value keeps colour on.
	return os.Getenv("NO_COLOR") == ""
}

// resolve turns a TTY fact into a Style under the overrides and gates.
func resolve(tty bool) Style {
	styleMu.Lock()
	defer styleMu.Unlock()
	if forceTTY != nil {
		tty = *forceTTY
	}
	color := tty && colorAllowed()
	if forceColor != nil {
		color = tty && *forceColor
	}
	return Style{TTY: tty, Color: color}
}

// Stdout is the style of the process's stdout.
func Stdout() Style {
	stdoutOnce.Do(func() { stdoutTTY = isTerminal(os.Stdout) })
	return resolve(stdoutTTY)
}

// Stderr is the style of the process's stderr.
func Stderr() Style {
	stderrOnce.Do(func() { stderrTTY = isTerminal(os.Stderr) })
	return resolve(stderrTTY)
}

// StdinIsTerminal reports whether stdin is a terminal (a prompt or a
// select needs both stdin and stdout on one).
func StdinIsTerminal() bool {
	styleMu.Lock()
	forced := forceTTY
	styleMu.Unlock()
	if forced != nil {
		return *forced
	}
	return isTerminal(os.Stdin)
}

func isTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// Styler is a writer that carries its own Style. Styled makes one. It is
// the per-writer test seam next to the process-wide ForceTTY/ForceColor:
// a test renders one buffer as a terminal while another stays a pipe.
type Styler interface {
	Style() Style
}

type styledWriter struct {
	io.Writer
	style Style
}

func (s styledWriter) Style() Style { return s.style }

// Styled wraps w with a fixed style. For honours the whole style: a
// Styled(w, Style{TTY: true, Color: false}) writer never carries colour,
// whatever the overrides say.
func Styled(w io.Writer, s Style) io.Writer { return styledWriter{Writer: w, style: s} }

// For is the style of w: a Styled wrapper's own (its Color gated by
// NO_COLOR and --no-color like every stream), the process stdout's or
// stderr's for those files, plain for anything else (a buffer, a pipe, a
// file). Under the test overrides every other writer gets the forced
// style.
func For(w io.Writer) Style {
	if s, ok := w.(Styler); ok {
		own := s.Style()
		styleMu.Lock()
		defer styleMu.Unlock()
		return Style{TTY: own.TTY, Color: own.TTY && own.Color && colorAllowed()}
	}
	switch w {
	case io.Writer(os.Stdout):
		return Stdout()
	case io.Writer(os.Stderr):
		return Stderr()
	}
	return resolve(false)
}

// Title writes a title line. Piped it is exactly text (the callers keep
// their literal `=== … ===`). On a TTY the `=== ` decoration goes and the
// text is bold.
func Title(w io.Writer, text string) {
	s := For(w)
	if !s.TTY {
		io.WriteString(w, text+"\n")
		return
	}
	io.WriteString(w, s.Bold(strings.TrimSpace(strings.Trim(text, "=")))+"\n")
}

// Section writes a section header: `--- label ---` piped, the bold label
// on a TTY.
func Section(w io.Writer, label string) {
	s := For(w)
	if !s.TTY {
		io.WriteString(w, "--- "+label+" ---\n")
		return
	}
	io.WriteString(w, s.Bold(label)+"\n")
}

// The marker set: ✓ ! ✗ ℹ at a two-space indent, coloured on a TTY.
const (
	markOK   = "✓"
	markWarn = "!"
	markBad  = "✗"
	markInfo = "ℹ"
)

func marker(w io.Writer, code, mark, msg string) {
	io.WriteString(w, "  "+For(w).Paint(code, mark)+" "+msg+"\n")
}

// MarkOK writes `  ✓ msg` (green on a TTY).
func MarkOK(w io.Writer, msg string) { marker(w, ansiGreen, markOK, msg) }

// MarkWarn writes `  ! msg` (yellow on a TTY).
func MarkWarn(w io.Writer, msg string) { marker(w, ansiYellow, markWarn, msg) }

// MarkBad writes `  ✗ msg` (red on a TTY).
func MarkBad(w io.Writer, msg string) { marker(w, ansiRed, markBad, msg) }

// MarkInfo writes `  ℹ msg`, a neutral fact that is neither a pass nor a
// finding. The whole line is dim on a TTY (the style the lifecycle
// commands use for their notes).
func MarkInfo(w io.Writer, msg string) {
	io.WriteString(w, "  "+For(w).Dim(markInfo+" "+msg)+"\n")
}

// Next writes the one hint shape: `next: lo <cmd>   # why`, dim on a TTY.
func Next(w io.Writer, cmd, why string) {
	line := "next: lo " + cmd
	if why != "" {
		line += "   # " + why
	}
	io.WriteString(w, For(w).Dim(line)+"\n")
}

// RawErrorTo writes the raw `error: …` line the bash `echo "error: …" >&2`
// family prints (no [error] prefix). The word is red on a TTY.
func RawErrorTo(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, For(w).Paint(ansiRed, "error:")+" "+format+"\n", a...)
}

// RawWarningTo writes the raw `warning: …` line. The word is yellow on a
// TTY.
func RawWarningTo(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, For(w).Paint(ansiYellow, "warning:")+" "+format+"\n", a...)
}

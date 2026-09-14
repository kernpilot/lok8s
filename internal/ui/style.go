package ui

// style.go is the presentation layer. Piped output (a stream that is not a
// terminal) is the CONTRACT: byte for byte what the bash implementation
// prints, which the parity harnesses diff through pipes. Terminal output
// is PRESENTATION: one house style, colour only on a TTY, and the
// NO_COLOR convention (https://no-color.org) plus the global --no-color
// flag honoured. Each stream is gated on its own: `lo doctor >out.txt`
// prints a plain report and coloured [error] lines on the terminal.

import (
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

// Plain is the piped style: no decoration, no colour.
var Plain = Style{}

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

// Bold, Dim, Accent, Warn and Bad are the palette on a style.
func (s Style) Bold(text string) string   { return s.Paint(ansiBold, text) }
func (s Style) Dim(text string) string    { return s.Paint(ansiDim, text) }
func (s Style) Accent(text string) string { return s.Paint(ansiGreen, text) }
func (s Style) WarnC(text string) string  { return s.Paint(ansiYellow, text) }
func (s Style) BadC(text string) string   { return s.Paint(ansiRed, text) }

var (
	styleMu     sync.Mutex
	noColorFlag bool
	// forceTTY / forceColor are the test overrides: set, every writer
	// resolves to the forced style, so a bytes.Buffer renders the terminal
	// form under go test without a pty.
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

// ForceTTY overrides the terminal detection for every writer (tests). The
// returned function restores the previous state.
func ForceTTY(on bool) (restore func()) {
	styleMu.Lock()
	prev := forceTTY
	forceTTY = &on
	styleMu.Unlock()
	return func() { styleMu.Lock(); forceTTY = prev; styleMu.Unlock() }
}

// ForceColor overrides the colour decision for every writer (tests): true
// colours even a buffer, false keeps a forced TTY plain. It implies
// nothing about TTY. Pair it with ForceTTY for the coloured terminal form.
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
	// no-color.org: any non-empty value disables colour.
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
		color = *forceColor
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

// Styler is a writer that carries its own Style (Styled wraps one).
type Styler interface {
	Style() Style
}

type styledWriter struct {
	io.Writer
	style Style
}

func (s styledWriter) Style() Style { return s.style }

// Styled wraps w with a fixed style: a test renders the terminal form into
// a buffer through it, and a command that already resolved a stream hands
// it on.
func Styled(w io.Writer, s Style) io.Writer { return styledWriter{Writer: w, style: s} }

// For is the style of w: a Styled wrapper's own, the process stdout's or
// stderr's for those files, Plain for anything else (a buffer, a pipe, a
// file). Under the test overrides every writer gets the forced style.
func For(w io.Writer) Style {
	if s, ok := w.(Styler); ok {
		return resolve(s.Style().TTY)
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

// The marker set: ✓ ! ✗ · at a two-space indent, coloured on a TTY.
const (
	markOK   = "✓"
	markWarn = "!"
	markBad  = "✗"
	markInfo = "·"
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

// MarkInfo writes `  · msg` (dim on a TTY).
func MarkInfo(w io.Writer, msg string) { marker(w, ansiDim, markInfo, msg) }

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
	fprintf(w, For(w).BadC("error:")+" "+format+"\n", a...)
}

// RawWarningTo writes the raw `warning: …` line. The word is yellow on a
// TTY.
func RawWarningTo(w io.Writer, format string, a ...any) {
	fprintf(w, For(w).WarnC("warning:")+" "+format+"\n", a...)
}

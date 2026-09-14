// Package ui carries the CLI's output conventions: the [error] / [warn] /
// [debug] prefixes of the bash implementation's verbose.sh, byte for byte
// off a terminal so ported commands stay indistinguishable from their
// argsh originals, and the terminal presentation layer (style.go) on top.
package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrHandled marks an error whose message was already printed in the bash
// implementation's own format ([error] … on stderr). It is the ONE sentinel:
// every package's name for it (cli.ErrHandled, kubehz.ErrHandled,
// secrets.ErrHandled, …) is this value, so errors.Is holds across package
// boundaries and a %w wrap anywhere still reads as handled. The caller
// exits non-zero without printing anything further.
var ErrHandled = errors.New("handled")

// Handled marks err as already printed: the returned error reads as
// ErrHandled to errors.Is, keeps err's text, and unwraps to err so any
// sentinel or type inside it still matches. A site that prints its own
// [error] line and then returns an error wraps it here, and the exit
// mapping at the top (cli dispatchExit) prints nothing more. An error
// that reaches the top WITHOUT this mark was never printed, so the mapping
// prints it. nil stays nil.
func Handled(err error) error {
	if err == nil {
		return nil
	}
	return &handledError{err: err}
}

type handledError struct{ err error }

func (e *handledError) Error() string { return e.err.Error() }

func (e *handledError) Unwrap() error { return e.err }

// Is answers errors.Is(err, ErrHandled) for the mark itself; everything
// else is answered through Unwrap.
func (e *handledError) Is(target error) bool { return target == ErrHandled }

// prefix writes `[tag] line`: the tag coloured when w is a terminal that
// allows colour, plain otherwise (the bash verbose.sh gates on `-t 2` the
// same way, so piped stderr matches byte for byte).
func prefix(w io.Writer, code, tag, format string, a ...any) {
	fmt.Fprintf(w, For(w).Paint(code, tag)+" "+format+"\n", a...)
}
const (
	green  = "\033[0;32m"
	red    = "\033[0;31m"
	yellow = "\033[0;33m"
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	// The doctor's `!` colour: the plain SGR colour, no intensity reset,
	// so a card row can carry it inside a dim or bold run.
	markYellow = "\033[33m"
)

// Paint applies the CLI's SGR styles when true and returns the text
// unchanged when false (stdout is not a terminal: --plan in a pipe, CI,
// a test). The card of `lo init` and the run header share these styles.
type Paint bool

// Bold is the emphasis of a heading (the project name of a card).
func (p Paint) Bold(s string) string { return p.wrap(bold, s) }

// Dim is the muted run (a key column, an equivalent command line).
func (p Paint) Dim(s string) string { return p.wrap(dim, s) }

// Yellow is the doctor's `!` colour (a row the user can act on).
func (p Paint) Yellow(s string) string { return p.wrap(markYellow, s) }

func (p Paint) wrap(code, s string) string {
	if !p || s == "" {
		return s
	}
	return code + s + reset
}

// Debug writes a [debug] line to stderr when DEBUG is set (bash: debug()).
func Debug(format string, a ...any) {
	DebugTo(os.Stderr, format, a...)
}

// DebugTo writes a [debug] line to w when DEBUG is set.
func DebugTo(w io.Writer, format string, a ...any) {
	if os.Getenv("DEBUG") == "" {
		return
	}
	prefix(w, ansiGreen, "[debug]", format, a...)
}

// Error writes an [error] line to stderr (bash: error()).
func Error(format string, a ...any) {
	ErrorTo(os.Stderr, format, a...)
}

// ErrorTo writes an [error] line to w.
func ErrorTo(w io.Writer, format string, a ...any) {
	prefix(w, ansiRed, "[error]", format, a...)
}

// Warn writes a [warn] line to stderr (bash: warn()).
func Warn(format string, a ...any) {
	WarnTo(os.Stderr, format, a...)
}

// WarnTo writes a [warn] line to w.
func WarnTo(w io.Writer, format string, a ...any) {
	prefix(w, ansiYellow, "[warn]", format, a...)
}

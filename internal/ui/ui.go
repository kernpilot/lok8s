// Package ui carries the CLI's output conventions, matching the bash
// implementation's verbose.sh helpers byte for byte so ported commands stay
// indistinguishable from their argsh originals.
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
// secrets.ErrPrinted, …) is this value, so errors.Is holds across package
// boundaries and a %w wrap anywhere still reads as handled. The caller
// exits non-zero without printing anything further.
var ErrHandled = errors.New("handled")

const (
	green  = "\033[0;32m"
	red    = "\033[0;31m"
	yellow = "\033[0;33m"
	reset  = "\033[0m"
)

// Debug writes a [debug] line to stderr when DEBUG is set (bash: debug()).
func Debug(format string, a ...any) {
	if os.Getenv("DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, green+"[debug]"+reset+" "+format+"\n", a...)
}

// Debugf writes a [debug] line to w when DEBUG is set.
func Debugf(w io.Writer, format string, a ...any) {
	if os.Getenv("DEBUG") == "" {
		return
	}
	fmt.Fprintf(w, green+"[debug]"+reset+" "+format+"\n", a...)
}

// Error writes an [error] line to stderr (bash: error()).
func Error(format string, a ...any) {
	Errorf(os.Stderr, format, a...)
}

// Errorf writes an [error] line to w.
func Errorf(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, red+"[error]"+reset+" "+format+"\n", a...)
}

// Warn writes a [warn] line to stderr (bash: warn()).
func Warn(format string, a ...any) {
	Warnf(os.Stderr, format, a...)
}

// Warnf writes a [warn] line to w.
func Warnf(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, yellow+"[warn]"+reset+" "+format+"\n", a...)
}

package ui

// errnext.go: the one error shape for a person at a terminal:
//
//	error: <what failed>: <observed value>
//	next: lo <command>   # why
//
// Off a terminal the exact legacy line prints instead ([error] … in the
// bash implementation's format). Scripts and the parity harnesses match
// on that line. A site that has a next step calls ErrorNext. A site
// without one keeps ErrorTo.

import "io"

// ErrorNext writes the error for w. On a terminal (the style of w, see
// For) the two-line shape prints: the `error:` line, then the Next line
// for cmd (without `lo`) and why. Otherwise the [error] line prints, byte
// for byte what ErrorTo prints. An empty cmd prints no second line.
func ErrorNext(w io.Writer, cmd, why, format string, a ...any) {
	if !For(w).TTY {
		ErrorTo(w, format, a...)
		return
	}
	RawErrorTo(w, format, a...)
	if cmd != "" {
		Next(w, cmd, why)
	}
}

package ui

// quiet.go: the --quiet level. Quiet silences the informational lines a
// run prints on stderr (the [assets] eject notices, the DOMAIN_NAME
// notice). Errors and warnings still print, and command output on stdout
// is never touched. Set once from the root flag. No environment variable
// reads it.

import (
	"fmt"
	"io"
)

var quiet bool

// SetQuiet sets the quiet level for the process.
func SetQuiet(q bool) { quiet = q }

// Quiet reports the quiet level.
func Quiet() bool { return quiet }

// NoticeTo writes an informational line to w, or nothing under --quiet.
func NoticeTo(w io.Writer, format string, a ...any) {
	if quiet {
		return
	}
	fmt.Fprintf(w, format+"\n", a...)
}

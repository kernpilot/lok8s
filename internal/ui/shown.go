package ui

import "strings"

// Shown is a name from an API (a cloud part, a plan line) as a message
// shows it. Each control character (C0, DEL and C1), each backslash and
// each bidi or zero-width format character (U+200B to U+200F, U+202A to
// U+202E, U+2066 to U+2069) becomes '?', as ls shows a name on a terminal.
// A newline would split a line, an ESC would reach the terminal, and a
// bidi character would turn the text after it around. Other characters
// stay, also multibyte ones. A byte that is not UTF-8 becomes U+FFFD, as
// jq reads it. The bash twin is the jq def shown_name
// (VERBOSE_JQ_SHOWN_NAME, .lok8s/utils/verbose.sh).
func Shown(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r >= 0x7f && r < 0xa0, r == '\\',
			r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return '?'
		}
		return r
	}, s)
}

package ui

import "strings"

// Shown is a name from an API (a cloud part, a plan line) as a message
// shows it. These characters become '?', as ls shows a name on a terminal:
//   - each control character (C0, DEL and C1) and each backslash;
//   - the line and paragraph separators (U+2028, U+2029);
//   - these bidi, zero-width and invisible format characters: U+061C,
//     U+200B to U+200F, U+202A to U+202E, U+2060 to U+2064, U+2066 to
//     U+2069 and U+FEFF. The list is explicit: other invisible characters
//     (for example U+00AD or U+206A to U+206F) stay.
//
// A newline or a separator would split a line, an ESC would reach the
// terminal, a bidi character would turn the text after it around, and an
// invisible one of the list would hide in the name. Other characters stay, also
// multibyte ones. Each byte that is not UTF-8 becomes U+FFFD. jq can make
// one U+FFFD of a longer invalid sequence, so only the number of U+FFFD
// shown can differ.
// The bash twin is the jq def shown_name (VERBOSE_JQ_SHOWN_NAME,
// .lok8s/utils/verbose.sh). Each range is a case of its own.
func Shown(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r >= 0x7f && r < 0xa0, r == '\\':
			return '?'
		case r == 0x061c:
			return '?'
		case r >= 0x200b && r <= 0x200f:
			return '?'
		case r >= 0x2028 && r <= 0x2029:
			return '?'
		case r >= 0x202a && r <= 0x202e:
			return '?'
		case r >= 0x2060 && r <= 0x2064:
			return '?'
		case r >= 0x2066 && r <= 0x2069:
			return '?'
		case r == 0xfeff:
			return '?'
		}
		return r
	}, s)
}

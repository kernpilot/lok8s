package ui

import "strings"

// Unshown reports whether a message must not show r as it is. These are:
//   - each control character (C0, DEL and C1);
//   - the line and paragraph separators (U+2028, U+2029);
//   - these bidi, zero-width and invisible format characters: U+00AD,
//     U+061C, U+200B to U+200F, U+202A to U+202E, U+2060 to U+2064, U+2066
//     to U+2069, U+206A to U+206F and U+FEFF;
//   - the Hangul fillers U+115F, U+1160, U+3164 and U+FFA0;
//   - the variation selectors (U+180B to U+180F with the Mongolian vowel
//     separator, U+FE00 to U+FE0F, U+E0100 to U+E01EF) and the tag
//     characters (U+E0000 to U+E007F).
//
// The list is explicit: other invisible characters (for example U+034F or
// U+17B4) stay. A newline or a separator would split a line, an ESC would
// reach the terminal, a bidi character would turn the text after it
// around, and an invisible one of the list would hide in the name. Thus an
// emoji also loses its variation selector, as it loses a zero-width joiner
// (U+200D).
//
// Shown replaces each one, and each backslash, with '?'. The api records
// of internal/kubehz drop each one (cleanText). The bash twin is the jq
// def unshown (VERBOSE_JQ_UNSHOWN, .lok8s/utils/verbose.sh). Each range is
// a case of its own (one mutant each). testdata/unshown.json holds the runs
// of Unshown (go test ./internal/ui/ -update makes it): unshown_test.go
// checks Unshown and tests/unit/verbose_shown_test.bats checks the jq def
// against it on every code point from 0 to U+10FFFF.
func Unshown(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r < 0xa0:
		return true
	case r == 0x00ad:
		return true
	case r == 0x061c:
		return true
	case r >= 0x115f && r <= 0x1160:
		return true
	case r >= 0x180b && r <= 0x180f:
		return true
	case r >= 0x200b && r <= 0x200f:
		return true
	case r >= 0x2028 && r <= 0x2029:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2060 && r <= 0x2064:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	case r >= 0x206a && r <= 0x206f:
		return true
	case r == 0x3164:
		return true
	case r >= 0xfe00 && r <= 0xfe0f:
		return true
	case r == 0xfeff:
		return true
	case r == 0xffa0:
		return true
	case r >= 0xe0000 && r <= 0xe007f:
		return true
	case r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return false
}

// Shown is a name from an API (a cloud part, a plan line) as a message
// shows it, as ls shows a name on a terminal: each character of Unshown,
// and each backslash, becomes '?'. Other characters stay, also multibyte
// ones. Each byte that is not UTF-8 becomes U+FFFD. jq can make one U+FFFD
// of a longer invalid sequence, so only the number of U+FFFD shown can
// differ.
// The bash twin is the jq def shown_name (VERBOSE_JQ_SHOWN_NAME,
// .lok8s/utils/verbose.sh).
func Shown(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\\' || Unshown(r) {
			return '?'
		}
		return r
	}, s)
}

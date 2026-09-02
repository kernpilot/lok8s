package secrets

// bashQuote replicates bash's printf %q output (the `secrets env` contract:
// `eval "$(lo secrets env …)"` must stay injection-safe AND byte-identical to
// the argsh implementation).
//
// bash picks one of two encodings (builtins/printf.def):
//   - the string contains a non-printable character → ANSI-C $'…' quoting
//     (ansic_quote)
//   - otherwise → backslash-escaping of shell metacharacters
//     (sh_backslash_quote)
//
// The escape sets below were derived empirically against GNU bash 5.3
// (`printf %q` over all single bytes + composites) and are pinned by
// TestBashQuote — including the cross-check against a real bash when one is
// on PATH.

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// bsQuoteSet is the set of printable ASCII characters sh_backslash_quote
// escapes anywhere in the string.
const bsQuoteSet = " \t\n!\"$&'()*,;<>?[\\]^`{|}"

func bashQuote(s string) string {
	if s == "" {
		return "''"
	}
	if ansicShouldQuote(s) {
		return ansicQuote(s)
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case strings.IndexByte(bsQuoteSet, ch) >= 0:
			b.WriteByte('\\')
			b.WriteByte(ch)
		case i == 0 && (ch == '#' || ch == '~'):
			// Comment/tilde only need quoting at the start of a word.
			b.WriteByte('\\')
			b.WriteByte(ch)
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// ansicShouldQuote reports whether s contains a character bash considers
// non-printable: ASCII control bytes, DEL, invalid UTF-8, or a non-printable
// rune (bash consults the locale; a UTF-8 locale is assumed, matching the
// environments lo runs in).
func ansicShouldQuote(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return true
		}
		if r < 0x20 || r == 0x7f || (r > 0x7f && !unicode.IsPrint(r)) {
			return true
		}
		i += size
	}
	return false
}

// ansicQuote renders $'…' the way bash's ansic_quote does: named escapes for
// the common controls (\E for ESC, matching bash's output), 3-digit octal for
// the rest, backslash and single quote escaped, everything printable literal
// (including double quotes and spaces).
func ansicQuote(s string) string {
	var b strings.Builder
	b.WriteString("$'")
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			writeOctal(&b, s[i])
			i++
			continue
		}
		switch r {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1b:
			b.WriteString(`\E`)
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 || r == 0x7f || (r > 0x7f && !unicode.IsPrint(r)) {
				for j := 0; j < size; j++ {
					writeOctal(&b, s[i+j])
				}
			} else {
				b.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	b.WriteString("'")
	return b.String()
}

func writeOctal(b *strings.Builder, ch byte) {
	b.WriteByte('\\')
	b.WriteByte('0' + (ch>>6)&7)
	b.WriteByte('0' + (ch>>3)&7)
	b.WriteByte('0' + ch&7)
}

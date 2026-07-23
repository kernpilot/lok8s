package spec

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// placeholderRef is one parsed `{name<op>}` occurrence in a pattern: the field
// name and the raw operator text (everything after the name, "" when none).
type placeholderRef struct {
	name string // the field name (before any operator sigil)
	op   string // the operator text, e.g. "^^", "%%.suf", "//a/b", ":1:3" ("" = plain)
}

// operatorSigils are the characters that INTRODUCE a bash-style substitution
// operator directly after a field name inside `{...}`. A field name is the run
// of characters up to the first sigil (or the closing brace). Field names that
// literally contain one of these are not expressible with an operator — but no
// Secret data key uses them (keys are typically [A-Za-z0-9._-]).
const operatorSigils = "^,%#/:"

// parsePlaceholders extracts the ordered list of `{name<op>}` placeholders from
// a pattern, treating `{{` and `}}` as literal braces. It validates that every
// brace is part of a placeholder or an escape — an unmatched `{` or a stray `}`
// is a config error — and that each placeholder has a non-empty name and a
// well-formed operator (validated eagerly so a mistyped pattern fails at decode).
func parsePlaceholders(pattern string) ([]placeholderRef, error) {
	var refs []placeholderRef
	for i := 0; i < len(pattern); {
		c := pattern[i]
		switch c {
		case '{':
			if i+1 < len(pattern) && pattern[i+1] == '{' {
				i += 2 // literal "{"
				continue
			}
			end := strings.IndexByte(pattern[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unterminated placeholder: '{' at offset %d has no closing '}'", i)
			}
			body := pattern[i+1 : i+1+end]
			if body == "" {
				return nil, fmt.Errorf("empty placeholder {} at offset %d", i)
			}
			if strings.ContainsAny(body, "{") {
				return nil, fmt.Errorf("placeholder %q at offset %d must not contain '{'", body, i)
			}
			ref, err := splitPlaceholder(body)
			if err != nil {
				return nil, fmt.Errorf("placeholder {%s} at offset %d: %v", body, i, err)
			}
			refs = append(refs, ref)
			i += 1 + end + 1
		case '}':
			if i+1 < len(pattern) && pattern[i+1] == '}' {
				i += 2 // literal "}"
				continue
			}
			return nil, fmt.Errorf("stray '}' at offset %d (write '}}' for a literal brace)", i)
		default:
			i++
		}
	}
	return refs, nil
}

// splitPlaceholder splits a placeholder body into its field name and operator
// text (name = the run before the first operator sigil), then validates the
// operator is well-formed. The name must be non-empty.
func splitPlaceholder(body string) (placeholderRef, error) {
	cut := strings.IndexAny(body, operatorSigils)
	if cut < 0 {
		return placeholderRef{name: body}, nil
	}
	name := body[:cut]
	op := body[cut:]
	if name == "" {
		return placeholderRef{}, fmt.Errorf("empty field name before operator %q", op)
	}
	// Validate the operator's SHAPE eagerly (value-independent). The range checks
	// in applySubstring are deferred to Substitute (they need the actual value).
	if err := validateOperatorShape(op); err != nil {
		return placeholderRef{}, err
	}
	return placeholderRef{name: name, op: op}, nil
}

// validateOperatorShape checks that op is a syntactically well-formed operator,
// WITHOUT applying it — so a mistyped operator fails at decode, while
// value-dependent checks (substring ranges) stay deferred to Substitute. Any
// error here is also returned (identically) by applyOperator, so the two stay
// in lockstep.
func validateOperatorShape(op string) error {
	if op == "" {
		return nil
	}
	switch op[0] {
	case '^', ',':
		// Must be exactly the sig once or twice.
		sig := string(op[0])
		if op != sig && op != sig+sig {
			return fmt.Errorf("case operator %q must be %q or %q", op, sig, sig+sig)
		}
		return nil
	case '%':
		_, err := operatorArg(op, '%')
		return err
	case '#':
		_, err := operatorArg(op, '#')
		return err
	case '/':
		// Reuse the replace parser's structural checks against a dummy value; a
		// replace op's shape errors are value-independent.
		_, err := applyReplace("", op)
		return err
	case ':':
		return validateSubstringShape(op)
	default:
		return fmt.Errorf("unknown operator %q", op)
	}
}

// validateSubstringShape checks a `:off`/`:off:len` operator parses (integers,
// non-negative) without checking the value's length.
func validateSubstringShape(op string) error {
	spec := op[1:]
	offStr := spec
	lenStr := ""
	hasLen := false
	if colon := strings.IndexByte(spec, ':'); colon >= 0 {
		offStr = spec[:colon]
		lenStr = spec[colon+1:]
		hasLen = true
	}
	off, err := strconv.Atoi(strings.TrimSpace(offStr))
	if err != nil {
		return fmt.Errorf("substring operator %q: offset must be an integer", op)
	}
	if off < 0 {
		return fmt.Errorf("substring operator %q: offset must be >= 0", op)
	}
	if hasLen {
		n, err := strconv.Atoi(strings.TrimSpace(lenStr))
		if err != nil {
			return fmt.Errorf("substring operator %q: length must be an integer", op)
		}
		if n < 0 {
			return fmt.Errorf("substring operator %q: length must be >= 0", op)
		}
	}
	return nil
}

// applyOperator applies a single bash-style substitution operator to value. The
// operators are a documented subset of bash parameter expansion, all LITERAL
// (no glob/regex):
//
//	^^ / ,,   uppercase / lowercase (whole string)
//	^  / ,    uppercase / lowercase the first character
//	%%suf / %suf   strip longest / shortest trailing literal suffix
//	##pre / #pre   strip longest / shortest leading literal prefix
//	//old/new / /old/new   replace all / first occurrence (literal)
//	:off / :off:len        substring by byte offset (and optional length)
//
// A malformed operator returns an error (surfaced at Substitute time, and also
// caught eagerly at decode via splitPlaceholder). op must be non-empty.
func applyOperator(value, op string) (string, error) {
	if op == "" {
		return value, nil
	}
	switch op[0] {
	case '^':
		return applyCaseOp(value, op, strings.ToUpper)
	case ',':
		return applyCaseOp(value, op, strings.ToLower)
	case '%':
		return applyStripSuffix(value, op)
	case '#':
		return applyStripPrefix(value, op)
	case '/':
		return applyReplace(value, op)
	case ':':
		return applySubstring(value, op)
	default:
		return "", fmt.Errorf("unknown operator %q", op)
	}
}

// applyCaseOp handles `^`/`,` (first char) and `^^`/`,,` (whole string). The
// caller passes the whole-string transform (ToUpper/ToLower); the first-char
// form applies it to just the leading byte. sig is the operator character.
func applyCaseOp(value, op string, transform func(string) string) (string, error) {
	sig := string(op[0])
	switch op {
	case sig + sig: // "^^" or ",,"
		return transform(value), nil
	case sig: // "^" or "," — first character only (rune-aware)
		if value == "" {
			return value, nil
		}
		_, w := utf8.DecodeRuneInString(value)
		return transform(value[:w]) + value[w:], nil
	default:
		return "", fmt.Errorf("case operator %q must be %q or %q", op, sig, sig+sig)
	}
}

// applyStripSuffix handles `%suf` (shortest) and `%%suf` (longest). Since the
// match is a literal (not a glob), shortest and longest are identical — a fixed
// suffix either matches or it doesn't. Both are supported for bash parity.
func applyStripSuffix(value, op string) (string, error) {
	suf, err := operatorArg(op, '%')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(value, suf), nil
}

// applyStripPrefix handles `#pre` (shortest) and `##pre` (longest). Literal, so
// both behave identically.
func applyStripPrefix(value, op string) (string, error) {
	pre, err := operatorArg(op, '#')
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(value, pre), nil
}

// operatorArg extracts the literal argument of a %/%%/#/## operator: the text
// after one or two leading sig bytes. A bare sig (e.g. "%") strips nothing.
func operatorArg(op string, sig byte) (string, error) {
	if op[0] != sig {
		return "", fmt.Errorf("internal: operator %q does not start with %q", op, string(sig))
	}
	n := 1
	if len(op) >= 2 && op[1] == sig {
		n = 2
	}
	// A THIRD leading sig would be a malformed operator (e.g. "%%%").
	if len(op) > n && op[n] == sig {
		return "", fmt.Errorf("malformed operator %q (too many %q)", op, string(sig))
	}
	return op[n:], nil
}

// applyReplace handles `/old/new` (first) and `//old/new` (all). The pattern
// and replacement are LITERAL. The separator is the first `/` after the leading
// sigil(s); an operator missing its separators is malformed.
func applyReplace(value, op string) (string, error) {
	// op starts with '/'. "//..." = replace-all, "/..." = replace-first.
	all := false
	rest := op[1:]
	if strings.HasPrefix(rest, "/") {
		all = true
		rest = rest[1:]
	}
	// rest is "old/new". Split on the FIRST '/'.
	sep := strings.IndexByte(rest, '/')
	if sep < 0 {
		return "", fmt.Errorf("replace operator %q must be /old/new or //old/new", op)
	}
	search := rest[:sep]
	repl := rest[sep+1:]
	if search == "" {
		return "", fmt.Errorf("replace operator %q has an empty search string", op)
	}
	if all {
		return strings.ReplaceAll(value, search, repl), nil
	}
	return strings.Replace(value, search, repl, 1), nil
}

// applySubstring handles `:off` and `:off:len` — byte offsets into value. A
// negative or out-of-range offset/length is an error (kept strict rather than
// bash's silent clamping, so a broken template fails loudly).
func applySubstring(value, op string) (string, error) {
	// op starts with ':'. Parse ":off" or ":off:len".
	spec := op[1:]
	offStr := spec
	lenStr := ""
	hasLen := false
	if colon := strings.IndexByte(spec, ':'); colon >= 0 {
		offStr = spec[:colon]
		lenStr = spec[colon+1:]
		hasLen = true
	}
	off, err := strconv.Atoi(strings.TrimSpace(offStr))
	if err != nil {
		return "", fmt.Errorf("substring operator %q: offset must be an integer", op)
	}
	if off < 0 || off > len(value) {
		return "", fmt.Errorf("substring operator %q: offset %d out of range for value of length %d", op, off, len(value))
	}
	// No colon → substring to end. A colon with a missing length (`:off:`) is
	// malformed — kept in lockstep with validateSubstringShape, which rejects it
	// at decode.
	if !hasLen {
		return value[off:], nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(lenStr))
	if err != nil {
		return "", fmt.Errorf("substring operator %q: length must be an integer", op)
	}
	if n < 0 || off+n > len(value) {
		return "", fmt.Errorf("substring operator %q: length %d out of range at offset %d for value of length %d", op, n, off, len(value))
	}
	return value[off : off+n], nil
}

// Substitute renders the pattern, replacing each `{name<op>}` with the operator
// applied to values[name], and unescaping `{{`/`}}`. It assumes the pattern
// already passed validation (every placeholder has a value); a missing value is
// treated as an internal error so the generator surfaces it rather than emitting
// a partial string. An operator error (malformed at value time) is returned.
//
// Substitution is a SINGLE pass: a substituted value that happens to contain
// `{...}` is emitted literally, never re-expanded — field values can't inject
// placeholders.
func (t TemplateEntry) Substitute(values map[string]string) (string, error) {
	var b strings.Builder
	b.Grow(len(t.Pattern))
	for i := 0; i < len(t.Pattern); {
		c := t.Pattern[i]
		switch c {
		case '{':
			if i+1 < len(t.Pattern) && t.Pattern[i+1] == '{' {
				b.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(t.Pattern[i+1:], '}')
			if end < 0 {
				return "", fmt.Errorf("internal: unterminated placeholder at offset %d", i)
			}
			body := t.Pattern[i+1 : i+1+end]
			ref, err := splitPlaceholder(body)
			if err != nil {
				return "", fmt.Errorf("internal: placeholder {%s}: %v", body, err)
			}
			v, ok := values[ref.name]
			if !ok {
				return "", fmt.Errorf("internal: no value for field %q", ref.name)
			}
			out, err := applyOperator(v, ref.op)
			if err != nil {
				return "", fmt.Errorf("field %q: %v", ref.name, err)
			}
			b.WriteString(out)
			i += 1 + end + 1
		case '}':
			if i+1 < len(t.Pattern) && t.Pattern[i+1] == '}' {
				b.WriteByte('}')
				i += 2
				continue
			}
			return "", fmt.Errorf("internal: stray '}' at offset %d", i)
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// Package charset implements the password-charset DSL used by the
// passwd generator. The DSL is small and intentionally limited:
//
//	alphanum            (default: a-z, A-Z, 0-9)
//	alphanum+symbols    (alphanum plus !@#$%^&*-_=+)
//	hex                 (0-9, a-f)
//	base64url           (a-z, A-Z, 0-9, -, _)
//	custom:<chars>      (literal characters; e.g. "custom:01")
//
// Custom charsets allow domain-specific needs (PIN codes, hex tokens,
// alphabetic-only passwords) without bloating the named-charset list.
package charset

import (
	"fmt"
	"strings"
)

// Standard charset names.
const (
	NameAlphanum        = "alphanum"
	NameAlphanumSymbols = "alphanum+symbols"
	NameHex             = "hex"
	NameBase64URL       = "base64url"
)

// DefaultName is used when a passwd entry omits chars.
const DefaultName = NameAlphanum

// charsetData maps named charsets to their literal character pool.
var charsetData = map[string]string{
	NameAlphanum:        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
	NameAlphanumSymbols: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*-_=+",
	NameHex:             "0123456789abcdef",
	NameBase64URL:       "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_",
}

// Resolve returns the literal character pool for the given charset
// specifier. Empty string returns the default (alphanum).
//
// Recognized forms:
//   - "" → DefaultName
//   - one of the standard names listed above
//   - "custom:<literal characters>"
//
// Returns an error for unrecognized names or empty custom charsets.
func Resolve(spec string) (string, error) {
	if spec == "" {
		return charsetData[DefaultName], nil
	}
	if strings.HasPrefix(spec, "custom:") {
		chars := strings.TrimPrefix(spec, "custom:")
		if chars == "" {
			return "", fmt.Errorf("charset: custom: requires at least one character")
		}
		return chars, nil
	}
	chars, ok := charsetData[spec]
	if !ok {
		return "", fmt.Errorf("charset: unknown name %q (valid: alphanum, alphanum+symbols, hex, base64url, custom:<chars>)", spec)
	}
	return chars, nil
}

// Names returns all standard charset names. Used by tests + docs.
func Names() []string {
	return []string{NameAlphanum, NameAlphanumSymbols, NameHex, NameBase64URL}
}

// Class is an ASCII character class a generated password can be REQUIRED to
// contain at least one of (the `require:` constraint). The four classes
// partition the ASCII byte space: every byte is exactly one of upper, lower,
// digit, or symbol.
//
// Why this exists: a downstream policy (e.g. an IdP's password complexity
// rules) may demand at least one of each class. A plain uniform draw can omit
// one — rare, but since generated secrets are CACHED and never re-rolled, a
// single non-compliant draw would be a permanent reject. require: guarantees
// the classes are present.
type Class string

// The recognized character classes.
const (
	ClassUpper  Class = "upper"
	ClassLower  Class = "lower"
	ClassDigit  Class = "digit"
	ClassSymbol Class = "symbol"
)

// Classes returns all recognized class names (for validation + docs).
func Classes() []Class { return []Class{ClassUpper, ClassLower, ClassDigit, ClassSymbol} }

// ParseClass validates a class name and returns the corresponding Class.
func ParseClass(s string) (Class, error) {
	switch Class(s) {
	case ClassUpper, ClassLower, ClassDigit, ClassSymbol:
		return Class(s), nil
	default:
		return "", fmt.Errorf("charset: unknown class %q (valid: upper, lower, digit, symbol)", s)
	}
}

// Contains reports whether byte b belongs to class c. Upper/lower/digit are
// the ASCII letter/number ranges; symbol is any other byte (non-alphanumeric).
func (c Class) Contains(b byte) bool {
	switch c {
	case ClassUpper:
		return b >= 'A' && b <= 'Z'
	case ClassLower:
		return b >= 'a' && b <= 'z'
	case ClassDigit:
		return b >= '0' && b <= '9'
	case ClassSymbol:
		return !(b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9')
	default:
		return false
	}
}

// PoolContains reports whether the character pool has at least one member of
// class c — i.e. whether a draw from this pool can ever satisfy the class.
// A require: class the pool can't satisfy is a configuration error.
func PoolContains(pool string, c Class) bool {
	for i := 0; i < len(pool); i++ {
		if c.Contains(pool[i]) {
			return true
		}
	}
	return false
}

// SatisfiesAll reports whether p contains at least one character of every
// class in classes (an empty classes list is trivially satisfied).
func SatisfiesAll(p []byte, classes []Class) bool {
	for _, c := range classes {
		found := false
		for _, b := range p {
			if c.Contains(b) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

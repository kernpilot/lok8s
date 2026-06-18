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

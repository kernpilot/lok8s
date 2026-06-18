// Package random provides crypto/rand-backed helpers for password and
// username generation. All randomness is bias-free: each character is
// drawn from the charset using rejection sampling so the output
// distribution is uniform regardless of charset size.
package random

import (
	"crypto/rand"
	"fmt"
	"io"
)

// Reader is the source of randomness. Tests swap this for a deterministic
// reader; production code uses crypto/rand.Reader.
var Reader io.Reader = rand.Reader

// Password generates a random password of the given length, drawn from
// charset. length must be > 0; charset must be non-empty.
//
// Uses rejection sampling: for each character, reads bytes from Reader
// and discards values that would bias the distribution toward the
// lower-indexed characters. This is essential for charsets whose length
// is not a power of 2.
func Password(length int, charset string) ([]byte, error) {
	return passwordFrom(Reader, length, charset)
}

func passwordFrom(r io.Reader, length int, charset string) ([]byte, error) {
	if length <= 0 {
		return nil, fmt.Errorf("random: password length must be > 0, got %d", length)
	}
	if len(charset) == 0 {
		return nil, fmt.Errorf("random: charset must not be empty")
	}
	if len(charset) > 256 {
		return nil, fmt.Errorf("random: charset must be ≤ 256 characters, got %d", len(charset))
	}

	// Compute the largest multiple of len(charset) that fits in 256, so
	// we can reject any byte value above it without bias.
	max := 256 - (256 % len(charset))

	out := make([]byte, length)
	buf := make([]byte, 1)
	for i := 0; i < length; {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("random: read: %w", err)
		}
		if int(buf[0]) >= max {
			continue // rejected — would bias the distribution
		}
		out[i] = charset[int(buf[0])%len(charset)]
		i++
	}
	return out, nil
}

// Username generates a random username: lowercase alphanumeric, starting
// with a letter, of the given length. Lengths < 3 are rejected.
func Username(length int) ([]byte, error) {
	return usernameFrom(Reader, length)
}

func usernameFrom(r io.Reader, length int) ([]byte, error) {
	if length < 3 {
		return nil, fmt.Errorf("random: username length must be ≥ 3, got %d", length)
	}
	const lower = "abcdefghijklmnopqrstuvwxyz"
	const lowerNum = "abcdefghijklmnopqrstuvwxyz0123456789"

	first, err := passwordFrom(r, 1, lower)
	if err != nil {
		return nil, err
	}
	rest, err := passwordFrom(r, length-1, lowerNum)
	if err != nil {
		return nil, err
	}
	return append(first, rest...), nil
}

// Digits generates a random sequence of n digits ('0'-'9').
func Digits(n int) ([]byte, error) {
	return passwordFrom(Reader, n, "0123456789")
}

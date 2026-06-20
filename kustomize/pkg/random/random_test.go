package random

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestPassword_Length(t *testing.T) {
	for _, n := range []int{1, 8, 32, 128} {
		got, err := Password(n, "abc")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != n {
			t.Errorf("Password(%d) returned %d chars", n, len(got))
		}
	}
}

func TestPassword_OnlyCharsetChars(t *testing.T) {
	got, err := Password(100, "abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if !strings.ContainsRune("abc", rune(c)) {
			t.Errorf("password contains non-charset char: %q", c)
		}
	}
}

func TestPassword_LongCharset(t *testing.T) {
	chars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*-_=+"
	got, err := Password(50, chars)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if !strings.ContainsRune(chars, rune(c)) {
			t.Errorf("password contains non-charset char: %q", c)
		}
	}
}

func TestPassword_RejectsBadInputs(t *testing.T) {
	if _, err := Password(0, "abc"); err == nil {
		t.Error("length 0 should error")
	}
	if _, err := Password(-1, "abc"); err == nil {
		t.Error("negative length should error")
	}
	if _, err := Password(10, ""); err == nil {
		t.Error("empty charset should error")
	}
}

func TestPassword_Uniqueness_Statistical(t *testing.T) {
	// Crude statistical sanity: 100 passwords of length 32 from a large
	// charset should produce 100 unique values (collision probability is
	// astronomical).
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		got, err := Password(32, "abcdefghijklmnopqrstuvwxyz0123456789")
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		if seen[s] {
			t.Errorf("collision: %q", s)
		}
		seen[s] = true
	}
}

func TestUsername_StartsWithLetter(t *testing.T) {
	for i := 0; i < 50; i++ {
		got, err := Username(8)
		if err != nil {
			t.Fatal(err)
		}
		if !((got[0] >= 'a' && got[0] <= 'z')) {
			t.Errorf("username[0] = %q, want lowercase letter", got[0])
		}
	}
}

func TestUsername_RejectsShort(t *testing.T) {
	for _, n := range []int{0, 1, 2} {
		if _, err := Username(n); err == nil {
			t.Errorf("Username(%d) should error", n)
		}
	}
}

func TestDigits_OnlyDigits(t *testing.T) {
	got, err := Digits(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c < '0' || c > '9' {
			t.Errorf("digits contains non-digit: %q", c)
		}
	}
}

func TestSwapReader_DeterministicForTests(t *testing.T) {
	// Verify that swapping the reader works (used by determinism tests
	// elsewhere in the codebase).
	saved := Reader
	defer func() { Reader = saved }()
	Reader = bytes.NewReader(make([]byte, 1024)) // all zeros
	got1, err := Password(16, "abc")
	if err != nil {
		t.Fatal(err)
	}
	Reader = bytes.NewReader(make([]byte, 1024))
	got2, err := Password(16, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if string(got1) != string(got2) {
		t.Errorf("same seed should produce same output: %q vs %q", got1, got2)
	}
	// Restore
	Reader = rand.Reader
}

func TestPasswordSatisfying_MeetsPredicate(t *testing.T) {
	hasUpperAndDigit := func(p []byte) bool {
		up, dig := false, false
		for _, b := range p {
			if b >= 'A' && b <= 'Z' {
				up = true
			}
			if b >= '0' && b <= '9' {
				dig = true
			}
		}
		return up && dig
	}
	for i := 0; i < 100; i++ {
		p, err := PasswordSatisfying(12, "abcABC123", hasUpperAndDigit)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 12 || !hasUpperAndDigit(p) {
			t.Fatalf("password %q did not satisfy predicate", p)
		}
	}
}

func TestPasswordSatisfying_NilPredicate(t *testing.T) {
	p, err := PasswordSatisfying(10, "abc", nil)
	if err != nil || len(p) != 10 {
		t.Fatalf("nil predicate: got %q, %v", p, err)
	}
}

func TestPasswordSatisfying_Unsatisfiable(t *testing.T) {
	// The charset has no uppercase, so this predicate can never pass — it must
	// error after the attempt bound rather than hang forever.
	wantsUpper := func(p []byte) bool {
		for _, b := range p {
			if b >= 'A' && b <= 'Z' {
				return true
			}
		}
		return false
	}
	if _, err := PasswordSatisfying(8, "abcdef0123", wantsUpper); err == nil {
		t.Error("expected error for an unsatisfiable constraint")
	}
}

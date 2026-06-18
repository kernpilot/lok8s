package htpasswdfmt

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormat_Roundtrip(t *testing.T) {
	// Use a low bcrypt cost to keep tests fast.
	const cost = 4
	line, err := FormatCost("alice", []byte("hunter2"), cost)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(line, []byte("alice:")) {
		t.Errorf("expected alice: prefix, got %s", line)
	}
	if !bytes.Contains(line, []byte("$2")) {
		t.Errorf("expected bcrypt hash marker, got %s", line)
	}
	// Verify round-trips.
	if err := Verify(line, []byte("hunter2")); err != nil {
		t.Errorf("Verify(correct password) = %v", err)
	}
	if err := Verify(line, []byte("wrong")); err == nil {
		t.Error("Verify(wrong password) should fail")
	}
}

func TestFormat_DifferentSaltsEachCall(t *testing.T) {
	// bcrypt salts are random — calling Format twice with the same input
	// should produce different lines (this is what makes caching the
	// hashed line necessary for deterministic output).
	const cost = 4
	a, _ := FormatCost("alice", []byte("hunter2"), cost)
	b, _ := FormatCost("alice", []byte("hunter2"), cost)
	if bytes.Equal(a, b) {
		t.Error("two Format calls should produce different hashes (random salt)")
	}
	// But both should verify successfully.
	if err := Verify(a, []byte("hunter2")); err != nil {
		t.Error(err)
	}
	if err := Verify(b, []byte("hunter2")); err != nil {
		t.Error(err)
	}
}

func TestFormat_RejectsEmptyUsername(t *testing.T) {
	if _, err := Format("", []byte("p")); err == nil {
		t.Error("expected error for empty username")
	}
}

func TestFormat_RejectsColonInUsername(t *testing.T) {
	if _, err := Format("a:b", []byte("p")); err == nil {
		t.Error("expected error for ':' in username")
	}
}

func TestVerify_MalformedLine(t *testing.T) {
	if err := Verify([]byte("no-colon-here"), []byte("p")); err == nil {
		t.Error("expected error for malformed line")
	}
}

func TestFormat_DefaultCostMatchesDefault(t *testing.T) {
	// Just exercise the no-cost wrapper to make sure it compiles and
	// produces a valid line.
	line, err := Format("u", []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(line), "u:") {
		t.Errorf("got %s", line)
	}
}

package fileio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeRead_Success(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := SafeRead(dir, "data.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestSafeRead_Subdirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "file.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := SafeRead(dir, "sub/file.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nested" {
		t.Errorf("got %q", got)
	}
}

func TestSafeRead_RejectsAbsolute(t *testing.T) {
	if _, err := SafeRead("/tmp", "/etc/passwd", 0); err == nil {
		t.Error("expected error for absolute path")
	}
}

func TestSafeRead_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"../etc/passwd", "../../etc/passwd", "..", "sub/../../escape"} {
		if _, err := SafeRead(dir, p, 0); err == nil {
			t.Errorf("expected error for %q", p)
		}
	}
}

func TestSafeRead_RejectsEmpty(t *testing.T) {
	if _, err := SafeRead("/tmp", "", 0); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestSafeRead_RejectsNullByte(t *testing.T) {
	if _, err := SafeRead("/tmp", "abc\x00def", 0); err == nil {
		t.Error("expected error for null byte in path")
	}
}

func TestSafeRead_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := SafeRead(dir, "nope.txt", 0)
	if err == nil {
		t.Error("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %q", err.Error())
	}
}

func TestSafeRead_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "asdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := SafeRead(dir, "asdir", 0)
	if err == nil {
		t.Error("expected error for directory")
	}
}

func TestSafeRead_SizeLimit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(target, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := SafeRead(dir, "big.txt", 50)
	if err == nil {
		t.Error("expected error for oversized file")
	}
	// Within limit succeeds.
	if _, err := SafeRead(dir, "big.txt", 200); err != nil {
		t.Error(err)
	}
}

func TestSafeRead_DefaultMaxSize(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeRead(dir, "x.txt", 0); err != nil {
		t.Error(err)
	}
}

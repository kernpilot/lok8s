package build

// move_test.go covers moveFile: the rename, the EXDEV fallback (copy +
// fsync + remove) and the pass-through of every other rename error. The
// seam renameFile stands in for a second filesystem.

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// swapRename installs fn as renameFile for the test.
func swapRename(t *testing.T, fn func(oldpath, newpath string) error) {
	t.Helper()
	prev := renameFile
	renameFile = fn
	t.Cleanup(func() { renameFile = prev })
}

// exdev is the error os.Rename returns for a move across filesystems.
func exdev(oldpath, newpath string) error {
	return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
}

func TestMoveFileFallsBackToCopyOnEXDEV(t *testing.T) {
	calls := 0
	swapRename(t, func(oldpath, newpath string) error {
		calls++
		if calls == 1 {
			return exdev(oldpath, newpath)
		}
		return os.Rename(oldpath, newpath)
	})
	dir := t.TempDir()
	src := filepath.Join(dir, "ConfigMap.ns1.cm.yml")
	dst := filepath.Join(dir, "stage", "ConfigMap.ns1.cm.yaml")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("kind: ConfigMap\ndata:\n  k: v\n"), 4096)
	if err := os.WriteFile(src, content, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile across devices: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination missing after the copy fallback: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("destination bytes differ from the source (%d vs %d bytes)", len(got), len(content))
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source must be removed after the copy, stat err = %v", err)
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("destination mode = %v, want 0640 (err %v)", info.Mode(), err)
	}
	if calls != 1 {
		t.Errorf("renameFile called %d times, want 1 (the fallback copies, it does not retry)", calls)
	}
}

func TestMoveFileReturnsOtherRenameErrors(t *testing.T) {
	swapRename(t, func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
	})
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := moveFile(src, dst)
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want the rename's EACCES passed through", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("a non-EXDEV failure must leave the source in place")
	}
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a non-EXDEV failure must not create the destination")
	}
}

func TestMoveFileRenamesOnOneFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("same fs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "same fs" {
		t.Errorf("dst = %q", got)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Error("source must be gone after a rename")
	}
}

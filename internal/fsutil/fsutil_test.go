package fsutil

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPredicates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "d")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	defer ln.Close()
	missing := filepath.Join(dir, "missing")

	rows := []struct {
		path                                     string
		fileExists, isRegular, dirExists, exists bool
	}{
		{file, true, true, false, true},
		{link, true, true, false, true}, // Stat follows the link
		{sub, false, false, true, true},
		{sock, true, false, false, true}, // the two file flavours differ here
		{missing, false, false, false, false},
	}
	for _, r := range rows {
		name := filepath.Base(r.path)
		if got := FileExists(r.path); got != r.fileExists {
			t.Errorf("FileExists(%s) = %v", name, got)
		}
		if got := IsRegular(r.path); got != r.isRegular {
			t.Errorf("IsRegular(%s) = %v", name, got)
		}
		if got := DirExists(r.path); got != r.dirExists {
			t.Errorf("DirExists(%s) = %v", name, got)
		}
		if got := Exists(r.path); got != r.exists {
			t.Errorf("Exists(%s) = %v", name, got)
		}
	}
}

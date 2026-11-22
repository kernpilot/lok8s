package storeguard

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteReplacing writes path through a temp file in the same directory, mode
// perm, and a rename over path; write fills the temp file. The temp file is
// new (an exclusive create) and private until the chmod to perm, and it is
// synced before the rename. A write that fails leaves the old file as it
// was, and the temp file is removed. The rename replaces a symlink or a
// hard link at path instead of writing through it. The temp name starts
// with a dot, so the store reads (Secret.*) never list it.
func WriteReplacing(path string, perm fs.FileMode, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	err = tmp.Chmod(perm)
	if err == nil {
		err = write(tmp)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		err = RemoveTemp(err, name)
	}
	return err
}

// RemoveTemp removes name, the temp file of a write that failed with err. A
// remove that fails too gives a *TempLeftError: the temp file stays. A temp
// file that is gone already is no error.
func RemoveTemp(err error, name string) error {
	if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return &TempLeftError{Err: err, Name: name, Rm: rerr}
	}
	return err
}

// TempLeftError is the error of a write through a temp file that failed,
// and whose temp file stays because its remove failed too.
type TempLeftError struct {
	Err  error  // the error of the write
	Name string // the temp file that stays
	Rm   error  // the error of the remove
}

func (e *TempLeftError) Error() string {
	return fmt.Sprintf("%v; cannot remove the temp file %s: %v", e.Err, e.Name, e.Rm)
}

func (e *TempLeftError) Unwrap() []error { return []error{e.Err, e.Rm} }

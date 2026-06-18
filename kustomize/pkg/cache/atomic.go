package cache

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path atomically by writing to a tmp
// file in the same directory and renaming it. The final file is
// chmod'd to perm before the rename so concurrent readers never see
// world-readable secret files even briefly.
//
// Same-directory tmp file is required for atomic rename on POSIX (cross-
// device renames fall back to copy+delete and aren't atomic).
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cache: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cache: write tmp: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cache: chmod tmp: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cache: sync tmp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("cache: close tmp: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("cache: rename tmp: %w", err)
	}
	return nil
}

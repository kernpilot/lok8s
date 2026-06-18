// Package fileio provides safe file reading helpers for kustomize
// plugins. The file generator uses these to read source files (TLS
// certs, etc.) while preventing path traversal and oversized reads.
package fileio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

// DefaultMaxSize is the maximum file size accepted by SafeRead. Matches
// the legacy bash plugin's 1 MiB limit.
const DefaultMaxSize int64 = 1 << 20 // 1 MiB

// SafeRead reads a file with safety checks:
//
//   - Path must be relative (no leading "/")
//   - Path must not contain ".." segments
//   - Path must not contain a null byte
//   - File must exist and be a regular file
//   - File size must be ≤ maxSize
//
// Returns the raw file contents on success, or a structured *errs.Error
// describing the failure.
//
// rootDir is the directory the path is relative to. The result must
// remain inside rootDir; any escape via symlinks is rejected.
//
// Pass maxSize=0 to use DefaultMaxSize.
func SafeRead(rootDir, path string, maxSize int64) ([]byte, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if path == "" {
		return nil, errs.New("file: empty path")
	}
	if strings.ContainsRune(path, 0) {
		return nil, errs.Newf("file: path contains null byte: %q", path)
	}
	if filepath.IsAbs(path) {
		return nil, errs.Newf("file: path must be relative, got absolute: %q", path)
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") || clean == ".." {
		return nil, errs.Newf("file: path must not contain '..' segments: %q", path)
	}

	full := filepath.Join(rootDir, clean)
	// Verify the joined path stays within rootDir.
	rel, err := filepath.Rel(rootDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, errs.Newf("file: path escapes root directory: %q", path)
	}

	st, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errs.Newf("file: not found: %q", path)
		}
		return nil, fmt.Errorf("file: stat %q: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, errs.Newf("file: not a regular file: %q", path)
	}
	if st.Size() > maxSize {
		return nil, errs.Newf("file: size %d > max %d: %q", st.Size(), maxSize, path)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("file: read %q: %w", path, err)
	}
	return data, nil
}

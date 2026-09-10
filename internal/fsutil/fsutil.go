// Package fsutil holds the file-existence predicates the bash `[[ -f ]]`,
// `[[ -d ]]` and `[[ -e ]]` tests port to. Every package used to carry its
// own copy; the four flavours live here once so a port picks by name.
package fsutil

import "os"

// FileExists reports a path that exists and is not a directory — the
// `[[ -f ]]` approximation most ports use (a symlink resolves through Stat;
// a device or socket counts too).
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// IsRegular reports a path that exists and is a regular file (secrets and
// lint use the stricter test: a socket or device in a store is not a
// secret).
func IsRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// DirExists reports a path that exists and is a directory (`[[ -d ]]`).
func DirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Exists reports a path that exists at all (`[[ -e ]]`).
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

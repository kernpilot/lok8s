package storeguard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ReservedName reports whether base, the name of a plaintext file in a
// store, ends in .enc in any ASCII letter case (the bash twin of lo:
// secrets::_enc_reserved). The rule !Secret.*.enc of the block re-includes
// such a name, and with core.ignorecase also a name that ends in .ENC: git
// would commit the plaintext. WritePlaintext and CheckName refuse such a
// name before they write it.
func ReservedName(base string) bool {
	return strings.HasSuffix(asciiLower(base), ".enc")
}

// PartProblem is the reason why CheckName refuses s, a name, a namespace or
// a key, as a part of the name of a plaintext file, or "" when it accepts it
// (the bash twin of lo: secrets::_set_part_ok). The file is
// Secret.<name>.<namespace>.<key> in the store. A part with a / or a \ can
// make that name a path into another directory (key = anchor/../.gitignore
// writes the .gitignore), and . and .. are parts of a path. A control byte
// (a NUL too) is refused: a refusal prints the file name. Every other byte
// stays accepted: the names and keys of Kubernetes use letters, digits, .,
// - and _ only.
func PartProblem(s string) string {
	switch {
	case s == "." || s == "..":
		return "it is . or .., a part of a path"
	case strings.ContainsAny(s, "/\\"):
		return "it has a / or a \\, which makes the file name a path"
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return "it has a control byte"
		}
	}
	return ""
}

// Destination is the plaintext file base in the store dir. It reports false
// when the file is not one entry of dir itself (the bash twin of lo:
// secrets::_set_destination): its last part is not base, or its directory
// is not dir. filepath.Split does not clean the path, so a base with a /
// in it fails, also when a .. in it leads back to dir.
func Destination(dir, base string) (string, bool) {
	dest := dir + "/" + base
	parent, file := filepath.Split(dest)
	return dest, file == base && filepath.Clean(parent) == filepath.Clean(dir)
}

// DestinationOK reports whether path, read with lstat (a symlink is not
// followed), is a regular file or does not exist (the bash twin of lo:
// secrets::_enc_path_ok). Any other lstat error refuses too, for example
// ENAMETOOLONG for a name that is too long.
//
// It checks the last part of the path only. The store directory and its
// parent directories are trusted: they belong to the user who runs lo. A
// symlink in a parent is followed, and a parent that another user can
// replace could move the temp file and the rename out of the store. The
// writes do not anchor to an opened store directory.
func DestinationOK(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return fi.Mode().IsRegular()
}

// asciiLower lowercases the ASCII letters of s only, as git does.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// Printable is s as a refusal prints it: each byte outside the printable
// ASCII range as ? (no terminal control from a file name or a key).
func Printable(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

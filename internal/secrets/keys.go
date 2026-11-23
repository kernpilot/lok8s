package secrets

// SSH→age key derivation. The bash implementation shells out to the
// ssh-to-age binary; here the same module that binary is built from
// (github.com/Mic92/ssh-to-age) is called directly, so the binary dependency
// disappears.
//
// CRITICAL CONTAINMENT: the ssh-to-age BINARY echoes its entire input on
// failure — feeding it a private key by mistake printed key material into
// terminals/CI logs twice (see tests/unit/secret_material_containment_test.bats).
// The library errors carry only parse diagnostics, and nothing in this file
// ever writes derivation input or raw library errors for PRIVATE keys to any
// stream.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"

	agessh "github.com/Mic92/ssh-to-age"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// agePubkeyRe validates the bech32 shape of an age public key. Validated
// because a typo here is silent: sops accepts the config and every later
// encrypt writes a recipient nobody holds.
var agePubkeyRe = regexp.MustCompile(`^age1[023456789acdefghjklmnpqrstuvwxyz]{58}$`)

// errUnsupportedKey marks a well-formed SSH public key of a non-ed25519 type.
// The ssh-to-age CLI treats this as a SKIP (rc 0, empty stdout, a
// "skipped key: …" note on stderr), and the bash callers see an empty
// derivation result: deriveAgePublicKey mirrors that split. Neither
// implementation prints the note: bash keeps the stderr of ssh-to-age from
// the terminal, because on a failure the tool quotes its input.
var errUnsupportedKey = errors.New("unsupported ssh key type")

// errPrivateKeyFile marks a file that holds a private key where a public key
// is expected (privateKeyFileRe). deriveAgePublicKey returns it before the
// library parses the file.
var errPrivateKeyFile = errors.New("the file holds a private key")

// errKeyFileUnreadable, errKeyFileTooBig and errKeyFileNUL mark a key file
// that deriveAgePublicKey does not parse: one it cannot open or read as a
// regular file, one bigger than maxPublicKeyFile, and one that holds a NUL
// byte.
var (
	errKeyFileUnreadable = errors.New("the key file cannot be read")
	errKeyFileTooBig     = errors.New("the key file is too big for a public key")
	errKeyFileNUL        = errors.New("the key file holds a NUL byte")
)

// maxPublicKeyFile is the most that deriveAgePublicKey reads from a key file:
// 64 KiB. An OpenSSH public key file is one line of a few hundred bytes, and
// the line of a 16384-bit RSA key is about 2.8 KiB. Bash twin: the cap of
// secrets::_read_public_key.
const maxPublicKeyFile = 64 << 10

// privateKeyFileRe matches a line that starts a private key: a PEM or
// OpenSSH block (-----BEGIN … PRIVATE KEY-----, in any form, the PGP form
// -----BEGIN PGP PRIVATE KEY BLOCK----- included), the SSH.com form (----
// BEGIN SSH2 ENCRYPTED PRIVATE KEY ----), a PuTTY key file, or an age
// identity in either case (bech32 allows both). The line can start with a
// UTF-8 byte order mark. The age markers match anywhere in the file: a .pub
// whose comment holds one is refused, which fails closed. A public key file
// has none of these. Bash twin: the pattern of secrets::_read_public_key.
var privateKeyFileRe = regexp.MustCompile(`(?m)^\x{FEFF}?[ \t]*(-{4,5} ?BEGIN ([A-Z0-9-]+ )*PRIVATE KEY( BLOCK)? ?-{4,5}|PuTTY-User-Key-File-)|AGE-SECRET-KEY-|age-secret-key-`)

// readPublicKeyFile reads a key file once, at most maxPublicKeyFile bytes,
// for the check and the parse to see the same bytes. It reads a regular file
// only, and refuses a file that holds a NUL byte: no public key file holds
// one, and the bash twin cannot keep one in a variable, so the twins refuse
// the same files.
func readPublicKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the key file that the user names
	if err != nil {
		return nil, errKeyFileUnreadable
	}
	// A read-only file: its close error changes nothing that was read.
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errKeyFileUnreadable
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPublicKeyFile+1))
	if err != nil {
		return nil, errKeyFileUnreadable
	}
	if len(raw) > maxPublicKeyFile {
		return nil, errKeyFileTooBig
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return nil, errKeyFileNUL
	}
	return raw, nil
}

// deriveAgePublicKey derives an age recipient from an SSH public key file.
// Returns errKeyFileUnreadable, errKeyFileTooBig or errKeyFileNUL for a file
// that it does not parse (readPublicKeyFile); ("", errPrivateKeyFile) for a file that holds
// a private key, before the library parses it; ("", errUnsupportedKey) for a
// parseable non-ed25519 key (the CLI's "skipped key" case → empty output,
// rc 0 in bash); and a generic error for anything unparseable (the CLI's rc 1
// case). No caller prints the error: the library error can quote the input.
func deriveAgePublicKey(path string) (string, error) {
	raw, err := readPublicKeyFile(path)
	if err != nil {
		return "", err
	}
	if privateKeyFileRe.Match(raw) {
		return "", errPrivateKeyFile
	}
	key, err := agessh.SSHPublicKeyToAge(raw)
	if err != nil {
		if errors.Is(err, agessh.ErrUnsupportedKeyType) {
			return "", errUnsupportedKey
		}
		return "", err
	}
	return strings.TrimSpace(*key), nil
}

// refuseKeyFile prints the refusal for a key file that deriveAgePublicKey
// did not parse: a file it could not read, a file too big for a public key,
// a file that holds a NUL byte, or a file that holds a private key. It
// prints the path (shownKeyPath) and the cause, and nothing from the file,
// and reports whether err was one of these. Bash twin:
// secrets::_read_public_key.
func refuseKeyFile(w io.Writer, path string, err error) bool {
	shown := shownKeyPath(path)
	switch {
	case errors.Is(err, errKeyFileUnreadable):
		ui.ErrorTo(w, "cannot read the key file %s: nothing from it is printed", shown)
	case errors.Is(err, errKeyFileTooBig):
		ui.ErrorTo(w, "%s is bigger than 64 KiB, which no public key file is: nothing from it is printed", shown)
	case errors.Is(err, errKeyFileNUL):
		ui.ErrorTo(w, "%s holds a NUL byte, which no public key file holds: nothing from it is printed", shown)
	case errors.Is(err, errPrivateKeyFile):
		refusePrivateKeyFile(w, path)
	default:
		return false
	}
	return true
}

// refusePrivateKeyFile prints the refusal for a key file that holds a
// private key (errPrivateKeyFile): the path and the fix, and nothing from
// the file. For a path that it does not print, the fix names ssh-keygen.
func refusePrivateKeyFile(w io.Writer, path string) {
	shown := shownKeyPath(path)
	ui.ErrorTo(w, "%s holds a private key, not a public key: nothing from it is printed", shown)
	if strings.HasSuffix(path, ".pub") || shown != path {
		ui.ErrorTo(w, "pass the public key file instead: ssh-keygen -y -f <private key> prints the public key")
		return
	}
	ui.ErrorTo(w, "pass the public key file instead: %s.pub", path)
}

// DefaultSSHKey is the SSH private key that lo decrypts with: LOK8S_SSH_KEY,
// default ~/.ssh/id_ed25519 (bash: the LOK8S_SSH_KEY default of the secrets
// library). decrypt reads it by design; no other command reads a private
// key.
func DefaultSSHKey() string {
	if v := os.Getenv("LOK8S_SSH_KEY"); v != "" {
		return v
	}
	return os.Getenv("HOME") + "/.ssh/id_ed25519"
}

// DefaultSSHPublicKey is the public half of DefaultSSHKey: the path itself
// when it ends in .pub, else the path with .pub added (default
// ~/.ssh/id_ed25519.pub). init derives the recipient from it when no
// --ssh-key is given, and remove-key compares the key to remove with it.
// Bash twin: secrets::_ssh_public_key.
func DefaultSSHPublicKey() string {
	k := DefaultSSHKey()
	if strings.HasSuffix(k, ".pub") {
		return k
	}
	return k + ".pub"
}

// deriveAgePrivateKey derives the age identity from an SSH PRIVATE key file
// (passphrase-less only — an encrypted key fails, exactly like the binary
// without a terminal). The returned error is never printed verbatim by
// callers (containment).
func deriveAgePrivateKey(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	priv, _, err := agessh.SSHPrivateKeyToAge(raw, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(*priv), nil
}

// keysFileIdentity reads the first age identity from the SOPS-standard key
// file (${SOPS_AGE_KEY_FILE:-~/.config/sops/age/keys.txt}): the first
// non-comment line containing AGE-SECRET-KEY-, taken whole (bash:
// `grep -v '^#' | grep -m1 'AGE-SECRET-KEY-'`). "" when the file is absent
// or holds no key.
func keysFileIdentity() string {
	keyFile := os.Getenv("SOPS_AGE_KEY_FILE")
	if keyFile == "" {
		keyFile = os.Getenv("HOME") + "/.config/sops/age/keys.txt"
	}
	if !fsutil.IsRegular(keyFile) {
		return ""
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "AGE-SECRET-KEY-") {
			return line
		}
	}
	return ""
}

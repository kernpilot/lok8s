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
	"errors"
	"os"
	"regexp"
	"strings"

	agessh "github.com/Mic92/ssh-to-age"
)

// agePubkeyRe validates the bech32 shape of an age public key. Validated
// because a typo here is silent: sops accepts the config and every later
// encrypt writes a recipient nobody holds.
var agePubkeyRe = regexp.MustCompile(`^age1[023456789acdefghjklmnpqrstuvwxyz]{58}$`)

// errUnsupportedKey marks a well-formed SSH public key of a non-ed25519 type.
// The ssh-to-age CLI treats this as a SKIP (rc 0, empty stdout, a
// "skipped key: …" note on stderr), and the bash callers see an empty
// derivation result — deriveAgePublicKey mirrors that split so the ported
// error surfaces stay byte-identical.
var errUnsupportedKey = errors.New("unsupported ssh key type")

// deriveAgePublicKey derives an age recipient from an SSH public key file.
// Returns ("", errUnsupportedKey) for a parseable non-ed25519 key (the CLI's
// "skipped key" case → empty output, rc 0 in bash) and a generic error for
// anything unreadable/unparseable (the CLI's rc 1 case). skipNote carries the
// CLI's stderr line for callers that (like bash init) let it through.
func deriveAgePublicKey(path string) (pubkey, skipNote string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	key, err := agessh.SSHPublicKeyToAge(raw)
	if err != nil {
		if errors.Is(err, agessh.ErrUnsupportedKeyType) {
			// The CLI prints: "skipped key: got ssh-rsa key type, but only
			// ed25519 keys are supported" — reuse the library message so the
			// leaked-on-purpose init line matches byte for byte. Safe to
			// surface: it names the key TYPE, never key material.
			return "", "skipped key: " + err.Error(), errUnsupportedKey
		}
		return "", "", err
	}
	return strings.TrimSpace(*key), "", nil
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
	if !isFile(keyFile) {
		return ""
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "AGE-SECRET-KEY-") {
			return line
		}
	}
	return ""
}

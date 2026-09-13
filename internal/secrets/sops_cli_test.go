package secrets

// sops_cli_test.go proves the cross-tool contract of sops.go against the
// pinned upstream sops CLI (.bin/sops, b-managed): a binary-mode .enc file
// written by lo's library path decrypts with `sops --decrypt`, and one
// written by the CLI decrypts with lo. The library is the kernpilot
// age-only fork; this is the gate that says its file format and MAC code
// still match upstream's. The CLI tests skip locally without the binary
// and FAIL under CI, like the render byte-parity tests: a skipped gate
// looks exactly like a green one.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/getsops/sops/v3/keys"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// crossToolPlaintext carries a trailing newline, a NUL and a multi-byte
// rune so the binary store round-trips bytes, not text.
var crossToolPlaintext = []byte("s3cr3t-token=abc123\n\x00tail\xc3\xa9\n")

// pinnedSops returns the repo's b-managed sops binary, skipping the test
// when it is not installed (failing on CI).
func pinnedSops(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(testutil.RepoRoot(t), ".bin", "sops")
	if info, err := os.Stat(bin); err != nil || info.Mode()&0o111 == 0 {
		what := "pinned sops CLI not installed under .bin (b install): cross-tool decrypt gate skipped"
		if os.Getenv("CI") != "" {
			t.Fatalf("%s; on CI the toolchain must be installed before the Go tests (ci.yml: Install toolchain), the cross-tool gate must not skip", what)
		}
		t.Skip(what)
	}
	return bin
}

// runSops runs the pinned CLI in dir with a minimal child environment: the
// age identity travels ONLY through the child's SOPS_AGE_KEY, never through
// the test process. stdout is returned; stderr rides along in the failure.
func runSops(t *testing.T, bin, dir, identity string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"SOPS_AGE_KEY=" + identity,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sops %s: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

// crossToolFixture returns a temp dir, a fresh age identity and a .sops.yaml
// in that dir whose one creation rule matches Secret.* with the identity's
// recipient.
func crossToolFixture(t *testing.T) (string, *age.X25519Identity, string) {
	t.Helper()
	dir := t.TempDir()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, ".sops.yaml")
	write(t, cfg, "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '"+id.Recipient().String()+"'\n")
	return dir, id, cfg
}

// TestSopsCLIDecryptsLoEncryptedFile: a file encrypted by sopsEncryptFile
// (the JSON binary store) decrypts with the upstream CLI's
// `--input-type binary --output-type binary`.
func TestSopsCLIDecryptsLoEncryptedFile(t *testing.T) {
	bin := pinnedSops(t)
	dir, id, cfg := crossToolFixture(t)

	plain := filepath.Join(dir, "Secret.app.default.TOKEN")
	if err := os.WriteFile(plain, crossToolPlaintext, 0o600); err != nil {
		t.Fatal(err)
	}
	enc := plain + ".enc"
	if err := sopsEncryptFile(cfg, plain, enc); err != nil {
		t.Fatalf("sopsEncryptFile: %v", err)
	}

	got := runSops(t, bin, dir, id.String(),
		"--decrypt", "--input-type", "binary", "--output-type", "binary", enc)
	if !bytes.Equal(got, crossToolPlaintext) {
		t.Fatalf("sops --decrypt of the lo-encrypted file:\n got %q\nwant %q", got, crossToolPlaintext)
	}
}

// TestLoDecryptsSopsCLIEncryptedFile: a file encrypted by the upstream CLI
// (`--age <recipient>`, binary in and out) decrypts with sopsDecryptData,
// and a corrupted envelope fails the MAC check instead of decrypting.
func TestLoDecryptsSopsCLIEncryptedFile(t *testing.T) {
	bin := pinnedSops(t)
	dir, id, _ := crossToolFixture(t)

	plain := filepath.Join(dir, "Secret.app.default.TOKEN")
	if err := os.WriteFile(plain, crossToolPlaintext, 0o600); err != nil {
		t.Fatal(err)
	}
	enc := runSops(t, bin, dir, id.String(),
		"--encrypt", "--input-type", "binary", "--output-type", "binary",
		"--age", id.Recipient().String(), plain)

	got, err := sopsDecryptData(enc, id.String())
	if err != nil {
		t.Fatalf("sopsDecryptData of the CLI-encrypted file: %v", err)
	}
	if !bytes.Equal(got, crossToolPlaintext) {
		t.Fatalf("sopsDecryptData of the CLI-encrypted file:\n got %q\nwant %q", got, crossToolPlaintext)
	}

	t.Run("corrupted envelope fails the MAC check", func(t *testing.T) {
		// Flip one base64 character inside the encrypted MAC value. The
		// envelope stays well-formed JSON with a well-formed ENC[...]
		// token, so the only thing that can reject it is the MAC.
		corrupt := bytes.Clone(enc)
		macAt := bytes.Index(corrupt, []byte(`"mac":`))
		if macAt < 0 {
			t.Fatalf("no mac field in the CLI envelope:\n%s", enc)
		}
		dataAt := bytes.Index(corrupt[macAt:], []byte("data:"))
		if dataAt < 0 {
			t.Fatalf("no data: segment in the mac value:\n%s", enc)
		}
		at := macAt + dataAt + len("data:")
		if corrupt[at] == 'A' {
			corrupt[at] = 'B'
		} else {
			corrupt[at] = 'A'
		}
		_, err := sopsDecryptData(corrupt, id.String())
		if err == nil {
			t.Fatal("sopsDecryptData accepted an envelope with a corrupted mac")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "mac") {
			t.Fatalf("corrupted mac: want a MAC error, got %v", err)
		}
		t.Logf("corrupted mac rejected: %v", err)
	})
}

// TestEncryptRejectsKMSCreationRule: a matching .sops.yaml rule that names
// a key type the age-only fork does not carry is rejected before any key
// is generated, with the fork's UnsupportedKeyTypeError. No CLI needed.
func TestEncryptRejectsKMSCreationRule(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".sops.yaml")
	write(t, cfg, "creation_rules:\n  - path_regex: 'Secret\\..*'\n    kms: 'arn:aws:kms:eu-central-1:123456789012:key/00000000-0000-0000-0000-000000000000'\n")
	plain := filepath.Join(dir, "Secret.app.default.TOKEN")
	if err := os.WriteFile(plain, crossToolPlaintext, 0o600); err != nil {
		t.Fatal(err)
	}
	enc := plain + ".enc"

	err := sopsEncryptFile(cfg, plain, enc)
	if err == nil {
		t.Fatal("sopsEncryptFile accepted a kms: creation rule on the age-only build")
	}
	var unsupported *keys.UnsupportedKeyTypeError
	if !errors.As(err, &unsupported) || unsupported.KeyType != "kms" {
		t.Fatalf("want keys.UnsupportedKeyTypeError{kms}, got %T: %v", err, err)
	}
	if want := "unsupported key type kms (age-only build)"; err.Error() != want {
		t.Fatalf("error text:\n got %q\nwant %q", err.Error(), want)
	}
	if _, statErr := os.Stat(enc); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a rejected encrypt must not leave %s behind (stat: %v)", enc, statErr)
	}
}

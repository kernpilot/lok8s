package secrets

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"

	"github.com/kernpilot/lok8s/internal/config"
)

// testEnv builds a throwaway project and a Context writing into buffers.
func testEnv(t *testing.T) (*Context, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	// Isolate every ambient identity source.
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("SOPS_AGE_KEY", "")
	os.Unsetenv("SOPS_AGE_KEY")
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(base, "no-keys.txt"))
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	c := &Context{
		Paths: &config.Paths{
			Base:     base,
			Bin:      filepath.Join(base, ".bin"),
			Lok8s:    filepath.Join(base, ".lok8s"),
			Clusters: filepath.Join(base, "clusters"),
		},
		Out:          out,
		ErrOut:       errOut,
		Stdin:        strings.NewReader(""),
		StdinIsTTY:   func() bool { return false },
		ReadPassword: func() (string, error) { return "", nil },
	}
	return c, out, errOut
}

// testAgeIdentity generates an age keypair and installs it: recipient into
// .sops.yaml, identity into SOPS_AGE_KEY.
func testAgeIdentity(t *testing.T, c *Context) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sopsYAML := "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + id.Recipient().String() + "'\n"
	if err := os.WriteFile(c.sopsConfigPath(), []byte(sopsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

// testSSHKeypair writes an ed25519 SSH keypair and returns (privPath, pubPath).
func testSSHKeypair(t *testing.T, dir string) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	privPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(privPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pubPath := privPath + ".pub"
	if err := os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		t.Fatal(err)
	}
	return privPath, pubPath
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ── store resolution ─────────────────────────────────────

func TestStorePathResolution(t *testing.T) {
	c, _, _ := testEnv(t)
	flat := c.Paths.Base + "/.secrets"

	// No domain → flat store.
	if got := c.StorePath(); got != flat {
		t.Errorf("flat: got %s want %s", got, flat)
	}
	// Domain without its own store dir → still the flat store (no fallback
	// TIER, but the flat store IS the resolution when no per-domain dir
	// exists).
	c.Domain = "a.dev"
	if got := c.StorePath(); got != flat {
		t.Errorf("domain w/o store: got %s want %s", got, flat)
	}
	// Domain with its own store → that store exclusively.
	own := c.Paths.Clusters + "/a.dev/secrets"
	os.MkdirAll(own, 0o755)
	if got := c.StorePath(); got != own {
		t.Errorf("domain store: got %s want %s", got, own)
	}
	// PATH_SECRETS overrides the flat default.
	c.Domain = ""
	c.Paths.SecretsEnv = c.Paths.Base + "/custom"
	if got := c.StorePath(); got != c.Paths.Base+"/custom" {
		t.Errorf("PATH_SECRETS: got %s", got)
	}
}

func TestEnsureStoreScaffoldsOnlyExistingDomains(t *testing.T) {
	c, _, _ := testEnv(t)
	c.Domain = "a.dev"
	// Domain dir absent → no scaffold.
	if err := c.ensureStore(); err != nil {
		t.Fatal(err)
	}
	if isDir(c.Paths.Clusters + "/a.dev/secrets") {
		t.Error("scaffolded a store for a nonexistent domain")
	}
	// Domain dir present → store created (so the first write lands there,
	// not in the flat fallback).
	os.MkdirAll(c.Paths.Clusters+"/a.dev", 0o755)
	if err := c.ensureStore(); err != nil {
		t.Fatal(err)
	}
	if !isDir(c.Paths.Clusters + "/a.dev/secrets") {
		t.Error("store not scaffolded")
	}
}

// ── init ─────────────────────────────────────────────────

func TestInitGolden(t *testing.T) {
	c, out, _ := testEnv(t)
	_, pubPath := testSSHKeypair(t, c.Paths.Base)

	if err := c.Init(pubPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(c.sopsConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{
		"# SOPS encryption config for lok8s secret cache files.\n",
		"# Recipients are age public keys derived from SSH keys via ssh-to-age.\n",
		"# Add team members with: lo secrets add-key <ssh-pubkey-path|age1…>\n",
		"creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: 'age1",
	} {
		if !strings.Contains(content, want) {
			t.Errorf(".sops.yaml missing %q:\n%s", want, content)
		}
	}
	if !strings.Contains(out.String(), "Created "+c.sopsConfigPath()) ||
		!strings.Contains(out.String(), "Age public key: age1") ||
		!strings.Contains(out.String(), "Derived from: "+pubPath) {
		t.Errorf("init output:\n%s", out.String())
	}
	// The flat store + gitignore were scaffolded.
	gi, err := os.ReadFile(c.Paths.Base + "/.secrets/.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if string(gi) != gitignoreContent {
		t.Errorf(".gitignore not byte-exact:\n%s", gi)
	}

	// Idempotent second run.
	out.Reset()
	if err := c.Init(pubPath); err != nil {
		t.Fatal(err)
	}
	if out.String() != "age key already configured in .sops.yaml\n" {
		t.Errorf("second init: %q", out.String())
	}
}

func TestInitMissingKey(t *testing.T) {
	c, _, errOut := testEnv(t)
	if err := c.Init(c.Paths.Base + "/nope"); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "SSH public key not found:") ||
		!strings.Contains(errOut.String(), "Generate one with: ssh-keygen -t ed25519") {
		t.Errorf("stderr: %s", errOut.String())
	}
}

// ── add-key ──────────────────────────────────────────────

const (
	keyA = "age1zvkyg2lc7kjhpnjwqpjkwzr9qkxnwqp5xzdmqvxqjhqx0nqwqjqs3xqzq7"
	keyB = "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqz"
)

func sopsYAMLWith(t *testing.T, c *Context, key string) {
	t.Helper()
	write(t, c.sopsConfigPath(), "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '"+key+"'\n")
}

func TestAddKeyAppendsCommaSeparated(t *testing.T) {
	c, out, _ := testEnv(t)
	sopsYAMLWith(t, c, keyA)
	os.MkdirAll(c.Paths.Base+"/.secrets", 0o755)

	if err := c.AddKey(keyB, false, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.sopsConfigPath())
	if !strings.Contains(string(raw), keyA+","+keyB) {
		t.Errorf("not a comma-separated recipient list:\n%s", raw)
	}
	if !strings.Contains(out.String(), "added recipient to .sops.yaml (1 creation rule(s))") {
		t.Errorf("stdout: %s", out.String())
	}
}

func TestAddKeyIdempotent(t *testing.T) {
	c, out, _ := testEnv(t)
	sopsYAMLWith(t, c, keyA)
	if err := c.AddKey(keyA, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recipient already present in .sops.yaml — nothing to add") {
		t.Errorf("stdout: %s", out.String())
	}
	raw, _ := os.ReadFile(c.sopsConfigPath())
	if strings.Count(string(raw), keyA) != 1 {
		t.Error("key duplicated")
	}
}

func TestAddKeyRejectsMalformed(t *testing.T) {
	c, _, errOut := testEnv(t)
	sopsYAMLWith(t, c, keyA)
	before, _ := os.ReadFile(c.sopsConfigPath())
	if err := c.AddKey("age1nope", false, false); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "not a valid age public key: age1nope") {
		t.Errorf("stderr: %s", errOut.String())
	}
	after, _ := os.ReadFile(c.sopsConfigPath())
	if !bytes.Equal(before, after) {
		t.Error(".sops.yaml modified despite rejection")
	}
}

func TestAddKeyOrphanFailsClosed(t *testing.T) {
	c, _, errOut := testEnv(t)
	c.Domain = "a.dev"
	sopsYAMLWith(t, c, keyA)
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.orphan.default.K.enc", "enc")

	if err := c.AddKey(keyB, false, false); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "no decrypted twin") {
		t.Errorf("stderr: %s", errOut.String())
	}
	// The orphan path is store-relative to PATH_BASE.
	if !strings.Contains(errOut.String(), "clusters/a.dev/secrets/Secret.orphan.default.K.enc") {
		t.Errorf("orphan path not relative: %s", errOut.String())
	}
}

func TestAddKeySkipOrphansRekeys(t *testing.T) {
	c, out, _ := testEnv(t)
	c.Domain = "a.dev"
	testAgeIdentity(t, c)
	// The added recipient must be REAL: the re-key sweep re-encrypts to the
	// updated recipient list, and sops rejects a recipient with a bad
	// checksum (the keyB fixture is shape-valid only).
	second, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.orphan.default.K.enc", "enc")
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.app.default.K", "v")

	if err := c.AddKey(second.Recipient().String(), false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "re-keying 1 cache file(s) across 1 store(s)…") {
		t.Errorf("stdout: %s", out.String())
	}
	if !strings.Contains(out.String(), "Encrypted 1 secret(s)") ||
		!strings.Contains(out.String(), "done — commit .sops.yaml and the updated .enc files together") {
		t.Errorf("stdout: %s", out.String())
	}
	if !isFile(c.Paths.Clusters + "/a.dev/secrets/Secret.app.default.K.enc") {
		t.Error("re-key did not produce the .enc")
	}
}

// ── set ──────────────────────────────────────────────────

func TestSetWritesCache(t *testing.T) {
	c, out, errOut := testEnv(t)
	if err := c.Set("app", "default", "KEY", "literal-v", false); err != nil {
		t.Fatal(err)
	}
	cache := c.Paths.Base + "/.secrets/Secret.app.default.KEY"
	raw, err := os.ReadFile(cache)
	if err != nil || string(raw) != "literal-v" {
		t.Fatalf("cache: %q err %v", raw, err)
	}
	info, _ := os.Stat(cache)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	if out.String() != "Set app/default/KEY\n" {
		t.Errorf("stdout: %q", out.String())
	}
	// No .sops.yaml → no plaintext-only warning.
	if strings.Contains(errOut.String(), "wrote plaintext cache only") {
		t.Errorf("unexpected warning: %s", errOut.String())
	}
}

func TestSetStdinAndErrors(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Stdin = strings.NewReader("piped-value\n\n")
	if err := c.Set("app", "default", "P", "", false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.Paths.Base + "/.secrets/Secret.app.default.P")
	if string(raw) != "piped-value" {
		t.Errorf("stdin trailing newlines not stripped: %q", raw)
	}
	_ = out

	// "-" reads stdin too (argsh :~stdin).
	c.Stdin = strings.NewReader("dash-v\n")
	if err := c.Set("app", "default", "D", "-", false); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(c.Paths.Base + "/.secrets/Secret.app.default.D")
	if string(raw) != "dash-v" {
		t.Errorf("dash: %q", raw)
	}

	// Empty everything → Empty value.
	c.Stdin = strings.NewReader("")
	errOut.Reset()
	if err := c.Set("app", "default", "E", "", false); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "Empty value") {
		t.Errorf("stderr: %s", errOut.String())
	}

	errOut.Reset()
	if err := c.Set("", "default", "K", "v", false); err != ErrPrinted || !strings.Contains(errOut.String(), "Secret --name is required") {
		t.Fatalf("name check: %v %s", err, errOut.String())
	}
	errOut.Reset()
	if err := c.Set("app", "default", "", "v", false); err != ErrPrinted || !strings.Contains(errOut.String(), "Key argument is required") {
		t.Fatalf("key check: %v %s", err, errOut.String())
	}
}

func TestSetWarnsWhenSopsConfigured(t *testing.T) {
	c, out, errOut := testEnv(t)
	write(t, c.sopsConfigPath(), "")
	if err := c.Set("app", "default", "KEY", "v", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "wrote plaintext cache only — no matching .enc for this value (missing or now stale); run 'lo secrets encrypt' or re-run with --encrypt/-e before committing") {
		t.Errorf("stderr: %s", errOut.String())
	}
	if out.String() != "Set app/default/KEY\n" {
		t.Errorf("stdout: %q", out.String())
	}
}

func TestSetEncryptOnlyThatFile(t *testing.T) {
	c, out, _ := testEnv(t)
	testAgeIdentity(t, c)
	// A pre-existing sibling plaintext with NO .enc — must stay
	// plaintext-only (never a whole-store sweep).
	write(t, c.Paths.Base+"/.secrets/Secret.other.default.SIB", "sib")

	if err := c.Set("app", "default", "KEY", "round-trip-me", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Set + encrypted app/default/KEY") {
		t.Errorf("stdout: %s", out.String())
	}
	if !isFile(c.Paths.Base + "/.secrets/Secret.app.default.KEY.enc") {
		t.Error("no .enc produced")
	}
	if isFile(c.Paths.Base + "/.secrets/Secret.other.default.SIB.enc") {
		t.Error("sibling swept up")
	}

	// A sweep right after `set --encrypt` must treat the just-written .enc as
	// fresh (the in-process encrypt can land plaintext and .enc in the same
	// clock tick; the -nt skip needs the .enc strictly newer — see
	// nudgeNewer). Only the sibling is due.
	out.Reset()
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Encrypted 1 secret(s)") {
		t.Errorf("sweep after set --encrypt re-encrypted the fresh file: %q", out.String())
	}
}

func TestSetEncryptWithoutSopsYAMLFails(t *testing.T) {
	c, _, errOut := testEnv(t)
	if err := c.Set("app", "default", "KEY", "v", true); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "No .sops.yaml found — run: lo secrets init") {
		t.Errorf("stderr: %s", errOut.String())
	}
	// Cache still written (plaintext), but no .enc produced.
	if !isFile(c.Paths.Base + "/.secrets/Secret.app.default.KEY") {
		t.Error("plaintext cache missing")
	}
	if isFile(c.Paths.Base + "/.secrets/Secret.app.default.KEY.enc") {
		t.Error(".enc produced without config")
	}
}

// ── encrypt / decrypt round trip ─────────────────────────

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, out, _ := testEnv(t)
	id := testAgeIdentity(t, c)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.app.default.TOKEN", "super-secret-value")
	write(t, store+"/Secret.gen.default.X.sha", "deadbeef")

	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	// The sweep includes .sha files (they get .sha.enc twins).
	if !isFile(store+"/Secret.app.default.TOKEN.enc") || !isFile(store+"/Secret.gen.default.X.sha.enc") {
		t.Fatal("missing .enc twins")
	}
	if !strings.Contains(out.String(), "Encrypted 2 secret(s)") {
		t.Errorf("stdout: %s", out.String())
	}
	gi, err := os.ReadFile(store + "/.gitignore")
	if err != nil || !strings.Contains(string(gi), "!Secret.*.enc") {
		t.Errorf(".gitignore missing: %v %s", err, gi)
	}

	// Drop the plaintext, then restore it from the .enc with the identity.
	os.Remove(store + "/Secret.app.default.TOKEN")
	os.Remove(store + "/Secret.gen.default.X.sha")
	t.Setenv("SOPS_AGE_KEY", id.String())
	out.Reset()
	if err := c.Decrypt("/nonexistent-ssh-key"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Decrypted 2 secret(s)") {
		t.Errorf("stdout: %s", out.String())
	}
	raw, _ := os.ReadFile(store + "/Secret.app.default.TOKEN")
	if string(raw) != "super-secret-value" {
		t.Errorf("round trip: %q", raw)
	}
	info, _ := os.Stat(store + "/Secret.app.default.TOKEN")
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
}

func TestEncryptFreshnessSkip(t *testing.T) {
	c, out, _ := testEnv(t)
	testAgeIdentity(t, c)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.app.default.K", "v")
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	encInfo1, _ := os.Stat(store + "/Secret.app.default.K.enc")

	// Second sweep: .enc newer → skipped, silent on stdout.
	out.Reset()
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Errorf("expected silent skip, got %q", out.String())
	}
	encInfo2, _ := os.Stat(store + "/Secret.app.default.K.enc")
	if !encInfo1.ModTime().Equal(encInfo2.ModTime()) {
		t.Error("up-to-date .enc was rewritten")
	}

	// Touch the plaintext → re-encrypted.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(store+"/Secret.app.default.K", future, future)
	out.Reset()
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Encrypted 1 secret(s)") {
		t.Errorf("stale plaintext not re-encrypted: %q", out.String())
	}
}

func TestEncryptNameFilter(t *testing.T) {
	c, out, errOut := testEnv(t)
	testAgeIdentity(t, c)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.alpha.default.T", "a")
	write(t, store+"/Secret.beta.default.T", "b")

	if err := c.Encrypt("alpha"); err != nil {
		t.Fatal(err)
	}
	if !isFile(store + "/Secret.alpha.default.T.enc") {
		t.Error("alpha not encrypted")
	}
	if isFile(store + "/Secret.beta.default.T.enc") {
		t.Error("beta swept up")
	}
	_ = out

	// A name that matches NOTHING fails loudly.
	errOut.Reset()
	if err := c.Encrypt("alphaa"); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "no cache files for Secret 'alphaa' in") {
		t.Errorf("stderr: %s", errOut.String())
	}

	// A name that could escape the store is rejected.
	errOut.Reset()
	if err := c.Encrypt("../../etc"); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "invalid --name '../../etc' — a Secret name is [a-zA-Z0-9][a-zA-Z0-9._-]*") {
		t.Errorf("stderr: %s", errOut.String())
	}
}

func TestDecryptViaSSHKeyDerivation(t *testing.T) {
	c, out, _ := testEnv(t)
	privPath, pubPath := testSSHKeypair(t, c.Paths.Base)

	// Encrypt to the recipient derived from the SSH public key…
	pub, _, err := deriveAgePublicKey(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	sopsYAMLWith(t, c, pub)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.app.default.K", "ssh-derived")
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}

	// …and decrypt via the private-key derivation chain (no SOPS_AGE_KEY, no
	// keys.txt).
	os.Remove(store + "/Secret.app.default.K")
	out.Reset()
	if err := c.Decrypt(privPath); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(store + "/Secret.app.default.K")
	if string(raw) != "ssh-derived" {
		t.Errorf("round trip: %q", raw)
	}
}

func TestDecryptIdentityErrors(t *testing.T) {
	c, _, errOut := testEnv(t)
	os.MkdirAll(c.Paths.Base+"/.secrets", 0o755)

	// Missing SSH key.
	if err := c.Decrypt(c.Paths.Base + "/no-key"); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "SSH private key not found: "+c.Paths.Base+"/no-key") {
		t.Errorf("stderr: %s", errOut.String())
	}

	// Underivable key → the passphrase guidance, with NO key material echoed.
	errOut.Reset()
	canary := "CANARY_SECRET_MATERIAL_DO_NOT_LEAK"
	badKey := c.Paths.Base + "/bad_key"
	write(t, badKey, "-----BEGIN OPENSSH PRIVATE KEY-----\n"+canary+"\n-----END OPENSSH PRIVATE KEY-----\n")
	if err := c.Decrypt(badKey); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	// Exact stderr: the six ported [error] lines and nothing else (same
	// containment rationale as TestAddKeyContainment).
	e := "\033[0;31m[error]\033[0m "
	want := e + "Failed to derive age key from " + badKey + "\n" +
		e + "If your SSH key is passphrase-protected, derive once:\n" +
		e + "  mkdir -p ~/.config/sops/age\n" +
		e + "  ssh-to-age -private-key -i " + badKey + " > ~/.config/sops/age/keys.txt\n" +
		e + "  chmod 600 ~/.config/sops/age/keys.txt\n" +
		e + "(you'll be prompted for the passphrase once)\n"
	if errOut.String() != want {
		t.Errorf("stderr not contained to the ported messages:\n%q\nwant:\n%q", errOut.String(), want)
	}
	if strings.Contains(errOut.String(), canary) {
		t.Error("private key material leaked to stderr")
	}
}

// TestAddKeyContainment: a mistakenly-passed PRIVATE key must never surface
// (the ssh-to-age binary echoes its whole input on failure; the port must
// not, see tests/unit/secret_material_containment_test.bats).
func TestAddKeyContainment(t *testing.T) {
	c, out, errOut := testEnv(t)
	sopsYAMLWith(t, c, keyA)
	canary := "CANARY_SECRET_MATERIAL_DO_NOT_LEAK"
	keyPath := c.Paths.Base + "/private"
	write(t, keyPath, "-----BEGIN OPENSSH PRIVATE KEY-----\n"+canary+"\n-----END OPENSSH PRIVATE KEY-----\n")

	if err := c.AddKey(keyPath, false, false); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, canary) {
		t.Error("private key material leaked")
	}
	// Exact stderr: the two [error] lines and NOTHING else. The Go library's
	// own parse errors happen not to embed input material today, so a
	// canary-only assertion would pass even if the raw error were surfaced —
	// this pins that nothing beyond the ported messages reaches the operator.
	want := "\033[0;31m[error]\033[0m could not derive an age key from " + keyPath + "\n" +
		"\033[0;31m[error]\033[0m only ed25519 SSH keys are supported, and it must be the PUBLIC key\n"
	if errOut.String() != want {
		t.Errorf("stderr not contained to the ported messages:\n%q\nwant:\n%q", errOut.String(), want)
	}
	if out.String() != "" {
		t.Errorf("stdout not empty: %q", out.String())
	}
}

// ── allow / list / print / env ───────────────────────────

func TestAllow(t *testing.T) {
	c, out, _ := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	os.MkdirAll(store, 0o755)

	if err := c.Allow(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "No bash: entries found (no .sha files)\n" {
		t.Errorf("stdout: %q", out.String())
	}

	write(t, store+"/Secret.gen.default.X.sha", "deadbeef  \n")
	write(t, store+"/other.sha", "cafe\n")
	write(t, store+"/dup.sha", "cafe\n")
	out.Reset()
	if err := c.Allow(); err != nil {
		t.Fatal(err)
	}
	want := "Approved 3 bash: entry/entries\n  Secret.gen.default.X.sha\n  dup.sha\n  other.sha\n"
	if out.String() != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), want)
	}
	allow, err := os.ReadFile(store + "/.bash-allow")
	if err != nil {
		t.Fatal(err)
	}
	if string(allow) != "cafe\ndeadbeef\n" {
		t.Errorf(".bash-allow: %q", allow)
	}
	info, _ := os.Stat(store + "/.bash-allow")
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
}

func TestListGolden(t *testing.T) {
	c, out, errOut := testEnv(t)
	store := c.Paths.Base + "/.secrets"

	// Missing store → warn, exit 0.
	if err := c.List(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "Secrets directory not found: "+store) {
		t.Errorf("stderr: %s", errOut.String())
	}

	write(t, store+"/Secret.a.default.K", "v")
	write(t, store+"/Secret.a.default.K.enc", "e")
	write(t, store+"/Secret.b.default.K", "v")
	write(t, store+"/Secret.c.default.K.enc", "e")
	write(t, store+"/Secret.s.default.X.sha", "h")
	out.Reset()
	if err := c.List(); err != nil {
		t.Fatal(err)
	}
	want := "Secret.a.default.K (encrypted)\n" +
		"Secret.b.default.K (plaintext)\n" +
		"Secret.s.default.X.sha (plaintext)\n" +
		"Secret.c.default.K.enc (needs decrypt)\n"
	if out.String() != want {
		t.Errorf("list:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestPrintGolden(t *testing.T) {
	c, out, errOut := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.app.default.ML", "line1\nline2")
	write(t, store+"/Secret.app.default.ONE", "raw-value")
	write(t, store+"/Secret.app.default.ONE.enc", "x")

	// Single match → raw cat, no decoration.
	if err := c.Print([]string{"ONE"}, false, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "raw-value" {
		t.Errorf("single: %q", out.String())
	}

	// Multiple matches → green basename + content + two newlines each.
	out.Reset()
	if err := c.Print([]string{"app"}, false, false); err != nil {
		t.Fatal(err)
	}
	want := "\033[0;32mSecret.app.default.ML\033[0m\nline1\nline2\n\n" +
		"\033[0;32mSecret.app.default.ONE\033[0m\nraw-value\n\n"
	if out.String() != want {
		t.Errorf("multi:\n%q\nwant:\n%q", out.String(), want)
	}

	// only-one with multiple → error to stderr, green FULL paths to stdout.
	out.Reset()
	errOut.Reset()
	if err := c.Print([]string{"app"}, true, false); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "Multiple matches found:") {
		t.Errorf("stderr: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "\033[0;32m"+store+"/Secret.app.default.ML\033[0m\n") {
		t.Errorf("stdout: %q", out.String())
	}

	// Zero matches, only-one → SAME quirk branch (empty list), preserved
	// from bash.
	out.Reset()
	errOut.Reset()
	if err := c.Print([]string{"zzz"}, true, false); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "Multiple matches found:") || out.String() != "" {
		t.Errorf("quirk: out=%q err=%s", out.String(), errOut.String())
	}

	// Zero matches → No matches found.
	errOut.Reset()
	if err := c.Print([]string{"zzz"}, false, false); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "No matches found") {
		t.Errorf("stderr: %s", errOut.String())
	}

	// Patterns AND-match case-insensitively.
	out.Reset()
	if err := c.Print([]string{"ml", "APP"}, false, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "line1\nline2" {
		t.Errorf("and-match: %q", out.String())
	}
}

func TestEnvGolden(t *testing.T) {
	c, out, errOut := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.hetzner.default.HCLOUD_TOKEN", "tok en$1")
	write(t, store+"/Secret.hetzner.default.PLAIN", "simple")
	write(t, store+"/Secret.hetzner.default.PLAIN.enc", "x")
	write(t, store+"/Secret.hetzner.default.SKIP.sha", "h")
	write(t, store+"/Secret.hetzner.other.NOPE", "other-ns")

	if err := c.Env("hetzner", "default"); err != nil {
		t.Fatal(err)
	}
	want := "export HCLOUD_TOKEN=tok\\ en\\$1\nexport PLAIN=simple\n"
	if out.String() != want {
		t.Errorf("env:\n%q\nwant:\n%q", out.String(), want)
	}

	errOut.Reset()
	if err := c.Env("nope", "default"); err != ErrPrinted {
		t.Fatalf("want ErrPrinted, got %v", err)
	}
	if !strings.Contains(errOut.String(), "No cached keys for nope/default in "+store) {
		t.Errorf("stderr: %s", errOut.String())
	}

	errOut.Reset()
	if err := c.Env("", "default"); err != ErrPrinted || !strings.Contains(errOut.String(), "Secret --name is required") {
		t.Fatalf("name check: %v %s", err, errOut.String())
	}
}

// ── checks (lint hooks) ──────────────────────────────────

func TestCheckUnencrypted(t *testing.T) {
	c, _, _ := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	var warn bytes.Buffer

	if !CheckUnencrypted(store, &warn) {
		t.Error("missing dir must pass")
	}
	write(t, store+"/Secret.a.default.K", "v")
	if CheckUnencrypted(store, &warn) {
		t.Error("unencrypted secret must fail")
	}
	if !strings.Contains(warn.String(), "Unencrypted secret: Secret.a.default.K — run: lo secrets encrypt") {
		t.Errorf("warn: %s", warn.String())
	}

	write(t, store+"/Secret.a.default.K.enc", "e")
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(store+"/Secret.a.default.K.enc", future, future)
	warn.Reset()
	if !CheckUnencrypted(store, &warn) {
		t.Errorf("fresh .enc must pass: %s", warn.String())
	}

	os.Chtimes(store+"/Secret.a.default.K", future.Add(2*time.Second), future.Add(2*time.Second))
	warn.Reset()
	if CheckUnencrypted(store, &warn) {
		t.Error("stale .enc must fail")
	}
	if !strings.Contains(warn.String(), "Secret changed since last encrypt: Secret.a.default.K — run: lo secrets encrypt") {
		t.Errorf("warn: %s", warn.String())
	}
}

func TestCheckFlatShadows(t *testing.T) {
	c, _, _ := testEnv(t)
	flat := c.Paths.Base + "/.secrets"
	domDir := c.Paths.Clusters + "/app.example.com"
	os.MkdirAll(flat, 0o755)
	os.MkdirAll(domDir+"/secrets", 0o755)
	var out bytes.Buffer

	// Clean: a global-only flat secret is not a shadow.
	write(t, domDir+"/secrets/Secret.app.default.TOKEN", "v")
	write(t, flat+"/Secret.registries-tls.lok8s-system.tls.crt", "global")
	if !CheckFlatShadows(flat, domDir, &out) || out.Len() != 0 {
		t.Errorf("clean case: %s", out.String())
	}

	// Identical duplicate → shadow.
	write(t, flat+"/Secret.app.default.TOKEN", "v")
	out.Reset()
	if CheckFlatShadows(flat, domDir, &out) {
		t.Error("shadow must fail")
	}
	if !strings.Contains(out.String(), "Flat-store shadow: Secret.app.default.TOKEN") {
		t.Errorf("out: %s", out.String())
	}

	// Divergent duplicate → DRIFT.
	write(t, flat+"/Secret.app.default.TOKEN", "STALE")
	out.Reset()
	if CheckFlatShadows(flat, domDir, &out) {
		t.Error("drift must fail")
	}
	if !strings.Contains(out.String(), "Flat-store DRIFT: Secret.app.default.TOKEN") {
		t.Errorf("out: %s", out.String())
	}

	// .enc/.sha siblings ignored.
	os.Remove(flat + "/Secret.app.default.TOKEN")
	write(t, domDir+"/secrets/Secret.app.default.TOKEN.enc", "e")
	write(t, domDir+"/secrets/Secret.app.default.TOKEN.sha", "h")
	write(t, flat+"/Secret.app.default.TOKEN.enc", "e")
	write(t, flat+"/Secret.app.default.TOKEN.sha", "h")
	out.Reset()
	if !CheckFlatShadows(flat, domDir, &out) || out.Len() != 0 {
		t.Errorf("enc/sha siblings: %s", out.String())
	}
}

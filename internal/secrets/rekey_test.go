package secrets

// rekey_test.go: add-key and remove-key share the re-key helpers in
// rekey.go. The add-key cases pin its whole output and its effects, so the
// shared code cannot change add-key; the remove-key cases prove the removal
// with real throwaway age identities: after it, the removed identity cannot
// decrypt the store, and the others can.

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

// newAgeIdentity makes a throwaway age identity. Its private half stays in
// the test: no output, name or message carries it.
func newAgeIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// ── add-key: the output and the effects stay as they were ─────────────────

// The re-key path with --all and --skip-orphans: every line, the edited
// .sops.yaml, and the scope of the sweep. The sweep re-encrypts the context's
// store only (the other store's plaintext is touched, but gets no .enc); this
// change keeps that behaviour of add-key.
func TestAddKeyOutputIsPinned(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	first := testAgeIdentity(t, c)
	second := newAgeIdentity(t)
	a := c.Paths.Clusters + "/a.dev/secrets"
	b := c.Paths.Clusters + "/b.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	write(t, a+"/Secret.orphan.default.K.enc", "enc")
	write(t, b+"/Secret.app.default.K", "w")

	if err := c.AddKey(second.Recipient().String(), true, true); err != nil {
		t.Fatal(err)
	}
	wantOut := "added recipient to .sops.yaml (1 creation rule(s))\n" +
		"  " + second.Recipient().String() + "\n" +
		"re-keying 2 cache file(s) across 2 store(s)…\n" +
		"Encrypted 1 secret(s)\n" +
		"done — commit .sops.yaml and the updated .enc files together\n"
	if out.String() != wantOut {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), wantOut)
	}
	wantErr := "[warn] orphan (no decrypted twin, cannot re-key): clusters/a.dev/secrets/Secret.orphan.default.K.enc\n"
	if errOut.String() != wantErr {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), wantErr)
	}
	wantYAML := "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" +
		first.Recipient().String() + "," + second.Recipient().String() + "'\n"
	if got := readFile(t, c.sopsConfigPath()); got != wantYAML {
		t.Errorf(".sops.yaml:\n%q\nwant:\n%q", got, wantYAML)
	}
	// A fixed text on failure: the decrypted value and the library error stay
	// out of the test output.
	dec, err := sopsDecryptData([]byte(readFile(t, a+"/Secret.app.default.K.enc")), second.String())
	if err != nil || string(dec) != "v" {
		t.Error("the added identity cannot read the re-keyed store")
	}
	if fsutil.Exists(b + "/Secret.app.default.K.enc") {
		t.Error("add-key --all now re-encrypts a store other than the context's: its behaviour changed")
	}
}

// The orphan refusal comes AFTER the .sops.yaml write: the order this change
// keeps for add-key.
func TestAddKeyRefusalOutputIsPinned(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	sopsYAMLWith(t, c, keyA)
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.orphan.default.K.enc", "enc")

	if err := c.AddKey(keyB, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	wantOut := "added recipient to .sops.yaml (1 creation rule(s))\n  " + keyB + "\n"
	if out.String() != wantOut {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), wantOut)
	}
	wantErr := "[warn] orphan (no decrypted twin, cannot re-key): clusters/a.dev/secrets/Secret.orphan.default.K.enc\n" +
		"[error] 1 encrypted file(s) have no decrypted twin — they would keep the OLD\n" +
		"[error]   recipient list while .sops.yaml claims the new key was added.\n" +
		"[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.\n"
	if errOut.String() != wantErr {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), wantErr)
	}
	if !strings.Contains(readFile(t, c.sopsConfigPath()), keyA+","+keyB) {
		t.Error("add-key no longer writes .sops.yaml before the orphan refusal: its behaviour changed")
	}
}

// --all without a domain store: the refusal comes after the write too.
func TestAddKeyNoStoreOutputIsPinned(t *testing.T) {
	c, out, errOut := testEnv(t)
	sopsYAMLWith(t, c, keyA)

	if err := c.AddKey(keyB, true, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	if want := "added recipient to .sops.yaml (1 creation rule(s))\n  " + keyB + "\n"; out.String() != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), want)
	}
	if want := "[error] no secret store found to re-key\n"; errOut.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
	}
}

// ── the private-key refusal (both commands) ───────────────────────────────

// fakeAgeIdentity has the shape of an age identity but is made of a fixed
// filler character that is not in the bech32 alphabet: not a key, and not
// a match for a secret scanner.
var fakeAgeIdentity = "AGE-SECRET-KEY-1" + strings.Repeat("O", 58)

// A private key pasted as the argument is refused, and no line echoes it.
// Before this check the not-a-file error printed the whole argument.
func TestKeyArgumentRefusesAPrivateKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Context, string) error
	}{
		{"add-key", func(c *Context, k string) error { return c.AddKey(k, false, false) }},
		{"remove-key", func(c *Context, k string) error { return c.RemoveKey(k, false, false) }},
	} {
		for _, arg := range []string{fakeAgeIdentity, strings.ToLower(fakeAgeIdentity), "-----BEGIN OPENSSH PRIVATE KEY-----", "-----BEGIN"} {
			t.Run(tc.name, func(t *testing.T) {
				c, out, errOut := testEnv(t)
				sopsYAMLWith(t, c, keyA+","+keyB)
				before := readFile(t, c.sopsConfigPath())

				if err := tc.run(c, arg); !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v", err)
				}
				want := "[error] the key argument looks like a private key — it is not printed\n" +
					"[error] pass an age public key (age1…) or the path to an ed25519 SSH public key\n"
				if errOut.String() != want || out.String() != "" {
					t.Errorf("output not contained to the two refusal lines: stdout %d bytes, stderr %d bytes", out.Len(), errOut.Len())
				}
				if strings.Contains(out.String()+errOut.String(), arg) {
					t.Error("the argument was echoed")
				}
				if readFile(t, c.sopsConfigPath()) != before {
					t.Error(".sops.yaml changed")
				}
			})
		}
	}
}

// ── remove-key ────────────────────────────────────────────────────────────

// sopsRules writes a .sops.yaml with one creation rule per recipient list. Only
// the first rule matches the cache files; the others must be edited all the
// same.
func sopsRules(t *testing.T, c *Context, lists ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("creation_rules:\n")
	for i, l := range lists {
		re := `Secret\..*`
		if i > 0 {
			re = "Other" + strings.Repeat("x", i) + `\..*`
		}
		b.WriteString("  - path_regex: '" + re + "'\n    age: '" + l + "'\n")
	}
	write(t, c.sopsConfigPath(), b.String())
}

// encryptStore encrypts a domain's store as it stands, under the current
// .sops.yaml, and clears the output buffers.
func encryptStore(t *testing.T, c *Context, domain string, out, errOut *bytes.Buffer) {
	t.Helper()
	saved := c.Domain
	c.Domain = domain
	if err := c.Encrypt(""); err != nil {
		t.Fatalf("encrypt %s: %v\n%s", domain, err, errOut.String())
	}
	c.Domain = saved
	out.Reset()
	errOut.Reset()
}

// opens reports whether the identity decrypts the .enc to the plaintext.
func opens(t *testing.T, enc string, id *age.X25519Identity, plain string) bool {
	t.Helper()
	dec, err := sopsDecryptData([]byte(readFile(t, enc)), id.String())
	return err == nil && string(dec) == plain
}

// freshen makes each plaintext one second newer than its .enc: the state
// after `lo secrets decrypt`. remove-key refuses a cache file older than its
// .enc.
func freshen(t *testing.T, plains ...string) {
	t.Helper()
	for _, p := range plains {
		info, err := os.Stat(p + ".enc")
		if err != nil {
			t.Fatal(err)
		}
		ts := info.ModTime().Add(time.Second)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
}

const rotateWarn = "[warn] a removal does not revoke what the key already read (older .enc files in git history included): rotate those secrets\n"

// scopeWarn is the scope line of the own-identity check, which every
// remove-key run that reaches the check prints, with a match or not. The
// bats twin pins the same text.
const scopeWarn = "[warn] the check for your own decrypt identity reads only SOPS_AGE_KEY_FILE (default ~/.config/sops/age/keys.txt) and the .pub of LOK8S_SSH_KEY (default ~/.ssh/id_ed25519); it does not read SOPS_AGE_KEY, SOPS_AGE_KEY_CMD or other files\n"

// The removal edits every creation rule and re-keys the store: after it the
// removed identity cannot open the store, and the one that stays can. Only
// the .enc files that listed the recipient are re-encrypted: a new plaintext
// gets no .enc. .sops.yaml is replaced, not rewritten in place: a hard link
// to the old file keeps the old content.
func TestRemoveKeyRekeysEveryRule(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g, g+", "+k)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	write(t, a+"/Secret.new.default.K", "n")
	if !opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
		t.Fatal("fixture: the identity to remove cannot open the store before the removal")
	}
	oldYAML := readFile(t, c.sopsConfigPath())
	link := c.Paths.Base + "/old.sops.yaml"
	if err := os.Link(c.sopsConfigPath(), link); err != nil {
		t.Fatal(err)
	}

	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	wantOut := "removed recipient from .sops.yaml (2 creation rule(s))\n" +
		"  " + g + "\n" +
		"re-keying 1 cache file(s) across 1 store(s)…\n" +
		"Encrypted 1 secret(s)\n" +
		"done — commit .sops.yaml and the updated .enc files together\n"
	if out.String() != wantOut {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), wantOut)
	}
	if errOut.String() != scopeWarn+rotateWarn {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), scopeWarn+rotateWarn)
	}
	wantYAML := "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + k + "'\n" +
		"  - path_regex: 'Otherx\\..*'\n    age: '" + k + "'\n"
	if got := readFile(t, c.sopsConfigPath()); got != wantYAML {
		t.Errorf(".sops.yaml:\n%q\nwant:\n%q", got, wantYAML)
	}
	enc := a + "/Secret.app.default.K.enc"
	if opens(t, enc, gone, "v") {
		t.Error("the removed identity still opens the store")
	}
	if !opens(t, enc, keep, "v") {
		t.Error("the remaining identity cannot open the store")
	}
	if fsutil.Exists(a + "/Secret.new.default.K.enc") {
		t.Error("a plaintext that no .enc listed was encrypted")
	}
	if readFile(t, link) != oldYAML {
		t.Error(".sops.yaml was rewritten in place, not replaced")
	}
}

// --all re-keys every domain store; without it only the context's store is
// re-keyed, and a warning names the other store that still lists the
// recipient. The flat store is never re-keyed and is named too.
func TestRemoveKeyAllRekeysEveryStore(t *testing.T) {
	for _, all := range []bool{true, false} {
		c, out, errOut := testEnv(t)
		c.Domain = "a.dev"
		keep, gone := newAgeIdentity(t), newAgeIdentity(t)
		k, g := keep.Recipient().String(), gone.Recipient().String()
		sopsRules(t, c, k+","+g)
		a := c.Paths.Clusters + "/a.dev/secrets"
		b := c.Paths.Clusters + "/b.dev/secrets"
		write(t, a+"/Secret.app.default.K", "v")
		write(t, b+"/Secret.app.default.K", "w")
		encryptStore(t, c, "a.dev", out, errOut)
		encryptStore(t, c, "b.dev", out, errOut)
		freshen(t, a+"/Secret.app.default.K", b+"/Secret.app.default.K")
		write(t, c.Paths.Base+"/.secrets/Secret.flat.default.K.enc", encEnvelope(g))
		bBefore := readFile(t, b+"/Secret.app.default.K.enc")

		if err := c.RemoveKey(g, all, false); err != nil {
			t.Fatalf("all=%v: %v\n%s", all, err, errOut.String())
		}
		if opens(t, a+"/Secret.app.default.K.enc", gone, "v") || !opens(t, a+"/Secret.app.default.K.enc", keep, "v") {
			t.Errorf("all=%v: the context's store was not re-keyed", all)
		}
		if all {
			if opens(t, b+"/Secret.app.default.K.enc", gone, "w") || !opens(t, b+"/Secret.app.default.K.enc", keep, "w") {
				t.Error("--all did not re-key the second store")
			}
			wantOut := "removed recipient from .sops.yaml (1 creation rule(s))\n" +
				"  " + g + "\n" +
				"re-keying 2 cache file(s) across 2 store(s)…\n" +
				"Encrypted 1 secret(s)\n" +
				"Encrypted 1 secret(s)\n" +
				"done — commit .sops.yaml and the updated .enc files together\n"
			if out.String() != wantOut {
				t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), wantOut)
			}
		} else if readFile(t, b+"/Secret.app.default.K.enc") != bBefore {
			t.Error("without --all a store other than the context's was re-encrypted")
		}
		wantErr := "[warn] still listed in 1 .enc file(s) of the flat store, which --all does not re-key: .secrets\n" + rotateWarn
		if !all {
			wantErr = "[warn] still listed in 1 .enc file(s) of a store this run did not re-key: clusters/b.dev/secrets\n" +
				"[warn]   rerun with --all to re-key every domain store\n" + wantErr
		}
		wantErr = scopeWarn + wantErr
		if errOut.String() != wantErr {
			t.Errorf("all=%v: stderr:\n%q\nwant:\n%q", all, errOut.String(), wantErr)
		}
	}
}

// The last recipient of a rule is not removed, and nothing is written.
func TestRemoveKeyRefusesTheLastRecipient(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	sopsRules(t, c, keyA+","+keyB, keyB)
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.app.default.K", "v")
	before := readFile(t, c.sopsConfigPath())

	if err := c.RemoveKey(keyB, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	want := "[error] refusing to remove the last recipient of 1 creation rule(s) — nothing could decrypt the files they match\n" +
		"[error]   add another recipient first (lo secrets add-key), or delete the rule by hand\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	if readFile(t, c.sopsConfigPath()) != before {
		t.Error(".sops.yaml changed")
	}
	if fsutil.Exists(c.Paths.Clusters + "/a.dev/secrets/Secret.app.default.K.enc") {
		t.Error("the store was encrypted")
	}
}

// An orphan .enc is refused before any write.
func TestRemoveKeyOrphanFailsClosedBeforeAnyWrite(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	sopsRules(t, c, keyA+","+keyB)
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.orphan.default.K.enc", encEnvelope(keyA, keyB))
	before := readFile(t, c.sopsConfigPath())

	if err := c.RemoveKey(keyB, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	want := "[warn] orphan (no decrypted twin, cannot re-key): clusters/a.dev/secrets/Secret.orphan.default.K.enc\n" +
		"[error] 1 encrypted file(s) have no decrypted twin — they would keep the REMOVED\n" +
		"[error]   recipient, so its key could still decrypt them.\n" +
		"[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	if readFile(t, c.sopsConfigPath()) != before {
		t.Error(".sops.yaml changed before the orphan refusal")
	}
}

// --skip-orphans proceeds, and says which files the removed key can still
// open.
func TestRemoveKeySkipOrphansWarns(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	write(t, a+"/Secret.orphan.default.K", "o")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	if err := os.Remove(a + "/Secret.orphan.default.K"); err != nil {
		t.Fatal(err)
	}

	if err := c.RemoveKey(g, false, true); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	wantErr := "[warn] orphan (no decrypted twin, cannot re-key): clusters/a.dev/secrets/Secret.orphan.default.K.enc\n" +
		scopeWarn +
		"[warn] 1 orphan .enc file(s) still list the removed recipient — its key can still decrypt them\n" +
		rotateWarn
	if errOut.String() != wantErr {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), wantErr)
	}
	if opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
		t.Error("the removed identity still opens the re-keyed file")
	}
	if !opens(t, a+"/Secret.orphan.default.K.enc", gone, "o") {
		t.Error("fixture: the orphan was expected to stay open to the removed identity")
	}
}

// A recipient in no rule and no .enc is refused: a typo must not look like
// a removal.
func TestRemoveKeyRefusesANonRecipient(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	sopsYAMLWith(t, c, keyA)
	write(t, c.Paths.Clusters+"/a.dev/secrets/Secret.app.default.K", "v")

	if err := c.RemoveKey(keyB, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	want := "[error] not a recipient in .sops.yaml or in any .enc file of the store(s) — nothing to remove\n" +
		"[error]   " + keyB + "\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
}

// A recipient that .sops.yaml lists outside a single-quoted age: list is
// left to the operator.
func TestRemoveKeyRefusesAnUnquotedList(t *testing.T) {
	for _, yaml := range []string{
		"creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: " + keyA + "," + keyB + "\n",
		"creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + keyA + "," + keyB + "'\n  # was: " + keyB + "\n",
	} {
		c, out, errOut := testEnv(t)
		c.Domain = "a.dev"
		write(t, c.sopsConfigPath(), yaml)

		if err := c.RemoveKey(keyB, false, false); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		want := "[error] " + c.sopsConfigPath() + " lists the recipient outside a single-quoted age: list — remove it there by hand\n" +
			"[error]   " + keyB + "\n"
		if errOut.String() != want || out.String() != "" {
			t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
		}
		if readFile(t, c.sopsConfigPath()) != yaml {
			t.Error(".sops.yaml changed")
		}
	}
}

// A recipient already gone from .sops.yaml but still in .enc files (a run
// that stopped after its first write, or a hand edit) is re-keyed out.
func TestRemoveKeyRekeysStaleEncFiles(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	sopsRules(t, c, k)
	yaml := readFile(t, c.sopsConfigPath())

	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "recipient not in .sops.yaml; 1 .enc file(s) still list it — re-keying them\n  "+g+"\n") {
		t.Errorf("stdout:\n%q", out.String())
	}
	if opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
		t.Error("the stale .enc still opens to the removed identity")
	}
	if readFile(t, c.sopsConfigPath()) != yaml {
		t.Error(".sops.yaml changed")
	}
}

// An .enc that lists the recipient and is newer than its plaintext is
// refused before any write: the re-key encrypts the plaintext, and would
// write its older value over the newer one. The state right after
// `lo secrets encrypt` is the common case; a future mtime is the extreme one.
func TestRemoveKeyRefusesAStaleCache(t *testing.T) {
	for _, future := range []bool{false, true} {
		c, out, errOut := testEnv(t)
		c.Domain = "a.dev"
		keep, gone := newAgeIdentity(t), newAgeIdentity(t)
		k, g := keep.Recipient().String(), gone.Recipient().String()
		sopsRules(t, c, k+","+g)
		a := c.Paths.Clusters + "/a.dev/secrets"
		write(t, a+"/Secret.app.default.K", "v")
		encryptStore(t, c, "a.dev", out, errOut)
		if future {
			ts := time.Now().Add(time.Hour)
			if err := os.Chtimes(a+"/Secret.app.default.K.enc", ts, ts); err != nil {
				t.Fatal(err)
			}
		}
		before, encBefore := readFile(t, c.sopsConfigPath()), readFile(t, a+"/Secret.app.default.K.enc")

		if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		want := "[error] newer than its decrypted twin: clusters/a.dev/secrets/Secret.app.default.K.enc\n" +
			"[error] 1 .enc file(s) are newer than their plaintext: the cache is stale, and the re-key would write the old value back over the newer one\n" +
			"[error]   Run 'lo secrets decrypt' first (in each store), then run this command again. Nothing was written.\n"
		if errOut.String() != want || out.String() != "" {
			t.Errorf("future=%v: stdout %q\nstderr:\n%q\nwant:\n%q", future, out.String(), errOut.String(), want)
		}
		if readFile(t, c.sopsConfigPath()) != before || readFile(t, a+"/Secret.app.default.K.enc") != encBefore {
			t.Errorf("future=%v: a file changed before the refusal", future)
		}
	}
}

// The edit itself: only the lines that list the recipient change, their
// other recipients are joined without blanks, the rest of each line stays,
// and every line ends in a newline. tests/unit/secrets_remove_key_test.bats
// runs the same fixture through the bash twin.
func TestDropRecipient(t *testing.T) {
	in := "creation_rules:\n" +
		"  - path_regex: 'a\\..*'\n" +
		"    age: 'KA, KB ,KC'   # team\n" +
		"  - path_regex: '.*'\n" +
		"    age: 'KA,KB,'\n" +
		"  - path_regex: 'c'\n" +
		"    age:   'KC'"
	for _, tc := range []struct {
		key         string
		want        string
		rules, last int
	}{
		{"KB", "creation_rules:\n  - path_regex: 'a\\..*'\n    age: 'KA,KC'   # team\n  - path_regex: '.*'\n    age: 'KA'\n  - path_regex: 'c'\n    age:   'KC'\n", 2, 0},
		{"KC", "creation_rules:\n  - path_regex: 'a\\..*'\n    age: 'KA,KB'   # team\n  - path_regex: '.*'\n    age: 'KA,KB,'\n  - path_regex: 'c'\n    age:   ''\n", 2, 1},
		{"KD", in + "\n", 0, 0},
	} {
		got, rules, last := dropRecipient(in, tc.key)
		if got != tc.want || rules != tc.rules || last != tc.last {
			t.Errorf("drop %s: rules %d last %d\n%q\nwant rules %d last %d\n%q", tc.key, rules, last, got, tc.rules, tc.last, tc.want)
		}
	}
}

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
	"strconv"
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

// callerIdentity makes id the decrypt identity of the caller: an age keys
// file in a temp dir, named by SOPS_AGE_KEY_FILE. remove-key decrypts each
// file that it re-keys with it, to compare the file with its twin before the
// first write. The file holds no public key line, so the own-identity check
// of remove-key finds no match in it.
func callerIdentity(t *testing.T, id *age.X25519Identity) {
	t.Helper()
	file := t.TempDir() + "/keys.txt"
	if err := os.WriteFile(file, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", file)
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
		"done. Commit .sops.yaml and the updated .enc files together\n"
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

// --all with every domain store a path it cannot read (each domain directory
// mode 000): add-key does not read those paths, and its output stays the one
// of no store, while remove-key names them (TestRemoveKeyAllNamesEveryStoreItCannotRead).
func TestAddKeyAllWithEveryStoreUnreadableSaysNoStore(t *testing.T) {
	skipUnlessPermissionsHold(t)
	c, out, errOut := testEnv(t)
	sopsYAMLWith(t, c, keyA)
	for _, d := range []string{"a.dev", "b.dev"} {
		dir := c.Paths.Clusters + "/" + d
		mkdir(t, dir+"/secrets")
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}

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

// freshen makes each plaintext one second newer than its .enc, both in the
// past: the state after `lo secrets decrypt`. (A plaintext dated after now
// stays the newer file after a re-key, with a warning: nudgeNewer.)
func freshen(t *testing.T, plains ...string) {
	t.Helper()
	enc := time.Now().Add(-2 * time.Second)
	plain := enc.Add(time.Second)
	for _, p := range plains {
		if err := os.Chtimes(p+".enc", enc, enc); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, plain, plain); err != nil {
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
// the managed .enc files are re-encrypted: a new plaintext
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

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	wantOut := "removed recipient from .sops.yaml (2 creation rule(s))\n" +
		"  " + g + "\n" +
		"re-keying 1 cache file(s) across 1 store(s)…\n" +
		"Encrypted 1 secret(s)\n" +
		rmSummary(2, 1, 1)
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

		callerIdentity(t, keep)
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
				rmSummary(1, 2, 2)
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

	callerIdentity(t, keep)
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
	wantTail := "1 creation rule(s) changed\n" +
		"2 .enc file(s) listed the recipient in their labels\n" +
		"1 .enc file(s) re-keyed with a new data key\n" +
		"done: every managed .enc of the selected stores but 1 orphan(s) was rewritten. Commit them together with .sops.yaml\n"
	if !strings.HasSuffix(out.String(), wantTail) {
		t.Errorf("stdout:\n%q\nwant the tail:\n%q", out.String(), wantTail)
	}
}

// rmSummary is the summary of a remove-key run that ended, with no orphan
// that --skip-orphans accepted: the rules that changed, the .enc files whose
// labels listed the recipient, the .enc files re-keyed. The bats twin pins
// the same text (_summary).
func rmSummary(rules, listed, rekeyed int) string {
	return strconv.Itoa(rules) + " creation rule(s) changed\n" +
		strconv.Itoa(listed) + " .enc file(s) listed the recipient in their labels\n" +
		strconv.Itoa(rekeyed) + " .enc file(s) re-keyed with a new data key\n" +
		"done: every managed .enc of the selected stores was rewritten. Commit them together with .sops.yaml\n"
}

// A recipient in no creation rule and on no .enc label is a warning, not a
// refusal: the run re-keys every managed .enc of the store and changes no
// rule. The summary counts no rule and no file that listed it.
// tests/unit/secrets_remove_key_test.bats runs the same case.
func TestRemoveKeyWarnsAboutAKeyInNoRuleAndOnNoLabel(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k)
	a := c.Paths.Clusters + "/a.dev/secrets"
	enc := a + "/Secret.app.default.K.enc"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	yaml, encBefore := readFile(t, c.sopsConfigPath()), readFile(t, enc)

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	wantOut := "recipient not in .sops.yaml: .sops.yaml is not changed\n" +
		"  " + g + "\n" +
		"re-keying 1 cache file(s) across 1 store(s)…\n" +
		"Encrypted 1 secret(s)\n" +
		rmSummary(0, 0, 1)
	wantErr := "[warn] " + g + " is in no creation rule and in no .enc label: remove-key re-keys every managed .enc anyway, and changes no rule\n" +
		scopeWarn + rotateWarn
	if out.String() != wantOut || errOut.String() != wantErr {
		t.Errorf("stdout:\n%q\nwant:\n%q\nstderr:\n%q\nwant:\n%q", out.String(), wantOut, errOut.String(), wantErr)
	}
	if readFile(t, c.sopsConfigPath()) != yaml {
		t.Error(".sops.yaml changed")
	}
	if readFile(t, enc) == encBefore || !opens(t, enc, keep, "v") {
		t.Error("the .enc was not re-keyed")
	}
}

// labelStore is a store of a.dev with two managed .enc files that the sops
// library wrote, in the state after a decrypt: app for the recipients k and
// g (g the one to remove), other for k only, so that its labels do not list
// g. It returns the two plaintext paths.
func labelStore(t *testing.T, c *Context, k, g string, out, errOut *bytes.Buffer) (app, other string) {
	t.Helper()
	c.Domain = "a.dev"
	a := c.Paths.Clusters + "/a.dev/secrets"
	app, other = a+"/Secret.app.default.K", a+"/Secret.other.default.K"
	sopsRules(t, c, k)
	write(t, other, "w")
	encryptStore(t, c, "a.dev", out, errOut)
	sopsRules(t, c, k+","+g)
	write(t, app, "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, app, other)
	return app, other
}

// Every managed .enc of the store is re-keyed, also a file whose labels do
// not list the removed key: the labels are not trusted to choose the files.
// Its bytes change (a new data key) and it still decrypts with the identity
// that stays. tests/unit/secrets_remove_key_test.bats runs the same case.
func TestRemoveKeyRekeysAFileWhoseLabelsDoNotListTheKey(t *testing.T) {
	c, out, errOut := testEnv(t)
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	app, other := labelStore(t, c, k, g, out, errOut)
	before := readFile(t, other+".enc")
	if !opens(t, other+".enc", keep, "w") || opens(t, other+".enc", gone, "w") || !opens(t, app+".enc", gone, "v") {
		t.Fatal("fixture: the labels of the two files are not as expected")
	}

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if readFile(t, other+".enc") == before {
		t.Error("the .enc whose labels do not list the key was not rewritten")
	}
	if !opens(t, other+".enc", keep, "w") {
		t.Error("the rewritten .enc does not decrypt with the identity that stays")
	}
	if opens(t, app+".enc", gone, "v") || !opens(t, app+".enc", keep, "v") {
		t.Error("the .enc that listed the key was not re-keyed")
	}
}

// The summary gives the files whose labels listed the key apart from the
// files that the run re-keyed: here one and two.
// tests/unit/secrets_remove_key_test.bats runs the same case.
func TestRemoveKeySummaryCountsTheListedAndTheRekeyedFiles(t *testing.T) {
	c, out, errOut := testEnv(t)
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	labelStore(t, c, k, g, out, errOut)

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	wantOut := "removed recipient from .sops.yaml (1 creation rule(s))\n" +
		"  " + g + "\n" +
		"re-keying 2 cache file(s) across 1 store(s)…\n" +
		"Encrypted 2 secret(s)\n" +
		rmSummary(1, 1, 2)
	if out.String() != wantOut {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), wantOut)
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

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "recipient not in .sops.yaml: .sops.yaml is not changed\n  "+g+"\n") ||
		!strings.HasSuffix(out.String(), rmSummary(0, 1, 1)) {
		t.Errorf("stdout:\n%q", out.String())
	}
	if opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
		t.Error("the stale .enc still opens to the removed identity")
	}
	if readFile(t, c.sopsConfigPath()) != yaml {
		t.Error(".sops.yaml changed")
	}
}

// A twin that holds the value of its .enc passes the compare also when it is
// the older file of the two, and the run re-keys it: the times of the files
// do not count. The state right after `lo secrets encrypt` is the common
// case; an .enc with a time in the future is the extreme one.
// tests/unit/secrets_remove_key_test.bats runs the same case.
func TestRemoveKeyRekeysATwinThatIsOlderThanItsEnc(t *testing.T) {
	for _, future := range []bool{false, true} {
		c, out, errOut := testEnv(t)
		c.Domain = "a.dev"
		keep, gone := newAgeIdentity(t), newAgeIdentity(t)
		k, g := keep.Recipient().String(), gone.Recipient().String()
		sopsRules(t, c, k+","+g)
		a := c.Paths.Clusters + "/a.dev/secrets"
		plain, enc := a+"/Secret.app.default.K", a+"/Secret.app.default.K.enc"
		write(t, plain, "v")
		encryptStore(t, c, "a.dev", out, errOut)
		if future {
			ts := time.Now().Add(time.Hour)
			if err := os.Chtimes(enc, ts, ts); err != nil {
				t.Fatal(err)
			}
		}
		if !newerThan(enc, plain) {
			t.Fatalf("future=%v: fixture: the twin is not the older file", future)
		}
		encBefore := readFile(t, enc)

		callerIdentity(t, keep)
		if err := c.RemoveKey(g, false, false); err != nil {
			t.Fatalf("future=%v: %v\n%s", future, err, errOut.String())
		}
		if !strings.HasSuffix(out.String(), rmSummary(1, 1, 1)) || errOut.String() != scopeWarn+rotateWarn {
			t.Errorf("future=%v: stdout:\n%q\nstderr:\n%q", future, out.String(), errOut.String())
		}
		if readFile(t, enc) == encBefore || opens(t, enc, gone, "v") || !opens(t, enc, keep, "v") {
			t.Errorf("future=%v: the .enc was not re-keyed", future)
		}
	}
}

// A plaintext dated in the future stays the newer file after the re-key:
// the .enc keeps the time of its write, never a time later than now, and
// the encrypt prints one warning line that names the plaintext. An edit of
// the plaintext after the run (dated now) is encrypted, and a decrypt keeps
// it. tests/unit/secrets_remove_key_test.bats runs the same case.
func TestRemoveKeyKeepsAFutureDatedPlaintextNewerAndAnEditIsNotLost(t *testing.T) {
	f := newLinkFixture(t, "remove-key")
	plain := f.store + "/Secret.app.default.K"
	enc := plain + ".enc"
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(plain, future, future); err != nil {
		t.Fatal(err)
	}

	if err := f.run(); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	warn := "[warn] " + f.c.rel(plain) + " has a time in the future: its .enc keeps the time of the encrypt, and each encrypt writes it again. Set the time of the plaintext to now (touch it)\n"
	if n := strings.Count(f.errOut.String(), warn); n != 1 {
		t.Errorf("the warning shows %d time(s), want 1:\n%s", n, f.errOut.String())
	}
	info, err := os.Stat(enc)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(time.Now()) {
		t.Errorf("the .enc has a time in the future: %v", info.ModTime())
	}
	if !newerThan(plain, enc) {
		t.Error("the .enc is the newer file of the pair")
	}
	f.rekeyed("app", "v")

	write(t, plain, "w")
	encryptStore(t, f.c, "a.dev", f.out, f.errOut)
	if !opens(t, enc, f.keep, "w") {
		t.Fatal("the encrypt skipped the edit of the plaintext")
	}
	t.Setenv("SOPS_AGE_KEY", f.keep.String())
	if err := f.c.Decrypt("/nonexistent-ssh-key"); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	if got := readFile(t, plain); got != "w" {
		t.Errorf("the decrypt put back %q", got)
	}
}

// add-key encrypts every file of the list that it checked, whatever the
// times say. The run sets the plaintexts to now, so an .enc dated in the
// future stays the newer file, and a sweep that skips it would leave it
// without the new recipient. tests/unit/secrets_add_key_test.bats runs the
// same case.
func TestAddKeyRekeysAnEncDatedInTheFuture(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	enc := f.store + "/Secret.app.default.K.enc"
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(enc, future, future); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	f.rekeyed("app", "v")
	f.rekeyed("db", "w")
	info, err := os.Stat(enc)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(time.Now()) {
		t.Errorf("the .enc has a time in the future: %v", info.ModTime())
	}
}

// After a remove-key each re-keyed .enc is the newer file of its pair, as
// after `lo secrets encrypt`: the encrypt sweep that follows re-encrypts
// nothing (each .enc stays byte-equal), and the lint check names no secret.
// labelStore starts from the state after a decrypt (each twin the newer
// file). tests/unit/secrets_remove_key_test.bats runs the same case.
func TestRemoveKeyLeavesNothingForTheEncryptSweepOrTheLintCheck(t *testing.T) {
	c, out, errOut := testEnv(t)
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	app, other := labelStore(t, c, k, g, out, errOut)

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	// The lint check first: it writes nothing, and the sweep could make the
	// files fresh again.
	var lint bytes.Buffer
	if !CheckUnencrypted(c.Paths.Clusters+"/a.dev/secrets", &lint) || lint.String() != "" {
		t.Errorf("the lint check after remove-key:\n%s", lint.String())
	}
	encs := []string{app + ".enc", other + ".enc"}
	before := map[string]string{}
	for _, enc := range encs {
		before[enc] = readFile(t, enc)
	}
	out.Reset()
	errOut.Reset()
	if err := c.Encrypt(""); err != nil {
		t.Fatalf("encrypt: %v\n%s", err, errOut.String())
	}
	if out.String() != "" || errOut.String() != "" {
		t.Errorf("the encrypt sweep after remove-key: stdout %q, stderr %q; want none", out.String(), errOut.String())
	}
	for _, enc := range encs {
		if readFile(t, enc) != before[enc] {
			t.Errorf("the encrypt sweep after remove-key re-encrypted %s", enc)
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

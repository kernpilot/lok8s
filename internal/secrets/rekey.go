package secrets

// rekey.go: the re-key path that add-key and remove-key share, and
// remove-key itself. Each helper has a bash twin of the same shape in
// .lok8s/libs/secrets (secrets::_recipient, _rekey_stores, _orphans,
// _touch_plaintexts, _encs_with, _drop_recipient, _encrypt_stores).

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// privateKeyMarkers are the spellings of a private key pasted as the key
// argument: an age identity (in either case, because bech32 allows both) and
// a PEM or OpenSSH private key block.
var privateKeyMarkers = []string{"AGE-SECRET-KEY-", "age-secret-key-", "PRIVATE KEY"}

func looksLikePrivateKey(arg string) bool {
	for _, m := range privateKeyMarkers {
		if strings.Contains(arg, m) {
			return true
		}
	}
	return false
}

// recipientArg turns the key argument of add-key or remove-key into an age
// recipient (bash: secrets::_recipient): an age public key as given, or the
// key derived from an ed25519 SSH public key file. It prints its own [error]
// lines and returns ErrHandled.
func (c *Context) recipientArg(key string) (string, error) {
	// Before any message that prints the argument: the not-a-file error
	// below echoes it, and for a pasted private key that is key material.
	if looksLikePrivateKey(key) {
		ui.ErrorTo(c.ErrOut, "the key argument looks like a private key — it is not printed")
		ui.ErrorTo(c.ErrOut, "pass an age public key (age1…) or the path to an ed25519 SSH public key")
		return "", ErrHandled
	}

	// Accept an age key directly, or derive one from an SSH public key.
	agePubkey := ""
	if strings.HasPrefix(key, "age1") {
		agePubkey = key
	} else {
		if !fsutil.IsRegular(key) {
			ui.ErrorTo(c.ErrOut, "not an age key and not a readable file: %s", key)
			ui.ErrorTo(c.ErrOut, "pass an age public key (age1…) or the path to an ed25519 SSH public key")
			return "", ErrHandled
		}
		// Contained (bash: 2>/dev/null): the ssh-to-age binary echoes its
		// ENTIRE input on failure, which for a mistakenly-passed PRIVATE key
		// would print the key material (see
		// tests/unit/secret_material_containment_test.bats). The library
		// error is likewise never surfaced.
		derived, _, err := deriveAgePublicKey(key)
		if err != nil && !errors.Is(err, errUnsupportedKey) {
			ui.ErrorTo(c.ErrOut, "could not derive an age key from %s", key)
			ui.ErrorTo(c.ErrOut, "only ed25519 SSH keys are supported, and it must be the PUBLIC key")
			return "", ErrHandled
		}
		// A parseable non-ed25519 key derives to "" (the binary's rc-0 skip)
		// and is caught by the shape check below, matching bash.
		agePubkey = derived
	}

	// bech32 age public key. Validated because a typo here is silent: sops
	// accepts the config and every later encrypt writes a recipient nobody
	// holds.
	if !agePubkeyRe.MatchString(agePubkey) {
		ui.ErrorTo(c.ErrOut, "not a valid age public key: %s", agePubkey)
		return "", ErrHandled
	}
	return agePubkey, nil
}

// rekeyStores lists the stores a re-key covers (bash: secrets::_rekey_stores):
// with all, every clusters/<domain>/secrets directory (never the flat store);
// without it, the store of the current context.
func (c *Context) rekeyStores(all bool) ([]string, error) {
	var stores []string
	if all {
		matches, _ := filepath.Glob(c.Paths.Clusters + "/*/secrets")
		sort.Strings(matches)
		for _, d := range matches {
			// bash's * glob skips dot-directories; Go's does not.
			if strings.HasPrefix(filepath.Base(filepath.Dir(d)), ".") {
				continue
			}
			if fsutil.DirExists(d) {
				stores = append(stores, d)
			}
		}
	} else {
		dir, err := c.ResolveStore()
		if err != nil {
			return nil, err
		}
		stores = append(stores, dir)
	}
	if len(stores) == 0 {
		ui.ErrorTo(c.ErrOut, "no secret store found to re-key")
		return nil, ErrHandled
	}
	return stores, nil
}

// isEncEntry is the bash glob Secret.*.enc on a store entry's name.
func isEncEntry(base string) bool {
	return strings.HasSuffix(base[len("Secret."):], ".enc")
}

// orphanEncs warns about each Secret.*.enc without a decrypted twin, which a
// re-key cannot rewrite, and returns them (bash: secrets::_orphans).
func (c *Context) orphanEncs(stores []string) []string {
	var orphans []string
	for _, store := range stores {
		for _, base := range storeEntries(store, "Secret.") {
			if !isEncEntry(base) {
				continue
			}
			enc := store + "/" + base
			if !fsutil.Exists(enc) {
				continue
			}
			if !fsutil.IsRegular(strings.TrimSuffix(enc, ".enc")) {
				ui.WarnTo(c.ErrOut, "orphan (no decrypted twin, cannot re-key): %s", strings.TrimPrefix(enc, c.Paths.Base+"/"))
				orphans = append(orphans, enc)
			}
		}
	}
	return orphans
}

// touchPlaintexts sets the mtime of every plaintext cache file in the stores
// to now and returns the count (bash: secrets::_touch_plaintexts). Encrypt
// skips a file whose .enc is newer, and a recipient change does not touch the
// plaintext, so without this nudge a re-key re-encrypts nothing: the manual
// step these commands exist to remove.
func touchPlaintexts(stores []string) (int, error) {
	touched := 0
	now := time.Now()
	for _, store := range stores {
		for _, base := range storeEntries(store, "Secret.") {
			plain := store + "/" + base
			// bash tests [[ -e && != *.enc ]] — any existing entry counts,
			// not just regular files.
			if !fsutil.Exists(plain) || strings.HasSuffix(base, ".enc") {
				continue
			}
			if err := os.Chtimes(plain, now, now); err != nil {
				return touched, err
			}
			touched++
		}
	}
	return touched, nil
}

// encsWithRecipient lists the Secret.*.enc files in the stores whose bytes
// contain the recipient (bash: secrets::_encs_with). sops writes each age
// recipient in clear text into a file's metadata, so a file that lists it is
// a file its key can open.
func encsWithRecipient(stores []string, recipient string) []string {
	var listed []string
	for _, store := range stores {
		for _, base := range storeEntries(store, "Secret.") {
			enc := store + "/" + base
			if !isEncEntry(base) || !fsutil.IsRegular(enc) {
				continue
			}
			raw, err := os.ReadFile(enc)
			if err == nil && bytes.Contains(raw, []byte(recipient)) {
				listed = append(listed, enc)
			}
		}
	}
	return listed
}

// storeDomain returns <domain> for a store at clusters/<domain>/secrets.
func (c *Context) storeDomain(store string) (string, bool) {
	parent := filepath.Dir(store)
	if filepath.Base(store) != "secrets" || filepath.Clean(filepath.Dir(parent)) != filepath.Clean(c.Paths.Clusters) {
		return "", false
	}
	return filepath.Base(parent), true
}

// encryptStores runs the whole-store Encrypt once per store, each under the
// domain the store belongs to (bash: secrets::_encrypt_stores). A store
// outside clusters/ (the flat store) keeps the context's domain.
func (c *Context) encryptStores(stores []string) error {
	saved := c.Domain
	defer func() { c.Domain = saved }()
	for _, store := range stores {
		c.Domain = saved
		if d, ok := c.storeDomain(store); ok {
			c.Domain = d
		}
		if err := c.Encrypt(""); err != nil {
			return err
		}
	}
	return nil
}

// ageListRe is ageLineRe with the rest of the line kept.
var ageListRe = regexp.MustCompile(`^([[:space:]]*age:[[:space:]]*')([^']*)(')(.*)$`)

// dropRecipient removes the recipient from every single-quoted age list of
// the .sops.yaml content (bash: secrets::_drop_recipient). It returns the
// edited content, the number of creation rules that listed the recipient, and
// how many of those list no other recipient. Only a line that listed it is
// rewritten (its other recipients joined with "," and no blanks). Every line
// of the result ends in a newline, as the bash read loop writes it.
func dropRecipient(content, recipient string) (edited string, rules, last int) {
	lines := strings.Split(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	for _, line := range lines {
		if m := ageListRe.FindStringSubmatch(line); m != nil {
			var kept []string
			found := false
			for _, r := range strings.Split(m[2], ",") {
				r = strings.Trim(r, " \t")
				switch {
				case r == recipient:
					found = true
				case r != "":
					kept = append(kept, r)
				}
			}
			if found {
				rules++
				if len(kept) == 0 {
					last++
				}
				line = m[1] + strings.Join(kept, ",") + m[3] + m[4]
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), rules, last
}

// ── Remove key ───────────────────────────────────────────

// RemoveKey drops an age recipient from every creation rule in .sops.yaml
// and re-keys the stores, so that no .enc file lists it (bash:
// secrets::remove-key). It is add-key in reverse, with two differences that
// matter for a removal: every refusal runs before the first write, and the
// re-encryption covers every selected store, not only the context's.
//
// It refuses to drop the last recipient of a rule (nothing could decrypt
// the files the rule matches), a recipient that is in no rule and no .enc
// file (a typo must not look like a removal), and, without skipOrphans, an
// .enc with no decrypted twin: a re-key cannot rewrite that file, so it keeps
// the removed recipient. After the re-key it reads the .enc files again and
// fails when one still lists the recipient.
//
// A removal does not revoke what the key already read; the last line says
// so.
func (c *Context) RemoveKey(key string, all, skipOrphans bool) error {
	agePubkey, err := c.recipientArg(key)
	if err != nil {
		return err
	}

	sopsConfig := c.sopsConfigPath()
	if !fsutil.IsRegular(sopsConfig) {
		ui.ErrorTo(c.ErrOut, "%s not found — run: lo secrets init", sopsConfig)
		return ErrHandled
	}
	raw, err := os.ReadFile(sopsConfig)
	if err != nil {
		return err
	}
	edited, rules, last := dropRecipient(string(raw), agePubkey)
	if strings.Contains(string(raw), agePubkey) && (rules == 0 || strings.Contains(edited, agePubkey)) {
		ui.ErrorTo(c.ErrOut, "%s lists the recipient outside a single-quoted age: list — remove it there by hand", sopsConfig)
		ui.ErrorTo(c.ErrOut, "  %s", agePubkey)
		return ErrHandled
	}
	if last > 0 {
		ui.ErrorTo(c.ErrOut, "refusing to remove the last recipient of %d creation rule(s) — nothing could decrypt the files they match", last)
		ui.ErrorTo(c.ErrOut, "  add another recipient first (lo secrets add-key), or delete the rule by hand")
		return ErrHandled
	}

	stores, err := c.rekeyStores(all)
	if err != nil {
		return err
	}
	listed := encsWithRecipient(stores, agePubkey)
	if rules == 0 && len(listed) == 0 {
		ui.ErrorTo(c.ErrOut, "not a recipient in .sops.yaml or in any .enc file of the store(s) — nothing to remove")
		ui.ErrorTo(c.ErrOut, "  %s", agePubkey)
		return ErrHandled
	}
	if orphans := c.orphanEncs(stores); len(orphans) > 0 && !skipOrphans {
		ui.ErrorTo(c.ErrOut, "%d encrypted file(s) have no decrypted twin — they would keep the REMOVED", len(orphans))
		ui.ErrorTo(c.ErrOut, "  recipient, so its key could still decrypt them.")
		ui.ErrorTo(c.ErrOut, "  Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.")
		return ErrHandled
	}

	// The first write. A run that stops after it (a failed encrypt) is
	// resumed by running the command again: the recipient is then in no
	// rule, and the .enc files that still list it are re-keyed.
	if rules > 0 {
		if err := os.WriteFile(sopsConfig, []byte(edited), 0o644); err != nil { // #nosec G306 -- .sops.yaml holds public age recipients only
			return err
		}
		fmt.Fprintf(c.Out, "removed recipient from .sops.yaml (%d creation rule(s))\n", rules)
	} else {
		fmt.Fprintf(c.Out, "recipient not in .sops.yaml; %d .enc file(s) still list it — re-keying them\n", len(listed))
	}
	fmt.Fprintf(c.Out, "  %s\n", agePubkey)

	rekeyed, err := touchPlaintexts(stores)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "re-keying %d cache file(s) across %d store(s)…\n", rekeyed, len(stores))
	if err := c.encryptStores(stores); err != nil {
		return err
	}

	// Check the files, not the exit codes: Encrypt skips an .enc that is
	// newer than its plaintext, and that file keeps the removed recipient.
	left, orphansLeft := 0, 0
	for _, enc := range encsWithRecipient(stores, agePubkey) {
		if !fsutil.IsRegular(strings.TrimSuffix(enc, ".enc")) {
			orphansLeft++
			continue
		}
		ui.ErrorTo(c.ErrOut, "still encrypted to the removed recipient: %s", strings.TrimPrefix(enc, c.Paths.Base+"/"))
		left++
	}
	if left > 0 {
		ui.ErrorTo(c.ErrOut, "%d .enc file(s) were not re-keyed (an .enc newer than its plaintext?) — touch the plaintext, then run 'lo secrets encrypt'", left)
		return ErrHandled
	}
	if orphansLeft > 0 {
		ui.WarnTo(c.ErrOut, "%d orphan .enc file(s) still list the removed recipient — its key can still decrypt them", orphansLeft)
	}
	fmt.Fprintln(c.Out, "done — commit .sops.yaml and the updated .enc files together")
	ui.WarnTo(c.ErrOut, "a removal does not revoke what the key already read (older .enc files in git history included): rotate those secrets")
	return nil
}

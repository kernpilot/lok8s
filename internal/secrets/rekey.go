package secrets

// rekey.go: the re-key path that add-key and remove-key share, and
// remove-key itself. Each helper has a bash twin of the same shape in
// .lok8s/libs/secrets (secrets::_no_private_key, _recipient, _rekey_stores,
// _orphans, _touch_plaintexts, _encs_with, _other_stores, _own_identity,
// _replace, _drop_recipient, _rekey_files).

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// privateKeyMarkers are the spellings of a private key pasted as the key
// argument: an age identity (in either case, because bech32 allows both), a
// PEM or OpenSSH private key block, and the first line of any PEM block
// (an unquoted paste splits the block into words, and the first one is
// "-----BEGIN").
var privateKeyMarkers = []string{"AGE-SECRET-KEY-", "age-secret-key-", "PRIVATE KEY", "-----BEGIN"}

// LooksLikePrivateKey reports whether arg has the shape of a private key.
func LooksLikePrivateKey(arg string) bool {
	for _, m := range privateKeyMarkers {
		if strings.Contains(arg, m) {
			return true
		}
	}
	return false
}

// RefusePrivateKey prints the refusal for a private key argument. It never
// prints the argument.
func RefusePrivateKey(w io.Writer) {
	ui.ErrorTo(w, "the key argument looks like a private key — it is not printed")
	ui.ErrorTo(w, "pass an age public key (age1…) or the path to an ed25519 SSH public key")
}

// QuoteArg is arg in Go quotes for a message, or a placeholder when arg has
// the shape of a private key.
func QuoteArg(arg string) string {
	if LooksLikePrivateKey(arg) {
		return "(not printed: it looks like a private key)"
	}
	return strconv.Quote(arg)
}

// recipientArg turns the key argument of add-key or remove-key into an age
// recipient (bash: secrets::_recipient): an age public key as given, or the
// key derived from an ed25519 SSH public key file. It prints its own [error]
// lines and returns ErrHandled.
func (c *Context) recipientArg(key string) (string, error) {
	// Before any message that prints the argument: the not-a-file error
	// below echoes it, and for a pasted private key that is key material.
	if LooksLikePrivateKey(key) {
		RefusePrivateKey(c.ErrOut)
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

// domainStores lists every clusters/<domain>/secrets directory, in bash glob
// order.
func (c *Context) domainStores() []string {
	var stores []string
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
	return stores
}

// rekeyStores lists the stores a re-key covers (bash: secrets::_rekey_stores):
// with all, every clusters/<domain>/secrets directory (never the flat store);
// without it, the store of the current context.
func (c *Context) rekeyStores(all bool) ([]string, error) {
	var stores []string
	if all {
		stores = c.domainStores()
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
				ui.WarnTo(c.ErrOut, "orphan (no decrypted twin, cannot re-key): %s", c.rel(enc))
				orphans = append(orphans, enc)
			}
		}
	}
	return orphans
}

// rel is a path for a message: relative to the project when it is inside it.
func (c *Context) rel(path string) string {
	return strings.TrimPrefix(path, c.Paths.Base+"/")
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

// encScan is what scanEncs read in a set of stores.
type encScan struct {
	all        []string // every regular Secret.*.enc
	listed     []string // those whose bytes hold the recipient
	unreadable []string // a store it cannot list, an .enc it cannot read
}

// scanEncs reads every Secret.*.enc of the stores (bash: secrets::_encs_with).
// sops writes each age recipient in clear text into a file's metadata, so a
// file that lists the recipient is a file its key can open. A store or a
// file it cannot read is reported, never taken as one that does not list it.
func scanEncs(stores []string, recipient string) encScan {
	var s encScan
	for _, store := range stores {
		if !fsutil.Exists(store) {
			continue // no store, no file
		}
		names, err := storeNames(store, "Secret.")
		if err == nil {
			_, err = os.Stat(store + "/.")
		}
		if err != nil {
			s.unreadable = append(s.unreadable, store)
			continue
		}
		for _, base := range names {
			enc := store + "/" + base
			if !isEncEntry(base) || !fsutil.IsRegular(enc) {
				continue
			}
			s.all = append(s.all, enc)
			raw, err := os.ReadFile(enc) // #nosec G304 -- an .enc of the store
			switch {
			case err != nil:
				s.unreadable = append(s.unreadable, enc)
			case bytes.Contains(raw, []byte(recipient)):
				s.listed = append(s.listed, enc)
			}
		}
	}
	return s
}

// reportOtherStores prints, at the level of say, one line for each store
// that this run does not re-key and that still lists the recipient (bash:
// secrets::_other_stores): the domain stores it did not select, then the
// flat store, which --all does not re-key either. A file it cannot read
// counts as one that lists it.
func (c *Context) reportOtherStores(say func(io.Writer, string, ...any), selected []string, recipient string) {
	picked := map[string]bool{}
	for _, s := range selected {
		picked[filepath.Clean(s)] = true
	}
	count := func(store string) int {
		s := scanEncs([]string{store}, recipient)
		return len(s.listed) + len(s.unreadable)
	}
	domains := 0
	for _, store := range c.domainStores() {
		if picked[filepath.Clean(store)] {
			continue
		}
		if n := count(store); n > 0 {
			say(c.ErrOut, "still listed in %d .enc file(s) of a store this run did not re-key: %s", n, c.rel(store))
			domains++
		}
	}
	if domains > 0 {
		say(c.ErrOut, "  rerun with --all to re-key every domain store")
	}
	if flat := c.flatStore(); fsutil.DirExists(flat) && !picked[filepath.Clean(flat)] {
		if n := count(flat); n > 0 {
			say(c.ErrOut, "still listed in %d .enc file(s) of the flat store, which --all does not re-key: %s", n, c.rel(flat))
		}
	}
}

// ownIdentity names the file of the caller's own decrypt identity that the
// recipient belongs to (bash: secrets::_own_identity): the sops age keys
// file (${SOPS_AGE_KEY_FILE:-~/.config/sops/age/keys.txt}) by its
// "# public key: " lines, or the SSH key lo decrypts with
// (${LOK8S_SSH_KEY:-~/.ssh/id_ed25519}) by the age key of its .pub. "" when
// neither matches. No private key is printed or derived.
func ownIdentity(recipient string) string {
	keyFile := os.Getenv("SOPS_AGE_KEY_FILE")
	if keyFile == "" {
		keyFile = os.Getenv("HOME") + "/.config/sops/age/keys.txt"
	}
	if fsutil.IsRegular(keyFile) {
		if raw, err := os.ReadFile(keyFile); err == nil { // #nosec G304 -- the caller's own keys file, read for its public key lines
			for line := range strings.SplitSeq(string(raw), "\n") {
				if line == "# public key: "+recipient {
					return keyFile
				}
			}
		}
	}
	sshKey := os.Getenv("LOK8S_SSH_KEY")
	if sshKey == "" {
		sshKey = os.Getenv("HOME") + "/.ssh/id_ed25519"
	}
	if pub := sshKey + ".pub"; fsutil.IsRegular(pub) {
		if derived, _, err := deriveAgePublicKey(pub); err == nil && derived == recipient {
			return pub
		}
	}
	return ""
}

// replaceFile writes content to path through a temp file in the same
// directory and a rename (bash: secrets::_replace), so no reader sees half a
// file. The new file keeps the mode of the old one. For a symlink it
// replaces the file the link points to.
func replaceFile(path string, content []byte) error {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, target); err != nil {
		return err
	}
	done = true
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
			for r := range strings.SplitSeq(m[2], ",") {
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

// rekeyFiles re-encrypts each .enc file from its plaintext twin, whatever
// the timestamps say (bash: secrets::_rekey_files), and prints a count per
// store as Encrypt does.
func (c *Context) rekeyFiles(stores, encs []string) error {
	for _, store := range stores {
		count := 0
		for _, enc := range encs {
			if filepath.Dir(enc) != filepath.Clean(store) {
				continue
			}
			if err := c.encryptFile(strings.TrimSuffix(enc, ".enc")); err != nil {
				return err
			}
			count++
		}
		if count > 0 {
			fmt.Fprintf(c.Out, "Encrypted %d secret(s)\n", count)
		}
	}
	return nil
}

// rerunRemoveKey prints the command that resumes a re-key that stopped after
// the .sops.yaml write.
func (c *Context) rerunRemoveKey(recipient string, all, skipOrphans bool) {
	cmd := "lo secrets remove-key " + recipient + domainFlag(c.Domain)
	if all {
		cmd += " --all"
	}
	if skipOrphans {
		cmd += " --skip-orphans"
	}
	ui.ErrorTo(c.ErrOut, "the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again resumes it:")
	ui.ErrorTo(c.ErrOut, "  %s", cmd)
}

// ── Remove key ───────────────────────────────────────────

// RemoveKey drops an age recipient from every creation rule in .sops.yaml
// and re-keys the selected stores, so that no .enc file of them lists it
// (bash: secrets::remove-key). It is add-key in reverse, with two differences
// that matter for a removal: every refusal runs before the first write, and
// the re-encryption covers every selected store, not only the context's.
//
// It holds the lock of .sops.yaml for the whole run. It reads .sops.yaml as
// YAML too and refuses when the recipient is in a value the line edit cannot
// rewrite (an escaped string, a YAML list, a key group, an alias or a merge
// key), when the edit would drop the last recipient of a rule, and when an
// .enc file of the stores matches no creation rule, a rule with a recipient
// that is not age, or a rule with an age recipient that does not parse. It
// refuses a recipient that is in no rule and no .enc file (a typo must not
// look like a removal), an .enc file it cannot read, an .enc that lists the
// recipient and is newer than its plaintext (the re-key would write the old
// value back over the newer one), and, without skipOrphans, an .enc with no
// decrypted twin: a re-key cannot rewrite that file, so it keeps the removed
// recipient.
//
// It re-encrypts each .enc that lists the recipient from its plaintext, then
// reads the .enc files again and fails when one still lists it, with the
// command that resumes the run. Other stores that still list the recipient
// are named in a warning; they are not re-keyed.
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
	unlock, err := c.lockSopsConfig()
	if err != nil {
		return err
	}
	defer unlock()

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

	// The line edit sees text. Read the YAML too: an escaped letter in a
	// double-quoted string, a YAML list, a key group, an alias or a merge
	// key can hold the recipient where the edit does not look.
	decoded, err := decodedRules(raw)
	var doc yaml.Node
	if err == nil {
		err = yaml.Unmarshal(raw, &doc)
	}
	if err != nil {
		ui.ErrorTo(c.ErrOut, "%s does not parse as YAML", sopsConfig)
		return ErrHandled
	}
	if ruleHits(decoded, agePubkey, true) > 0 {
		ui.ErrorTo(c.ErrOut, "%s lists the recipient in key_groups — remove-key does not edit key groups; remove it there by hand", sopsConfig)
		ui.ErrorTo(c.ErrOut, "  %s", agePubkey)
		return ErrHandled
	}
	if encodedAgeHits(&doc, agePubkey) > 0 {
		ui.ErrorTo(c.ErrOut, "%s lists the recipient in an escaped string or a YAML list, which remove-key does not edit — remove it there by hand", sopsConfig)
		ui.ErrorTo(c.ErrOut, "  %s", agePubkey)
		return ErrHandled
	}
	if last > 0 {
		ui.ErrorTo(c.ErrOut, "refusing to remove the last recipient of %d creation rule(s) — nothing could decrypt the files they match", last)
		ui.ErrorTo(c.ErrOut, "  add another recipient first (lo secrets add-key), or delete the rule by hand")
		return ErrHandled
	}
	// The file as it will be: the edit, or, when no rule listed the
	// recipient (a run that resumes), the file as it is.
	next := string(raw)
	if rules > 0 {
		next = edited
	}
	nextRules, err := decodedRules([]byte(next))
	if err != nil {
		ui.ErrorTo(c.ErrOut, "the edited %s does not parse as YAML — nothing was written", sopsConfig)
		return ErrHandled
	}
	if ruleHits(nextRules, agePubkey, false) > 0 {
		ui.ErrorTo(c.ErrOut, "%s still lists the recipient after the edit (through an alias or a merge key) — nothing was written; remove it there by hand", sopsConfig)
		ui.ErrorTo(c.ErrOut, "  %s", agePubkey)
		return ErrHandled
	}

	stores, err := c.rekeyStores(all)
	if err != nil {
		return err
	}
	scan := scanEncs(stores, agePubkey)
	if len(scan.unreadable) > 0 {
		for _, u := range scan.unreadable {
			ui.ErrorTo(c.ErrOut, "cannot read %s", c.rel(u))
		}
		ui.ErrorTo(c.ErrOut, "  a file that is not read may still list the recipient — fix the permissions first; nothing was written")
		return ErrHandled
	}
	if rules == 0 && len(scan.listed) == 0 {
		ui.ErrorTo(c.ErrOut, "not a recipient in .sops.yaml or in any .enc file of the store(s) — nothing to remove")
		ui.ErrorTo(c.ErrOut, "  %s", agePubkey)
		c.reportOtherStores(ui.ErrorTo, stores, agePubkey)
		return ErrHandled
	}
	if orphans := c.orphanEncs(stores); len(orphans) > 0 && !skipOrphans {
		ui.ErrorTo(c.ErrOut, "%d encrypted file(s) have no decrypted twin — they would keep the REMOVED", len(orphans))
		ui.ErrorTo(c.ErrOut, "  recipient, so its key could still decrypt them.")
		ui.ErrorTo(c.ErrOut, "  Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.")
		return ErrHandled
	}
	// The re-key encrypts the plaintext twin. A twin older than its .enc
	// holds an older value, and the re-key would write it over the newer
	// one: refuse until the cache is fresh.
	var rekey []string
	stale := 0
	for _, enc := range scan.listed {
		plain := strings.TrimSuffix(enc, ".enc")
		if !fsutil.IsRegular(plain) {
			continue
		}
		if newerThan(enc, plain) {
			ui.ErrorTo(c.ErrOut, "newer than its decrypted twin: %s", c.rel(enc))
			stale++
		}
		rekey = append(rekey, enc)
	}
	if stale > 0 {
		ui.ErrorTo(c.ErrOut, "%d .enc file(s) are newer than their plaintext: the cache is stale, and the re-key would write the old value back over the newer one", stale)
		ui.ErrorTo(c.ErrOut, "  Run 'lo secrets decrypt' first (in each store), then run this command again. Nothing was written.")
		return ErrHandled
	}
	// sops picks the first creation rule whose path_regex matches the
	// plaintext path. A file with no rule, or with a rule this age-only
	// build cannot encrypt to, would stop the re-key half way.
	found, err := checkRules(sopsConfig, next, scan.all)
	if err != nil {
		ui.ErrorTo(c.ErrOut, "cannot match the .enc files against the creation rules of %s — nothing was written", sopsConfig)
		return ErrHandled
	}
	for _, enc := range found.none {
		ui.ErrorTo(c.ErrOut, "no creation rule matches %s", c.rel(enc))
	}
	for _, enc := range found.notAge {
		ui.ErrorTo(c.ErrOut, "the creation rule for %s has a recipient that is not age (pgp, kms, …)", c.rel(enc))
	}
	for _, enc := range found.badAge {
		ui.ErrorTo(c.ErrOut, "the creation rule for %s has an age recipient that does not parse", c.rel(enc))
	}
	if len(found.none)+len(found.notAge)+len(found.badAge) > 0 {
		ui.ErrorTo(c.ErrOut, "  the re-key encrypts with age through the first matching rule: fix the creation rules first. Nothing was written.")
		return ErrHandled
	}
	if own := ownIdentity(agePubkey); own != "" {
		ui.WarnTo(c.ErrOut, "the recipient is your own decrypt identity (%s): once it is removed, that identity cannot decrypt the re-keyed files", own)
	}

	// The first write. A run that stops after it (a failed encrypt) is
	// resumed by running the command again: the recipient is then in no
	// rule, and the .enc files that still list it are re-keyed.
	if rules > 0 {
		if err := replaceFile(sopsConfig, []byte(edited)); err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "removed recipient from .sops.yaml (%d creation rule(s))\n", rules)
	} else {
		fmt.Fprintf(c.Out, "recipient not in .sops.yaml; %d .enc file(s) still list it — re-keying them\n", len(scan.listed))
	}
	fmt.Fprintf(c.Out, "  %s\n", agePubkey)

	fmt.Fprintf(c.Out, "re-keying %d cache file(s) across %d store(s)…\n", len(rekey), len(stores))
	if err := c.rekeyFiles(stores, rekey); err != nil {
		c.rerunRemoveKey(agePubkey, all, skipOrphans)
		return err
	}

	// Check the files, not the exit codes.
	after := scanEncs(stores, agePubkey)
	left, orphansLeft := 0, 0
	for _, u := range after.unreadable {
		ui.ErrorTo(c.ErrOut, "cannot read %s", c.rel(u))
	}
	for _, enc := range after.listed {
		if !fsutil.IsRegular(strings.TrimSuffix(enc, ".enc")) {
			orphansLeft++
			continue
		}
		ui.ErrorTo(c.ErrOut, "still encrypted to the removed recipient: %s", c.rel(enc))
		left++
	}
	if left > 0 || len(after.unreadable) > 0 {
		ui.ErrorTo(c.ErrOut, "%d .enc file(s) still list the removed recipient after the re-key, %d cannot be read", left, len(after.unreadable))
		c.rerunRemoveKey(agePubkey, all, skipOrphans)
		return ErrHandled
	}
	if orphansLeft > 0 {
		ui.WarnTo(c.ErrOut, "%d orphan .enc file(s) still list the removed recipient — its key can still decrypt them", orphansLeft)
	}
	c.reportOtherStores(ui.WarnTo, stores, agePubkey)
	fmt.Fprintln(c.Out, "done — commit .sops.yaml and the updated .enc files together")
	ui.WarnTo(c.ErrOut, "a removal does not revoke what the key already read (older .enc files in git history included): rotate those secrets")
	return nil
}

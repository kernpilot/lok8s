package secrets

// The lo secrets operations, ported one-for-one from .lok8s/libs/secrets.
// Every user-visible string, exit path, and ordering quirk mirrors the bash
// implementation; deliberate divergences are limited to tool-not-found checks
// (sops/ssh-to-age are libraries here) and are commented where they occur.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/ui"
)

// ── Init ─────────────────────────────────────────────────

// Init configures SOPS/age encryption from an SSH public key (bash:
// secrets::init). The bash `command -v ssh-to-age/sops` gates disappear —
// both are libraries in the Go build.
func (c *Context) Init(sshKey string) error {
	if !isFile(sshKey) {
		ui.Errorf(c.ErrOut, "SSH public key not found: %s", sshKey)
		ui.Errorf(c.ErrOut, "Generate one with: ssh-keygen -t ed25519")
		return ErrPrinted
	}

	agePubkey, skipNote, err := deriveAgePublicKey(sshKey)
	if err != nil && !errors.Is(err, errUnsupportedKey) {
		ui.Errorf(c.ErrOut, "Failed to derive age public key from %s", sshKey)
		ui.Errorf(c.ErrOut, "Only ed25519 SSH keys are supported")
		return ErrPrinted
	}
	// bash leaves ssh-to-age's stderr uncontained here (public keys only —
	// leaking a public key is not a leak); the CLI's "skipped key: …" note for
	// a non-ed25519 key is the one line that surfaces, and the derivation
	// result stays empty exactly like the binary's rc-0 skip.
	if skipNote != "" {
		fmt.Fprintln(c.ErrOut, skipNote)
	}

	sopsConfig := c.sopsConfigPath()

	if isFile(sopsConfig) {
		raw, _ := os.ReadFile(sopsConfig)
		// Check if this key is already present
		if grepQF(string(raw), agePubkey) {
			fmt.Fprintln(c.Out, "age key already configured in .sops.yaml")
			return nil
		}
		ui.Warnf(c.ErrOut, ".sops.yaml exists but doesn't contain this key")
		ui.Warnf(c.ErrOut, "Add it with: lo secrets add-key %s", sshKey)
		fmt.Fprintf(c.Out, "Your age public key: %s\n", agePubkey)
		return nil
	}

	// A non-ed25519 key derives NOTHING (the skip note above); bash still
	// wrote `age: ''` and exited 0, leaving a .sops.yaml no encrypt can use.
	if !agePubkeyRe.MatchString(agePubkey) {
		ui.Errorf(c.ErrOut, "no age recipient derived from %s (ed25519 SSH keys only) — nothing written to %s", sshKey, sopsConfig)
		return ErrPrinted
	}

	content := `# SOPS encryption config for lok8s secret cache files.
# Recipients are age public keys derived from SSH keys via ssh-to-age.
# Add team members with: lo secrets add-key <ssh-pubkey-path|age1…>
creation_rules:
  - path_regex: 'Secret\..*'
    age: '` + agePubkey + `'
`
	if err := os.WriteFile(sopsConfig, []byte(content), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(c.Out, "Created %s\n", sopsConfig)
	fmt.Fprintf(c.Out, "Age public key: %s\n", agePubkey)
	fmt.Fprintf(c.Out, "Derived from: %s\n", sshKey)

	// Ensure secrets directory exists with a .gitignore that blocks
	// everything except SOPS-encrypted .enc files.
	if err := c.ensureStore(); err != nil {
		return err
	}
	dir := c.StorePath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return ensureGitignore(dir)
}

// grepQF mirrors `grep -qF pattern file` on in-memory content: an empty
// pattern matches any content with at least one line (which is how bash
// init's empty-derivation quirk behaves against an existing .sops.yaml).
func grepQF(content, pattern string) bool {
	if pattern == "" {
		return len(content) > 0
	}
	return strings.Contains(content, pattern)
}

// ── Add key ──────────────────────────────────────────────

// AddKey adds a recipient to .sops.yaml and re-keys every cache file so they
// can actually read it (bash: secrets::add-key). Both halves are required:
// appending the key alone leaves the new recipient unable to decrypt
// ANYTHING, because sops only rewrites a file's recipients when that file is
// re-encrypted.
//
// Was documented before it was implemented (issue #73). The manual
// equivalent — edit .sops.yaml, `touch` every plaintext twin (Encrypt skips
// files whose .enc is newer, so a recipient change re-encrypts nothing), then
// encrypt per domain — is exactly what this removes.
//
// FAILS CLOSED on an orphan: an `.enc` with no decrypted twin cannot be
// re-keyed here, and silently leaving it behind is the worst outcome — the
// new recipient would appear to be added while some secrets stayed unreadable
// to them. Decrypt first, or pass skipOrphans to accept the gap knowingly.
// (Like bash, the orphan check runs AFTER .sops.yaml was already modified —
// the ordering is part of the observable contract.)
func (c *Context) AddKey(key string, all, skipOrphans bool) error {
	// Accept an age key directly, or derive one from an SSH public key.
	agePubkey := ""
	if strings.HasPrefix(key, "age1") {
		agePubkey = key
	} else {
		if !isFile(key) {
			ui.Errorf(c.ErrOut, "not an age key and not a readable file: %s", key)
			ui.Errorf(c.ErrOut, "pass an age public key (age1…) or the path to an ed25519 SSH public key")
			return ErrPrinted
		}
		// Contained (bash: 2>/dev/null): the ssh-to-age binary echoes its
		// ENTIRE input on failure, which for a mistakenly-passed PRIVATE key
		// would print the key material (see
		// tests/unit/secret_material_containment_test.bats). The library
		// error is likewise never surfaced.
		derived, _, err := deriveAgePublicKey(key)
		if err != nil && !errors.Is(err, errUnsupportedKey) {
			ui.Errorf(c.ErrOut, "could not derive an age key from %s", key)
			ui.Errorf(c.ErrOut, "only ed25519 SSH keys are supported, and it must be the PUBLIC key")
			return ErrPrinted
		}
		// A parseable non-ed25519 key derives to "" (the binary's rc-0 skip)
		// and is caught by the shape check below, matching bash.
		agePubkey = derived
	}

	// bech32 age public key. Validated because a typo here is silent: sops
	// accepts the config and every later encrypt writes a recipient nobody
	// holds.
	if !agePubkeyRe.MatchString(agePubkey) {
		ui.Errorf(c.ErrOut, "not a valid age public key: %s", agePubkey)
		return ErrPrinted
	}

	sopsConfig := c.sopsConfigPath()
	if !isFile(sopsConfig) {
		ui.Errorf(c.ErrOut, "%s not found — run: lo secrets init", sopsConfig)
		return ErrPrinted
	}

	raw, err := os.ReadFile(sopsConfig)
	if err != nil {
		return err
	}
	if strings.Contains(string(raw), agePubkey) {
		fmt.Fprintln(c.Out, "recipient already present in .sops.yaml — nothing to add")
		fmt.Fprintf(c.Out, "  %s\n", agePubkey)
		return nil
	}

	// Append to every creation rule's age list (comma-separated, sops' own
	// format).
	before := 0
	edited, after := appendRecipient(string(raw), agePubkey, &before)
	if err := os.WriteFile(sopsConfig, []byte(edited), 0o644); err != nil {
		return err
	}
	if after < 1 {
		ui.Errorf(c.ErrOut, "failed to add the recipient to %s — is the age: line quoted with '?", sopsConfig)
		return ErrPrinted
	}
	fmt.Fprintf(c.Out, "added recipient to .sops.yaml (%d creation rule(s))\n", before)
	fmt.Fprintf(c.Out, "  %s\n", agePubkey)

	// Re-key the stores. Without this the recipient is listed but holds
	// nothing.
	var stores []string
	if all {
		matches, _ := filepath.Glob(c.Paths.Clusters + "/*/secrets")
		sort.Strings(matches)
		for _, d := range matches {
			// bash's * glob skips dot-directories; Go's does not.
			if strings.HasPrefix(filepath.Base(filepath.Dir(d)), ".") {
				continue
			}
			if isDir(d) {
				stores = append(stores, d)
			}
		}
	} else {
		stores = append(stores, c.StorePath())
	}
	if len(stores) == 0 {
		ui.Errorf(c.ErrOut, "no secret store found to re-key")
		return ErrPrinted
	}

	orphans := 0
	for _, store := range stores {
		for _, base := range storeEntries(store, "Secret.") {
			if !strings.HasSuffix(base[len("Secret."):], ".enc") {
				continue
			}
			enc := store + "/" + base
			if !exists(enc) {
				continue
			}
			if !isFile(strings.TrimSuffix(enc, ".enc")) {
				ui.Warnf(c.ErrOut, "orphan (no decrypted twin, cannot re-key): %s", strings.TrimPrefix(enc, c.Paths.Base+"/"))
				orphans++
			}
		}
	}
	if orphans > 0 && !skipOrphans {
		ui.Errorf(c.ErrOut, "%d encrypted file(s) have no decrypted twin — they would keep the OLD", orphans)
		ui.Errorf(c.ErrOut, "  recipient list while .sops.yaml claims the new key was added.")
		ui.Errorf(c.ErrOut, "  Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.")
		return ErrPrinted
	}

	rekeyed := 0
	now := time.Now()
	for _, store := range stores {
		for _, base := range storeEntries(store, "Secret.") {
			plain := store + "/" + base
			// bash tests [[ -e && != *.enc ]] — any existing entry counts,
			// not just regular files.
			if !exists(plain) || strings.HasSuffix(base, ".enc") {
				continue
			}
			// The freshness skip in Encrypt is keyed on mtime, and a
			// recipient change does not touch the plaintext — so nudge it,
			// which is exactly the manual step this command exists to remove.
			if err := os.Chtimes(plain, now, now); err != nil {
				return err
			}
			rekeyed++
		}
	}
	fmt.Fprintf(c.Out, "re-keying %d cache file(s) across %d store(s)…\n", rekeyed, len(stores))
	// No name filter on purpose: a re-key rewrote every cache file in every
	// store, so the whole store is exactly what must be re-encrypted here.
	// (Faithful to bash: the sweep covers the CURRENT context's store; other
	// stores' touched plaintexts stay newer than their .enc twins, so a later
	// per-domain encrypt still picks them up.)
	if err := c.Encrypt(""); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "done — commit .sops.yaml and the updated .enc files together")
	return nil
}

// ageLineRe is the bash sed's address: a creation rule's single-quoted age
// recipient list.
var ageLineRe = regexp.MustCompile(`^([[:space:]]*age:[[:space:]]*')([^']*)(')`)

// appendRecipient applies the bash sed
// `s|^([[:space:]]*age:[[:space:]]*')([^']*)(')|\1\2,KEY\3|` line-wise and
// returns the edited content, filling before (count of lines containing
// "age:") and returning after (count of lines containing the new key).
func appendRecipient(content, agePubkey string, before *int) (string, int) {
	lines := strings.Split(content, "\n")
	after := 0
	for i, line := range lines {
		if strings.Contains(line, "age:") {
			*before++
		}
		if m := ageLineRe.FindStringSubmatch(line); m != nil {
			lines[i] = strings.Replace(line, m[0], m[1]+m[2]+","+agePubkey+m[3], 1)
		}
		if strings.Contains(lines[i], agePubkey) {
			after++
		}
	}
	return strings.Join(lines, "\n"), after
}

// ── Set ──────────────────────────────────────────────────

// Set writes a literal value into the secret cache (bash: secrets::set).
// value semantics mirror the argsh spec: a bare "-" reads stdin, an
// empty/omitted value falls back to a silent tty prompt or piped stdin
// (command-substitution semantics: trailing newlines stripped).
func (c *Context) Set(name, namespace, key, value string, doEncrypt bool) error {
	if name == "" {
		ui.Errorf(c.ErrOut, "Secret --name is required")
		return ErrPrinted
	}
	if key == "" {
		ui.Errorf(c.ErrOut, "Key argument is required")
		return ErrPrinted
	}

	if value == "-" {
		// argsh's :~stdin type: an explicit `-` reads stdin during parsing.
		raw, err := io.ReadAll(c.Stdin)
		if err != nil {
			return err
		}
		value = trimTrailingNewlines(string(raw))
	}
	if value == "" {
		if c.StdinIsTTY() {
			fmt.Fprintf(c.ErrOut, "Enter value for %s/%s: ", name, key)
			v, err := c.ReadPassword()
			fmt.Fprintln(c.ErrOut)
			if err != nil {
				return err
			}
			// bash `read -rs value` strips leading/trailing IFS whitespace.
			value = strings.Trim(v, " \t")
		} else {
			raw, err := io.ReadAll(c.Stdin)
			if err != nil {
				return err
			}
			value = trimTrailingNewlines(string(raw))
		}
	}

	if value == "" {
		ui.Errorf(c.ErrOut, "Empty value")
		return ErrPrinted
	}

	if err := c.ensureStore(); err != nil {
		return err
	}
	secretsDir := c.StorePath()
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		return err
	}

	cacheFile := secretsDir + "/Secret." + name + "." + namespace + "." + key
	if err := os.WriteFile(cacheFile, []byte(value), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(cacheFile, 0o600); err != nil {
		return err
	}
	ui.Debug("Wrote %s", cacheFile)

	if doEncrypt {
		// Encrypt ONLY this one file — never a whole-store sweep (a sweep
		// would re-encrypt every other plaintext entry, a documented staging
		// hazard). encryptFile errors + returns non-zero when .sops.yaml is
		// absent.
		if err := c.encryptFile(cacheFile); err != nil {
			return err
		}
		c.liveDrift(name, namespace, key, value)
		fmt.Fprintf(c.Out, "Set + encrypted %s/%s/%s\n", name, namespace, key)
		return nil
	}

	// Plaintext-only write. If encryption is configured (.sops.yaml present)
	// the freshly-written cache has no matching .enc — either none exists yet
	// (a first-time write) or the committed one is now stale. Either way it
	// is not committable as-is, so always warn (there is deliberately no
	// quiet level to gate this on). Silent when .sops.yaml is absent
	// (encryption isn't set up). The wording covers both cases (missing vs.
	// stale) without asserting which.
	if isFile(c.sopsConfigPath()) {
		ui.Warnf(c.ErrOut, "wrote plaintext cache only — no matching .enc for this value (missing or now stale); run 'lo secrets encrypt' or re-run with --encrypt/-e before committing")
	}
	c.liveDrift(name, namespace, key, value)
	fmt.Fprintf(c.Out, "Set %s/%s/%s\n", name, namespace, key)
	return nil
}

// ── Encrypt ──────────────────────────────────────────────

// encryptFile encrypts a single plaintext cache file to <file>.enc with SOPS
// (bash: secrets::_encrypt_file). Gated on ${PATH_BASE}/.sops.yaml existing
// (its presence = "encryption is configured"); errors + returns non-nil when
// it's absent. Shared by Encrypt (the whole-store sweep) and Set with encrypt
// (this one file only) so both go through the same .sops.yaml check + sops
// invocation.
//
// The pinned config is load-bearing: without it sops discovers config by
// walking up from the FILE's path, so a different .sops.yaml higher up (e.g.
// the repo root, or under Docker) could silently supply other
// creation_rules/recipients than the one we validated.
func (c *Context) encryptFile(plaintext string) error {
	sopsConfig := c.sopsConfigPath()
	if !isFile(sopsConfig) {
		ui.Errorf(c.ErrOut, "No .sops.yaml found — run: lo secrets init")
		return ErrPrinted
	}
	ui.Debug("Encrypting %s", filepath.Base(plaintext))
	if err := sopsEncryptFile(sopsConfig, plaintext, plaintext+".enc"); err != nil {
		// The bash path surfaces the sops CLI's own stderr before the [error]
		// line; the library error stands in for it here.
		fmt.Fprintln(c.ErrOut, err)
		ui.Errorf(c.ErrOut, "Failed to encrypt %s", filepath.Base(plaintext))
		return ErrPrinted
	}
	return nil
}

// Encrypt sweeps plaintext cache files into .enc twins (bash:
// secrets::encrypt). name narrows the sweep to ONE Secret's cache files.
//
// Without it this walks the whole store and encrypts every plaintext whose
// .enc is older — including entries the operator was mid-edit on, or values
// they were only trying out. That is fine as the deliberate "stage
// everything" move and wrong as the default for "I changed one secret", which
// is the common case; committing an .enc for a value nobody meant to publish
// is not something the store can walk back.
//
// The sweep includes .sha files on purpose — they get .sha.enc twins.
func (c *Context) Encrypt(name string) error {
	// The name lands in a glob under the store dir; a slash or .. would let
	// it address files outside the store. Same charset the domain resolver
	// enforces.
	if name != "" && !nameRe.MatchString(name) {
		ui.Errorf(c.ErrOut, "invalid --name '%s' — a Secret name is [a-zA-Z0-9][a-zA-Z0-9._-]*", name)
		return ErrPrinted
	}

	secretsDir := c.StorePath()
	if !isDir(secretsDir) {
		ui.Warnf(c.ErrOut, "No secrets directory: %s", secretsDir)
		return nil
	}

	// Ensure .gitignore blocks plaintext before we create .enc files
	if err := ensureGitignore(secretsDir); err != nil {
		return err
	}

	if !isFile(c.sopsConfigPath()) {
		ui.Errorf(c.ErrOut, "No .sops.yaml found — run: lo secrets init")
		return ErrPrinted
	}

	// Cache files are Secret.<name>.<namespace>.<key>, so name anchors on the
	// second field.
	prefix := "Secret."
	if name != "" {
		prefix = "Secret." + name + "."
	}

	count, skipped := 0, 0
	for _, base := range storeEntries(secretsDir, prefix) {
		plaintext := secretsDir + "/" + base
		if !isFile(plaintext) {
			continue
		}
		// Skip .enc files
		if strings.HasSuffix(base, ".enc") {
			continue
		}

		// Skip if .enc is newer than plaintext (already up to date)
		encFile := plaintext + ".enc"
		if isFile(encFile) && newerThan(encFile, plaintext) {
			skipped++
			continue
		}

		if err := c.encryptFile(plaintext); err != nil {
			return err
		}
		count++
	}

	if count > 0 {
		fmt.Fprintf(c.Out, "Encrypted %d secret(s)\n", count)
	}
	if skipped > 0 {
		ui.Debug("Skipped %d (already up to date)", skipped)
	}
	if count == 0 && skipped == 0 {
		// A name that matched nothing is a FAILURE, not a quiet no-op. The
		// operator asked for one specific Secret; a typo would otherwise
		// print a reassuring line, exit 0, and leave them committing whatever
		// stale .enc is already on disk. Without a name an empty store is
		// genuinely nothing to do.
		if name != "" {
			ui.Errorf(c.ErrOut, "no cache files for Secret '%s' in %s", name, secretsDir)
			ui.Errorf(c.ErrOut, "  expected %s/Secret.%s.<namespace>.<key>; check the name or run without --name to see the store", secretsDir, name)
			return ErrPrinted
		}
		fmt.Fprintf(c.Out, "No plaintext secrets found in %s\n", secretsDir)
	}
	return nil
}

// ── Decrypt ──────────────────────────────────────────────

// Decrypt restores plaintext cache files from .enc twins (bash:
// secrets::decrypt).
func (c *Context) Decrypt(sshKey string) error {
	secretsDir := c.StorePath()
	if !isDir(secretsDir) {
		ui.Warnf(c.ErrOut, "No secrets directory: %s", secretsDir)
		return nil
	}

	// Resolve the age private key. Checked in order:
	//   1. SOPS_AGE_KEY env var (CI, explicit)
	//   2. SOPS_AGE_KEY_FILE (~/.config/sops/age/keys.txt, SOPS standard)
	//   3. derived from SSH private key (only works without passphrase)
	// The bash "install ssh-to-age (b install)" variant is gone — the
	// derivation is a library call here and always available.
	ageKey := os.Getenv("SOPS_AGE_KEY")
	if ageKey == "" {
		ageKey = keysFileIdentity()
	}
	if ageKey == "" {
		if !isFile(sshKey) {
			ui.Errorf(c.ErrOut, "SSH private key not found: %s", sshKey)
			return ErrPrinted
		}
		derived, err := deriveAgePrivateKey(sshKey)
		if err != nil {
			// Contained: the library error (like the binary's stderr) can
			// carry input material for a private key — never surface it.
			ui.Errorf(c.ErrOut, "Failed to derive age key from %s", sshKey)
			ui.Errorf(c.ErrOut, "If your SSH key is passphrase-protected, derive once:")
			ui.Errorf(c.ErrOut, "  mkdir -p ~/.config/sops/age")
			ui.Errorf(c.ErrOut, "  ssh-to-age -private-key -i %s > ~/.config/sops/age/keys.txt", sshKey)
			ui.Errorf(c.ErrOut, "  chmod 600 ~/.config/sops/age/keys.txt")
			ui.Errorf(c.ErrOut, "(you'll be prompted for the passphrase once)")
			return ErrPrinted
		}
		ageKey = derived
	}

	count, skipped := 0, 0
	for _, base := range storeEntries(secretsDir, "Secret.") {
		if !strings.HasSuffix(base[len("Secret."):], ".enc") {
			continue
		}
		encFile := secretsDir + "/" + base
		if !isFile(encFile) {
			continue
		}

		// Skip if plaintext is newer than .enc (already decrypted)
		plaintext := strings.TrimSuffix(encFile, ".enc")
		if isFile(plaintext) && newerThan(plaintext, encFile) {
			skipped++
			continue
		}

		ui.Debug("Decrypting %s", base)
		raw, err := os.ReadFile(encFile)
		if err == nil {
			var dec []byte
			if dec, err = sopsDecryptData(raw, ageKey); err == nil {
				err = os.WriteFile(plaintext, dec, 0o600)
			}
		}
		if err != nil {
			// The bash path surfaces the sops CLI's own stderr before the
			// [error] line; the library error stands in for it. It never
			// carries key material.
			fmt.Fprintln(c.ErrOut, err)
			ui.Errorf(c.ErrOut, "Failed to decrypt %s", base)
			return ErrPrinted
		}
		if err := os.Chmod(plaintext, 0o600); err != nil {
			return err
		}
		nudgeNewer(plaintext, encFile)
		count++
	}

	if count > 0 {
		fmt.Fprintf(c.Out, "Decrypted %d secret(s)\n", count)
	}
	if skipped > 0 {
		ui.Debug("Skipped %d (already up to date)", skipped)
	}
	if count == 0 && skipped == 0 {
		fmt.Fprintf(c.Out, "No encrypted secrets found in %s\n", secretsDir)
	}
	return nil
}

package secrets

// The lo secrets operations, ported one-for-one from .lok8s/libs/secrets.
// Every user-visible string, exit path, and ordering quirk mirrors the bash
// implementation; deliberate divergences are limited to tool-not-found checks
// (sops/ssh-to-age are libraries here) and are commented where they occur.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/pkg/storeguard"
)

// ── Init ─────────────────────────────────────────────────

// Init configures SOPS/age encryption from an SSH public key (bash:
// secrets::init). The bash `command -v ssh-to-age/sops` gates disappear —
// both are libraries in the Go build.
func (c *Context) Init(sshKey string) error {
	if !fsutil.IsRegular(sshKey) {
		ui.ErrorTo(c.ErrOut, "SSH public key not found: %s", sshKey)
		ui.ErrorTo(c.ErrOut, "Generate one with: ssh-keygen -t ed25519")
		return ErrHandled
	}

	agePubkey, skipNote, err := deriveAgePublicKey(sshKey)
	if err != nil && !errors.Is(err, errUnsupportedKey) {
		ui.ErrorTo(c.ErrOut, "Failed to derive age public key from %s", sshKey)
		ui.ErrorTo(c.ErrOut, "Only ed25519 SSH keys are supported")
		return ErrHandled
	}
	// bash leaves ssh-to-age's stderr uncontained here (public keys only —
	// leaking a public key is not a leak); the CLI's "skipped key: …" note for
	// a non-ed25519 key is the one line that surfaces, and the derivation
	// result stays empty exactly like the binary's rc-0 skip.
	if skipNote != "" {
		fmt.Fprintln(c.ErrOut, skipNote)
	}

	sopsConfig := c.sopsConfigPath()

	// A non-ed25519 key derives NOTHING (the skip note above). Init refuses
	// it before any other step, on a rerun too: the empty recipient is in
	// every config (grepQF), so a rerun took it for a key that is
	// configured. A config with `age: ''` is one that no encrypt can use.
	if !agePubkeyRe.MatchString(agePubkey) {
		ui.ErrorTo(c.ErrOut, "no age recipient derived from %s (ed25519 SSH keys only) — nothing written to %s", sshKey, sopsConfig)
		return ErrHandled
	}

	// Before the first write (the lock file), and on a rerun with a config
	// too: the .gitignore of the store that init writes (gitignoreOK).
	if !c.gitignoreOK(c.initStorePath(), stepFirstWrite) {
		return ErrHandled
	}

	if fsutil.IsRegular(sopsConfig) {
		// A rerun writes no config, but it checks the .gitignore of the
		// store and writes it when it needs a change, as the first run
		// does. A store that does not exist holds no plaintext.
		if store := c.initStorePath(); fsutil.DirExists(store) {
			if err := c.ensureGitignore(store, stepFirstWrite); err != nil {
				return err
			}
		}
		raw, _ := os.ReadFile(sopsConfig)
		// Check if this key is already present
		if grepQF(string(raw), agePubkey) {
			fmt.Fprintln(c.Out, "age key already configured in .sops.yaml")
			return nil
		}
		ui.WarnTo(c.ErrOut, ".sops.yaml exists but doesn't contain this key")
		ui.WarnTo(c.ErrOut, "Add it with: lo secrets add-key %s", sshKey)
		fmt.Fprintf(c.Out, "Your age public key: %s\n", agePubkey)
		return nil
	}

	// No regular file is at the path. An entry of another kind (a link that
	// names no file, a directory) is refused here, before the lock file is
	// made, and again under the lock.
	if !c.initConfigFree(sopsConfig) {
		return ErrHandled
	}

	// The writes run under the lock that add-key and remove-key hold for
	// .sops.yaml (lock.go), so another init, add-key or remove-key cannot
	// change the file between the check below and the rename. No regular
	// file is at the path (the check above), so the lock is beside the path
	// itself, where add-key takes it for a .sops.yaml that is not a link.
	// The lock is held until Init returns, after the rename.
	unlock, err := c.lockSopsConfig(sopsConfig)
	if err != nil {
		return err
	}
	defer unlock()
	if !c.initConfigFree(sopsConfig) {
		return ErrHandled
	}

	// The store and its .gitignore come before the config: a .sops.yaml is
	// what makes a rerun, and a rerun does not make the store. So a store
	// step that fails leaves no .sops.yaml, and the next init runs as a
	// first run. The .gitignore blocks everything except the SOPS-encrypted
	// .enc files.
	if err := c.ensureStore(); err != nil {
		return err
	}
	dir, err := c.ResolveStore()
	if err != nil {
		return err
	}
	if err := c.makeStore(dir); err != nil {
		return err
	}
	if err := c.ensureGitignore(dir, stepGitignoreWrite); err != nil {
		return err
	}

	content := `# SOPS encryption config for lok8s secret cache files.
# Recipients are age public keys derived from SSH keys via ssh-to-age.
# Add team members with: lo secrets add-key <ssh-pubkey-path|age1…>
creation_rules:
  - path_regex: 'Secret\..*'
    age: '` + agePubkey + `'
`
	// Through a temp file in the same directory and a rename
	// (writeReplacing): a run that stops, or a full disk, leaves no part of
	// a config at the path, and the temp file (..sops.yaml.tmp-*) is never
	// read as the config. The temp file gets mode 0644 before the write:
	// .sops.yaml holds public age recipients only, and it is committed.
	if err := writeReplacing(sopsConfig, 0o644, func(w io.Writer) error {
		return writeInitConfig(w, []byte(content))
	}); err != nil {
		if left, ok := errors.AsType[*tempLeftError](err); ok {
			ui.ErrorTo(c.ErrOut, "cannot remove the temp file %s: remove it by hand", c.rel(left.Name))
		}
		ui.ErrorTo(c.ErrOut, "cannot write %s: no config was written", sopsConfig)
		return ErrHandled
	}

	fmt.Fprintf(c.Out, "Created %s\n", sopsConfig)
	fmt.Fprintf(c.Out, "Age public key: %s\n", agePubkey)
	fmt.Fprintf(c.Out, "Derived from: %s\n", sshKey)
	return nil
}

// writeInitConfig fills the temp file of the .sops.yaml write of Init with
// content (a seam: a test makes the write stop part way).
var writeInitConfig = func(w io.Writer, content []byte) error {
	_, err := w.Write(content)
	return err
}

// initConfigFree reports whether the .sops.yaml path still has no entry,
// read without following a link (bash: the checks before and after
// secrets::_lock in secrets::init). Init runs it before the lock, so a
// refusal makes no lock file, and again under the lock: another init can
// have written the file while this run waited for the lock. An entry that is
// not a regular file (a link that names no file, a directory) is not
// replaced. Each case prints its refusal.
func (c *Context) initConfigFree(path string) bool {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true
	case err == nil && info.Mode().IsRegular():
		ui.ErrorTo(c.ErrOut, "another run wrote %s after this run checked it: this run wrote nothing. Run lo secrets init again", path)
	default:
		ui.ErrorTo(c.ErrOut, "%s is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was written. Remove it, then run lo secrets init again", path)
	}
	return false
}

// grepQF mirrors `grep -qF pattern file` on in-memory content: an empty
// pattern matches any content with at least one line. Init refuses an empty
// recipient before its rerun branch, so the empty pattern does not get here
// from Init.
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
// the ordering is part of the observable contract. A .gitignore of the
// store that is not a regular file, a store of the context that does not
// resolve or that it cannot list, and an .enc of the store that the
// encrypt refuses (a symlink, a directory, a FIFO, a socket or a device),
// are refused BEFORE the write, and the .gitignore of the store is written
// before it, so the run cannot stop half way at them.) The first run
// re-keys every plaintext of the sweep from its cache file: a plaintext
// that is older than its .enc (after a pull) replaces the value of that
// .enc.
//
// A rerun finishes the work of a run that stopped after its write, as far
// as it can detect that work. When .sops.yaml lists the recipient already,
// the run reads the sops envelope of each .enc of the sweep
// (unfinishedEncs, at most 16 MiB each): an .enc that is missing, or that
// does not pass the recipient envelope check (encAdded: an entry of the
// recipient with an armored age block, a mac and a lastmodified), is not
// finished. That check is structural: it does not show that sops wrote the
// entry or that the key can decrypt the file. Before it decides, the run
// refuses what the first run refuses (a store that does not resolve or
// list, an .enc that is not a regular file, a .sops.yaml that is also a
// file that the run writes, an orphan that does not pass the check without
// skipOrphans) and writes the .gitignore. Then it re-keys the files that
// are not finished, and only those, and leaves .sops.yaml as it is. After
// the encrypt it reads the sweep again, and a file that still does not
// pass the check (its creation rule does not list the key) fails the run.
// With nothing left, the run changes nothing.
//
// The writes replace each file with a rename, so a run that stops or is
// killed leaves each file whole, old or new, and a rerun finishes the work
// that it can detect. They are not synced to the disk (no fsync): a power
// loss can lose a write or change their order.
//
// The argument check and the re-key steps are the helpers in rekey.go, which
// RemoveKey shares; rekey_test.go pins this command's output and effects.
func (c *Context) AddKey(key string, all, skipOrphans bool) error {
	agePubkey, err := c.recipientArg(key)
	if err != nil {
		return err
	}

	sopsConfig := c.sopsConfigPath()
	if !fsutil.IsRegular(sopsConfig) {
		ui.ErrorTo(c.ErrOut, "%s not found. Run: lo secrets init", sopsConfig)
		return ErrHandled
	}
	// Before the first write (the lock file): the .gitignore of the store
	// that the encrypt below writes (gitignoreOK). A store that does not
	// resolve is refused below (encryptTargets), and StorePath names
	// another store then.
	if !c.gitignoreOK(c.StorePath(), stepFirstWrite) {
		return ErrHandled
	}
	// The file that the lock, the read and the write use, resolved once
	// (lock.go).
	dest, err := c.sopsConfigDest()
	if err != nil {
		return err
	}
	// One run at a time, from this read to the last encrypt (lock.go).
	unlock, err := c.lockSopsConfig(dest)
	if err != nil {
		return err
	}
	defer unlock()

	raw, err := os.ReadFile(dest)
	if err != nil {
		return err
	}

	// Before the first write, on both paths: the store of the context must
	// resolve and list, with --all too, because the encrypt below re-keys
	// that store (encryptTargets). Found after the write, a store that does
	// not resolve stopped the run with the recipient in .sops.yaml, and a
	// rerun found nothing to re-key. The list is the one of the sweep, and
	// the encrypt below reads this same list (encryptChecked).
	checked, err := c.encryptTargets()
	if err != nil {
		return err
	}
	// Each .enc that the encrypt below writes must be a regular file or
	// absent (encryptFile refuses any other entry, and that refusal comes
	// after the write). A rerun refuses it too, before it decides that
	// nothing is left to do.
	if c.refuseEncPaths(checked.encs()) {
		return ErrHandled
	}
	// The .gitignore and the .enc files that the run writes must not be the
	// file of .sops.yaml: their write would replace it (refuseConfigAlias).
	// A rerun with nothing to do writes the .gitignore too.
	if c.refuseConfigAlias(dest, checked) {
		return ErrHandled
	}
	// A recipient that .sops.yaml lists already is done when the re-key of
	// the sweep is finished. An .enc that is missing or does not pass the
	// recipient envelope check is the work of an earlier run that stopped
	// after its write of .sops.yaml (at the orphan check, or in the
	// encrypt), and this run finishes that work (rerunLeft, finishReKey).
	present := strings.Contains(string(raw), agePubkey)
	var left []string
	if present {
		if left, err = c.rerunLeft(checked, agePubkey, all, skipOrphans); err != nil {
			return err
		}
	}
	// The .gitignore of the store keeps the plaintext cache files out of
	// git. The encrypt writes it too, but after the write of .sops.yaml: a
	// .gitignore that it cannot write stopped the run there, with the
	// recipient in .sops.yaml and no .enc re-keyed. A rerun with nothing to
	// do writes it too. A store directory that does not exist holds no
	// plaintext, and the encrypt skips it.
	if fsutil.DirExists(checked.dir) {
		if err := c.ensureGitignore(checked.dir, stepSopsConfig); err != nil {
			ui.ErrorTo(c.ErrOut, "  add-key did not change .sops.yaml or an .enc file: fix the cause above, then run this command again")
			return err
		}
	}
	if present {
		return c.finishReKey(checked, left, agePubkey)
	}

	// Append to every creation rule's age list (comma-separated, sops' own
	// format). replaceFile writes dest, the file that a symlinked .sops.yaml
	// names, and the link stays a link.
	before := 0
	edited, after := appendRecipient(string(raw), agePubkey, &before)
	if err := replaceFile(dest, []byte(edited)); err != nil {
		if c.reportTempLeft(err) {
			return ErrHandled
		}
		return err
	}
	if after < 1 {
		ui.ErrorTo(c.ErrOut, "failed to add the recipient to %s — is the age: line quoted with '?", sopsConfig)
		return ErrHandled
	}
	fmt.Fprintf(c.Out, "added recipient to .sops.yaml (%d creation rule(s))\n", before)
	fmt.Fprintf(c.Out, "  %s\n", agePubkey)

	// Re-key the stores. Without this the recipient is listed but holds
	// nothing.
	stores, _, err := c.rekeyStores(all)
	if err != nil {
		return err
	}
	if len(stores) == 0 { // every domain store is a path it cannot read
		ui.ErrorTo(c.ErrOut, "no secret store found to re-key")
		return ErrHandled
	}
	// One listing of each store, whose error is checked: the store of the
	// sweep was listed before the write (storeListing).
	names, err := c.storeListing(stores, checked, true)
	if err != nil {
		return err
	}
	orphans, err := c.addKeyOrphans(stores, names, "")
	if err != nil {
		return err
	}
	if len(orphans) > 0 && !skipOrphans {
		c.refuseOrphans(len(orphans))
		return ErrHandled
	}

	rekeyed, err := touchPlaintexts(stores, names)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "re-keying %d cache file(s) across %d store(s)…\n", rekeyed, len(stores))
	// No name filter on purpose: a re-key rewrote every cache file in every
	// store, so the whole store is exactly what must be re-encrypted here.
	// (Faithful to bash: the sweep covers the CURRENT context's store; other
	// stores' touched plaintexts stay newer than their .enc twins, so a later
	// per-domain encrypt still picks them up.) The sweep is the list checked
	// before the write.
	if err := c.encryptChecked(checked); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "done. Commit .sops.yaml and the updated .enc files together")
	return nil
}

// rerunLeft is the check of a rerun of AddKey, when .sops.yaml lists the
// recipient already (bash: the same steps in secrets::add-key), and returns
// the .enc files of the sweep whose re-key is not finished
// (unfinishedEncs). It runs before the decision that nothing is left to
// do. The orphans of the stores of the run (rekeyStores, storeListing) that
// do not pass the recipient envelope check (addKeyOrphans) are refused
// without skipOrphans, with the lines of the first run: an earlier run that
// stopped at the orphan check is not reported as done.
func (c *Context) rerunLeft(checked sweepList, recipient string, all, skipOrphans bool) ([]string, error) {
	left, err := c.unfinishedEncs(checked, recipient)
	if err != nil {
		return nil, err
	}
	stores, _, err := c.rekeyStores(all)
	if err != nil {
		return nil, err
	}
	if len(stores) == 0 { // every domain store is a path it cannot read
		ui.ErrorTo(c.ErrOut, "no secret store found to re-key")
		return nil, ErrHandled
	}
	names, err := c.storeListing(stores, checked, false)
	if err != nil {
		return nil, err
	}
	orphans, err := c.addKeyOrphans(stores, names, recipient)
	if err != nil {
		return nil, err
	}
	if len(orphans) > 0 && !skipOrphans {
		c.refuseOrphans(len(orphans))
		return nil, ErrHandled
	}
	return left, nil
}

// finishReKey ends a rerun of AddKey (bash: the same steps in
// secrets::add-key). With nothing left, it changes nothing. Else it
// re-keys the files of left only: an .enc whose re-key is finished keeps
// its bytes, so a plaintext that is older than a pulled .enc does not
// replace it. Then it reads the sweep again (unfinishedEncs). An .enc that
// still does not pass the recipient envelope check can mean that its
// creation rule does not list the key (for example, the key is in a
// comment of .sops.yaml only): the run names each such file and fails,
// with no "done" line.
func (c *Context) finishReKey(checked sweepList, left []string, recipient string) error {
	if len(left) == 0 {
		fmt.Fprintln(c.Out, "recipient already present in .sops.yaml — nothing to add")
		fmt.Fprintf(c.Out, "  %s\n", recipient)
		return nil
	}
	fmt.Fprintf(c.Out, "recipient already present in .sops.yaml, but %d .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key\n", len(left))
	fmt.Fprintf(c.Out, "  %s\n", recipient)
	plains := make([]string, 0, len(left))
	for _, enc := range left {
		plains = append(plains, strings.TrimSuffix(enc, ".enc"))
	}
	if err := c.encryptSweep(checked.dir, "", true, func() []string { return plains }); err != nil {
		return err
	}
	still, err := c.unfinishedEncs(checked, recipient)
	if err != nil {
		return err
	}
	if len(still) > 0 {
		for _, enc := range still {
			ui.ErrorTo(c.ErrOut, "the sops metadata of %s does not pass the recipient envelope check after the re-key", c.rel(enc))
		}
		ui.ErrorTo(c.ErrOut, "  the key is in .sops.yaml, but the creation rule of these files may not list it (for example, the key is only in a comment): fix .sops.yaml, then run this command again")
		return ErrHandled
	}
	fmt.Fprintln(c.Out, "done. Commit .sops.yaml and the updated .enc files together")
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
func (c *Context) Set(ctx context.Context, name, namespace, key, value string, doEncrypt bool) error {
	if name == "" {
		ui.ErrorTo(c.ErrOut, "Secret --name is required")
		return ErrHandled
	}
	if key == "" {
		ui.ErrorTo(c.ErrOut, "Key argument is required")
		return ErrHandled
	}
	// Before the prompt and any write (storeguard.CheckName): each part of
	// the name of the plaintext file is one part of a path, the file is an
	// entry of the store itself, and its name does not end in .enc (the rule
	// !Secret.*.enc of the .gitignore re-includes it, and git would commit
	// the plaintext). The store of this check is StorePath: the write below
	// checks the resolved store again. Each refusal names the field and its
	// text, never the value.
	base, err := storeguard.CheckName(c.StorePath(), name, namespace, key)
	if err != nil {
		return c.printStop(err)
	}

	if value == "-" {
		// argsh's :~stdin type: an explicit `-` reads stdin during parsing.
		raw, err := io.ReadAll(c.Stdin)
		if err != nil {
			return err
		}
		value = execx.TrimNewlines(string(raw))
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
			value = execx.TrimNewlines(string(raw))
		}
	}

	if value == "" {
		ui.ErrorTo(c.ErrOut, "Empty value")
		return ErrHandled
	}

	if err := c.ensureStore(); err != nil {
		return err
	}
	secretsDir, err := c.ResolveStore()
	if err != nil {
		return err
	}
	if err := c.makeStore(secretsDir); err != nil {
		return err
	}
	// The one plaintext write of a store (storeguard: Guard.WritePlaintext),
	// also of decrypt and the Secret generator. It refuses a file that is
	// not an entry of the resolved store, a name that ends in .enc, and a
	// file that is not a regular file and not absent (a symlink, a
	// directory, a FIFO, a socket or a device). Then it checks the
	// .gitignore that keeps the plaintext out of git, and writes a new
	// private temp file in the store (mode 0600, an exclusive create) and a
	// rename onto the file: a symlink or a hard link there is replaced as a
	// name, and the file that it names keeps its old content.
	cacheFile := secretsDir + "/" + base
	if err := c.writePlaintext(storeguard.Plaintext{Dir: secretsDir, Base: base, Step: stepPlaintext, Who: "set"}, []byte(value)); err != nil {
		return err
	}
	ui.Debug("Wrote %s", printable(cacheFile))

	if doEncrypt {
		// Encrypt ONLY this one file — never a whole-store sweep (a sweep
		// would re-encrypt every other plaintext entry, a documented staging
		// hazard). encryptFile errors + returns non-zero when .sops.yaml is
		// absent.
		if err := c.encryptFile(cacheFile); err != nil {
			return err
		}
		c.liveDrift(ctx, name, namespace, key, value)
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
	if fsutil.IsRegular(c.sopsConfigPath()) {
		ui.WarnTo(c.ErrOut, "wrote plaintext cache only — no matching .enc for this value (missing or now stale); run 'lo secrets encrypt' or re-run with --encrypt/-e before committing")
	}
	c.liveDrift(ctx, name, namespace, key, value)
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
//
// The .enc path must be a regular file or absent (encDestinationOK) before
// sops runs: a symlink (to a file or to a directory), a directory, a FIFO, a
// socket, a device or a path whose lstat fails with another error than "does
// not exist" is refused with the same line as the bash twin, and nothing is
// written. The rename would replace a symlink and refuse a directory. The
// bash twin renames with GNU mv -T (or gmv -T) for the same reason, and
// refuses to encrypt without one: a mv without -T moves the file INTO a
// directory, or into the directory that a symlink names.
func (c *Context) encryptFile(plaintext string) error {
	sopsConfig := c.sopsConfigPath()
	if !fsutil.IsRegular(sopsConfig) {
		ui.ErrorTo(c.ErrOut, "No .sops.yaml found. Run: lo secrets init")
		return ErrHandled
	}
	encPath := plaintext + ".enc"
	if !encDestinationOK(encPath) {
		ui.ErrorTo(c.ErrOut, "%s is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: the .enc was not written. Remove it, then encrypt again", c.rel(encPath))
		return ErrHandled
	}
	ui.Debug("Encrypting %s", filepath.Base(plaintext))
	if err := sopsEncryptFile(sopsConfig, plaintext, encPath); err != nil {
		// The bash path surfaces the sops CLI's own stderr before the [error]
		// line; the library error stands in for it here.
		if !c.reportTempLeft(err) {
			fmt.Fprintln(c.ErrOut, err)
		}
		ui.ErrorTo(c.ErrOut, "Failed to encrypt %s", filepath.Base(plaintext))
		return ErrHandled
	}
	// An .enc with the same time as its plaintext gets a time of the kernel
	// after it (nudgeNewer). A plaintext dated after now stays the newer
	// file, with a warning (bash: the test after the rename in
	// secrets::_encrypt_file): each encrypt writes its .enc again, and an
	// edit of it is not lost. A plaintext that is newer than its .enc but
	// not after now gets no warning: the clock of the kernel, which stamps
	// the .enc, can be behind time.Now().
	if !nudgeNewer(encPath, plaintext) {
		ui.WarnTo(c.ErrOut, "%s has a time in the future: its .enc keeps the time of the encrypt, and each encrypt writes it again. Set the time of the plaintext to now (touch it)", c.rel(plaintext))
	}
	return nil
}

// encDestinationOK is storeguard.DestinationOK: path, read with lstat (a
// symlink is not followed), is a regular file or does not exist (bash:
// secrets::_enc_path_ok; the bash twin tells "does not exist" from the
// other lstat errors with secrets::_absent). It checks the last part of the
// path only: the store directory and its parents are trusted.
func encDestinationOK(path string) bool { return storeguard.DestinationOK(path) }

// sweepPlaintexts returns the plaintext cache files of dir that the encrypt
// sweep reads (plaintextsOf over the names of storeEntries, bash:
// secrets::_sweep_files). Encrypt lists the store with it after the
// .gitignore write.
func sweepPlaintexts(dir, prefix string) []string {
	return plaintextsOf(dir, storeEntries(dir, prefix))
}

// plaintextsOf returns the plaintext cache files among names, entries of
// dir: each regular file whose name does not end in .enc. AddKey lists the
// store once, before its first write (encryptTargets), and gets its sweep
// from those names: it checks the .enc of each file, and its encrypt reads
// that same list (encryptChecked), so a file that the run itself changes is
// not added. Another process that writes to the store during the run is
// outside the threat model.
func plaintextsOf(dir string, names []string) []string {
	var plains []string
	for _, base := range names {
		plaintext := dir + "/" + base
		if !fsutil.IsRegular(plaintext) || strings.HasSuffix(base, ".enc") {
			continue
		}
		plains = append(plains, plaintext)
	}
	return plains
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
		ui.ErrorTo(c.ErrOut, "invalid --name '%s' — a Secret name is [a-zA-Z0-9][a-zA-Z0-9._-]*", name)
		return ErrHandled
	}

	secretsDir, err := c.ResolveStore()
	if err != nil {
		return err
	}
	// Cache files are Secret.<name>.<namespace>.<key>, so name anchors on the
	// second field.
	prefix := "Secret."
	if name != "" {
		prefix = "Secret." + name + "."
	}
	return c.encryptSweep(secretsDir, name, false, func() []string { return sweepPlaintexts(secretsDir, prefix) })
}

// encryptSweep is the sweep of Encrypt over the store secretsDir (bash:
// secrets::_encrypt_store): the .gitignore of the store, the .sops.yaml
// check, then each plaintext of list() whose .enc is not newer. list runs
// after the .gitignore write: Encrypt lists the store there, and AddKey
// gives the list that it checked before its first write (encryptChecked).
// name is for the message when nothing matched.
//
// With force it encrypts every file of the list, also one whose .enc is
// newer: the re-key of AddKey, where a new .enc must list the new recipient
// whatever the times say (an .enc dated in the future stays newer than the
// plaintexts that the run set to now).
func (c *Context) encryptSweep(secretsDir, name string, force bool, list func() []string) error {
	if !fsutil.DirExists(secretsDir) {
		ui.WarnTo(c.ErrOut, "No secrets directory: %s", secretsDir)
		return nil
	}

	// Ensure .gitignore blocks plaintext before we create .enc files
	if err := c.ensureGitignore(secretsDir, stepEncWrites); err != nil {
		return err
	}

	if !fsutil.IsRegular(c.sopsConfigPath()) {
		ui.ErrorTo(c.ErrOut, "No .sops.yaml found. Run: lo secrets init")
		return ErrHandled
	}

	count, skipped := 0, 0
	for _, plaintext := range list() {
		// Skip if .enc is newer than plaintext (already up to date)
		encFile := plaintext + ".enc"
		if !force && fsutil.IsRegular(encFile) && newerThan(encFile, plaintext) {
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
			ui.ErrorTo(c.ErrOut, "no cache files for Secret '%s' in %s", name, secretsDir)
			ui.ErrorTo(c.ErrOut, "  expected %s/Secret.%s.<namespace>.<key>; check the name or run without --name to see the store", secretsDir, name)
			return ErrHandled
		}
		fmt.Fprintf(c.Out, "No plaintext secrets found in %s\n", secretsDir)
	}
	return nil
}

// ── Decrypt ──────────────────────────────────────────────

// Decrypt restores plaintext cache files from .enc twins (bash:
// secrets::decrypt). A plaintext newer than its .enc is kept. Decrypt
// compares it in memory with the .enc, warns by entry name when the two
// differ, and counts both kinds of skip in a summary on stderr (stdout is
// unchanged). The exit code stays 0 for a difference.
func (c *Context) Decrypt(sshKey string) error {
	secretsDir, err := c.ResolveStore()
	if err != nil {
		return err
	}
	if !fsutil.DirExists(secretsDir) {
		ui.WarnTo(c.ErrOut, "No secrets directory: %s", secretsDir)
		return nil
	}
	// Before any write: a ciphertext whose plaintext name ends in .enc
	// (refuseReservedDestinations), and a plaintext name that is not a
	// regular file and not absent (refuseLinkedDestinations). Then, before
	// the first plaintext, the .gitignore that keeps it out of git.
	if c.refuseReservedDestinations(secretsDir) || c.refuseLinkedDestinations(secretsDir) {
		return ErrHandled
	}
	if err := c.ensureGitignore(secretsDir, stepDecrypt); err != nil {
		return err
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
		if !fsutil.IsRegular(sshKey) {
			ui.ErrorTo(c.ErrOut, "SSH private key not found: %s", sshKey)
			return ErrHandled
		}
		derived, err := deriveAgePrivateKey(sshKey)
		if err != nil {
			// Contained: the library error (like the binary's stderr) can
			// carry input material for a private key — never surface it.
			ui.ErrorTo(c.ErrOut, "Failed to derive age key from %s", sshKey)
			ui.ErrorTo(c.ErrOut, "If your SSH key is passphrase-protected, derive once:")
			ui.ErrorTo(c.ErrOut, "  mkdir -p ~/.config/sops/age")
			ui.ErrorTo(c.ErrOut, "  ssh-to-age -private-key -i %s > ~/.config/sops/age/keys.txt", sshKey)
			ui.ErrorTo(c.ErrOut, "  chmod 600 ~/.config/sops/age/keys.txt")
			ui.ErrorTo(c.ErrOut, "(you'll be prompted for the passphrase once)")
			return ErrHandled
		}
		ageKey = derived
	}

	count, skippedEqual, skippedDiffer := 0, 0, 0
	for _, base := range storeEntries(secretsDir, "Secret.") {
		if !strings.HasSuffix(base[len("Secret."):], ".enc") {
			continue
		}
		encFile := secretsDir + "/" + base
		if !fsutil.IsRegular(encFile) {
			continue
		}

		// Skip if plaintext is newer than .enc (already decrypted). The skip
		// keeps the plaintext, so compare it in memory with the decrypted
		// .enc: a plaintext that differs holds a value the .enc does not (a
		// local edit, or a value a build generated before the decrypt).
		// Name it, never its value.
		plaintext := strings.TrimSuffix(encFile, ".enc")
		if fsutil.IsRegular(plaintext) && newerThan(plaintext, encFile) {
			cur, err := os.ReadFile(plaintext)
			if err != nil {
				ui.ErrorTo(c.ErrOut, "Failed to read %s", printable(filepath.Base(plaintext)))
				return ErrHandled
			}
			dec, err := decryptEncFile(encFile, ageKey)
			if err != nil {
				// No library error on this path: a sops error can carry
				// digests of the plaintext (bash discards the CLI's stderr
				// here for the same reason).
				ui.ErrorTo(c.ErrOut, "Failed to decrypt %s", printable(base))
				return ErrHandled
			}
			if bytes.Equal(dec, cur) {
				skippedEqual++
			} else {
				ui.WarnTo(c.ErrOut, "%s: the plaintext is newer than %s and differs from it. Decrypt kept the plaintext.", printable(filepath.Base(plaintext)), printable(base))
				skippedDiffer++
			}
			continue
		}

		ui.Debug("Decrypting %s", printable(base))
		raw, err := os.ReadFile(encFile)
		var dec []byte
		if err == nil {
			dec, err = sopsDecryptData(raw, ageKey)
		}
		if err != nil {
			// The [error] line names the file only. The bash path discards
			// the sops CLI's stderr: a MAC mismatch prints two unkeyed
			// SHA-512 digests of the plaintext. Both print this one line.
			ui.ErrorTo(c.ErrOut, "Failed to decrypt %s", printable(base))
			return ErrHandled
		}
		// The one plaintext write of a store (Guard.WritePlaintext): the
		// checks above ran for each entry before the first write, and the
		// write checks this entry and the .gitignore again. A new private
		// temp file in the store (mode 0600) and a rename onto the plaintext
		// file: a hard link there is replaced as a name, and its other name
		// keeps its old content.
		if err := c.writePlaintext(storeguard.Plaintext{Dir: secretsDir, Base: filepath.Base(plaintext), Step: stepPlaintext, Who: "decrypt"}, dec); err != nil {
			return err
		}
		// An .enc dated in the future keeps the plaintext the older file,
		// as in the bash twin, which sets no time. On an equal time the
		// plaintext gets a time of the kernel after the .enc.
		_ = nudgeNewer(plaintext, encFile)
		count++
	}

	// A differing plaintext is not an error: `lo secrets set` without
	// --encrypt makes exactly that state on purpose. The warnings name each
	// entry, and this line gives the two ways out once.
	if skippedDiffer > 0 {
		ui.WarnTo(c.ErrOut, "%d plaintext(s) differ from the .enc. To store them, run \"lo secrets encrypt%s\". To use the .enc values, delete these plaintexts, then run \"lo secrets decrypt%s\".",
			skippedDiffer, domainFlag(c.Domain), domainFlag(c.Domain))
	}
	if count > 0 {
		fmt.Fprintf(c.Out, "Decrypted %d secret(s)\n", count)
	}
	// The skip count goes to stderr: piped stdout is the contract (memory
	// rule), and it carried no skip line before.
	skipped := skippedEqual + skippedDiffer
	if skipped > 0 {
		fmt.Fprintf(c.ErrOut, "Skipped %d secret(s) with a newer plaintext: %d equal, %d differ\n", skipped, skippedEqual, skippedDiffer)
	}
	if count == 0 && skipped == 0 {
		fmt.Fprintf(c.Out, "No encrypted secrets found in %s\n", secretsDir)
	}
	return nil
}

// refuseReservedDestinations prints a refusal and returns true when a
// ciphertext of the store dir would decrypt to a plaintext whose name ends
// in .enc (reservedPlaintextName; bash: secrets::_decrypt_reserved), for
// example Secret.app.default.enc.enc. The rule !Secret.*.enc of the
// .gitignore re-includes that plaintext. Decrypt runs it before its first
// write. It reads the same entries as the decrypt loop.
func (c *Context) refuseReservedDestinations(dir string) bool {
	refused := false
	for _, base := range storeEntries(dir, "Secret.") {
		if !strings.HasSuffix(base[len("Secret."):], ".enc") || !fsutil.IsRegular(dir+"/"+base) {
			continue
		}
		if dest := strings.TrimSuffix(base, ".enc"); reservedPlaintextName(dest) {
			ui.ErrorTo(c.ErrOut, "%s: decrypt would write the plaintext file %s, which ends in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it", printable(c.rel(dir+"/"+base)), printable(dest))
			refused = true
		}
	}
	if refused {
		ui.ErrorTo(c.ErrOut, "decrypt wrote no plaintext file: move these .enc files out of the store, then run this command again")
	}
	return refused
}

// refuseLinkedDestinations prints a refusal and returns true when the
// plaintext file of a ciphertext of the store dir is not a regular file
// and not absent (encDestinationOK; bash: secrets::_decrypt_links): a
// symlink, a directory, a FIFO, a socket or a device, or its lookup fails.
// A symlink there can name a file that git commits (Secret.x.enc, the
// .gitignore). Decrypt runs it before its first write, also for a
// plaintext that it would keep. It reads the same entries as the decrypt
// loop.
func (c *Context) refuseLinkedDestinations(dir string) bool {
	refused := false
	for _, base := range storeEntries(dir, "Secret.") {
		if !strings.HasSuffix(base[len("Secret."):], ".enc") || !fsutil.IsRegular(dir+"/"+base) {
			continue
		}
		if dest := dir + "/" + strings.TrimSuffix(base, ".enc"); !encDestinationOK(dest) {
			ui.ErrorTo(c.ErrOut, "%s is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: decrypt would write the plaintext of %s there", printable(c.rel(dest)), printable(base))
			refused = true
		}
	}
	if refused {
		ui.ErrorTo(c.ErrOut, "decrypt wrote no plaintext file: remove these entries, then run this command again")
	}
	return refused
}

// decryptEncFile decrypts encFile in memory. Nothing is written: the
// decrypted value lives only in the caller.
func decryptEncFile(encFile, ageKey string) ([]byte, error) {
	raw, err := os.ReadFile(encFile)
	if err != nil {
		return nil, err
	}
	return sopsDecryptData(raw, ageKey)
}

// domainFlag is the " --domain <d>" suffix of a hint command, empty without
// a domain.
func domainFlag(d string) string {
	if d == "" {
		return ""
	}
	return " --domain " + d
}

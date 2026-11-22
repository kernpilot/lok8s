package secrets

// rekey.go: the re-key path that add-key and remove-key share, and
// remove-key itself. Each helper has a bash twin of the same shape in
// .lok8s/libs/secrets (secrets::_no_private_key, _recipient, _lstat,
// _domain_stores, _rekey_stores, _orphans, _open_orphans, _touch_plaintexts,
// _enc_paths, _encs_with, _other_encs, _enc_lists, _key_added, _unfinished,
// _other_stores, _own_identity, _replace, _drop_recipient, _rekey_files).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"filippo.io/age/armor"
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

// statPath is Lstat, then Stat for a symlink (bash: secrets::_lstat and the
// symlink checks of its callers). absent is true only when Lstat finds no
// entry (ENOENT). Every other error is returned, a dangling or unreadable
// symlink target included, so that a scan names the path instead of
// skipping it.
func statPath(path string) (info os.FileInfo, absent bool, err error) {
	info, err = os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
	} else if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return info, false, nil
}

// domainStores lists every clusters/<domain>/secrets directory in bash glob
// order (bash: secrets::_domain_stores), together with the paths on the way
// that it cannot read, which bad marks: the clusters directory, a domain
// entry whose symlink target it cannot stat, a store path that Lstat or Stat
// cannot read (a domain directory it cannot search, a dangling store link). A
// path that does not exist is skipped, as the glob skips it.
func (c *Context) domainStores() (paths []string, bad map[string]bool) {
	bad = map[string]bool{}
	unreadable := func(p string) {
		paths = append(paths, p)
		bad[p] = true
	}
	clusters := c.Paths.Clusters
	info, absent, err := statPath(clusters)
	if absent || (err == nil && !info.IsDir()) {
		return nil, bad
	}
	var entries []os.DirEntry
	if err == nil {
		entries, err = os.ReadDir(clusters)
	}
	if err == nil {
		_, err = os.Stat(clusters + "/.")
	}
	if err != nil {
		unreadable(clusters)
		return paths, bad
	}
	for _, e := range entries {
		// bash's * glob skips dot entries; ReadDir does not.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		d := clusters + "/" + e.Name()
		info, absent, err := statPath(d)
		switch {
		case absent:
			continue
		case err != nil:
			unreadable(d)
			continue
		case !info.IsDir():
			continue
		}
		store := d + "/secrets"
		info, absent, err = statPath(store)
		switch {
		case absent:
		case err != nil:
			unreadable(store)
		case info.IsDir():
			paths = append(paths, store)
		}
	}
	return paths, bad
}

// rekeyStores lists the stores a re-key covers (bash: secrets::_rekey_stores):
// with all, every clusters/<domain>/secrets directory (never the flat store);
// without it, the store of the current context. With all it also returns the
// domain paths it cannot read: remove-key refuses them; add-key, which wrote
// .sops.yaml before this step, does not read them. When it can read no store
// and found paths it cannot read, it returns those paths and no error, so
// that remove-key names them in its refusal; add-key then stops with "no
// secret store found to re-key", as before.
func (c *Context) rekeyStores(all bool) (stores, unreadable []string, err error) {
	if all {
		paths, bad := c.domainStores()
		for _, p := range paths {
			if bad[p] {
				unreadable = append(unreadable, p)
			} else {
				stores = append(stores, p)
			}
		}
	} else {
		dir, rerr := c.ResolveStore()
		if rerr != nil {
			return nil, nil, rerr
		}
		stores = append(stores, dir)
	}
	if len(stores) == 0 && len(unreadable) == 0 {
		ui.ErrorTo(c.ErrOut, "no secret store found to re-key")
		return nil, nil, ErrHandled
	}
	return stores, unreadable, nil
}

// isEncEntry is the bash glob Secret.*.enc on a store entry's name.
func isEncEntry(base string) bool {
	return strings.HasSuffix(base[len("Secret."):], ".enc")
}

// managedEnc reports whether rel, a path relative to a store, is a managed
// Secret.*.enc at the top of the store: a file that the scans read.
func managedEnc(rel string) bool {
	return !strings.Contains(rel, "/") && strings.HasPrefix(rel, "Secret.") && isEncEntry(rel)
}

// otherEncs walks each store, at any depth and dot names included, for an
// entry whose name ends in .enc and that is not a managed Secret.*.enc at the
// top of the store (bash: secrets::_other_encs). remove-key re-keys only the
// managed files, so such a copy (a backup.enc, a file in a subdirectory)
// would keep the removed recipient. It returns those paths and the
// directories below a store that it cannot list or search, each list sorted
// by byte per store. The walk follows no symlink: a link is an entry, never a
// directory to walk into; a store that is a symlink to a directory is walked.
// A directory named *.enc is walked, not listed. A store that it cannot
// search is left to scanEncs, which names it (or skips it when it does not
// exist). The temp file of an encrypt (writeReplacing:
// .<name>.enc.tmp-<random>) does not end in .enc.
func otherEncs(stores []string) (other, unreadable []string) {
	for _, store := range stores {
		if _, err := os.Stat(store + "/."); err != nil {
			continue
		}
		var found, bad []string
		_ = fs.WalkDir(os.DirFS(store), ".", func(rel string, d fs.DirEntry, err error) error {
			if rel == "." {
				return nil // the store itself: scanEncs reads it
			}
			p := store + "/" + rel
			switch {
			case err != nil: // a directory it cannot list
				bad = append(bad, p)
			case d.IsDir():
				if _, err := os.Stat(p + "/."); err != nil { // a directory it cannot search
					bad = append(bad, p)
					return fs.SkipDir
				}
			case strings.HasSuffix(rel, ".enc") && !managedEnc(rel):
				found = append(found, p)
			}
			return nil
		})
		slices.Sort(found)
		slices.Sort(bad)
		other = append(other, found...)
		unreadable = append(unreadable, bad...)
	}
	return other, unreadable
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
// step these commands exist to remove. The time is the one of the kernel
// (touchNow, as the touch of the bash twin), not time.Now(): an .enc that
// the encrypt writes right after is then never older than its plaintext.
// It reads the names of each store from names (storeListing) and does not
// list a store again.
func touchPlaintexts(stores []string, names map[string][]string) (int, error) {
	touched := 0
	for _, store := range stores {
		for _, base := range names[store] {
			plain := store + "/" + base
			// bash tests [[ -e && != *.enc ]] — any existing entry counts,
			// not just regular files.
			if !fsutil.Exists(plain) || strings.HasSuffix(base, ".enc") {
				continue
			}
			if err := touchNow(plain); err != nil {
				return touched, err
			}
			touched++
		}
	}
	return touched, nil
}

// sweepList is the list of the encrypt sweep that AddKey checks before its
// first write, and that its encrypt then reads (encryptChecked): the store
// of the context, the Secret.* names of its one listing (names), and the
// plaintext cache files among them (plains). The later steps of AddKey read
// names for that store (storeListing) and do not list it again.
type sweepList struct {
	dir    string
	names  []string
	plains []string
}

// encryptTargets resolves the store of the context and lists the plaintext
// cache files that the sweep of Encrypt("") reads there (bash: the list
// before the first write of secrets::add-key), with one listing whose error
// it checks (listStore). AddKey reads it before its first write, also when
// .sops.yaml lists the recipient already, and stops with --all too: its
// encrypt re-keys this store. A store that does not resolve is refused
// (ResolveStore prints why), and so is a store that it cannot list
// (refuseUnlistable): an empty list would let the run report the re-key as
// done.
func (c *Context) encryptTargets() (sweepList, error) {
	dir, err := c.ResolveStore()
	if err != nil {
		ui.ErrorTo(c.ErrOut, "  add-key did not change .sops.yaml: create that directory, or select a domain that has a store, then run this command again")
		return sweepList{}, err
	}
	names, err := listStore(dir)
	if err != nil {
		c.refuseUnlistable(dir, false)
		return sweepList{}, ErrHandled
	}
	return sweepList{dir: dir, names: names, plains: plaintextsOf(dir, names)}, nil
}

// listStore lists the store dir once and returns its Secret.* names, or the
// error of a store that it cannot list (bash: secrets::_store_names and its
// callers): a path that is not a directory, a directory that it cannot read
// or search, a read of the directory that fails (storeNames returns the
// error of the read), a symlink whose target it cannot stat. A store that
// does not exist holds no file and is no error: the encrypt warns about it.
func listStore(dir string) ([]string, error) {
	_, absent, err := statPath(dir)
	if absent {
		return nil, nil
	}
	var names []string
	if err == nil {
		names, err = storeNames(dir, "Secret.")
	}
	if err == nil {
		_, err = os.Stat(dir + "/.")
	}
	if err != nil {
		return nil, err
	}
	return names, nil
}

// refuseUnlistable prints the refusal of AddKey for a store that it cannot
// list (listStore; bash: secrets::_refuse_unlistable). wrote tells whether
// the run wrote .sops.yaml before that listing: only a store of --all that
// is not the store of the sweep is listed after the write.
func (c *Context) refuseUnlistable(dir string, wrote bool) {
	outcome := "add-key did not change .sops.yaml"
	if wrote {
		outcome = "add-key wrote .sops.yaml and stopped before the re-key"
	}
	ui.ErrorTo(c.ErrOut, "cannot list the store %s (not a directory, no read or search permission, or the read of the directory failed): %s; fix the store, then run this command again", c.rel(dir), outcome)
}

// storeListing returns the Secret.* names of each store of the re-key
// (bash: secrets::_rekey_names): for the store of the sweep, the names of
// its one listing (checked.names), and for each other store (--all), one
// listing whose error it checks (listStore). The orphan check and the touch
// read these names, so no step of AddKey lists a store again or drops the
// error of a listing. A store that it cannot list is refused
// (refuseUnlistable, with wrote for that line): ErrHandled.
func (c *Context) storeListing(stores []string, checked sweepList, wrote bool) (map[string][]string, error) {
	names := make(map[string][]string, len(stores))
	for _, store := range stores {
		if store == checked.dir {
			names[store] = checked.names
			continue
		}
		list, err := listStore(store)
		if err != nil {
			c.refuseUnlistable(store, wrote)
			return nil, ErrHandled
		}
		names[store] = list
	}
	return names, nil
}

// refuseConfigAlias refuses a .sops.yaml whose file (dest, after its links)
// is also a file that AddKey writes, and reports whether it refused (bash:
// secrets::_config_alias): the .gitignore of the store, or an .enc of the
// sweep. ensureGitignore and the encrypt replace such a file, so the run
// would replace its own .sops.yaml. It compares the two files (os.SameFile:
// the same device and inode), so a hard link counts too. AddKey calls it
// before its first change of .sops.yaml, the .gitignore or an .enc. A path
// that it cannot stat is not a file that it can compare, and is no alias:
// the .gitignore and an .enc that do not exist yet, and dest after a change
// during the run (outside the threat model). It does not compare an orphan
// .enc, a plaintext or a file outside the store, and it runs once, not
// before each write. The lock file can be new when it refuses.
func (c *Context) refuseConfigAlias(dest string, checked sweepList) bool {
	conf, err := os.Stat(dest)
	if err != nil {
		return false
	}
	for _, out := range append([]string{checked.dir + "/.gitignore"}, checked.encs()...) {
		info, err := os.Stat(out)
		if err != nil || !os.SameFile(conf, info) {
			continue
		}
		ui.ErrorTo(c.ErrOut, ".sops.yaml names %s, and add-key writes that file as %s: add-key stopped before it changed .sops.yaml, the .gitignore or an .enc file, but the lock file %s.lock can be new; give .sops.yaml a file of its own, then run this command again", c.rel(dest), c.rel(out), c.rel(dest))
		return true
	}
	return false
}

// unfinishedEncs returns each .enc of the list l whose re-key is not
// finished for recipient (bash: secrets::_unfinished): an .enc that does not
// exist, a file that it cannot open or read, and a file whose sops envelope
// does not pass the recipient envelope check (keyAdded). refuseEncPaths runs
// before it, so each .enc of the list is a regular file or absent. An .enc
// larger than maxEncRead is refused (keyAdded): ErrHandled. AddKey reads it
// when .sops.yaml lists the recipient already, before and after its
// re-key.
func (c *Context) unfinishedEncs(l sweepList, recipient string) ([]string, error) {
	var left []string
	for _, enc := range l.encs() {
		added, err := c.keyAdded(enc, recipient)
		if err != nil {
			return nil, err
		}
		if !added {
			left = append(left, enc)
		}
	}
	return left, nil
}

// addKeyOrphans warns about each Secret.*.enc of the stores without a
// decrypted twin, and returns them (bash: secrets::_add_key_orphans): the
// orphans that a re-key cannot rewrite. It reads the names of each store
// from names (storeListing). Each listed entry counts, whatever kind of
// entry it is: a symlink whose target does not exist counts too, because it
// is in the store and a re-key cannot rewrite it. With a recipient (a rerun
// of AddKey), an orphan whose sops envelope passes the recipient envelope
// check (keyAdded) needs no re-key and gets no warning, and an .enc larger
// than maxEncRead is refused: ErrHandled. (remove-key uses orphanEncs.)
func (c *Context) addKeyOrphans(stores []string, names map[string][]string, recipient string) ([]string, error) {
	var orphans []string
	for _, store := range stores {
		for _, base := range names[store] {
			enc := store + "/" + base
			if !isEncEntry(base) || fsutil.IsRegular(strings.TrimSuffix(enc, ".enc")) {
				continue
			}
			if recipient != "" {
				added, err := c.keyAdded(enc, recipient)
				if err != nil {
					return nil, err
				}
				if added {
					continue
				}
			}
			ui.WarnTo(c.ErrOut, "orphan (no decrypted twin, cannot re-key): %s", c.rel(enc))
			orphans = append(orphans, enc)
		}
	}
	return orphans, nil
}

// refuseOrphans prints the refusal of add-key for orphan .enc files that a
// re-key cannot rewrite (bash: secrets::_refuse_orphans): the first run and
// a rerun print the same lines.
func (c *Context) refuseOrphans(n int) {
	ui.ErrorTo(c.ErrOut, "%d encrypted file(s) have no decrypted twin — they would keep the OLD", n)
	ui.ErrorTo(c.ErrOut, "  recipient list while .sops.yaml claims the new key was added.")
	ui.ErrorTo(c.ErrOut, "  Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.")
}

// maxEncRead is the largest .enc whose sops metadata add-key reads: 16 MiB,
// a limit of this metadata check. A Kubernetes Secret holds at most 1 MiB,
// and the .enc of one of its keys (the value in base64, in the JSON
// envelope of sops) stays below 2 MiB. The bash twin has the same limit
// (secrets::_key_added).
const maxEncRead = 16 << 20

// errEncTooLarge: the .enc is larger than maxEncRead.
var errEncTooLarge = errors.New("the .enc is larger than the limit of the metadata read")

// errEncNotRegular: the .enc is not a regular file.
var errEncNotRegular = errors.New("the .enc is not a regular file")

// readEncMeta reads the .enc at path once, from one descriptor: the open
// does not follow a symlink and does not wait on a FIFO, the descriptor
// must be a regular file (fstat), and the read stops after maxEncRead
// bytes. It returns errEncTooLarge for a larger file, errEncNotRegular for
// another kind of file, and the error of the open or the read.
func readEncMeta(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- an .enc of the store
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, errEncNotRegular
	case info.Size() > maxEncRead:
		return nil, errEncTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxEncRead+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxEncRead {
		return nil, errEncTooLarge
	}
	return raw, nil
}

// keyAdded reads the .enc at enc (readEncMeta) and reports whether its sops
// envelope passes the recipient envelope check (encAdded). A file that does
// not exist, or that it cannot open or read, does not. A file larger than
// maxEncRead is refused with one line that names it: ErrHandled.
func (c *Context) keyAdded(enc, recipient string) (bool, error) {
	raw, err := readEncMeta(enc)
	if errors.Is(err, errEncTooLarge) {
		ui.ErrorTo(c.ErrOut, "%s is larger than 16 MiB, the size limit of the metadata check: add-key does not read its sops metadata and stops here; restore or remove that file, then run this command again", c.rel(enc))
		return false, ErrHandled
	}
	if err != nil {
		return false, nil
	}
	return encAdded(raw, recipient), nil
}

// encAdded is the recipient envelope check: it reports whether raw, the
// JSON envelope of an .enc, has the structure that sops writes for
// recipient (bash: secrets::_enc_lists with added). encRecipients reads it
// without an error (it parses, holds no member twice, and spells no key of
// the metadata in another case), its sops metadata holds a mac and a
// lastmodified that are strings and not empty, and an age entry of
// recipient (in sops.age or in a sops.key_groups item) holds an enc that is
// an armored age block: its first line is the header of the age armor and
// its last line the footer. A label alone ({"recipient"} with no enc) does
// not pass. Only the enc fields of the entries of recipient take part in
// the envelope test: a fault in the enc of another entry does not make the
// run re-key the file from a plaintext that can be older than the .enc
// (the bash twin reads the same fields). encRecipients still reads every
// entry: a recipient of another entry that is not a non-empty string, or
// a member that the metadata holds twice, fails the check of the whole
// file. The check is structural: it does not decrypt the file, so it does
// not show that sops wrote the entry or that the key can decrypt the file.
func encAdded(raw []byte, recipient string) bool {
	if _, err := encRecipients(raw); err != nil {
		return false
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return false
	}
	meta, _ := top["sops"].(map[string]any)
	for _, name := range []string{"mac", "lastmodified"} {
		v, err := metaField(meta, name)
		if s, ok := v.(string); err != nil || !ok || s == "" {
			return false
		}
	}
	entries, _ := meta["age"].([]any)
	groups, _ := meta["key_groups"].([]any)
	for _, g := range groups {
		group, _ := g.(map[string]any)
		list, _ := group["age"].([]any)
		entries = append(entries, list...)
	}
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if entry["recipient"] != recipient {
			continue
		}
		v, err := metaField(entry, "enc")
		if s, ok := v.(string); err == nil && ok && armoredAge(s) {
			return true
		}
	}
	return false
}

// armoredAge reports whether s is an armored age block: its first line is
// armor.Header and its last line armor.Footer (one newline at the end is
// allowed, as age writes it).
func armoredAge(s string) bool {
	return strings.HasPrefix(s, armor.Header+"\n") && strings.HasSuffix(strings.TrimSuffix(s, "\n"), "\n"+armor.Footer)
}

// encs returns the .enc path of each plaintext of the list.
func (l sweepList) encs() []string {
	var encs []string
	for _, plaintext := range l.plains {
		encs = append(encs, plaintext+".enc")
	}
	return encs
}

// encryptChecked is the encrypt of AddKey (bash: secrets::_encrypt_store
// with list): the sweep of Encrypt("") over the list that AddKey checked
// before its first write. It does not list the store again, so a file that
// the run itself changes (a plaintext link to the .gitignore that the sweep
// writes) is not added. Another process that writes to the store during the
// run is outside the threat model. It encrypts every file of the list,
// whatever the times say (force): an .enc dated in the future would
// otherwise keep the old recipients.
func (c *Context) encryptChecked(l sweepList) error {
	return c.encryptSweep(l.dir, "", true, func() []string { return l.plains })
}

// refuseEncPaths refuses each .enc of encs that the encrypt refuses
// (encDestinationOK: a symlink, a directory, a FIFO, a socket or a device,
// or a path whose lstat fails with an error other than "does not exist"),
// before the first write, and reports whether it refused (bash:
// secrets::_enc_paths). The re-key writes each .enc in place, and
// encryptFile refuses such an entry, so one in the list would stop the run
// after .sops.yaml was written, and a rerun would stop at the same file.
// The last line names what was refused: links, other entries, or both.
func (c *Context) refuseEncPaths(encs []string) bool {
	links, others := 0, 0
	for _, enc := range encs {
		if encDestinationOK(enc) {
			continue
		}
		info, err := os.Lstat(enc)
		switch {
		case err != nil:
			ui.ErrorTo(c.ErrOut, "%s cannot be looked up (an error other than \"no such file or directory\", for example a name that is too long): the re-key does not write an .enc there", c.rel(enc))
			others++
		case info.Mode()&fs.ModeSymlink != 0:
			ui.ErrorTo(c.ErrOut, "%s is a symlink: the re-key does not write an .enc through a link", c.rel(enc))
			links++
		default:
			ui.ErrorTo(c.ErrOut, "%s is not a regular file (a directory, a FIFO, a socket or a device): the re-key does not write an .enc there", c.rel(enc))
			others++
		}
	}
	switch {
	case links+others == 0:
		return false
	case others == 0:
		ui.ErrorTo(c.ErrOut, "  replace each link with the file it names, or remove it from the store, then run this command again; nothing was written")
	case links == 0:
		ui.ErrorTo(c.ErrOut, "  remove or fix each entry named above, then run this command again; nothing was written")
	default:
		ui.ErrorTo(c.ErrOut, "  replace each link with the file it names, and remove or fix each other entry, then run this command again; nothing was written")
	}
	return true
}

// encScan is what scanEncs read in a set of stores.
type encScan struct {
	all        []string // every regular Secret.*.enc
	listed     []string // those whose sops metadata lists the recipient
	unreadable []string // a store or an .enc it cannot stat, list or read
	malformed  []string // an .enc whose sops metadata does not parse or lists no age recipient
	duplicate  []string // an .enc whose sops metadata holds a member twice
	other      []string // an .enc that is not a managed Secret.*.enc (otherEncs; RemoveKey fills it)
}

// scanEncs reads the sops metadata of every Secret.*.enc of the stores (bash:
// secrets::_encs_with). sops writes each age recipient into a file's
// metadata, so a file that lists the recipient is a file its key can open.
// The recipients are read from the parsed metadata (encRecipients) and
// compared exactly, so a JSON escape in the file cannot hide one. A store or
// a file it cannot read, a file whose metadata does not parse, and a file
// whose metadata holds a member twice are reported, never taken as one that
// does not list it.
func scanEncs(stores []string, recipient string) encScan {
	var s encScan
	for _, store := range stores {
		_, absent, err := statPath(store)
		if absent {
			continue // no store, no file
		}
		var names []string
		if err == nil {
			names, err = storeNames(store, "Secret.")
		}
		if err == nil {
			_, err = os.Stat(store + "/.")
		}
		if err != nil {
			s.unreadable = append(s.unreadable, store)
			continue
		}
		for _, base := range names {
			if !isEncEntry(base) {
				continue
			}
			enc := store + "/" + base
			info, absent, err := statPath(enc)
			switch {
			case absent:
				continue
			case err != nil:
				s.unreadable = append(s.unreadable, enc)
				continue
			case !info.Mode().IsRegular():
				continue
			}
			s.all = append(s.all, enc)
			raw, err := os.ReadFile(enc) // #nosec G304 -- an .enc of the store
			if err != nil {
				s.unreadable = append(s.unreadable, enc)
				continue
			}
			rcpts, err := encRecipients(raw)
			switch {
			case errors.Is(err, errEncDuplicate):
				s.duplicate = append(s.duplicate, enc)
			case err != nil:
				s.malformed = append(s.malformed, enc)
			case slices.Contains(rcpts, recipient):
				s.listed = append(s.listed, enc)
			}
		}
	}
	return s
}

// errEncMetadata: the sops metadata of an .enc does not parse or lists no age
// recipient. It never quotes the file.
var errEncMetadata = errors.New("the sops metadata does not parse or lists no age recipient")

// errEncDuplicate: the sops metadata of an .enc holds a member twice. It
// never quotes the file.
var errEncDuplicate = errors.New("the sops metadata holds a duplicate member")

// encRecipients reads the age recipients from the sops metadata of an .enc,
// the JSON envelope of the binary store (bash: secrets::_enc_lists): each
// recipient of sops.age and of the age list of every sops.key_groups entry,
// with its JSON escapes decoded. It returns errEncMetadata when the file is
// not one JSON object with a sops object, when it holds no age entry or an
// age entry without a recipient string, and when a key that the sops
// metadata decoder matches regardless of case is spelled another way ("AGE",
// "Recipient"): sops would then read a value that this read does not. For a
// file that passes those checks, it returns errEncDuplicate when the file
// holds the sops member twice, or an object in the sops value holds a member
// twice (metadataDuplicate): the map decode keeps the last of two members,
// so the first one, with the recipients it holds, is not read.
func encRecipients(raw []byte) ([]string, error) {
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, errEncMetadata
	}
	root, _ := top.(map[string]any)
	meta, ok := root["sops"].(map[string]any)
	if !ok {
		return nil, errEncMetadata
	}
	flat, err := metaField(meta, "age")
	if err != nil {
		return nil, err
	}
	out, err := ageRecipients(flat)
	if err != nil {
		return nil, err
	}
	groups, err := metaField(meta, "key_groups")
	if err != nil {
		return nil, err
	}
	switch g := groups.(type) {
	case nil:
	case []any:
		for _, item := range g {
			group, ok := item.(map[string]any)
			if !ok {
				return nil, errEncMetadata
			}
			age, err := metaField(group, "age")
			if err != nil {
				return nil, err
			}
			rcpts, err := ageRecipients(age)
			if err != nil {
				return nil, err
			}
			out = append(out, rcpts...)
		}
	default:
		return nil, errEncMetadata
	}
	if len(out) == 0 {
		return nil, errEncMetadata
	}
	if err := metadataDuplicate(raw); err != nil {
		return nil, err
	}
	return out, nil
}

// metadataDuplicate reads the tokens of raw, one JSON object, and returns
// errEncDuplicate when it holds the sops member twice or an object in the
// sops value holds a member twice: at the top of the sops value, in a
// key_groups item, in a recipient entry, at any depth. The other top-level
// members, the data payload among them, are not metadata and are skipped.
// It returns errEncMetadata when the tokens do not parse.
func metadataDuplicate(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errEncMetadata
	}
	seen := false
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return errEncMetadata
		}
		if t != "sops" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return errEncMetadata
			}
			continue
		}
		if seen {
			return errEncDuplicate
		}
		seen = true
		if err := valueDuplicate(dec); err != nil {
			return err
		}
	}
	return nil
}

// valueDuplicate reads one JSON value from dec and returns errEncDuplicate
// when an object in it holds a member twice (the names compared after their
// escapes are decoded, as sops reads them), errEncMetadata when the tokens
// do not parse.
func valueDuplicate(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return errEncMetadata
	}
	switch t {
	case json.Delim('{'):
		names := map[string]bool{}
		for dec.More() {
			nt, err := dec.Token()
			if err != nil {
				return errEncMetadata
			}
			name, _ := nt.(string)
			if names[name] {
				return errEncDuplicate
			}
			names[name] = true
			if err := valueDuplicate(dec); err != nil {
				return err
			}
		}
	case json.Delim('['):
		for dec.More() {
			if err := valueDuplicate(dec); err != nil {
				return err
			}
		}
	default:
		return nil // a scalar
	}
	if _, err := dec.Token(); err != nil { // the closing delimiter
		return errEncMetadata
	}
	return nil
}

// metaField is m[name], or errEncMetadata when m has another key that equals
// name regardless of case.
func metaField(m map[string]any, name string) (any, error) {
	for k := range m {
		if k != name && strings.EqualFold(k, name) {
			return nil, errEncMetadata
		}
	}
	return m[name], nil
}

// ageRecipients reads a sops age list: absent (nil), or a list of objects
// that each hold a recipient string that is not empty.
func ageRecipients(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errEncMetadata
	}
	var out []string
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, errEncMetadata
		}
		r, err := metaField(entry, "recipient")
		if err != nil {
			return nil, err
		}
		s, ok := r.(string)
		if !ok || s == "" {
			return nil, errEncMetadata
		}
		out = append(out, s)
	}
	return out, nil
}

// reportOtherStores prints, at the level of say, one line for each store
// that this run does not re-key and that still lists the recipient (bash:
// secrets::_other_stores): the domain stores it did not select, then the
// flat store, which --all does not re-key either. A path or a file it cannot
// read, a file whose metadata does not parse, and a file whose metadata holds
// a member twice count as one that lists it.
func (c *Context) reportOtherStores(say func(io.Writer, string, ...any), selected []string, recipient string) {
	picked := map[string]bool{}
	for _, s := range selected {
		picked[filepath.Clean(s)] = true
	}
	count := func(store string) int {
		s := scanEncs([]string{store}, recipient)
		return len(s.listed) + len(s.unreadable) + len(s.malformed) + len(s.duplicate)
	}
	domains := 0
	paths, _ := c.domainStores()
	for _, store := range paths {
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
// neither matches. No private key is printed or derived. It reads these two
// sources only, and the scope line says so (ownIdentityScope).
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

// ownIdentityScope is the line that every remove-key run that reaches the
// own-identity check prints, with a match or not (bash: the same line in
// secrets::remove-key): the two sources ownIdentity reads, and those it does
// not. A run that prints no match warning has not read the others.
const ownIdentityScope = "the check for your own decrypt identity reads only SOPS_AGE_KEY_FILE (default ~/.config/sops/age/keys.txt) and the .pub of LOK8S_SSH_KEY (default ~/.ssh/id_ed25519); it does not read SOPS_AGE_KEY, SOPS_AGE_KEY_CMD or other files"

// replaceFile writes content to target, the file that sopsConfigDest
// resolved, through a temp file in the same directory and a rename (bash:
// secrets::_replace), so no reader sees half a file. The new file keeps the
// mode of the old one. It does not resolve target again: the caller took the
// lock beside it and read it. The rename replaces a link at target instead
// of writing through it, and never moves the temp file into a directory. A
// temp file that it cannot remove gives a *tempLeftError (removeTemp), and
// the callers print it with reportTempLeft.
func replaceFile(target string, content []byte) (err error) {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			err = removeTemp(err, name)
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
// store as Encrypt does. Each new .enc is the newer file of its pair, as
// after Encrypt, so a later encrypt sweep skips it and the lint check does
// not name it.
func (c *Context) rekeyFiles(stores, encs []string) error {
	for _, store := range stores {
		count := 0
		for _, enc := range encs {
			if filepath.Dir(enc) != filepath.Clean(store) {
				continue
			}
			plain := strings.TrimSuffix(enc, ".enc")
			if err := c.encryptFile(plain); err != nil {
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
	ui.ErrorTo(c.ErrOut, "the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again, with an identity that decrypts every file, resumes it:")
	ui.ErrorTo(c.ErrOut, "  %s", cmd)
}

// removeKeySummary prints the summary of a remove-key run that ended (bash:
// secrets::_rm_summary): the creation rules that changed, the .enc files
// whose labels listed the recipient, and the .enc files that the run
// re-keyed, which are every managed .enc of the selected stores but the
// orphans that skipOrphans accepted.
func (c *Context) removeKeySummary(rules, listed, rekeyed, orphans int) {
	fmt.Fprintf(c.Out, "%d creation rule(s) changed\n", rules)
	fmt.Fprintf(c.Out, "%d .enc file(s) listed the recipient in their labels\n", listed)
	fmt.Fprintf(c.Out, "%d .enc file(s) re-keyed with a new data key\n", rekeyed)
	if orphans > 0 {
		fmt.Fprintf(c.Out, "done: every managed .enc of the selected stores but %d orphan(s) was rewritten. Commit them together with .sops.yaml\n", orphans)
		return
	}
	fmt.Fprintln(c.Out, "done: every managed .enc of the selected stores was rewritten. Commit them together with .sops.yaml")
}

// twinsMatch compares each .enc of encs, decrypted in memory with the
// caller's identity (DecryptEntry), with the bytes of its decrypted twin,
// before the first write (bash: secrets::_twins_match). The re-key encrypts
// the twin, so a twin that holds another value would replace the value of
// the .enc. It names each .enc that differs from its twin and each that it
// cannot decrypt, and each twin that it cannot read; it never prints or
// keeps a value. It reports whether every file matched.
func (c *Context) twinsMatch(encs []string) bool {
	bad := 0
	for _, enc := range encs {
		plain := strings.TrimSuffix(enc, ".enc")
		dec, err := DecryptEntry(enc)
		if err != nil {
			ui.ErrorTo(c.ErrOut, "cannot decrypt %s to compare it with its twin", c.rel(enc))
			bad++
			continue
		}
		twin, err := os.ReadFile(plain) // #nosec G304 -- the twin of an .enc of the store
		switch {
		case err != nil:
			ui.ErrorTo(c.ErrOut, "cannot read %s", c.rel(plain))
			bad++
		case !bytes.Equal(dec, twin):
			ui.ErrorTo(c.ErrOut, "%s differs from its decrypted twin: run lo secrets decrypt for this store, or encrypt the twin first", c.rel(enc))
			bad++
		}
		clear(dec)
		clear(twin)
	}
	if bad > 0 {
		ui.ErrorTo(c.ErrOut, "  the re-key encrypts each twin, so a twin that differs would replace the value of its .enc (lo secrets decrypt keeps a twin that is newer than its .enc: to take the .enc value, remove that twin first). Nothing was written.")
		return false
	}
	return true
}

// refuseScan prints the refusal for what the scan before the first write could
// not read, in this order: a path it cannot read, an .enc that is not a
// managed Secret.*.enc, an .enc whose sops metadata holds a member twice, an
// .enc whose sops metadata does not parse or lists no age recipient. It
// reports whether it refused.
func (c *Context) refuseScan(s encScan) bool {
	switch {
	case len(s.unreadable) > 0:
		for _, u := range s.unreadable {
			ui.ErrorTo(c.ErrOut, "cannot read %s", c.rel(u))
		}
		ui.ErrorTo(c.ErrOut, "  a file that is not read may still list the recipient — fix the permissions first; nothing was written")
	case len(s.other) > 0:
		for _, o := range s.other {
			ui.ErrorTo(c.ErrOut, "remove-key re-keys only the managed Secret.*.enc files; %s is another .enc file: re-key or remove it by hand, then run again", c.rel(o))
		}
		ui.ErrorTo(c.ErrOut, "  an .enc file that is not re-keyed can keep the removed recipient — nothing was written")
	case len(s.duplicate) > 0:
		for _, d := range s.duplicate {
			ui.ErrorTo(c.ErrOut, "the sops metadata of %s holds a duplicate member", c.rel(d))
		}
		ui.ErrorTo(c.ErrOut, "  a duplicate member can hide a recipient from this check — re-encrypt those files with sops from their plaintext, then run this command again; nothing was written")
	case len(s.malformed) > 0:
		for _, m := range s.malformed {
			ui.ErrorTo(c.ErrOut, "the sops metadata of %s does not parse or lists no age recipient", c.rel(m))
		}
		ui.ErrorTo(c.ErrOut, "  remove-key reads the recipients of each .enc from its sops metadata — fix or restore those files first; nothing was written")
	default:
		return false
	}
	return true
}

// postCheck reads the stores again after the re-key, with the reads and the
// walk of the check before the first write (scanEncs, otherEncs), and
// reports whether the files pass (bash: secrets::_post_check). It fails on a
// path that it cannot read (a store, an .enc, a directory below a store), an
// .enc whose sops metadata does not parse or holds a member twice, a managed
// .enc with a decrypted twin that still lists the recipient, a managed .enc
// with a decrypted twin that is not in known (the run did not re-key it),
// another .enc, and, without skipOrphans, an .enc with no decrypted twin.
// Each line names the path. With skipOrphans an orphan that lists the
// recipient is a warning. A file that another writer replaced in place under
// the same name is not seen: the stores must be quiet while remove-key runs.
func (c *Context) postCheck(stores []string, recipient string, known []string, skipOrphans bool) bool {
	after := scanEncs(stores, recipient)
	other, dirs := otherEncs(stores)
	unreadable := slices.Concat(after.unreadable, dirs)
	for _, u := range unreadable {
		ui.ErrorTo(c.ErrOut, "cannot read %s", c.rel(u))
	}
	for _, m := range after.malformed {
		ui.ErrorTo(c.ErrOut, "the sops metadata of %s does not parse or lists no age recipient", c.rel(m))
	}
	for _, d := range after.duplicate {
		ui.ErrorTo(c.ErrOut, "the sops metadata of %s holds a duplicate member", c.rel(d))
	}
	left, changed, orphansLeft := 0, 0, 0
	for _, enc := range after.all {
		twin := fsutil.IsRegular(strings.TrimSuffix(enc, ".enc"))
		switch {
		case slices.Contains(after.malformed, enc) || slices.Contains(after.duplicate, enc):
			// named above
		case !twin && skipOrphans:
			if slices.Contains(after.listed, enc) {
				orphansLeft++
			}
		case !twin:
			ui.ErrorTo(c.ErrOut, "an .enc with no decrypted twin after the re-key: %s", c.rel(enc))
			changed++
		case !slices.Contains(known, enc):
			ui.ErrorTo(c.ErrOut, "a managed .enc that this run did not re-key: %s", c.rel(enc))
			changed++
		case slices.Contains(after.listed, enc):
			ui.ErrorTo(c.ErrOut, "still encrypted to the removed recipient: %s", c.rel(enc))
			left++
		}
	}
	for _, o := range other {
		ui.ErrorTo(c.ErrOut, "remove-key re-keys only the managed Secret.*.enc files; %s is another .enc file: re-key or remove it by hand, then run again", c.rel(o))
		changed++
	}
	bad := len(unreadable) + len(after.malformed) + len(after.duplicate)
	if left > 0 || bad > 0 {
		ui.ErrorTo(c.ErrOut, "%d .enc file(s) still list the removed recipient after the re-key, %d cannot be read or do not parse", left, bad)
	}
	if changed > 0 {
		ui.ErrorTo(c.ErrOut, "%d file(s) of the stores changed during the run: the stores must be quiet while remove-key runs", changed)
	}
	if left+bad+changed > 0 {
		return false
	}
	if orphansLeft > 0 {
		ui.WarnTo(c.ErrOut, "%d orphan .enc file(s) still list the removed recipient — its key can still decrypt them", orphansLeft)
	}
	return true
}

// ── Remove key ───────────────────────────────────────────

// RemoveKey drops an age recipient from every creation rule in .sops.yaml
// and re-keys the selected stores, so that no managed Secret.*.enc of them
// lists it, an orphan that skipOrphans accepts the one exception (bash:
// secrets::remove-key). It is add-key in reverse, with two differences
// that matter for a removal: every refusal runs before the first write, and
// the re-encryption covers every selected store, not only the context's.
//
// It holds the lock of .sops.yaml for the whole run. It reads .sops.yaml as
// YAML too and refuses a YAML tag under creation_rules other than !!str,
// !!seq or !!map and a mapping key that is not a string, and refuses when the
// recipient is in a value the line edit cannot rewrite (an escaped string, a
// YAML list, a key group, an alias or a merge key), when the edit would drop
// the last recipient of a rule, and when an .enc file of the stores matches
// no creation rule, a rule with a recipient that is not age, or a rule with
// an age recipient that does not parse. It refuses a store path or an .enc
// file it cannot stat or read (a dangling symlink included), a
// directory below a store that it cannot list or search, a file at any depth
// of a store whose name ends in .enc and that is not a managed Secret.*.enc
// at its top (the re-key would not rewrite it: otherEncs), an .enc whose
// sops metadata holds a member twice (the read keeps the last one,
// so the first could hide a recipient), an .enc whose sops metadata does not
// parse or lists no age recipient, and, without skipOrphans, an .enc with no
// decrypted twin: a re-key cannot rewrite that file, so it keeps the removed
// recipient. Last, it decrypts each file that it re-keys, in memory, with the
// identity of the caller, and refuses a file whose twin holds other bytes, a
// file that it cannot decrypt and a twin that it cannot read (twinsMatch):
// the re-key encrypts the twin, so a twin that differs would replace the
// value of the .enc. The times of the files do not count. A recipient
// that is in no rule and on no .enc label is a warning, not a refusal: the
// run then re-keys every managed .enc and changes no rule.
//
// It re-encrypts every managed .enc of the selected stores from its
// plaintext, an orphan that skipOrphans accepts the one exception: the labels
// are not trusted to choose the files. Then it reads the stores again with
// the same reads and walk (postCheck) and fails, with the command that
// resumes the run, when a file still lists it, when a path cannot be read,
// and when a file changed during the run: another .enc, a managed .enc with
// a decrypted twin that the run did not re-key, or, without skipOrphans, an
// orphan. Each read takes the recipients from the parsed sops metadata of
// the file. Other stores that still list the recipient are named in a
// warning; they are not re-keyed. The last lines count the rules that
// changed, the files whose labels listed the recipient, and the files that
// the run re-keyed (removeKeySummary).
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
	// The file that the lock, the read and the write use, resolved once
	// (lock.go).
	dest, err := c.sopsConfigDest()
	if err != nil {
		return err
	}
	unlock, err := c.lockSopsConfig(dest)
	if err != nil {
		return err
	}
	defer unlock()

	raw, err := os.ReadFile(dest)
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
	// Refuse, do not interpret: the YAML forms that yaml.v3 and yq read
	// differently.
	switch yamlFormCheck(&doc) {
	case formTag:
		ui.ErrorTo(c.ErrOut, "%s has a YAML tag under creation_rules other than !!str, !!seq or !!map — remove-key does not read tagged values; remove the tag by hand", sopsConfig)
		return ErrHandled
	case formKey:
		ui.ErrorTo(c.ErrOut, "%s has a mapping key that is not a string — remove-key does not read such a mapping; quote the key or remove it by hand", sopsConfig)
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

	stores, unreadableStores, err := c.rekeyStores(all)
	if err != nil {
		return err
	}
	scan := scanEncs(stores, agePubkey)
	other, unreadableDirs := otherEncs(stores)
	scan.unreadable = slices.Concat(unreadableStores, scan.unreadable, unreadableDirs)
	scan.other = other
	if c.refuseScan(scan) {
		return ErrHandled
	}
	// The re-key rewrites every managed .enc, so a key in no rule and on no
	// label is not a refusal: the run that resumes a stopped re-key is
	// such a run.
	if rules == 0 && len(scan.listed) == 0 {
		ui.WarnTo(c.ErrOut, "%s is in no creation rule and in no .enc label: remove-key re-keys every managed .enc anyway, and changes no rule", agePubkey)
	}
	if orphans := c.orphanEncs(stores); len(orphans) > 0 && !skipOrphans {
		ui.ErrorTo(c.ErrOut, "%d encrypted file(s) have no decrypted twin — they would keep the REMOVED", len(orphans))
		ui.ErrorTo(c.ErrOut, "  recipient, so its key could still decrypt them.")
		ui.ErrorTo(c.ErrOut, "  Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.")
		return ErrHandled
	}
	// The re-key encrypts the plaintext twin of every managed .enc of the
	// stores, an orphan that skipOrphans accepts the one exception: the
	// labels are not trusted to choose the files. The compare below
	// (twinsMatch) refuses a twin that holds another value than its .enc;
	// the times of the two files do not count.
	var rekey []string
	for _, enc := range scan.all {
		if fsutil.IsRegular(strings.TrimSuffix(enc, ".enc")) {
			rekey = append(rekey, enc)
		}
	}
	// The scan follows a symlink, but the re-key does not write through one.
	if c.refuseEncPaths(rekey) {
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
	// Each twin must hold the value of its .enc: the re-key encrypts the
	// twin.
	if !c.twinsMatch(rekey) {
		return ErrHandled
	}
	if own := ownIdentity(agePubkey); own != "" {
		ui.WarnTo(c.ErrOut, "the recipient is your own decrypt identity (%s): once it is removed, that identity cannot decrypt the re-keyed files, and a resume of a stopped run needs another identity that decrypts every file", own)
	}
	ui.WarnTo(c.ErrOut, "%s", ownIdentityScope)

	// The first write. A run that stops after it (a failed encrypt) is
	// resumed by running the command again: the recipient is then in no
	// rule, and every managed .enc is re-keyed again.
	if rules > 0 {
		if err := replaceFile(dest, []byte(edited)); err != nil {
			if c.reportTempLeft(err) {
				return ErrHandled
			}
			return err
		}
		fmt.Fprintf(c.Out, "removed recipient from .sops.yaml (%d creation rule(s))\n", rules)
	} else {
		fmt.Fprintln(c.Out, "recipient not in .sops.yaml: .sops.yaml is not changed")
	}
	fmt.Fprintf(c.Out, "  %s\n", agePubkey)

	fmt.Fprintf(c.Out, "re-keying %d cache file(s) across %d store(s)…\n", len(rekey), len(stores))
	if err := c.rekeyFiles(stores, rekey); err != nil {
		c.rerunRemoveKey(agePubkey, all, skipOrphans)
		return err
	}

	// Check the files, not the exit codes: the same reads and walk as before
	// the first write. A managed file with a twin that this run did not
	// re-key fails it.
	if !c.postCheck(stores, agePubkey, rekey, skipOrphans) {
		c.rerunRemoveKey(agePubkey, all, skipOrphans)
		return ErrHandled
	}
	c.reportOtherStores(ui.WarnTo, stores, agePubkey)
	c.removeKeySummary(rules, len(scan.listed), len(rekey), len(scan.all)-len(rekey))
	ui.WarnTo(c.ErrOut, "a removal does not revoke what the key already read (older .enc files in git history included): rotate those secrets")
	return nil
}

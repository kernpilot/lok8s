// Package secrets is the Go port of .lok8s/libs/secrets — secret cache
// management with SOPS/age encryption.
//
// The lok8s secret plugin generates values into the secrets store as
// plaintext cache files (Secret.<name>.<ns>.<key>). This package adds
// SOPS/age encryption so those cache files can be committed safely.
//
// Layout (single-instance project — flat store):
//
//	.secrets/Secret.myapp.default.PASSWORD       ← plaintext (gitignored)
//	.secrets/Secret.myapp.default.PASSWORD.enc   ← SOPS-encrypted (committed)
//	.secrets/tls/                                ← mkcert certs (gitignored)
//
// Per-INSTANCE isolation (multi-domain repos): a domain keeps its OWN secrets
// under clusters/<domain>/secrets/, and the CLI + `lo build` use only that
// store for that domain. This is the secure default for anything serving
// multiple environments — dev/prod never share a generated value, and SOPS
// creation_rules can encrypt each domain's store to DIFFERENT recipients so a
// dev/CI key cannot decrypt prod. There is deliberately NO shared tier: a
// value needed in two instances is a manual copy (the operator consenting to
// that exposure), never a silent fallback. See docs/guide/secrets.md.
//
// Encryption uses age keys derived from SSH keys (the ssh-to-age scheme).
// No new key ceremony — your existing ~/.ssh/id_ed25519 is your encryption
// identity.
package secrets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kernpilot/lok8s/internal/clock"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks an error whose message was already printed in the bash
// implementation's own format ([error] … on stderr). Callers exit non-zero
// without printing anything further.
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// nameRe is the Secret --name allowlist. The name lands in a glob under the
// store dir; a slash or .. would let it address files outside the store. Same
// charset the domain resolver enforces.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Context carries the resolved environment a secrets operation runs in.
type Context struct {
	Paths  *config.Paths
	Domain string // resolved domain ("" = flat store only)
	Out    io.Writer
	ErrOut io.Writer

	// Stdin and StdinIsTTY feed `set`'s value fallback (piped stdin vs the
	// interactive prompt). ReadPassword is the silent tty read; injectable
	// for tests.
	Stdin        io.Reader
	StdinIsTTY   func() bool
	ReadPassword func() (string, error)

	// Kubeconfig is the KUBECONFIG value the bash entrypoint exported
	// (PATH_BASE/.kubeconfig/<cluster>.yaml) — the live-drift check's kubectl
	// runs against exactly that file, never the caller's ambient KUBECONFIG.
	Kubeconfig string

	// Runner runs the two external tools this package still shells out to
	// (kubectl for the live-drift check, the clipboard tool for print
	// --copy). nil = execx.NewRunner(Paths); tests install a fake.
	Runner execx.Runner
}

func (c *Context) runner() execx.Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return execx.NewRunner(c.Paths)
}

// flatStore resolves the deprecated flat store the way bash does:
// ${PATH_SECRETS:-${PATH_BASE}/.secrets}.
func (c *Context) flatStore() string {
	return config.FlatSecretsStore(c.Paths)
}

// StorePath resolves the secrets store for the current context (bash:
// secrets::path). A domain with its own clusters/<domain>/secrets/ uses THAT
// store exclusively (per-instance isolation, no flat fallback for it);
// otherwise the flat PATH_SECRETS store (single-instance projects).
// `lo build` mirrors this — see build::_export_secrets_path.
//
// String concatenation on purpose (not filepath.Join): bash interpolates the
// domain into the path verbatim, and `lo secrets path` must print the same
// bytes.
func (c *Context) StorePath() string {
	if c.Domain != "" {
		d := c.Paths.Clusters + "/" + c.Domain + "/secrets"
		if fsutil.DirExists(d) {
			return d
		}
	}
	return c.flatStore()
}

// ResolveStore is StorePath for a command (bash: `secrets_dir=$(secrets::path)`,
// which stops the command under set -e when secrets::path fails). A domain
// whose directory exists without a store, in a project that keeps a store
// per domain, does not fall back to the flat store: ResolveStore prints the
// refusal (domain.CheckSecretsStore) and returns ErrHandled.
func (c *Context) ResolveStore() (string, error) {
	return c.resolveStore(c.ErrOut)
}

// resolveStore is ResolveStore, with the refusal printed to w.
func (c *Context) resolveStore(w io.Writer) (string, error) {
	if c.Domain == "" || !fsutil.DirExists(c.Paths.Clusters+"/"+c.Domain+"/secrets") {
		if err := domain.CheckSecretsStore(c.Paths.Clusters, c.Domain, c.flatStore(), w); err != nil {
			return "", ErrHandled
		}
	}
	return c.StorePath(), nil
}

// ensureStore scaffolds a domain's per-domain store so a WRITE lands there
// (not the flat fallback) even on the very first command — StorePath only
// returns the per-domain path once the dir exists, so create it first when a
// domain is in context (bash: secrets::_ensure_store). A no-op when no domain
// is in context or the domain dir doesn't exist.
func (c *Context) ensureStore() error {
	if c.Domain != "" && fsutil.DirExists(c.Paths.Clusters+"/"+c.Domain) {
		return os.MkdirAll(c.Paths.Clusters+"/"+c.Domain+"/secrets", 0o755)
	}
	return nil
}

// sopsConfigPath is ${PATH_BASE}/.sops.yaml — its presence means "encryption
// is configured".
func (c *Context) sopsConfigPath() string {
	return c.Paths.Base + "/.sops.yaml"
}

// gitignoreOK reports whether the .gitignore of dir protects the store or
// lo can make it do so, and prints the refusal when it cannot (bash:
// secrets::_gitignore_ok). step names the step of the command that the
// check stops before. Init and AddKey check the store that they write
// before their first write, so the refusal comes before any change.
func (c *Context) gitignoreOK(dir, step string) bool {
	_, _, ok := c.gitignorePlan(dir, step)
	return ok
}

// gitignorePlan reads the .gitignore of dir once and returns the bytes that
// it must have (planGitignore), and whether they differ from the file
// (write). It prints the refusal and returns ok false in these cases:
//   - The .gitignore is not a regular file and not absent (encDestinationOK):
//     a symlink, a directory, a FIFO, a socket or a device, or its lookup
//     fails. The write would go through a link to the file that it names,
//     and git does not read a .gitignore that is a symlink.
//   - The file cannot be read: a rewrite would drop the lines of the user.
//   - The file cannot protect the store (planGitignore).
//
// It reads at most gitignoreMaxBytes and one byte more, and the plan
// refuses a file larger than that. Each refusal names step, the step of the
// command that the check stops before, and the file with printable bytes
// only.
func (c *Context) gitignorePlan(dir, step string) (want []byte, write, ok bool) {
	gitignore := dir + "/.gitignore"
	rel := printable(c.rel(gitignore))
	if !encDestinationOK(gitignore) {
		ui.ErrorTo(c.ErrOut, "%s is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: lo stopped before %s — remove it, then run this command again", rel, step)
		return nil, false, false
	}
	f, err := os.Open(gitignore)
	if errors.Is(err, fs.ErrNotExist) {
		return []byte(gitignoreContent), true, true
	}
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(io.LimitReader(f, gitignoreMaxBytes+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	var problems []string
	if err != nil {
		problems = []string{"cannot read " + rel}
	} else {
		want, problems = planGitignore(raw, rel)
	}
	if problems != nil {
		for _, p := range problems {
			ui.ErrorTo(c.ErrOut, "%s", p)
		}
		ui.ErrorTo(c.ErrOut, "%s", gitignoreFinal(rel, step))
		return nil, false, false
	}
	return want, !bytes.Equal(raw, want), true
}

// initStorePath is the store whose .gitignore Init writes, read before its
// first write (bash: secrets::_store_gitignore_ok init): the store of the
// domain when the domain directory exists (ensureStore makes the store),
// else StorePath.
func (c *Context) initStorePath() string {
	if c.Domain != "" && fsutil.DirExists(c.Paths.Clusters+"/"+c.Domain) {
		return c.Paths.Clusters + "/" + c.Domain + "/secrets"
	}
	return c.StorePath()
}

// ensureGitignore makes sure the secrets dir has a .gitignore that blocks
// plaintext but allows .enc files (bash: secrets::_ensure_gitignore). It
// writes the file only when its bytes differ from the plan (gitignorePlan):
// no file, no block, the legacy lines, or a changed block. A refused file
// is not changed, and ensureGitignore returns ErrHandled. The write goes to
// a temp file in the store and a rename over the old name (writeReplacing,
// mode 0644): it replaces a link or a hard link instead of writing through
// it. A write that fails leaves the old file as it was, prints one line
// that names the file (reportFailedWrite: no error text of the file
// system), and returns ErrHandled. step names the step of the command that
// a refusal stops before (gitignorePlan).
//
// The file gets mode 0644 on each rewrite: the .gitignore is committed, and
// git reads the same rules for each user of the repository. It holds no
// secret.
func (c *Context) ensureGitignore(dir, step string) error {
	gitignore := dir + "/.gitignore"
	want, write, ok := c.gitignorePlan(dir, step)
	if !ok {
		return ErrHandled
	}
	if !write {
		return nil
	}
	if err := writeReplacing(gitignore, 0o644, func(w io.Writer) error { return writeGitignore(w, want) }); err != nil {
		c.reportFailedWrite(err, "cannot write "+printable(c.rel(gitignore))+": it was not changed")
		return ErrHandled
	}
	ui.Debug("Wrote %s", printable(gitignore))
	return nil
}

// reportFailedWrite prints the lines of a write through writeReplacing that
// failed, on a file that lo checks before it writes it (the .gitignore of a
// store, a plaintext file of set and decrypt): the temp file that stays, if
// its remove failed too, then final (bash: the same lines in
// secrets::_ensure_gitignore and secrets::_plain_write_failed). It prints
// no error text of the file system, because that text has the raw path. A
// path prints with printable bytes only.
func (c *Context) reportFailedWrite(err error, final string) {
	if left, ok := errors.AsType[*tempLeftError](err); ok {
		ui.ErrorTo(c.ErrOut, "cannot remove the temp file %s: remove it by hand", printable(c.rel(left.name)))
	}
	ui.ErrorTo(c.ErrOut, "%s", final)
}

// writeGitignore fills the temp file of the .gitignore write of
// ensureGitignore with want (a seam: a test makes it stop part way).
var writeGitignore = func(w io.Writer, want []byte) error {
	_, err := w.Write(want)
	return err
}

// storeEntries lists the store's file names in bash glob order (byte-sorted),
// filtered to those with the given prefix. Only names are filtered here;
// callers apply their own regular-file / .enc checks like the bash loops do.
// It drops the error of the directory read. AddKey does not use it: each of
// its listings checks the error (storeNames).
func storeEntries(dir, prefix string) []string {
	names, _ := storeNames(dir, prefix)
	return names
}

// readDir reads a directory for storeNames (a seam: a test makes a listing
// of the store fail).
var readDir = os.ReadDir

// storeNames is storeEntries with the error of the directory read, for a
// caller that must fail closed when it cannot list the store.
func storeNames(dir, prefix string) ([]string, error) {
	entries, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// newerThan mirrors bash [[ a -nt b ]]: a exists and its mtime is strictly
// newer than b's.
func newerThan(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return true
	}
	return ai.ModTime().After(bi.ModTime())
}

// nudgeNewer makes dst, the file just written from src, strictly newer than
// src when the two have the same mtime. The bash implementation gets this
// ordering for free — spawning the sops binary always lands the output file
// a clock tick after its input — and the -nt freshness skips depend on it:
// an in-process encrypt/decrypt can finish within the SAME tick, and an
// equal mtime reads as "stale", causing pointless .enc/plaintext churn on
// every subsequent sweep.
//
// It sets no time from time.Now(). The kernel stamps a write with its own
// clock, which can be behind time.Now() by up to one tick: a time from
// time.Now() can be newer than a file that the kernel writes after it. So
// on an equal time it touches dst with the clock of the kernel (touchNow)
// until dst is newer than src, one try per millisecond. A later edit of src,
// stamped by the same clock, is never older than dst. When dst is not newer
// after stampTries tries (a file system that keeps whole seconds), the
// times stay equal.
//
// It returns false only when src has a time after now (a time in the
// future): dst keeps the time of its write. A dst dated after src would make
// a later edit of src (dated now) look older than dst, so encrypt would skip
// the edit and decrypt could overwrite it. A src that is newer than dst but
// not after now (a program set its time, or src changed after the write of
// dst) keeps its time too, with no warning: the next encrypt writes dst
// again.
func nudgeNewer(dst, src string) bool {
	si, err := os.Stat(src)
	if err != nil {
		return true
	}
	di, err := os.Stat(dst)
	if err != nil || di.ModTime().After(si.ModTime()) {
		return true
	}
	if si.ModTime().After(time.Now()) {
		return false
	}
	if si.ModTime().After(di.ModTime()) {
		return true
	}
	for try := range stampTries {
		if try > 0 {
			_ = stampSleep(context.Background(), time.Millisecond)
		}
		if stampTouch(dst) != nil {
			return true
		}
		if di, err = os.Stat(dst); err != nil || di.ModTime().After(si.ModTime()) {
			return true
		}
	}
	return true
}

// stampTries bounds the tries of nudgeNewer: 50 ms, more than one tick of
// the clock of the kernel (at most 10 ms).
const stampTries = 50

// stampTouch and stampSleep are the touch and the wait of each try of
// nudgeNewer (seams for the tests).
var (
	stampTouch                 = touchNow
	stampSleep clock.SleepFunc = clock.Sleep
)

// touchNow sets the access and modification times of path to the current
// time of the kernel (utimensat with no times, as touch(1) does; bash:
// touch). The kernel stamps a write with the same clock, so a file written
// after the touch is never older than path. os.Chtimes with time.Now() can
// set a time that is newer than such a file.
func touchNow(path string) error {
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, nil, 0)
}

// CheckUnencrypted reports plaintext secrets without a fresh .enc twin in dir
// (used by lint; bash: secrets::check_unencrypted). Returns false if any
// plaintext secret lacks a corresponding up-to-date .enc file, true when all
// secrets are encrypted or no secrets exist.
func CheckUnencrypted(dir string, warnTo io.Writer) bool {
	if !fsutil.DirExists(dir) {
		return true
	}
	ok := true
	for _, name := range storeEntries(dir, "Secret.") {
		plaintext := dir + "/" + name
		if !fsutil.IsRegular(plaintext) || strings.HasSuffix(name, ".enc") {
			continue
		}
		if !fsutil.IsRegular(plaintext + ".enc") {
			ui.WarnTo(warnTo, "Unencrypted secret: %s — run: lo secrets encrypt", name)
			ok = false
		} else if newerThan(plaintext, plaintext+".enc") {
			ui.WarnTo(warnTo, "Secret changed since last encrypt: %s — run: lo secrets encrypt", name)
			ok = false
		}
	}
	return ok
}

// CheckFlatShadows reports deprecated flat-store shadows of a domain's
// per-domain secrets (bash: secrets::check_flat_shadows).
//
// A domain with its OWN store (clusters/<domain>/secrets/) must not ALSO keep
// the same Secret.<name>.<ns>.<key> in the flat store: the two diverge
// silently and different tools read different stores (lo build/Tilt →
// per-domain; a manual PATH_SECRETS=.../.secrets kustomize → flat), which can
// re-key a live cluster from the WRONG store (this orphaned a dev IdP's etcd
// encryption once). Per-domain is authoritative; nothing is legitimately
// flat-only since v0.4.0 (the registry TLS cert lives in a docker volume,
// internal/driver/lo/registrytls.go).
//
// Emits one line per shadow to out (lint wraps each as a warning); returns
// false if any shadow exists, true otherwise. An identical copy is a stale
// duplicate; a DIFFERING copy is active drift — the dangerous case.
func CheckFlatShadows(flat, domainDir string, out io.Writer) bool {
	store := domainDir + "/secrets"
	dname := filepath.Base(domainDir)

	// Only meaningful when the domain has its OWN store, distinct from the
	// flat one.
	if flat == "" || !fsutil.DirExists(store) || !fsutil.DirExists(flat) {
		return true
	}
	if samePath(store, flat) {
		return true
	}

	ok := true
	for _, base := range storeEntries(store, "Secret.") {
		plaintext := store + "/" + base
		if !fsutil.IsRegular(plaintext) || strings.HasSuffix(base, ".enc") || strings.HasSuffix(base, ".sha") {
			continue
		}
		flatCopy := flat + "/" + base
		if !fsutil.IsRegular(flatCopy) {
			continue
		}
		if sameContent(flatCopy, plaintext) {
			io.WriteString(out, "Flat-store shadow: "+base+" is duplicated in the flat .secrets/ store — flat is deprecated for domain secrets; remove the flat copy\n")
		} else {
			io.WriteString(out, "Flat-store DRIFT: "+base+" differs between the flat .secrets/ store and "+dname+"/secrets/ — the stores diverged (a build from the wrong store can re-key a live cluster); reconcile to the per-domain value, then remove the flat copy\n")
		}
		ok = false
	}
	return ok
}

// samePath mirrors bash [[ a -ef b ]] (same device+inode).
func samePath(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// sameContent mirrors `cmp -s`.
func sameContent(a, b string) bool {
	ab, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}

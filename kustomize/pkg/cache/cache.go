// Package cache implements the $PATH_SECRETS-backed deterministic cache
// used by Secret-generating plugins.
//
// The cache is the **source of truth** for stable output: every cached
// generator (passwd, secretRef, htpasswd) checks here first and only
// generates a new value on a miss. Cached values are stored at
// $PATH_SECRETS/Secret.<name>.<namespace>.<key> with mode 0600.
//
// The directory naming scheme is identical to the legacy bash plugin
// (see plugins/secret/README.md) so existing cache directories continue
// to work after migration.
//
// A miss is a miss only when the entry has no encrypted twin either
// (<entry>.enc, written by `lo secrets encrypt`). When the twin exists, the
// store holds a value that is not decrypted yet: Get, ReadByName and
// GetOrCreate return an *EncryptedOnlyError and never generate a new value
// over it.
//
// Put writes an entry through storeguard, the one plaintext write of a
// store, which `lo secrets set` and `lo secrets decrypt` use too: the same
// name checks with the same refusal texts, a .gitignore that keeps every
// plaintext out of git before the first write, and no write through a
// link. A path in a refusal is relative to the project of a per-domain
// store, as set prints it (storeRel). On a miss, GetOrCreate runs these
// checks before it generates a value (checkWrite), and the dry-run overlay
// runs them in place of the write. An env: entry and an update: true entry
// compute their value before Put, so for them only the write is refused
// (a bash: command with update: true has run). A read writes nothing, so
// Get and ReadByName keep
// reading an entry that Put would refuse to write (a name that ends in
// .enc, from a store made before the check).
package cache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/storeguard"
)

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// EncSuffix is the suffix of an entry's SOPS-encrypted twin: `lo secrets
// encrypt` writes Secret.<name>.<namespace>.<key>.enc next to the plaintext.
const EncSuffix = ".enc"

// EncryptedOnlyError reports a store entry that has its encrypted twin but
// no plaintext. The store holds a value, and the plugin cannot read it until
// `lo secrets decrypt` writes the plaintext. The plugin does not generate a
// new value over it: in a fresh worktree, where only the encrypted twins
// exist, that would replace every committed value of the store. It is not
// os.ErrNotExist, so
// GetOrCreate returns it and does not generate.
type EncryptedOnlyError struct {
	Dir  string // the store directory
	Name string // the entry file name, Secret.<name>.<namespace>.<key>
}

func (e *EncryptedOnlyError) Error() string {
	return fmt.Sprintf("cannot read %s: the store %s has %s%s but no plaintext. The plugin does not generate a new value over an encrypted value. Run %q, then build again",
		e.Name, e.Dir, e.Name, EncSuffix, DecryptCommand(e.Dir))
}

// DecryptCommand is the lo command that writes the plaintexts of the store
// dir. A per-domain store (<project>/clusters/<domain>/secrets) names its
// domain; any other store (the flat .secrets/, a copy elsewhere) gets the
// command without --domain.
func DecryptCommand(dir string) string {
	domainDir := filepath.Dir(dir)
	d := filepath.Base(domainDir)
	if filepath.Base(dir) == "secrets" && filepath.Base(filepath.Dir(domainDir)) == "clusters" &&
		d != "." && d != string(filepath.Separator) && d != "" {
		return "lo secrets decrypt --domain " + d
	}
	return "lo secrets decrypt"
}

// EncryptedOnly returns an *EncryptedOnlyError when the entry name has no
// plaintext in dir and its encrypted twin exists, else nil. The plaintext
// check follows a symlink: a dangling symlink is no plaintext, so a value is
// not generated next to the .enc through it.
func EncryptedOnly(dir, name string) error {
	full := filepath.Join(dir, name)
	if _, err := os.Stat(full); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := os.Stat(full + EncSuffix); err != nil {
		return nil
	}
	return &EncryptedOnlyError{Dir: dir, Name: name}
}

// Cache stores secret values for a single (name, namespace) tuple.
// Concurrent reads are safe; concurrent writes from different processes
// are made safe via atomic rename. Within a single process, callers are
// expected to operate per-secret without concurrency.
type Cache struct {
	dir       string
	namespace string
	name      string
	// fillGitignore is the storeguard.Guard.FillGitignore of the store:
	// nil in production (a seam: a test makes the .gitignore write fail).
	fillGitignore func(w io.Writer, want []byte) error
}

// New creates a Cache scoped to the given Secret name + namespace,
// rooted at dir (typically $PATH_SECRETS). The directory is created
// with 0700 if it doesn't exist, and the parent of each directory that it
// makes is synced (storeguard.Guard.MakeDir), before any entry is written.
//
// Returns an error if dir is empty (caller should check $PATH_SECRETS
// before calling), if mkdir fails, or if a sync fails (a
// *storeguard.Stop).
func New(dir, namespace, name string) (*Cache, error) {
	if dir == "" {
		return nil, errs.New("cache: PATH_SECRETS is not set")
	}
	if name == "" {
		return nil, errs.New("cache: secret name is empty")
	}
	if err := (storeguard.Guard{Rel: storeRel(dir)}).MakeDir(dir, dirPerm); err != nil {
		if _, ok := errors.AsType[*storeguard.Stop](err); ok {
			return nil, err
		}
		return nil, fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}
	return &Cache{dir: dir, namespace: namespace, name: name}, nil
}

// Dir returns the cache root directory.
func (c *Cache) Dir() string { return c.dir }

// filename returns the absolute path of the cache entry for key.
func (c *Cache) filename(key string) string {
	return filepath.Join(c.dir, FormatName(c.name, c.namespace, key))
}

// Has reports whether the store holds an entry for key: the plaintext, or
// only its encrypted twin (Get then returns an *EncryptedOnlyError).
func (c *Cache) Has(key string) bool {
	if _, err := os.Stat(c.filename(key)); err == nil {
		return true
	}
	_, err := os.Stat(c.filename(key) + EncSuffix)
	return err == nil
}

// Get returns the cached value for key. It returns an *EncryptedOnlyError
// when only the encrypted twin exists, and os.ErrNotExist when neither
// exists.
func (c *Cache) Get(key string) ([]byte, error) {
	data, err := os.ReadFile(c.filename(key))
	if err != nil {
		if eerr := EncryptedOnly(c.dir, FormatName(c.name, c.namespace, key)); eerr != nil {
			return nil, eerr
		}
		return nil, err
	}
	return data, nil
}

// Put writes value to the entry of key, mode 0600, through the one
// plaintext write of a store (storeguard.Guard.WritePlaintext). Before the
// write it refuses, with the texts of `lo secrets set`:
//
//   - a name, a namespace or a key that makes the file name a path, or a
//     file that is not an entry of the store directory itself;
//   - a file name that ends in .enc in any letter case: the rule
//     !Secret.*.enc of the .gitignore would let git commit the plaintext;
//   - a file that is not a regular file and not absent (a symlink, a
//     directory, a FIFO, a socket or a device): it never writes through a
//     link;
//   - a .gitignore of the store that cannot keep the plaintext out of git.
//     A store with no .gitignore, or with one without the lok8s block, gets
//     the block first.
//
// The value goes to a new private temp file in the store and a rename onto
// the entry, then a sync of the store directory. A refusal returns a
// *storeguard.Stop and changes no file. A write that fails returns one too
// and leaves the old entry as it was; the .gitignore that the write put in
// place before stays, and a temp file stays only when its remove failed
// too (the error names it). A sync that fails after a rename says that
// the file was written. A path in it is relative as storeRel makes it, and
// the line of a failed write names no command (the line of set starts
// with "set").
func (c *Cache) Put(key string, value []byte) error {
	base, err := c.entryName(key)
	if err != nil {
		return err
	}
	return c.guard().WritePlaintext(storeguard.Plaintext{Dir: c.dir, Base: base}, value)
}

// checkWrite runs the checks of Put for key and writes nothing: the name
// (entryName), then the path of the entry and the .gitignore of the store
// (storeguard.Guard.CheckPlaintext). It returns the refusal of Put; it
// cannot tell that the write will fail (a full disk). On a miss,
// GetOrCreate runs it before it generates a value, so a refused write runs
// no generator (a bash: command has side effects). The dry-run overlay runs
// it in place of the write. Put does not run it: a caller of Put has its
// value already (env:, update: true).
func (c *Cache) checkWrite(key string) error {
	base, err := c.entryName(key)
	if err != nil {
		return err
	}
	return c.guard().CheckPlaintext(storeguard.Plaintext{Dir: c.dir, Base: base})
}

// entryName is the file name of the entry of key, checked as `lo secrets
// set` checks a key (storeguard.CheckName).
func (c *Cache) entryName(key string) (string, error) {
	return storeguard.CheckName(c.dir, c.name, defaultNamespace(c.namespace), key)
}

// guard is the storeguard.Guard of the store: a path in a message is
// relative as storeRel makes it.
func (c *Cache) guard() storeguard.Guard {
	return storeguard.Guard{Rel: storeRel(c.dir), FillGitignore: c.fillGitignore}
}

// storeRel is the storeguard.Guard.Rel of the store dir, so that a refusal
// names a file as `lo secrets set` names it, relative to the project. For a
// per-domain store, <project>/clusters/<domain>/secrets, the project is the
// parent of clusters. For any other store, it is the parent of the store
// (the flat <project>/.secrets gives .secrets/<file>). A path that does
// not start with that directory and a separator prints as it is: a path of
// a relative store dir, or of a store whose parent is the root.
func storeRel(dir string) func(string) string {
	clean := filepath.Clean(dir)
	root := filepath.Dir(clean)
	if filepath.Base(clean) == "secrets" && filepath.Base(filepath.Dir(root)) == "clusters" {
		root = filepath.Dir(filepath.Dir(root))
	}
	prefix := root + string(filepath.Separator)
	return func(path string) string { return strings.TrimPrefix(path, prefix) }
}

// GetOrCreate returns the cached value if present, otherwise calls gen,
// stores the result, and returns it. It calls gen only when neither the
// plaintext nor its encrypted twin exists, and only when the checks of Put
// pass (checkWrite: the name, the path and the .gitignore): a refused
// write generates nothing. This is the canonical pattern for cache-first
// generators:
//
//	cached, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) {
//	    return random.Password(32, charset.Alphanum)
//	})
func (c *Cache) GetOrCreate(key string, gen func() ([]byte, error)) ([]byte, error) {
	if data, err := c.Get(key); err == nil {
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := c.checkWrite(key); err != nil {
		return nil, err
	}
	data, err := gen()
	if err != nil {
		return nil, err
	}
	if err := c.Put(key, data); err != nil {
		return nil, err
	}
	return data, nil
}

// ReadByName reads a cache entry by its full filename within the cache
// directory. Used by secretRef to read entries that were written by
// other Cache instances (different secret name).
//
// The path is validated to prevent traversal: it must be a plain
// filename with no path separators or `..` components, and the result
// must remain inside c.dir.
func (c *Cache) ReadByName(filename string) ([]byte, error) {
	full, err := c.checkName(filename)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if eerr := EncryptedOnly(c.dir, filename); eerr != nil {
			return nil, eerr
		}
		return nil, err
	}
	return data, nil
}

// checkName validates a ReadByName filename and returns its full path.
func (c *Cache) checkName(filename string) (string, error) {
	if filename == "" {
		return "", errs.New("cache: empty filename")
	}
	clean := filepath.Clean(filename)
	if clean != filename {
		return "", errs.Newf("cache: filename must be plain (got %q, cleaned to %q)", filename, clean)
	}
	if strings.ContainsAny(filename, "/\\") {
		return "", errs.Newf("cache: filename must not contain path separators: %q", filename)
	}
	if filename == ".." || filename == "." || strings.HasPrefix(filename, "..") {
		return "", errs.Newf("cache: filename must not start with .: %q", filename)
	}
	full := filepath.Join(c.dir, filename)
	// Verify the resolved path stays within c.dir.
	rel, err := filepath.Rel(c.dir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errs.Newf("cache: filename escapes cache dir: %q", filename)
	}
	return full, nil
}

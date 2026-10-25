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
package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
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
// new value over it: in a fresh worktree that silently replaced every
// committed value of the store (B425). It is not os.ErrNotExist, so
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

// FirstEncryptedOnly returns the *EncryptedOnlyError of the first entry (in
// byte order) in dir whose name starts with prefix, whose .enc exists and
// whose plaintext does not, else nil. Entries named in skip are not checked
// (the plugin passes the `update: true` keys: it writes them whatever the
// store holds). The plugin checks a Secret's whole prefix before any
// generator runs, so a refusal never leaves the store half-written (an
// earlier key generated, a later key refused).
func FirstEncryptedOnly(dir, prefix string, skip map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), EncSuffix) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if skip[strings.TrimSuffix(n, EncSuffix)] {
			continue
		}
		if eerr := EncryptedOnly(dir, strings.TrimSuffix(n, EncSuffix)); eerr != nil {
			return eerr
		}
	}
	return nil
}

// Cache stores secret values for a single (name, namespace) tuple.
// Concurrent reads are safe; concurrent writes from different processes
// are made safe via atomic rename. Within a single process, callers are
// expected to operate per-secret without concurrency.
type Cache struct {
	dir       string
	namespace string
	name      string
}

// New creates a Cache scoped to the given Secret name + namespace,
// rooted at dir (typically $PATH_SECRETS). The directory is created
// with 0700 if it doesn't exist.
//
// Returns an error if dir is empty (caller should check $PATH_SECRETS
// before calling) or if mkdir fails.
func New(dir, namespace, name string) (*Cache, error) {
	if dir == "" {
		return nil, errs.New("cache: PATH_SECRETS is not set")
	}
	if name == "" {
		return nil, errs.New("cache: secret name is empty")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
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

// Put writes value to the cache atomically with mode 0600.
func (c *Cache) Put(key string, value []byte) error {
	return writeFileAtomic(c.filename(key), value, filePerm)
}

// GetOrCreate returns the cached value if present, otherwise calls gen,
// stores the result, and returns it. It calls gen only when neither the
// plaintext nor its encrypted twin exists. This is the canonical pattern for
// cache-first generators:
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

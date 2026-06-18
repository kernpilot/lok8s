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
package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

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

// Has reports whether a cache entry exists for key.
func (c *Cache) Has(key string) bool {
	_, err := os.Stat(c.filename(key))
	return err == nil
}

// Get returns the cached value for key, or os.ErrNotExist if absent.
func (c *Cache) Get(key string) ([]byte, error) {
	data, err := os.ReadFile(c.filename(key))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Put writes value to the cache atomically with mode 0600.
func (c *Cache) Put(key string, value []byte) error {
	return writeFileAtomic(c.filename(key), value, filePerm)
}

// GetOrCreate returns the cached value if present, otherwise calls gen,
// stores the result, and returns it. This is the canonical pattern for
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
	if filename == "" {
		return nil, errs.New("cache: empty filename")
	}
	clean := filepath.Clean(filename)
	if clean != filename {
		return nil, errs.Newf("cache: filename must be plain (got %q, cleaned to %q)", filename, clean)
	}
	if strings.ContainsAny(filename, "/\\") {
		return nil, errs.Newf("cache: filename must not contain path separators: %q", filename)
	}
	if filename == ".." || filename == "." || strings.HasPrefix(filename, "..") {
		return nil, errs.Newf("cache: filename must not start with .: %q", filename)
	}
	full := filepath.Join(c.dir, filename)
	// Verify the resolved path stays within c.dir.
	rel, err := filepath.Rel(c.dir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, errs.Newf("cache: filename escapes cache dir: %q", filename)
	}
	return os.ReadFile(full)
}

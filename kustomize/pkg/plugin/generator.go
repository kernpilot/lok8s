// Package plugin defines the runtime contract shared by all kustomize
// exec generator plugins in this repo. A plugin's job is:
//
//  1. Decode a CRD spec from stdin
//  2. Run a set of named Generators against shared Context
//  3. Marshal the merged result as a Kubernetes resource and write to stdout
//
// The Plugin interface (plugin.go) is implemented per-plugin (Secret,
// future ConfigMap, etc.). The Registry (registry.go) handles ordered
// generator execution and key-collision detection. The Runner
// (runner.go) is the entrypoint that wires it all together so each
// cmd/<name>/main.go can be ~10 lines.
package plugin

import (
	"io"
	"os"
)

// Context is passed to every Generator. It bundles the metadata,
// caching, and I/O surface a generator needs without coupling each
// generator to a specific plugin's CRD type.
type Context struct {
	// Name is the metadata.name of the resource being generated.
	Name string
	// Namespace is the metadata.namespace, normalized to "default" if empty.
	Namespace string
	// Cache is the $PATH_SECRETS-backed cache scoped to (Name, Namespace).
	// May be nil if PATH_SECRETS is unset and the plugin doesn't need
	// cache-first determinism — generators that require cache must
	// check for nil and return a clear error.
	Cache CacheStore
	// Env looks up environment variables. Tests inject their own.
	Env func(string) (string, bool)
	// FileRoot is the directory the plugin was invoked from. Used by
	// the file generator for relative path resolution.
	FileRoot string
	// Rand is the source of randomness. Defaults to crypto/rand.Reader.
	// Tests inject deterministic readers.
	Rand io.Reader
}

// CacheStore is the minimal cache interface a Context exposes to
// generators. It's a subset of *cache.Cache so the plugin package
// doesn't import cache (avoids a circular dependency once cache or
// future packages depend on plugin types).
type CacheStore interface {
	Get(key string) ([]byte, error)
	Put(key string, value []byte) error
	Has(key string) bool
	GetOrCreate(key string, gen func() ([]byte, error)) ([]byte, error)
	ReadByName(filename string) ([]byte, error)
	Dir() string
}

// Entry is one (key, value) contribution from a Generator. The Builder
// in pkg/kresource is responsible for base64-encoding the value at
// emit time, so generators store **raw bytes** here, not encoded.
type Entry struct {
	Key   string
	Value []byte
}

// Generator is the per-section contract: a single field in the CRD
// spec (literals, passwd, env, ...) is wrapped in a Generator that
// knows how to produce its (key, value) entries given a Context.
type Generator interface {
	// Name returns the field name this generator owns. Used by the
	// Registry for ordering and collision-error messages.
	Name() string
	// Generate produces zero or more Entry values for the given Context.
	// Implementations must be deterministic given the same Context +
	// cache state.
	Generate(ctx *Context) ([]Entry, error)
}

// DefaultEnv is the production env lookup. Tests usually replace this.
func DefaultEnv(key string) (string, bool) {
	return os.LookupEnv(key)
}

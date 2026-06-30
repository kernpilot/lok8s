package generator

import (
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Env is the generator for the `env:` field. It reads values from the
// environment with cache-first stability:
//
//   - First run: read $VAR, store in cache, emit
//   - Subsequent runs: read cache, emit (env not consulted)
//   - update: true: bypass cache, always read $VAR
//
// A missing env var is an error UNLESS the cache has a previous value or the
// entry sets a fallback: optional (omit the key), default (a literal value),
// or passwd (generate + cache a random value).
type Env struct {
	spec map[string]specpkg.EnvEntry
}

// NewEnv wraps an env map.
func NewEnv(spec map[string]specpkg.EnvEntry) *Env { return &Env{spec: spec} }

// Name returns the generator's spec field name.
func (g *Env) Name() string { return "env" }

// Generate returns one Entry per env entry; an `optional` entry whose env var
// is unset (and uncached) is skipped entirely.
func (g *Env) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		val, omit, err := g.lookup(ctx, k, g.spec[k])
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		if omit {
			continue
		}
		out = append(out, plugin.Entry{Key: k, Value: val})
	}
	return out, nil
}

// lookup resolves one env entry. Returns (value, omit, error): omit is true
// only for an `optional` entry whose env var is unset (and uncached), telling
// Generate to drop the key. Policy: cache-first (unless update) → read $var →
// on miss, the entry's fallback (default / passwd / optional / error).
func (g *Env) lookup(ctx *plugin.Context, key string, entry specpkg.EnvEntry) ([]byte, bool, error) {
	varName := entry.EffectiveVar(key)

	// Cache-first, unless update bypasses it (always re-read env then).
	if !entry.Update && ctx.Cache != nil && ctx.Cache.Has(key) {
		v, err := ctx.Cache.Get(key)
		return v, false, err
	}

	// On a cache MISS (or update mode): a set env var wins over the fallback and
	// is cached (so secretRef sees it). NB the cache-first branch above means a
	// cached value — INCLUDING a previously-cached default/passwd fallback —
	// shadows a newly-set env var until update:true or the cache file is removed.
	if v, ok := ctx.Env(varName); ok {
		if err := g.put(ctx, key, []byte(v)); err != nil {
			return nil, false, err
		}
		return []byte(v), false, nil
	}

	// Env var missing → fallback (mutually exclusive, validated in the spec).
	switch {
	case entry.Default != nil:
		v := []byte(*entry.Default)
		if err := g.put(ctx, key, v); err != nil {
			return nil, false, err
		}
		return v, false, nil
	case entry.Passwd != nil:
		v, err := passwdValue(ctx, key, *entry.Passwd)
		return v, false, err
	case entry.Optional:
		return nil, true, nil
	default:
		if entry.Update {
			return nil, false, errs.Newf("env var %q not set (and update: true)", varName)
		}
		return nil, false, errs.Newf("env var %q not set", varName)
	}
}

// put writes to the cache when present (no-op without PATH_SECRETS).
func (g *Env) put(ctx *plugin.Context, key string, val []byte) error {
	if ctx.Cache == nil {
		return nil
	}
	return ctx.Cache.Put(key, val)
}

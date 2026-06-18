package generator

import (
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Env is the generator for the `env:` field. It reads values from the
// environment, falling back to cache-first behavior for stability:
//
//   - First run: read $VAR, store in cache, emit
//   - Subsequent runs: read cache, emit (env not consulted)
//   - update: true: bypass cache, always read $VAR
//
// Missing env vars are an error unless the cache has a previous value.
type Env struct {
	spec map[string]specpkg.EnvEntry
}

// NewEnv wraps an env map.
func NewEnv(spec map[string]specpkg.EnvEntry) *Env { return &Env{spec: spec} }

// Name returns the generator's spec field name.
func (g *Env) Name() string { return "env" }

// Generate returns one Entry per env entry. For each key:
//
//   - update=true: read $var, error if missing, store in cache, emit
//   - update=false (default): cache-first; on miss, read $var, error if
//     missing, store, emit
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
		entry := g.spec[k]
		varName := entry.EffectiveVar(k)
		val, err := g.lookup(ctx, k, varName, entry.Update)
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		out = append(out, plugin.Entry{Key: k, Value: val})
	}
	return out, nil
}

// lookup implements the cache + env-var resolution policy.
func (g *Env) lookup(ctx *plugin.Context, key, varName string, update bool) ([]byte, error) {
	// Update mode: always read env, write to cache (so secretRef can
	// see the current value), emit current value.
	if update {
		val, ok := ctx.Env(varName)
		if !ok {
			return nil, errs.Newf("env var %q not set (and update: true)", varName)
		}
		if ctx.Cache != nil {
			if err := ctx.Cache.Put(key, []byte(val)); err != nil {
				return nil, err
			}
		}
		return []byte(val), nil
	}

	// Default cache-first mode.
	if ctx.Cache != nil && ctx.Cache.Has(key) {
		return ctx.Cache.Get(key)
	}
	val, ok := ctx.Env(varName)
	if !ok {
		return nil, errs.Newf("env var %q not set", varName)
	}
	if ctx.Cache != nil {
		if err := ctx.Cache.Put(key, []byte(val)); err != nil {
			return nil, err
		}
	}
	return []byte(val), nil
}

package generator

import (
	"fmt"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/charset"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/pkg/random"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Passwd is the generator for the `passwd:` field. It produces random
// passwords using crypto/rand and stores them in the cache so output
// is byte-stable across runs (the cache is the source of truth).
type Passwd struct {
	spec map[string]specpkg.PasswdEntry
}

// NewPasswd wraps a passwd map.
func NewPasswd(spec map[string]specpkg.PasswdEntry) *Passwd { return &Passwd{spec: spec} }

// Name returns the generator's spec field name.
func (g *Passwd) Name() string { return "passwd" }

// Generate produces one Entry per passwd spec, using GetOrCreate so
// existing cached values are reused.
func (g *Passwd) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	if ctx.Cache == nil {
		return nil, errs.New("passwd generator requires PATH_SECRETS to be set")
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		entry := g.spec[k]
		length := entry.EffectiveLength()
		chars, err := charset.Resolve(entry.EffectiveChars())
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		required, err := entry.RequireClasses()
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		// Validate feasibility up front (clear config error, not a generation
		// retry-exhaustion): the password must be long enough to hold every
		// required class, and the charset must be able to supply each one.
		if len(required) > length {
			return nil, errs.Wrap(k, fmt.Errorf("require lists %d classes but length is %d", len(required), length))
		}
		for _, c := range required {
			if !charset.PoolContains(chars, c) {
				return nil, errs.Wrap(k, fmt.Errorf("charset %q has no %q characters to satisfy require", entry.EffectiveChars(), c))
			}
		}
		val, err := ctx.Cache.GetOrCreate(k, func() ([]byte, error) {
			if len(required) == 0 {
				return random.Password(length, chars)
			}
			return random.PasswordSatisfying(length, chars, func(p []byte) bool {
				return charset.SatisfiesAll(p, required)
			})
		})
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		out = append(out, plugin.Entry{Key: k, Value: val})
	}
	return out, nil
}

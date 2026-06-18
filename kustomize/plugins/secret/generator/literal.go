// Package generator implements the secret-specific Generator
// implementations for each spec field. Every generator is constructed
// from its parsed spec sub-tree (literals, passwd, env, ...) and run
// against a shared *plugin.Context to produce []plugin.Entry.
package generator

import (
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
)

// Literal is the generator for the `literals:` field. It emits each
// (key, value) pair verbatim — no caching, no env lookup, no surprises.
type Literal struct {
	spec map[string]string
}

// NewLiteral wraps a literals map.
func NewLiteral(spec map[string]string) *Literal { return &Literal{spec: spec} }

// Name returns the generator's spec field name.
func (g *Literal) Name() string { return "literals" }

// Generate returns one Entry per literal, in deterministic key order.
func (g *Literal) Generate(_ *plugin.Context) ([]plugin.Entry, error) {
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
		out = append(out, plugin.Entry{Key: k, Value: []byte(g.spec[k])})
	}
	return out, nil
}

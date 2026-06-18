package plugin

import (
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

// Registry runs an ordered set of generators against a single Context.
// Generators are added in their natural order; Run executes them in
// that order and detects key collisions across generators.
//
// The order matters because some generators may depend on cache state
// produced by earlier generators (e.g. secretRef reading values written
// by passwd in the same plugin run). The Secret plugin uses this order:
// literals → env → b64 → file → passwd → secretRef → htpasswd.
type Registry struct {
	generators []Generator
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Add appends a generator to the registry. Generators are run in the
// order they're added.
func (r *Registry) Add(g Generator) {
	if g != nil {
		r.generators = append(r.generators, g)
	}
}

// Run executes all generators against ctx and returns the merged
// entries, sorted by key. Returns an error if any generator fails or
// if two generators produce the same key (a collision is always a
// user error: the same data key can't be produced twice).
func (r *Registry) Run(ctx *Context) ([]Entry, error) {
	merged := make(map[string]string, 16) // key → producing-generator-name
	var entries []Entry
	for _, g := range r.generators {
		got, err := g.Generate(ctx)
		if err != nil {
			return nil, errs.Wrap(g.Name(), err)
		}
		for _, e := range got {
			if other, ok := merged[e.Key]; ok {
				return nil, errs.Newf(
					"key %q is produced by both %q and %q (a key may only appear in one generator section)",
					e.Key, other, g.Name())
			}
			merged[e.Key] = g.Name()
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

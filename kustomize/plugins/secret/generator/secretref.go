package generator

import (
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// SecretRef is the generator for the `secretRef:` field. It reads
// values that were written to the cache by other generators (typically
// passwd or htpasswd in another Secret resource within the same
// kustomize build).
//
// Cross-secret references compute the cache filename from
// (secret, namespace, key) using the same convention as the
// Cache.FormatName helper, so a passwd entry in one Secret can be
// referenced from a secretRef entry in another.
type SecretRef struct {
	spec map[string]specpkg.SecretRefEntry
}

// NewSecretRef wraps a secretRef map.
func NewSecretRef(spec map[string]specpkg.SecretRefEntry) *SecretRef {
	return &SecretRef{spec: spec}
}

// Name returns the generator's spec field name.
func (g *SecretRef) Name() string { return "secretRef" }

// Generate reads each referenced cache entry and emits its value.
func (g *SecretRef) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	if ctx.Cache == nil {
		return nil, errs.New("secretRef generator requires PATH_SECRETS to be set")
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		entry := g.spec[k]
		ns := entry.EffectiveNamespace(ctx.Namespace)
		filename := cache.FormatName(entry.Secret, ns, entry.Key)
		val, err := ctx.Cache.ReadByName(filename)
		if err != nil {
			return nil, errs.Wrap(k, errs.Newf("read %s/%s/%s: %v", entry.Secret, ns, entry.Key, err))
		}
		out = append(out, plugin.Entry{Key: k, Value: val})
	}
	return out, nil
}

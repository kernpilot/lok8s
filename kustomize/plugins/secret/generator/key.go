package generator

import (
	"crypto/rand"
	"io"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/certgen"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Key is the generator for the `key:` field. It mints an asymmetric private key
// (RSA or Ed25519) as PKCS#8 PEM — the openssl `genpkey` default format — and
// caches it by the entry key, so an existing key is reused verbatim (NO re-key).
// It replaces `bash: { exec: "openssl genpkey ..." }` for private keys: same
// output shape, but no shell and no approval gate.
type Key struct {
	spec map[string]specpkg.KeyEntry
}

// NewKey wraps a key map.
func NewKey(spec map[string]specpkg.KeyEntry) *Key { return &Key{spec: spec} }

// Name returns the generator's spec field name.
func (g *Key) Name() string { return "key" }

// Generate produces one PEM key per spec entry, using GetOrCreate so an existing
// cached key is returned unchanged.
func (g *Key) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	if ctx.Cache == nil {
		return nil, errs.New("key generator requires PATH_SECRETS to be set")
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		entry := g.spec[k]
		val, err := ctx.Cache.GetOrCreate(k, func() ([]byte, error) {
			return generateKey(ctx, entry)
		})
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		out = append(out, plugin.Entry{Key: k, Value: val})
	}
	return out, nil
}

// generateKey mints a single private key per the entry's algorithm/bits, reusing
// certgen's PKCS#8 PEM helpers (the same marshalling the cert: generator uses).
func generateKey(ctx *plugin.Context, entry specpkg.KeyEntry) ([]byte, error) {
	r := randReader(ctx)
	switch entry.EffectiveAlgorithm() {
	case specpkg.KeyAlgoEd25519:
		return certgen.NewEd25519KeyPEM(r)
	default: // rsa (validated in spec)
		return certgen.NewRSAKeyPEM(r, entry.EffectiveBits())
	}
}

// randReader returns ctx.Rand when set (tests inject a deterministic reader),
// else crypto/rand.Reader.
func randReader(ctx *plugin.Context) io.Reader {
	if ctx.Rand != nil {
		return ctx.Rand
	}
	return rand.Reader
}

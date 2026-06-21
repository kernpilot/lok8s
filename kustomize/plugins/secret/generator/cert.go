package generator

import (
	"strings"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/certgen"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// pathSecretsEnv mirrors secret.PathSecretsEnv (duplicated to avoid an import
// cycle: the secret package imports this generator package).
const pathSecretsEnv = "PATH_SECRETS"

// Cert is the generator for the `cert:` field. It produces either a self-signed
// development CA (emits ca.crt; caches ca.key for signing) or a leaf certificate
// signed by such a CA (emits tls.crt + tls.key), using crypto/x509 — no mkcert
// binary. All material is cached (the cache is the source of truth), so output is
// byte-stable across runs; rotate by deleting the cache file(s).
type Cert struct {
	spec *specpkg.CertSpec
}

// NewCert wraps a cert spec (nil → no-op).
func NewCert(spec *specpkg.CertSpec) *Cert { return &Cert{spec: spec} }

// Name returns the generator's spec field name.
func (g *Cert) Name() string { return "cert" }

// Generate produces the CA or leaf certificate.
func (g *Cert) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if g.spec == nil {
		return nil, nil
	}
	if ctx.Cache == nil {
		return nil, errs.New("cert generator requires PATH_SECRETS to be set")
	}
	if g.spec.CA && len(g.spec.Hosts) > 0 {
		return nil, errs.New("cert: set either `ca: true` or `hosts:` (a leaf), not both")
	}
	if g.spec.CA {
		// The CA lives in THIS Secret's own cache.
		return caEntries(ctx, ctx.Cache)
	}
	if len(g.spec.Hosts) == 0 {
		return nil, errs.New("cert: a leaf needs `hosts:` (or set `ca: true`)")
	}
	if g.spec.CARef == "" {
		return nil, errs.New("cert: a leaf needs `caRef: <secret>[/<namespace>]` naming its signing CA")
	}
	return g.leafEntries(ctx)
}

// caEntries loads-or-creates the CA (key + self-signed cert) in store c and emits
// ca.crt only — the CA private key (ca.key) stays cached for signing and is never
// written into the Kubernetes Secret.
func caEntries(ctx *plugin.Context, c plugin.CacheStore) ([]plugin.Entry, error) {
	caKey, err := c.GetOrCreate("ca.key", func() ([]byte, error) { return certgen.NewCAKey(ctx.Rand) })
	if err != nil {
		return nil, errs.Wrap("ca.key", err)
	}
	caCrt, err := c.GetOrCreate("ca.crt", func() ([]byte, error) { return certgen.SelfSignCA(ctx.Rand, caKey) })
	if err != nil {
		return nil, errs.Wrap("ca.crt", err)
	}
	return []plugin.Entry{{Key: "ca.crt", Value: caCrt}}, nil
}

// leafEntries reads (creating if absent) the CA referenced by caRef, signs a leaf
// for the spec hosts, and emits tls.crt + tls.key.
func (g *Cert) leafEntries(ctx *plugin.Context) ([]plugin.Entry, error) {
	caName, caNs := parseCARef(g.spec.CARef, ctx.Namespace)
	pathSecrets, _ := ctx.Env(pathSecretsEnv)
	if pathSecrets == "" {
		return nil, errs.New("cert: PATH_SECRETS must be set to resolve caRef")
	}
	caStore, err := cache.New(pathSecrets, caNs, caName)
	if err != nil {
		return nil, errs.Wrap("caRef", err)
	}
	// Load-or-create the CA (mkcert's loadCA flow) so build order is irrelevant.
	caKey, err := caStore.GetOrCreate("ca.key", func() ([]byte, error) { return certgen.NewCAKey(ctx.Rand) })
	if err != nil {
		return nil, errs.Wrap("caRef ca.key", err)
	}
	caCrt, err := caStore.GetOrCreate("ca.crt", func() ([]byte, error) { return certgen.SelfSignCA(ctx.Rand, caKey) })
	if err != nil {
		return nil, errs.Wrap("caRef ca.crt", err)
	}
	// Leaf key + cert in THIS Secret's cache.
	leafKey, err := ctx.Cache.GetOrCreate("tls.key", func() ([]byte, error) { return certgen.NewLeafKey(ctx.Rand) })
	if err != nil {
		return nil, errs.Wrap("tls.key", err)
	}
	leafCrt, err := ctx.Cache.GetOrCreate("tls.crt", func() ([]byte, error) {
		return certgen.SignLeaf(ctx.Rand, caCrt, caKey, leafKey, g.spec.Hosts)
	})
	if err != nil {
		return nil, errs.Wrap("tls.crt", err)
	}
	return []plugin.Entry{
		{Key: "tls.crt", Value: leafCrt},
		{Key: "tls.key", Value: leafKey},
	}, nil
}

// parseCARef splits "<secret>[/<namespace>]" into (secret, namespace),
// defaulting the namespace to defNs.
func parseCARef(ref, defNs string) (secret, namespace string) {
	secret, namespace, found := strings.Cut(ref, "/")
	if !found || namespace == "" {
		namespace = defNs
	}
	return secret, namespace
}

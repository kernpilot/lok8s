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
// development CA or a leaf certificate signed by one, using crypto/x509 — no
// mkcert binary.
//
// A leaf's signing CA, by default, is the SHARED mkcert CA at CAROOT (one CA per
// developer, across all projects, browser-trustable via `mkcert -install`). Set
// `caRef` to sign with an OWN, managed CA in the lok8s store instead — for CI,
// separated instances, or special CAs where a machine-shared CA is undesirable.
// `ca: true` declares such an own CA Secret.
//
// All material is cached (the cache is the source of truth) so output is
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
	if g.spec.CARoot {
		if g.spec.CA || len(g.spec.Hosts) > 0 || g.spec.CARef != "" {
			return nil, errs.New("cert: `caRoot: true` (emit the shared CAROOT CA cert) takes no other fields")
		}
		caCrt, _, err := caRootCA(ctx) // load-or-create at CAROOT
		if err != nil {
			return nil, err
		}
		return []plugin.Entry{{Key: "ca.crt", Value: caCrt}}, nil
	}
	if g.spec.CA && len(g.spec.Hosts) > 0 {
		return nil, errs.New("cert: set either `ca: true` or `hosts:` (a leaf), not both")
	}
	if g.spec.CA {
		if g.spec.CARef != "" {
			return nil, errs.New("cert: `ca: true` and `caRef` are mutually exclusive")
		}
		// An own CA, in THIS Secret's store cache.
		return caEntries(ctx, ctx.Cache)
	}
	if len(g.spec.Hosts) == 0 {
		return nil, errs.New("cert: a leaf needs `hosts:` (or set `ca: true`)")
	}
	return g.leafEntries(ctx)
}

// caEntries loads-or-creates a CA (key + self-signed cert) in store c and emits
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

// leafEntries signs a leaf for the spec hosts and emits tls.crt + tls.key. The
// signing CA is the own store CA named by caRef, or — by default — the shared
// mkcert CA at CAROOT.
func (g *Cert) leafEntries(ctx *plugin.Context) ([]plugin.Entry, error) {
	var caCrt, caKey []byte
	var err error
	if g.spec.CARef != "" {
		caCrt, caKey, err = storeCA(ctx, g.spec.CARef)
	} else {
		caCrt, caKey, err = caRootCA(ctx)
	}
	if err != nil {
		return nil, err
	}

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

// storeCA reads (creating if absent) an own CA in the lok8s store, named by ref
// "<secret>[/<namespace>]". Auto-create makes build order irrelevant.
func storeCA(ctx *plugin.Context, ref string) (cert, key []byte, err error) {
	caName, caNs := parseCARef(ref, ctx.Namespace)
	pathSecrets, _ := ctx.Env(pathSecretsEnv)
	if pathSecrets == "" {
		return nil, nil, errs.New("cert: PATH_SECRETS must be set to resolve caRef")
	}
	store, err := cache.New(pathSecrets, caNs, caName)
	if err != nil {
		return nil, nil, errs.Wrap("caRef", err)
	}
	key, err = store.GetOrCreate("ca.key", func() ([]byte, error) { return certgen.NewCAKey(ctx.Rand) })
	if err != nil {
		return nil, nil, errs.Wrap("caRef ca.key", err)
	}
	cert, err = store.GetOrCreate("ca.crt", func() ([]byte, error) { return certgen.SelfSignCA(ctx.Rand, key) })
	if err != nil {
		return nil, nil, errs.Wrap("caRef ca.crt", err)
	}
	return cert, key, nil
}

// caRootCA loads the shared mkcert CA from CAROOT, creating it there (exactly as
// mkcert would) if absent — the DEFAULT signing CA, shared across projects and
// trustable via `mkcert -install` / `lo trust`. It writes rootCA.pem under the
// user's CAROOT (a side effect outside PATH_SECRETS); use caRef for a
// self-contained own CA (CI / isolated instances).
func caRootCA(ctx *plugin.Context) (cert, key []byte, err error) {
	cert, key, err = certgen.LoadOrCreateCARoot(ctx.Rand)
	if err != nil {
		return nil, nil, errs.Wrap("CAROOT", err)
	}
	return cert, key, nil
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

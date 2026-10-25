package generator

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

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
			return nil, errs.New("cert: `caRoot: true` (emit the shared CAROOT CA cert) takes no other field except `includeKey`")
		}
		caCrt, caKey, err := caRootCA(ctx) // load-or-create at CAROOT
		if err != nil {
			return nil, err
		}
		if g.spec.IncludeKey {
			return caIssuerEntries(caCrt, caKey)
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
		return caEntries(ctx, ctx.Cache, g.spec.IncludeKey)
	}
	if g.spec.IncludeKey {
		return nil, errs.New("cert: `includeKey` is for CA Secrets (`ca: true` or `caRoot: true`) — a leaf always emits its key")
	}
	if len(g.spec.Hosts) == 0 {
		return nil, errs.New("cert: a leaf needs `hosts:` (or set `ca: true`)")
	}
	return g.leafEntries(ctx)
}

// caEntries loads-or-creates a CA (key + self-signed cert) in store c. By
// default it emits ca.crt only — the CA private key (ca.key) stays cached for
// signing and is never written into the Kubernetes Secret. With includeKey the
// keypair is emitted as tls.crt + tls.key — the shape a cert-manager CA issuer
// signs with (explicit opt-in; see CertSpec.IncludeKey).
func caEntries(ctx *plugin.Context, c plugin.CacheStore, includeKey bool) ([]plugin.Entry, error) {
	caKey, err := c.GetOrCreate("ca.key", func() ([]byte, error) { return certgen.NewCAKey(ctx.Rand) })
	if err != nil {
		return nil, errs.Wrap("ca.key", err)
	}
	caCrt, err := c.GetOrCreate("ca.crt", func() ([]byte, error) { return certgen.SelfSignCA(ctx.Rand, caKey) })
	if err != nil {
		return nil, errs.Wrap("ca.crt", err)
	}
	if includeKey {
		return caIssuerEntries(caCrt, caKey)
	}
	return []plugin.Entry{{Key: "ca.crt", Value: caCrt}}, nil
}

// caIssuerEntries shapes a CA keypair for a cert-manager CA issuer: a
// kubernetes.io/tls data map whose tls.crt + tls.key hold the CA itself. The
// pair is validated first (parse + public-key match + IsCA) so a corrupt or
// mismatched CAROOT fails the BUILD, not cluster-side issuance later.
func caIssuerEntries(caCrt, caKey []byte) ([]plugin.Entry, error) {
	if err := certgen.ValidateCAPair(caCrt, caKey); err != nil {
		return nil, errs.Wrap("includeKey", err)
	}
	return []plugin.Entry{
		{Key: "tls.crt", Value: caCrt},
		{Key: "tls.key", Value: caKey},
	}, nil
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
	// OpenStore is the overlay in a dry run: the CA is read, or minted in
	// memory and reported, never written.
	var store plugin.CacheStore
	if ctx.OpenStore != nil {
		store, err = ctx.OpenStore(caNs, caName)
	} else {
		store, err = cache.New(pathSecrets, caNs, caName)
	}
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
//
// A dry run never writes CAROOT: it uses the existing CA, or mints one in
// memory (shared by every leaf of this plugin process) and reports it, so lo
// marks the entry non-repeatable.
func caRootCA(ctx *plugin.Context) (cert, key []byte, err error) {
	if ctx.DryRun {
		return dryRunCARoot(ctx)
	}
	cert, key, err = certgen.LoadOrCreateCARoot(ctx.Rand)
	if err != nil {
		return nil, nil, errs.Wrap("CAROOT", err)
	}
	return cert, key, nil
}

// ephemeralCA holds the CAs a dry run minted in memory, by CAROOT path,
// when no run store is wired (a direct caller; lo always wires one).
var ephemeralCA struct {
	mu   sync.Mutex
	byID map[string][2][]byte
}

// dryRunCARoot is caRootCA for a dry run: the CA in CAROOT, else the CA this
// run minted already (the run store: every Secret of the run signs with the
// same one; the reading render is reported too, a real run signs with
// another CA), else a new one, minted in memory, kept for the run and
// reported.
func dryRunCARoot(ctx *plugin.Context) (cert, key []byte, err error) {
	dir, cert, key, found, err := certgen.LoadCARoot()
	if err != nil {
		return nil, nil, errs.Wrap("CAROOT", err)
	}
	if found {
		return cert, key, nil
	}
	sum := sha256.Sum256([]byte(dir))
	id := "caroot-" + hex.EncodeToString(sum[:8])
	ephemeralCA.mu.Lock()
	defer ephemeralCA.mu.Unlock()
	if ctx.RunLoad != nil {
		c, okc := ctx.RunLoad(id + "/" + certgen.RootName)
		k, okk := ctx.RunLoad(id + "/" + certgen.RootKeyName)
		if okc && okk {
			if err := ctx.Report("reuse caroot " + dir); err != nil {
				return nil, nil, errs.Wrap("CAROOT", err)
			}
			return c, k, nil
		}
	} else if pair, ok := ephemeralCA.byID[dir]; ok {
		if err := ctx.Report("reuse caroot " + dir); err != nil {
			return nil, nil, errs.Wrap("CAROOT", err)
		}
		return pair[0], pair[1], nil
	}
	if cert, key, err = certgen.NewCA(ctx.Rand); err != nil {
		return nil, nil, errs.Wrap("CAROOT", err)
	}
	if err := ctx.Report("caroot " + dir); err != nil {
		return nil, nil, errs.Wrap("CAROOT", err)
	}
	if ctx.RunSave != nil {
		if err := ctx.RunSave(id+"/"+certgen.RootKeyName, key); err != nil {
			return nil, nil, errs.Wrap("CAROOT", err)
		}
		if err := ctx.RunSave(id+"/"+certgen.RootName, cert); err != nil {
			return nil, nil, errs.Wrap("CAROOT", err)
		}
		return cert, key, nil
	}
	if ephemeralCA.byID == nil {
		ephemeralCA.byID = map[string][2][]byte{}
	}
	ephemeralCA.byID[dir] = [2][]byte{cert, key}
	return cert, key, nil
}

// parseCARef splits "<secret>[/<namespace>]" into (secret, namespace),
// defaulting the namespace to defNs.
// ParseCARef is parseCARef for the plugin's preflight (secret.Plugin.Build):
// the Secret name and namespace of the CA store a caRef names.
func ParseCARef(ref, defNs string) (secret, namespace string) { return parseCARef(ref, defNs) }

func parseCARef(ref, defNs string) (secret, namespace string) {
	secret, namespace, found := strings.Cut(ref, "/")
	if !found || namespace == "" {
		namespace = defNs
	}
	return secret, namespace
}

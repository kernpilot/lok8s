// Package secret is the plugin assembly for secrets.lok8s.dev/v1/Secret.
//
// It implements the plugin.Plugin interface from pkg/plugin so the
// runner can decode → build registry → run generators → emit Secret.
// The cmd/secret/main.go entrypoint is ~10 lines that just call
// plugin.Run(stdin, stdout, secret.New()).
//
// Generator order (set in Build) is:
//
//	literals → env → b64 → file → passwd → key → template → bash → secretRef → htpasswd → cert
//
// Rationale: stateless generators run first so any cache state needed
// by secretRef/htpasswd (and template: secretRef sub-sections) has already
// been written by passwd/key/template within the same plugin run if applicable.
package secret

import (
	"io"
	"os"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/kresource"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/pkg/random"

	"github.com/kernpilot/lok8s/kustomize/plugins/secret/generator"
	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// PathSecretsEnv is the env var that holds the cache directory.
// Matches the bash plugin's convention so existing $PATH_SECRETS
// directories work without migration.
const PathSecretsEnv = "PATH_SECRETS"

// Plugin is the secrets.lok8s.dev/v1/Secret plugin assembly.
// Construct via New().
type Plugin struct {
	spec *specpkg.Secret
}

// New returns a new Plugin instance.
func New() *Plugin { return &Plugin{} }

// Decode reads the Secret CRD from r with strict field checking.
func (p *Plugin) Decode(r io.Reader) error {
	s, err := specpkg.Decode(r)
	if err != nil {
		return err
	}
	p.spec = s
	return nil
}

// Build returns a registry populated with the configured generators
// and a Context with cache + env wired up.
func (p *Plugin) Build(env func(string) (string, bool), fileRoot string) (*plugin.Registry, *plugin.Context, error) {
	if p.spec == nil {
		return nil, nil, errs.New("secret plugin: Build called before Decode")
	}

	// Resolve $PATH_SECRETS for the cache. Allowed to be unset; the
	// cache pointer will be nil and cached generators will return a
	// clear error if used.
	//
	// A dry run (LOK8S_BOOTSTRAP_DRYRUN=1) opens the read-only overlay
	// instead: it reads the existing store, generates a missing value in
	// memory, writes nothing and reports what it generated.
	pathSecrets, _ := env(PathSecretsEnv)
	dr := cache.DryRunFromEnv(env)
	var c plugin.CacheStore
	var open func(namespace, name string) (plugin.CacheStore, error)
	if pathSecrets != "" {
		open = func(namespace, name string) (plugin.CacheStore, error) {
			return cache.Open(pathSecrets, namespace, name, dr)
		}
		concreteCache, err := open(p.spec.Metadata.Namespace, p.spec.Metadata.Name)
		if err != nil {
			return nil, nil, err
		}
		c = concreteCache
		if err := p.preflight(pathSecrets); err != nil {
			return nil, nil, err
		}
	}

	ctx := &plugin.Context{
		Name:      p.spec.Metadata.Name,
		Namespace: p.spec.Metadata.Namespace,
		Cache:     c,
		Env:       env,
		FileRoot:  fileRoot,
		Rand:      random.Reader,
		OpenStore: open,
	}
	if dr != nil {
		ctx.DryRun = true
		ctx.Minted = dr.Minted
		ctx.RunLoad = dr.Load
		ctx.RunSave = dr.Save
	}

	r := plugin.NewRegistry()
	// Order matters: stateless first, cached after.
	r.Add(generator.NewLiteral(p.spec.Literals))
	r.Add(generator.NewEnv(p.spec.Env))
	r.Add(generator.NewB64(p.spec.B64))
	r.Add(generator.NewFile(p.spec.File))
	r.Add(generator.NewPasswd(p.spec.Passwd))
	r.Add(generator.NewKey(p.spec.Key))
	r.Add(generator.NewTemplate(p.spec.Template))
	r.Add(generator.NewBash(p.spec.Bash))
	r.Add(generator.NewSecretRef(p.spec.SecretRef))
	r.Add(generator.NewHtpasswd(p.spec.Htpasswd))
	r.Add(generator.NewCert(p.spec.Cert))

	return r, ctx, nil
}

// preflight refuses the run before any generator reads or writes the store
// when an entry the Secret uses has its .enc but no plaintext (B425): every
// entry of this Secret, each secretRef target and the caRef CA store. A
// refusal in the middle of the run (an earlier key generated, a later key
// refused) would leave the store half-written. An `update: true` key (env,
// bash) is not checked: the generator writes it whatever the store holds.
func (p *Plugin) preflight(dir string) error {
	ns := p.spec.Metadata.Namespace
	name := p.spec.Metadata.Name
	overwrite := map[string]bool{}
	for k, e := range p.spec.Env {
		if e.Update {
			overwrite[cache.FormatName(name, ns, k)] = true
		}
	}
	for k, e := range p.spec.Bash {
		if e.Update {
			overwrite[cache.FormatName(name, ns, k)] = true
		}
	}
	if err := cache.FirstEncryptedOnly(dir, cache.FormatName(name, ns, ""), overwrite); err != nil {
		return err
	}
	refs := make([]string, 0, len(p.spec.SecretRef))
	for k := range p.spec.SecretRef {
		refs = append(refs, k)
	}
	sort.Strings(refs)
	for _, k := range refs {
		e := p.spec.SecretRef[k]
		if err := cache.EncryptedOnly(dir, cache.FormatName(e.Secret, e.EffectiveNamespace(ns), e.Key)); err != nil {
			return err
		}
	}
	if p.spec.Cert != nil && p.spec.Cert.CARef != "" {
		caName, caNs := generator.ParseCARef(p.spec.Cert.CARef, ns)
		if err := cache.FirstEncryptedOnly(dir, cache.FormatName(caName, caNs, ""), nil); err != nil {
			return err
		}
	}
	return nil
}

// Emit serializes the merged entries as a Kubernetes Secret resource.
// Type-key validation runs unless the spec disabled it (validate: false).
func (p *Plugin) Emit(entries []plugin.Entry, w io.Writer) error {
	if p.spec == nil {
		return errs.New("secret plugin: Emit called before Decode")
	}
	b := kresource.NewSecret(p.spec.Metadata.Name, p.spec.Metadata.Namespace, p.spec.Type)
	if len(p.spec.Metadata.Labels) > 0 {
		b.Labels = p.spec.Metadata.Labels
	}
	if len(p.spec.Metadata.Annotations) > 0 {
		b.Annotations = p.spec.Metadata.Annotations
	}
	b.Immutable = p.spec.Immutable
	for _, e := range entries {
		b.Add(e.Key, e.Value)
	}

	// Type-key validation (opt-out via validate: false).
	if p.spec.ValidationEnabled() {
		if err := kresource.ValidateSecretKeys(b.Type, b.Keys()); err != nil {
			return err
		}
	}

	out, err := b.Marshal()
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// envFromOS is the production env lookup. Exported for callers that
// need to bypass cmd/secret/main.go (e.g. integration tests). Not used
// by the runner — that uses plugin.DefaultEnv directly.
func envFromOS(key string) (string, bool) { return os.LookupEnv(key) }

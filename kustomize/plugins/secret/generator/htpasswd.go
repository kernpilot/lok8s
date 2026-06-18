package generator

import (
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/charset"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/htpasswdfmt"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/pkg/random"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Htpasswd is the generator for the `htpasswd:` field. Each entry
// produces a single output value of the form
// "username:$2y$10$<bcrypt hash>" and writes three cache files:
//
//	Secret.<name>.<ns>.<key>.username  → plaintext username
//	Secret.<name>.<ns>.<key>.password  → plaintext password
//	Secret.<name>.<ns>.<key>.bcrypt    → "username:bcrypt-hash" final line
//
// The .bcrypt entry exists so the output is byte-stable across runs
// despite bcrypt's non-deterministic salt. To rotate, delete .bcrypt
// (or all three) from $PATH_SECRETS.
//
// Other secrets can reference the .username and .password cache files
// directly via secretRef:
//
//	secretRef:
//	  USER: my-secret/smtp.htpasswd.username
//	  PASS: my-secret/smtp.htpasswd.password
type Htpasswd struct {
	spec map[string]specpkg.HtpasswdEntry
}

// NewHtpasswd wraps an htpasswd map.
func NewHtpasswd(spec map[string]specpkg.HtpasswdEntry) *Htpasswd { return &Htpasswd{spec: spec} }

// Name returns the generator's spec field name.
func (g *Htpasswd) Name() string { return "htpasswd" }

// Generate processes each entry: resolve username + password (literal
// or generated), bcrypt the password, and emit the htpasswd line.
//
// All three cache files are populated on the first run; subsequent
// runs read the .bcrypt file directly so the output never changes.
func (g *Htpasswd) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	if ctx.Cache == nil {
		return nil, errs.New("htpasswd generator requires PATH_SECRETS to be set")
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		entry := g.spec[k]
		line, err := g.processEntry(ctx, k, entry)
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		out = append(out, plugin.Entry{Key: k, Value: line})
	}
	return out, nil
}

// processEntry handles one htpasswd entry's full lifecycle:
//  1. Resolve username (cache .username, then literal/generate)
//  2. Resolve password (cache .password, then literal/generate)
//  3. Resolve bcrypt line (cache .bcrypt, then bcrypt the password)
//
// Each resolution uses GetOrCreate so the cache is always written on
// the first run and read on subsequent runs.
func (g *Htpasswd) processEntry(ctx *plugin.Context, key string, entry specpkg.HtpasswdEntry) ([]byte, error) {
	username, err := resolveUserOrGen(ctx, key+".username", entry.Username, defaultUsernameLength)
	if err != nil {
		return nil, errs.Wrap("username", err)
	}
	password, err := resolveUserOrGen(ctx, key+".password", entry.Password, defaultPasswordLength)
	if err != nil {
		return nil, errs.Wrap("password", err)
	}
	// Cache the final bcrypt line so output is byte-stable across runs.
	bcryptKey := key + ".bcrypt"
	line, err := ctx.Cache.GetOrCreate(bcryptKey, func() ([]byte, error) {
		return htpasswdfmt.Format(string(username), password)
	})
	if err != nil {
		return nil, errs.Wrap("bcrypt", err)
	}
	return line, nil
}

const (
	defaultUsernameLength = 16
	defaultPasswordLength = 32
)

// resolveUserOrGen returns the cached value at cacheKey, generating one
// from the spec on cache miss. Literal specs are stored verbatim;
// generated specs use random.Username for usernames (alphanumeric,
// starts with a letter) and random.Password for passwords.
func resolveUserOrGen(ctx *plugin.Context, cacheKey string, spec specpkg.UserOrGen, defaultLen int) ([]byte, error) {
	return ctx.Cache.GetOrCreate(cacheKey, func() ([]byte, error) {
		if spec.IsLiteral() {
			return []byte(spec.Literal), nil
		}
		length := spec.Gen.EffectiveLength()
		if length == 0 {
			length = defaultLen
		}
		// Username generator path: only used for usernames (the cache
		// key has ".username" suffix).
		if isUsernameKey(cacheKey) {
			return random.Username(length)
		}
		chars, err := charset.Resolve(spec.Gen.EffectiveChars())
		if err != nil {
			return nil, err
		}
		return random.Password(length, chars)
	})
}

// isUsernameKey reports whether the cache key targets the .username
// slot (heuristic: ends in ".username"). Used to dispatch between
// random.Username (alphanumeric, starts with letter) and random.Password.
func isUsernameKey(key string) bool {
	const suffix = ".username"
	if len(key) < len(suffix) {
		return false
	}
	return key[len(key)-len(suffix):] == suffix
}

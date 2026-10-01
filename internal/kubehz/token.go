package kubehz

// token.go — `lo kubehz token`: the kubectl exec credential plugin for a
// kubehz AGENT KEY (a machine identity an owner creates in the dashboard or
// with POST /api/agent-keys). The key is a client id and a client secret;
// this command runs the OAuth client_credentials grant at the platform's
// token endpoint and prints the access token, by default as a kubectl
// ExecCredential:
//
//	users:
//	- name: agent
//	  user:
//	    exec:
//	      apiVersion: client.authentication.k8s.io/v1
//	      command: lo
//	      args: [kubehz, token, --token-url, <tokenEndpoint>, --scope, <tokenScope>]
//	      interactiveMode: Never
//
// The same token authenticates against the platform api (KUBEHZ_TOKEN=$(lo
// kubehz token --format token)).
//
// SECRET CONTAINMENT: the client secret comes from KUBEHZ_AGENT_CLIENT_SECRET
// or --secret-file, never from a flag (argv is world-readable through ps). It
// travels only in the Authorization header of an https:// request and is
// never printed, logged, cached or embedded in an error. The cache holds the
// ACCESS TOKEN only, in a 0600 file under a 0700 directory, and is reused
// until five minutes before the token expires.
//
// The bash twin (libs/kubehz/main kubehz::token) prints the same bytes and
// shares the cache file format: {"access_token":…,"expires_at":<epoch s>}.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Agent-key environment (the names the platform docs and the create answer use).
const (
	EnvAgentClientID     = "KUBEHZ_AGENT_CLIENT_ID"
	EnvAgentClientSecret = "KUBEHZ_AGENT_CLIENT_SECRET" // #nosec G101 -- the variable NAME, not a credential
	EnvAgentTokenURL     = "KUBEHZ_AGENT_TOKEN_URL"     // #nosec G101 -- the variable NAME, not a credential
	EnvAgentScope        = "KUBEHZ_AGENT_SCOPE"
)

// Token output formats.
const (
	TokenFormatExecCredential = "exec-credential" // #nosec G101 -- an output format name, not a credential
	TokenFormatToken          = "token"           // #nosec G101 -- an output format name, not a credential
)

const (
	// tokenRefreshMargin: a cached token is reused only while it lives at
	// least this long, so kubectl never sends one that expires mid-request.
	tokenRefreshMargin = 5 * time.Minute
	// tokenMaxLifetime caps a token endpoint's expires_in: a cache entry
	// never outlives a day, whatever the endpoint claims.
	tokenMaxLifetime = 24 * time.Hour
	// execCredentialV1 is the default ExecCredential apiVersion; kubectl
	// names the one it wants in KUBERNETES_EXEC_INFO.
	execCredentialV1 = "client.authentication.k8s.io/v1"
)

// TokenOptions are the command's flags.
type TokenOptions struct {
	TokenURL   string // --token-url, else KUBEHZ_AGENT_TOKEN_URL
	Scope      string // --scope, else KUBEHZ_AGENT_SCOPE
	SecretFile string // --secret-file, else KUBEHZ_AGENT_CLIENT_SECRET
	Format     string // exec-credential (default) | token
	NoCache    bool   // --no-cache: always ask the endpoint, write nothing
	CacheDir   string // test seam; "" = ${XDG_CACHE_HOME:-~/.cache}/lok8s/kubehz-token
}

// tokenCacheEntry is the on-disk cache shape, shared with the bash twin.
type tokenCacheEntry struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"`
}

// execCredential is the kubectl ExecCredential, keys in the bash twin's order.
type execCredential struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Status     execCredentialStatus `json:"status"`
}

type execCredentialStatus struct {
	Token               string `json:"token"`
	ExpirationTimestamp string `json:"expirationTimestamp"`
}

// Token prints an access token for the agent key in the environment.
func (c *Context) Token(ctx context.Context, o TokenOptions) error {
	format := o.Format
	if format == "" {
		format = TokenFormatExecCredential
	}
	if format != TokenFormatExecCredential && format != TokenFormatToken {
		c.errorf("unknown --format %s (want exec-credential or token)", format)
		return ErrHandled
	}
	tokenURL := firstNonEmpty(o.TokenURL, c.getenv(EnvAgentTokenURL))
	scope := firstNonEmpty(o.Scope, c.getenv(EnvAgentScope))
	if tokenURL == "" || scope == "" {
		c.errorf("token url and scope are required: pass --token-url and --scope, or set %s and %s", EnvAgentTokenURL, EnvAgentScope)
		return ErrHandled
	}
	if err := c.requireHTTPS(tokenURL, "Token URL"); err != nil {
		return ErrHandled
	}
	clientID := c.getenv(EnvAgentClientID)
	secret := c.getenv(EnvAgentClientSecret)
	if o.SecretFile != "" {
		b, err := os.ReadFile(o.SecretFile)
		if err != nil {
			c.errorf("cannot read the secret file %s", o.SecretFile)
			return ErrHandled
		}
		secret = strings.TrimSpace(string(b))
	}
	if clientID == "" || secret == "" {
		c.errorf("no agent key: set %s and %s (or pass --secret-file)", EnvAgentClientID, EnvAgentClientSecret)
		return ErrHandled
	}

	now := c.now()
	cacheFile := ""
	if !o.NoCache {
		if dir := c.tokenCacheDir(o.CacheDir); dir != "" {
			cacheFile = filepath.Join(dir, tokenCacheName(tokenURL, clientID, scope))
		}
	}
	if cacheFile != "" {
		if e, ok := readTokenCache(cacheFile); ok && time.Unix(e.ExpiresAt, 0).Sub(now) > tokenRefreshMargin {
			return c.emitToken(format, e)
		}
	}

	e, err := c.requestToken(ctx, tokenURL, clientID, secret, scope, now)
	if err != nil {
		return err
	}
	if cacheFile != "" {
		if err := writeTokenCache(cacheFile, e); err != nil {
			c.debugf("token cache not written: %v", err)
		}
	}
	return c.emitToken(format, e)
}

// requestToken runs client_credentials (RFC 6749 4.4, client_secret_basic).
func (c *Context) requestToken(ctx context.Context, tokenURL, clientID, secret, scope string, now time.Time) (tokenCacheEntry, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		c.errorf("invalid token url: %s", tokenURL)
		return tokenCacheEntry{}, ErrHandled
	}
	// Raw, like curl's `user` in the bash twin: ZITADEL's client ids and
	// generated secrets are URL-safe, so the RFC 6749 2.3.1 form-encoding
	// changes nothing for them, and both implementations send one header.
	req.SetBasicAuth(clientID, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		c.errorf("token request to %s failed: no answer", tokenURL)
		return tokenCacheEntry{}, ErrHandled
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var answer struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &answer)
	if resp.StatusCode != http.StatusOK {
		reason := strings.TrimSpace(answer.Error + " " + answer.ErrorDescription)
		if reason == "" {
			reason = "no reason given"
		}
		c.errorf("token request refused: HTTP %d: %s", resp.StatusCode, printable(reason, 200))
		return tokenCacheEntry{}, ErrHandled
	}
	if answer.AccessToken == "" || answer.ExpiresIn <= 0 {
		c.errorf("token endpoint answered without an access token or a lifetime")
		return tokenCacheEntry{}, ErrHandled
	}
	life := min(time.Duration(answer.ExpiresIn)*time.Second, tokenMaxLifetime)
	return tokenCacheEntry{AccessToken: answer.AccessToken, ExpiresAt: now.Add(life).Unix()}, nil
}

func (c *Context) emitToken(format string, e tokenCacheEntry) error {
	if format == TokenFormatToken {
		c.echo("%s", e.AccessToken)
		return nil
	}
	cred := execCredential{
		APIVersion: c.execCredentialAPIVersion(),
		Kind:       "ExecCredential",
		Status: execCredentialStatus{
			Token:               e.AccessToken,
			ExpirationTimestamp: time.Unix(e.ExpiresAt, 0).UTC().Format(time.RFC3339),
		},
	}
	enc := json.NewEncoder(c.out())
	enc.SetEscapeHTML(false)
	return enc.Encode(cred)
}

// execCredentialAPIVersion is the apiVersion kubectl asked for in
// KUBERNETES_EXEC_INFO (v1 or v1beta1), else v1.
func (c *Context) execCredentialAPIVersion() string {
	var info struct {
		APIVersion string `json:"apiVersion"`
	}
	if raw := c.getenv("KUBERNETES_EXEC_INFO"); raw != "" && json.Unmarshal([]byte(raw), &info) == nil {
		switch info.APIVersion {
		case "client.authentication.k8s.io/v1", "client.authentication.k8s.io/v1beta1":
			return info.APIVersion
		}
	}
	return execCredentialV1
}

func (c *Context) tokenCacheDir(override string) string {
	if override != "" {
		return override
	}
	base := c.getenv("XDG_CACHE_HOME")
	if base == "" {
		home := c.getenv("HOME")
		if home == "" {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "lok8s", "kubehz-token")
}

// tokenCacheName keys the cache on the endpoint, the client and the scope,
// never on the secret. The bash twin: printf '%s\n%s\n%s' … | sha256sum.
func tokenCacheName(tokenURL, clientID, scope string) string {
	sum := sha256.Sum256([]byte(tokenURL + "\n" + clientID + "\n" + scope))
	return hex.EncodeToString(sum[:])[:32] + ".json"
}

func readTokenCache(file string) (tokenCacheEntry, bool) {
	fi, err := os.Stat(file)
	// A cache file anyone else can read is not ours to trust: ignore it.
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return tokenCacheEntry{}, false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return tokenCacheEntry{}, false
	}
	var e tokenCacheEntry
	if json.Unmarshal(b, &e) != nil || e.AccessToken == "" || e.ExpiresAt <= 0 {
		return tokenCacheEntry{}, false
	}
	return e, true
}

func writeTokenCache(file string, e tokenCacheEntry) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// #nosec G117 -- the cache exists to hold the access token: a 0600 file
	// under a 0700 dir, read back only when nobody else can read it.
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(b, '\n'))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(name, file)
		}
		if err == nil {
			return nil
		}
	} else {
		_ = tmp.Close()
	}
	_ = os.Remove(name)
	return fmt.Errorf("write %s: %w", file, err)
}

// printable keeps an endpoint's error text to one line of plain characters.
func printable(s string, limit int) string {
	r := []rune(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s))
	if len(r) > limit {
		r = r[:limit]
	}
	return string(r)
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

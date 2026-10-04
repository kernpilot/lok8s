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
	"errors"
	"fmt"
	"io"
	"math"
	"net"
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

// tokenRetryDelays are the waits before the second, third and fourth grant.
// A key made a moment ago can answer invalid_client until the identity
// provider's read side has it (about 100 ms measured on ZITADEL), and a
// gateway can drop one request: a pipeline that creates a key and runs
// kubectl at once must not fail on either.
var tokenRetryDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// tokenAttemptTimeout bounds one grant, the body included: kubectl waits on
// this command, so a stalled endpoint must end it (bash: curl --max-time 30).
// A variable only so the tests can shorten it.
var tokenAttemptTimeout = 30 * time.Second

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
	o.TokenURL = firstNonEmpty(o.TokenURL, c.getenv(EnvAgentTokenURL))
	o.Scope = firstNonEmpty(o.Scope, c.getenv(EnvAgentScope))
	if o.TokenURL == "" || o.Scope == "" {
		c.errorf("token url and scope are required: pass --token-url and --scope, or set %s and %s", EnvAgentTokenURL, EnvAgentScope)
		return ErrHandled
	}
	e, err := c.agentAccessToken(ctx, o)
	if err != nil {
		return err
	}
	return c.emitToken(format, e)
}

// agentAccessToken is the grant behind `lo kubehz token` and the agent
// commands (agent.go): the https gate, the key from the environment (or
// o.SecretFile), the cache, then client_credentials. The caller resolves
// o.TokenURL and o.Scope. The bash twin is kubehz::token_access.
func (c *Context) agentAccessToken(ctx context.Context, o TokenOptions) (tokenCacheEntry, error) {
	tokenURL, scope := o.TokenURL, o.Scope
	if err := c.requireHTTPS(tokenURL, "Token URL"); err != nil {
		return tokenCacheEntry{}, ErrHandled
	}
	clientID := c.getenv(EnvAgentClientID)
	secret := c.getenv(EnvAgentClientSecret)
	if o.SecretFile != "" {
		b, err := os.ReadFile(o.SecretFile)
		if err != nil {
			c.errorf("cannot read the secret file %s", o.SecretFile)
			return tokenCacheEntry{}, ErrHandled
		}
		secret = strings.TrimSpace(string(b))
	}
	if clientID == "" || secret == "" {
		c.errorf("no agent key: set %s and %s (or pass --secret-file)", EnvAgentClientID, EnvAgentClientSecret)
		return tokenCacheEntry{}, ErrHandled
	}
	// A line break inside a value would end the bash twin's curl config
	// line and make the rest a curl option: both refuse it.
	if hasControl(clientID) || hasControl(secret) {
		c.errorf("the agent key holds a control character: check %s and the client secret", EnvAgentClientID)
		return tokenCacheEntry{}, ErrHandled
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
			return e, nil
		}
	}

	e, err := c.requestToken(ctx, tokenURL, clientID, secret, scope, now)
	if err != nil {
		return tokenCacheEntry{}, err
	}
	if cacheFile != "" {
		if err := writeTokenCache(cacheFile, e); err != nil {
			c.debugf("token cache not written: %v", err)
		}
	}
	return e, nil
}

// requestToken runs client_credentials (RFC 6749 4.4, client_secret_basic).
// A failure that can pass (no answer, a 5xx, invalid_client) is tried again
// after each of tokenRetryDelays. An attempt that ran out of time is not:
// the endpoint stalls, and four stalls would hold kubectl for two minutes.
// Only the last failure is printed, and no failure is cached.
func (c *Context) requestToken(ctx context.Context, tokenURL, clientID, secret, scope string, now time.Time) (tokenCacheEntry, error) {
	for attempt := 0; ; attempt++ {
		e, fail := c.tokenAttempt(ctx, tokenURL, clientID, secret, scope, now)
		if fail == nil {
			return e, nil
		}
		if !fail.retry || attempt == len(tokenRetryDelays) || c.sleep(ctx, tokenRetryDelays[attempt]) != nil {
			c.errorf("%s", fail.msg)
			return tokenCacheEntry{}, ErrHandled
		}
		c.debugf("token request failed, trying again: %s", fail.msg)
	}
}

// tokenFailure is one failed grant: the message the command prints if it is
// the last, and whether another attempt can pass.
type tokenFailure struct {
	msg   string
	retry bool
}

// tokenAttempt is one grant, bounded by tokenAttemptTimeout, body included.
func (c *Context) tokenAttempt(ctx context.Context, tokenURL, clientID, secret, scope string, now time.Time) (tokenCacheEntry, *tokenFailure) {
	ctx, cancel := context.WithTimeout(ctx, tokenAttemptTimeout)
	defer cancel()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenCacheEntry{}, &tokenFailure{msg: "invalid token url: " + tokenURL}
	}
	// Raw, like curl's `user` in the bash twin: ZITADEL's client ids and
	// generated secrets are URL-safe, so the RFC 6749 2.3.1 form-encoding
	// changes nothing for them, and both implementations send one header.
	req.SetBasicAuth(clientID, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// noAnswer: the error names the url and the transport cause, never the
	// Authorization header. The cause can carry server-chosen text (a
	// certificate name), so it is printed plain.
	noAnswer := func(err error) *tokenFailure {
		c.debugf("token request: %s", printable(err.Error(), 300))
		// Any timeout ends the grant: the attempt deadline, and the
		// transport's own TLS handshake timeout, which is no
		// DeadlineExceeded (curl's --connect-timeout covers both, exit 28).
		var ne net.Error
		timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
		return &tokenFailure{
			msg:   "token request to " + tokenURL + " failed: no answer",
			retry: !timedOut,
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return tokenCacheEntry{}, noAnswer(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenCacheEntry{}, noAnswer(err)
	}

	var answer struct {
		AccessToken string `json:"access_token"`
		// A number, whole or not: the bash twin floors it (jq floor).
		ExpiresIn        float64 `json:"expires_in"`
		Error            string  `json:"error"`
		ErrorDescription string  `json:"error_description"`
	}
	_ = json.Unmarshal(body, &answer)
	if resp.StatusCode != http.StatusOK {
		reason := strings.TrimSpace(answer.Error + " " + answer.ErrorDescription)
		if reason == "" {
			reason = "no reason given"
		}
		return tokenCacheEntry{}, &tokenFailure{
			msg:   fmt.Sprintf("token request refused: HTTP %d: %s", resp.StatusCode, printable(reason, 200)),
			retry: resp.StatusCode >= 500 || answer.Error == "invalid_client",
		}
	}
	seconds := math.Floor(answer.ExpiresIn)
	if answer.AccessToken == "" || seconds <= 0 {
		return tokenCacheEntry{}, &tokenFailure{msg: "token endpoint answered without an access token or a lifetime"}
	}
	// Clamp in seconds first: a huge expires_in would wrap the Duration.
	life := tokenMaxLifetime
	if seconds < tokenMaxLifetime.Seconds() {
		life = time.Duration(seconds) * time.Second
	}
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
	b, err := readPrivateFile(file)
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
	// CreateTemp makes the file 0600 (bash: mktemp), so the token is never
	// readable by others, not even before the rename.
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
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
	_ = os.Remove(name)
	return fmt.Errorf("write %s: %w", file, err)
}

// isControl reports a C0 control character or DEL. The bash twin tests the
// same set under LC_ALL=C (kubehz::token_has_control).
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// printable keeps an endpoint's error text to one line of plain characters.
func printable(s string, limit int) string {
	r := []rune(strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, s))
	if len(r) > limit {
		r = r[:limit]
	}
	return string(r)
}

// hasControl reports whether s holds a control character (isControl).
func hasControl(s string) bool { return strings.ContainsFunc(s, isControl) }

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

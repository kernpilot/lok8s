package kubehz

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// tokenHarness: an agent key in the environment, a token route on the TLS
// server answering one access token per call, and a private cache dir.
func tokenHarness(t *testing.T) (*harness, *int, string) {
	t.Helper()
	h := newHarness(t)
	h.env[EnvAgentClientID] = "kubehz-agent-ak-1a2b3c4d"
	h.env[EnvAgentClientSecret] = "s3cr3t-value"
	calls := 0
	h.handleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"jwt-` + string(rune('0'+calls)) + `","token_type":"Bearer","expires_in":43199}`))
	})
	return h, &calls, filepath.Join(t.TempDir(), "cache")
}

func tokenOpts(h *harness, cache string) TokenOptions {
	return TokenOptions{TokenURL: h.apiURL() + "/oauth/v2/token", Scope: "openid urn:zitadel:iam:org:project:id:P:aud urn:zitadel:iam:org:projects:roles", CacheDir: cache}
}

func TestTokenPrintsAnExecCredentialAndSendsTheGrant(t *testing.T) {
	h, calls, cache := tokenHarness(t)
	if err := h.ctx.Token(context.Background(), tokenOpts(h, cache)); err != nil {
		t.Fatalf("Token: %v (stderr %q)", err, h.errOut.String())
	}
	want := `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"jwt-1","expirationTimestamp":"2023-11-15T10:13:19Z"}}` + "\n"
	if got := h.out.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if *calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", *calls)
	}
	req := h.requests[len(h.requests)-1]
	if req.Body != "grant_type=client_credentials&scope=openid+urn%3Azitadel%3Aiam%3Aorg%3Aproject%3Aid%3AP%3Aaud+urn%3Azitadel%3Aiam%3Aorg%3Aprojects%3Aroles" {
		t.Errorf("form body = %q", req.Body)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("kubehz-agent-ak-1a2b3c4d:s3cr3t-value"))
	if req.Auth != wantAuth {
		t.Errorf("Authorization = %q, want client_secret_basic", req.Auth)
	}
	if strings.Contains(h.out.String()+h.errOut.String(), "s3cr3t-value") {
		t.Error("the client secret reached an output stream")
	}
}

func TestTokenReusesTheCacheUntilFiveMinutesBeforeExpiry(t *testing.T) {
	h, calls, cache := tokenHarness(t)
	o := tokenOpts(h, cache)
	start := time.Unix(1700000000, 0)
	h.ctx.Now = func() time.Time { return start }
	for range 2 {
		h.out.Reset()
		if err := h.ctx.Token(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 1 {
		t.Fatalf("second call asked the endpoint again (calls = %d)", *calls)
	}

	// The cache holds the access token only, in a 0600 file under a 0700 dir.
	entries, _ := os.ReadDir(cache)
	if len(entries) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(entries))
	}
	file := filepath.Join(cache, entries[0].Name())
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode = %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(cache); fi.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode = %v, want 0700", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "s3cr3t-value") {
		t.Error("the cache holds the client secret")
	}
	if string(raw) != `{"access_token":"jwt-1","expires_at":1700043199}`+"\n" {
		t.Errorf("cache file = %q (the bash twin reads this shape)", raw)
	}

	// 4 minutes before expiry: refreshed.
	h.ctx.Now = func() time.Time { return start.Add(43199*time.Second - 4*time.Minute) }
	h.out.Reset()
	if err := h.ctx.Token(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || !strings.Contains(h.out.String(), `"token":"jwt-2"`) {
		t.Errorf("near expiry: calls = %d, out = %q", *calls, h.out.String())
	}
}

func TestTokenIgnoresACacheOthersCanRead(t *testing.T) {
	h, calls, cache := tokenHarness(t)
	o := tokenOpts(h, cache)
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cache, tokenCacheName(o.TokenURL, "kubehz-agent-ak-1a2b3c4d", o.Scope))
	if err := os.WriteFile(file, []byte(`{"access_token":"planted","expires_at":9999999999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.ctx.Token(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || strings.Contains(h.out.String(), "planted") {
		t.Errorf("a world-readable cache file was trusted: calls = %d, out = %q", *calls, h.out.String())
	}
}

func TestTokenIgnoresASymlinkedCacheFile(t *testing.T) {
	h, calls, cache := tokenHarness(t)
	o := tokenOpts(h, cache)
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	// A private 0600 file elsewhere, linked in under the cache name: the
	// link must not be followed (the bash twin refuses -L).
	target := filepath.Join(t.TempDir(), "planted.json")
	if err := os.WriteFile(target, []byte(`{"access_token":"planted","expires_at":9999999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cache, tokenCacheName(o.TokenURL, "kubehz-agent-ak-1a2b3c4d", o.Scope))); err != nil {
		t.Fatal(err)
	}
	if err := h.ctx.Token(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || strings.Contains(h.out.String(), "planted") {
		t.Errorf("a symlinked cache file was trusted: calls = %d, out = %q", *calls, h.out.String())
	}
}

// sequenceHarness answers the token route from a list of (status, body)
// pairs, one per call, the last one repeating; it records every wait.
func sequenceHarness(t *testing.T, answers ...[2]string) (*harness, *int, *[]time.Duration, string) {
	t.Helper()
	h, calls, cache := tokenHarness(t)
	h.handleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		a := answers[min(*calls, len(answers)-1)]
		*calls++
		code := map[string]int{"200": 200, "400": 400, "401": 401, "503": 503}[a[0]]
		w.WriteHeader(code)
		_, _ = w.Write([]byte(a[1]))
	})
	var waits []time.Duration
	h.ctx.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	return h, calls, &waits, cache
}

func TestTokenRetriesAFreshKeyAndAnOutage(t *testing.T) {
	h, calls, waits, cache := sequenceHarness(t,
		[2]string{"401", `{"error":"invalid_client","error_description":"client not found"}`},
		[2]string{"503", `upstream unavailable`},
		[2]string{"200", `{"access_token":"jwt-ok","expires_in":3600}`},
	)
	if err := h.ctx.Token(context.Background(), tokenOpts(h, cache)); err != nil {
		t.Fatalf("Token: %v (stderr %q)", err, h.errOut.String())
	}
	if *calls != 3 || !strings.Contains(h.out.String(), `"token":"jwt-ok"`) {
		t.Errorf("calls = %d, out = %q", *calls, h.out.String())
	}
	if want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}; !slices.Equal(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
	if strings.Contains(h.errOut.String(), "refused") {
		t.Errorf("a passed retry printed a failure: %q", h.errOut.String())
	}
}

func TestTokenGivesUpAfterFourAttemptsAndPrintsOnce(t *testing.T) {
	h, calls, waits, cache := sequenceHarness(t, [2]string{"503", `{"error":"server_error"}`})
	err := h.ctx.Token(context.Background(), tokenOpts(h, cache))
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if *calls != 4 || len(*waits) != 3 {
		t.Errorf("calls = %d, waits = %v, want 4 and 3", *calls, *waits)
	}
	if n := strings.Count(h.errOut.String(), "token request refused: HTTP 503: server_error"); n != 1 {
		t.Errorf("the failure was printed %d times: %q", n, h.errOut.String())
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Error("a failed grant wrote a cache entry")
	}
}

func TestTokenDoesNotRetryARealRefusal(t *testing.T) {
	h, calls, waits, cache := sequenceHarness(t, [2]string{"400", `{"error":"invalid_scope"}`})
	if err := h.ctx.Token(context.Background(), tokenOpts(h, cache)); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if *calls != 1 || len(*waits) != 0 {
		t.Errorf("invalid_scope was retried: calls = %d, waits = %v", *calls, *waits)
	}
}

func TestTokenEndsAStalledAttempt(t *testing.T) {
	saved := tokenAttemptTimeout
	tokenAttemptTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tokenAttemptTimeout = saved })
	h, calls, cache := tokenHarness(t)
	h.handleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.WriteHeader(200)
		w.(http.Flusher).Flush() // headers out, the body never: only a deadline ends it
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	start := time.Now()
	err := h.ctx.Token(context.Background(), tokenOpts(h, cache))
	if !errors.Is(err, ErrHandled) || !strings.Contains(h.errOut.String(), "failed: no answer") {
		t.Fatalf("err = %v, stderr = %q", err, h.errOut.String())
	}
	if *calls != 4 || time.Since(start) > 3*time.Second {
		t.Errorf("calls = %d after %v, want 4 bounded attempts", *calls, time.Since(start))
	}
}

func TestTokenClampsAHugeLifetimeBeforeConverting(t *testing.T) {
	h, _, cache := tokenHarness(t)
	h.handle("POST /oauth/v2/token", 200, `{"access_token":"jwt-long","expires_in":9300000000}`)
	start := time.Unix(1700000000, 0)
	h.ctx.Now = func() time.Time { return start }
	if err := h.ctx.Token(context.Background(), tokenOpts(h, cache)); err != nil {
		t.Fatal(err)
	}
	// 1700000000 + 86400: a day, not a wrapped Duration in the past.
	if !strings.Contains(h.out.String(), `"expirationTimestamp":"2023-11-15T22:13:20Z"`) {
		t.Errorf("out = %q, want the one-day cap", h.out.String())
	}
}

func TestTokenFormatTokenAndTheKubectlAPIVersion(t *testing.T) {
	h, _, cache := tokenHarness(t)
	o := tokenOpts(h, cache)
	o.Format = TokenFormatToken
	if err := h.ctx.Token(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if h.out.String() != "jwt-1\n" {
		t.Errorf("--format token = %q", h.out.String())
	}
	h.out.Reset()
	h.env["KUBERNETES_EXEC_INFO"] = `{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential"}`
	o.Format = ""
	if err := h.ctx.Token(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var cred execCredential
	if err := json.Unmarshal(h.out.Bytes(), &cred); err != nil || cred.APIVersion != "client.authentication.k8s.io/v1beta1" {
		t.Errorf("apiVersion = %q (%v), want the one kubectl asked for", cred.APIVersion, err)
	}
}

func TestTokenRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(h *harness, o *TokenOptions)
		stderr string
	}{
		{"no url", func(h *harness, o *TokenOptions) { o.TokenURL = "" }, "token url and scope are required"},
		{"plain http", func(h *harness, o *TokenOptions) { o.TokenURL = "http://id.example/oauth/v2/token" }, "Token URL must use HTTPS: http://id.example/oauth/v2/token"},
		{"no secret", func(h *harness, o *TokenOptions) { h.env[EnvAgentClientSecret] = "" }, "no agent key: set KUBEHZ_AGENT_CLIENT_ID and KUBEHZ_AGENT_CLIENT_SECRET"},
		{"bad format", func(h *harness, o *TokenOptions) { o.Format = "yaml" }, "unknown --format yaml"},
		{"missing secret file", func(h *harness, o *TokenOptions) { o.SecretFile = "/nonexistent/secret" }, "cannot read the secret file /nonexistent/secret"},
		{"secret file is a directory", func(h *harness, o *TokenOptions) { o.SecretFile = os.TempDir() }, "cannot read the secret file " + os.TempDir()},
		{"line break in the client id", func(h *harness, o *TokenOptions) { h.env[EnvAgentClientID] = "cid\nproxy = \"http://x\"" }, "the agent key holds a control character: check KUBEHZ_AGENT_CLIENT_ID and the client secret"},
		{"control character in the secret", func(h *harness, o *TokenOptions) { h.env[EnvAgentClientSecret] = "s3cr3t\x1b" }, "the agent key holds a control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, calls, cache := tokenHarness(t)
			o := tokenOpts(h, cache)
			tc.mutate(h, &o)
			err := h.ctx.Token(context.Background(), o)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("err = %v, want ErrHandled", err)
			}
			if !strings.Contains(h.errOut.String(), tc.stderr) {
				t.Errorf("stderr = %q, want %q", h.errOut.String(), tc.stderr)
			}
			if *calls != 0 || h.out.Len() != 0 {
				t.Errorf("a refusal reached the endpoint (%d) or stdout (%q)", *calls, h.out.String())
			}
		})
	}
}

func TestTokenEndpointRefusalNamesTheReasonNotTheSecret(t *testing.T) {
	h, _, cache := tokenHarness(t)
	h.handle("POST /oauth/v2/token", 401, `{"error":"invalid_client","error_description":"client authentication failed\nfor s3cr3t"}`)
	err := h.ctx.Token(context.Background(), tokenOpts(h, cache))
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errOut.String(), "token request refused: HTTP 401: invalid_client client authentication failed for s3cr3t") {
		t.Errorf("stderr = %q", h.errOut.String())
	}
	if strings.Contains(h.errOut.String(), "s3cr3t-value") {
		t.Error("the secret reached stderr")
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Error("a refused grant wrote a cache entry")
	}
}

func TestTokenSecretFileAndNoCache(t *testing.T) {
	h, calls, cache := tokenHarness(t)
	h.env[EnvAgentClientSecret] = ""
	sf := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(sf, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := tokenOpts(h, cache)
	o.SecretFile = sf
	o.NoCache = true
	for range 2 {
		if err := h.ctx.Token(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 2 {
		t.Errorf("--no-cache: calls = %d, want 2", *calls)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Error("--no-cache wrote a cache dir")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("kubehz-agent-ak-1a2b3c4d:from-file"))
	if h.requests[len(h.requests)-1].Auth != want {
		t.Error("the secret file was not used (or not trimmed)")
	}
}

func TestTokenCacheNameMatchesTheBashTwin(t *testing.T) {
	// printf '%s\n%s\n%s' https://id.example/oauth/v2/token cid scope | sha256sum | cut -c1-32
	if got := tokenCacheName("https://id.example/oauth/v2/token", "cid", "scope"); got != "307dcc769135510a4f46243554b6d3b1.json" {
		t.Errorf("cache name = %s, want the bash twin's 307dcc769135510a4f46243554b6d3b1.json", got)
	}
}

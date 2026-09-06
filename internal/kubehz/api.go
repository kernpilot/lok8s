package kubehz

// api.go — the two curl shapes the bash used against the platform api and
// the Hetzner Cloud api, over net/http:
//
//	curl -fsSL …                  → fetch: a non-2xx (or a transport error)
//	                                 is a failure, the body is discarded;
//	curl -sSL -w $'\n%{http_code}' → fetchStatus: status + body, so a
//	                                 refused call surfaces with the api's
//	                                 own message.
//
// Tokens go into the Authorization header and nowhere else: not into debug
// lines, not into errors. Every caller asserts https:// on its base URL
// before reaching here (http::require_https), so a bearer never travels in
// clear.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
)

type httpResult struct {
	Status int
	Body   []byte
}

// bearer wraps an Authorization value. The bash sent
// `Authorization: Bearer ${KUBEHZ_TOKEN:-}` — the header is present even
// when the token is empty — so the flag distinguishes "no header" from "an
// empty bearer".
type bearer struct {
	set   bool
	token string
}

func withBearer(token string) bearer { return bearer{set: true, token: token} }

// request performs one call and returns status + body; err only for a
// transport failure (bash: curl's own non-zero exit).
func (c *Context) request(ctx context.Context, method, url string, auth bearer, body []byte) (*httpResult, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if auth.set {
		req.Header.Set("Authorization", "Bearer "+auth.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &httpResult{Status: resp.StatusCode, Body: raw}, nil
}

// fetch is `curl -fsSL`: any status >= 400 is a failure (curl exit 22).
func (c *Context) fetch(ctx context.Context, method, url string, auth bearer, body []byte) ([]byte, error) {
	res, err := c.request(ctx, method, url, auth, body)
	if err != nil {
		return nil, err
	}
	if res.Status >= 400 {
		return nil, errHTTPStatus(res.Status)
	}
	return res.Body, nil
}

// fetchStatus is `curl -sSL -w $'\n%{http_code}'`.
func (c *Context) fetchStatus(ctx context.Context, method, url string, auth bearer, body []byte) (*httpResult, error) {
	return c.request(ctx, method, url, auth, body)
}

type errHTTPStatus int

func (e errHTTPStatus) Error() string { return "http status " + strconv.Itoa(int(e)) }

func is2xx(status int) bool { return status >= 200 && status < 300 }

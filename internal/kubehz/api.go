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
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type httpResult struct {
	Status int
	Body   []byte
	Header http.Header
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
	return &httpResult{Status: resp.StatusCode, Body: raw, Header: resp.Header}, nil
}

// fetch is `curl -fsSL`: any status >= 400 is a failure (curl exit 22),
// returned as an *apiError.
func (c *Context) fetch(ctx context.Context, method, url string, auth bearer, body []byte) ([]byte, error) {
	res, err := c.request(ctx, method, url, auth, body)
	if err != nil {
		return nil, err
	}
	if res.Status >= 400 {
		return nil, newAPIError(res)
	}
	return res.Body, nil
}

// fetchStatus is `curl -sSL -w $'\n%{http_code}'`.
func (c *Context) fetchStatus(ctx context.Context, method, url string, auth bearer, body []byte) (*httpResult, error) {
	return c.request(ctx, method, url, auth, body)
}

// apiError is a refused call (status >= 400). It keeps what the answer
// says about the refusal: the status, the problem code of the body and
// the Retry-After header. The text stays "http status <n>": the code and
// the wait come from the server, and only the callers that check them
// print them.
type apiError struct {
	Status int
	// Code is `.data.code // .code` of the body (the problem shape of the
	// api), or "" when the value does not look like a code (problemCodeRe).
	Code string
	// RetryAfter is the Retry-After header; HasRetryAfter tells whether the
	// answer carried one in the delta-seconds form (see retryAfter).
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func (e *apiError) Error() string { return "http status " + strconv.Itoa(e.Status) }

// problemCodeRe is the shape of an api problem code (REGISTRATION_UNAVAILABLE,
// RATE_LIMITED). The code comes from the server and goes to the terminal, so
// any other value (an escape sequence, a long text) is dropped.
var problemCodeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func newAPIError(res *httpResult) *apiError {
	e := &apiError{Status: res.Status}
	if code := apiCode(res.Body); problemCodeRe.MatchString(code) {
		e.Code = code
	}
	e.RetryAfter, e.HasRetryAfter = retryAfter(res.Header)
	return e
}

// retryAfter reads a Retry-After header in the delta-seconds form ("30"),
// the form the kubehz api sends. ok is false for no header and for any
// other form, an HTTP-date included: the bash twin reads the same form
// only, and lo then does not wait. More than retryAfterMaxDigits digits
// read as retryAfterClamp seconds, so a huge value cannot overflow. The
// caller caps the wait.
func retryAfter(h http.Header) (d time.Duration, ok bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" || strings.Trim(v, "0123456789") != "" {
		return 0, false
	}
	if len(v) > retryAfterMaxDigits {
		v = retryAfterClamp
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// The register retry bound (#312). A register call that gets 503 or 429
// with Retry-After tries again: at most registerTries calls in total, and
// one wait is at most registerMaxWait, so a call waits at most two minutes.
//
// The bound holds for each call, not for the whole registration. A mode
// whose last answer is a refusal lo does not try again (a 502 after two
// 503s, say) falls back to the next mode, and that mode can wait again. So
// the worst case is three modes with two waits each: six minutes of
// waiting, plus up to the 2-minute response-header timeout of httpClient
// for each of the nine requests (bash curl has no timeout). A shared budget
// would need state across the bash subshells of the frozen tree for a case
// where the platform changes its answer in the middle of a call.
//
// The bash twin (kubehz::register_post) holds the same values;
// TestRegisterRetryBoundMatchesBash pins them.
const (
	registerTries   = 3
	registerMaxWait = 60 * time.Second

	// retryAfterMaxDigits and retryAfterClamp: the overflow guard of
	// retryAfter.
	retryAfterMaxDigits = 6
	retryAfterClamp     = "999999"
)

// registerUnavailableError is a register call that still got 503 or 429 with
// Retry-After on its last try. The caller stops the registration there: it
// does not fall back to the next flow, and it names the recovery command.
type registerUnavailableError struct {
	Status int
	Code   string
	Tries  int
}

func (e *registerUnavailableError) Error() string {
	return "kubehz: the platform did not register the cluster: http status " + strconv.Itoa(e.Status) + optSuffix(" ", e.Code)
}

// registerRefusedError is a register call that got a refusal (status >= 400)
// that lo does not try again. The fall back warnings name its status and
// code; a transport failure, or a refusal from the Hetzner Cloud api, is not
// one, the same as in the bash twin.
type registerRefusedError struct{ *apiError }

func (e *registerRefusedError) Unwrap() error { return e.apiError }

// refusedSuffix is " (HTTP <status>[ <code>])" for a registerRefusedError,
// else "": the part of a fall back warning that names the refusal.
func refusedSuffix(err error) string {
	var r *registerRefusedError
	if !errors.As(err, &r) {
		return ""
	}
	return " (HTTP " + strconv.Itoa(r.Status) + optSuffix(" ", r.Code) + ")"
}

// unavailable reports a refusal the server asks to try again later: 503
// or 429 with a Retry-After header. A 503 without the header (a proxy in
// front of the api, for example) gives no time to wait, so it is not
// tried again and keeps the old failure path.
func (e *apiError) unavailable() bool {
	return (e.Status == http.StatusServiceUnavailable || e.Status == http.StatusTooManyRequests) && e.HasRetryAfter
}

// registerPost is POST <apiURL>/api/clusters/register with the bound above.
// A wait goes through the Sleep seam, so a cancelled context (Ctrl-C) ends
// it at once and the context error comes back. After the last try the
// error is *registerUnavailableError. Any other failure comes back as fetch
// returns it.
func (c *Context) registerPost(ctx context.Context, apiURL string, auth bearer, body []byte) ([]byte, error) {
	for try := 1; ; try++ {
		out, err := c.fetch(ctx, "POST", apiURL+"/api/clusters/register", auth, body)
		var ae *apiError
		if err == nil || !errors.As(err, &ae) {
			return out, err
		}
		if !ae.unavailable() {
			return nil, &registerRefusedError{ae}
		}
		if try >= registerTries {
			return nil, &registerUnavailableError{Status: ae.Status, Code: ae.Code, Tries: try}
		}
		wait := min(ae.RetryAfter, registerMaxWait)
		c.warnf("kubehz: the platform answered HTTP %d%s. Try %d of %d in %ds.",
			ae.Status, optSuffix(" ", ae.Code), try+1, registerTries, int(wait/time.Second))
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func is2xx(status int) bool { return status >= 200 && status < 300 }

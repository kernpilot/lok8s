package kubehz

// register_retry_test.go covers #312: a register call that gets 503 or 429
// with Retry-After tries again (at most registerTries calls, one wait at
// most registerMaxWait), and a platform that stays unavailable stops the
// registration with an error that names the code and the recovery command,
// instead of the fall back to the next flow. The api is the httptest TLS
// server of the harness; the waits go through a recording Sleep seam, so no
// test waits for real.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

const (
	unavailableBody = `{"statusCode":503,"data":{"code":"REGISTRATION_UNAVAILABLE","message":"The registration limiter is not available","retryAfterSeconds":30}}`
	rateLimitedBody = `{"statusCode":429,"data":{"code":"RATE_LIMITED","message":"Too many registration attempts"}}`
	registeredBody  = `{"id":"cl-001","registered":true,"claimed":true,"claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-test.kubehz.dev"}}`
)

// waitRecorder is a Sleep seam that records each wait and returns at once.
// onWait, when set, runs before the return (a test cancels there).
type waitRecorder struct {
	mu     sync.Mutex
	waits  []time.Duration
	onWait func()
}

func (w *waitRecorder) sleep(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.waits = append(w.waits, d)
	w.mu.Unlock()
	if w.onWait != nil {
		w.onWait()
	}
	return ctx.Err()
}

func (w *waitRecorder) got() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.waits)
}

// scriptRegister answers POST /api/clusters/register from a script: the
// n-th call gets answers[n] (the last answer repeats). An answer is a
// status, a Retry-After value ("" = no header) and a body.
type answer struct {
	status     int
	retryAfter string
	body       string
}

func scriptRegister(h *harness, answers ...answer) {
	var mu sync.Mutex
	n := 0
	h.handleFunc("POST /api/clusters/register", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		a := answers[min(n, len(answers)-1)]
		n++
		mu.Unlock()
		if a.retryAfter != "" {
			w.Header().Set("Retry-After", a.retryAfter)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	})
	// The claim key mode uploads the key to Hetzner Cloud after the register.
	h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[]}`)
	h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
}

// retryHarness is a harness with the recording Sleep seam and the given
// register mode: "legacy" (no token), "bearer" (KUBEHZ_TOKEN, the direct
// claim) or "claim-key" (HCLOUD_TOKEN).
func retryHarness(t *testing.T, mode string) (*harness, *waitRecorder) {
	t.Helper()
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	switch mode {
	case "bearer":
		h.env["KUBEHZ_TOKEN"] = "khzt_test"
	case "claim-key":
		h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
	}
	w := &waitRecorder{}
	h.ctx.Sleep = w.sleep
	return h, w
}

func registerCalls(h *harness) int {
	n := 0
	for _, r := range h.reqs() {
		if r.Method == "POST" && r.Path == "/api/clusters/register" {
			n++
		}
	}
	return n
}

func assertWaits(t *testing.T, w *waitRecorder, want ...time.Duration) {
	t.Helper()
	if got := w.got(); !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
}

// registeredLine is the success line of each mode for registeredBody.
var registeredLine = map[string]string{
	"legacy":    "kubehz: cluster 'test.kubehz.dev' registered (pending). Claim it in the dashboard:\n",
	"bearer":    "kubehz: cluster 'test.kubehz.dev' registered and claimed to your account (cl-001).\n",
	"claim-key": "kubehz: cluster 'test.kubehz.dev' registered (pending). Claim key 'kubehz-claim-test.kubehz.dev' uploaded to your Hetzner Cloud account.\n",
}

func TestRegisterRetriesA503WithRetryAfterThenSucceeds(t *testing.T) {
	for _, mode := range []string{"legacy", "bearer", "claim-key"} {
		t.Run(mode, func(t *testing.T) {
			h, w := retryHarness(t, mode)
			scriptRegister(h, answer{503, "30", unavailableBody}, answer{200, "", registeredBody})
			cfg := &Config{APIURL: h.apiURL(), Access: "registered"}
			mustOK(t, h.ctx.RegisterCluster(t.Context(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
			if n := registerCalls(h); n != 2 {
				t.Fatalf("register calls = %d, want 2 (one retry)", n)
			}
			assertWaits(t, w, 30*time.Second)
			mustContain(t, h.output(), "[warn] kubehz: the platform answered HTTP 503 REGISTRATION_UNAVAILABLE. Try 2 of 3 in 30s.")
			mustContain(t, h.out.String(), registeredLine[mode])
			mustNotContain(t, h.output(), "falling back")
			mustNotContain(t, h.output(), "[error]")
		})
	}
	// Both tries send the same body: the retry posts it again in full.
	h, _ := retryHarness(t, "legacy")
	scriptRegister(h, answer{503, "1", unavailableBody}, answer{200, "", registeredBody})
	mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
	var bodies []string
	for _, r := range h.reqs() {
		if r.Path == "/api/clusters/register" {
			bodies = append(bodies, r.Body)
		}
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || !strings.Contains(bodies[1], `"fingerprint":"lo:test.kubehz.dev"`) {
		t.Fatalf("retry bodies = %q, want the same full body twice", bodies)
	}
}

// After the bound the registration stops: no fall back to the next flow
// (each mode would post to the same endpoint and get the same answer), and
// the [error] line names the status, the code, the tries and the command.
func TestRegisterStopsWhenStillUnavailableAfterTheBound(t *testing.T) {
	for _, mode := range []string{"legacy", "bearer", "claim-key"} {
		t.Run(mode, func(t *testing.T) {
			h, w := retryHarness(t, mode)
			// bearer and claim key both set: a fall back would reach the
			// claim key or the legacy announce, and add calls.
			if mode == "bearer" {
				h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
			}
			scriptRegister(h, answer{503, "30", unavailableBody})
			err := h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h))
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("err = %v, want a handled error (the registration must not look like a success)", err)
			}
			if n := registerCalls(h); n != registerTries {
				t.Fatalf("register calls = %d, want %d (the bound, no fall back)", n, registerTries)
			}
			assertWaits(t, w, 30*time.Second, 30*time.Second)
			mustContain(t, h.errOut.String(), "[error] kubehz: the platform did not register test.kubehz.dev (HTTP 503 REGISTRATION_UNAVAILABLE, 3 tries). Run 'lo kubehz register --domain test.kubehz.dev' when the platform is available again.\n")
			mustNotContain(t, h.output(), "falling back")
			mustNotContain(t, h.output(), "continue without kubehz integration")
			if h.anyReq("POST", "/v1/ssh_keys") {
				t.Fatal("no claim key may be uploaded for a cluster the platform did not register")
			}
		})
	}
}

func TestRegisterRetriesA429WithRetryAfter(t *testing.T) {
	h, w := retryHarness(t, "legacy")
	scriptRegister(h, answer{429, "5", rateLimitedBody}, answer{200, "", registeredBody})
	mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
	if n := registerCalls(h); n != 2 {
		t.Fatalf("register calls = %d, want 2", n)
	}
	assertWaits(t, w, 5*time.Second)
	mustContain(t, h.output(), "HTTP 429 RATE_LIMITED. Try 2 of 3 in 5s.")

	// A 429 that stays: the same stop, with the 429 code.
	h, _ = retryHarness(t, "legacy")
	scriptRegister(h, answer{429, "5", rateLimitedBody})
	mustErr(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)))
	mustContain(t, h.output(), "(HTTP 429 RATE_LIMITED, 3 tries)")
}

// ctxBlindTransport sends a request even when its context has ended: with
// it, only registerPost's own stop keeps a request from going out after a
// cancel (the default transport would refuse it and hide a loop that goes
// on after the wait).
type ctxBlindTransport struct{ rt http.RoundTripper }

func (t ctxBlindTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.rt.RoundTrip(r.WithContext(context.Background()))
}

// Ctrl-C cancels the command context: the wait ends at once, nothing is
// tried again, and nothing falls back. The context error comes back (the
// cli maps it to the signal exit and prints nothing more).
func TestRegisterCancelDuringTheWait(t *testing.T) {
	for _, mode := range []string{"legacy", "bearer"} {
		t.Run(mode, func(t *testing.T) {
			h, w := retryHarness(t, mode)
			h.ctx.HTTP = &http.Client{Transport: ctxBlindTransport{h.srv.Client().Transport}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w.onWait = cancel
			scriptRegister(h, answer{503, "30", unavailableBody}, answer{200, "", registeredBody})
			err := h.ctx.RegisterCluster(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if n := registerCalls(h); n != 1 {
				t.Fatalf("register calls = %d, want 1 (no retry and no fall back after a cancel)", n)
			}
			assertWaits(t, w, 30*time.Second)
			mustNotContain(t, h.output(), "falling back")
			mustNotContain(t, h.output(), "continue without kubehz integration")
		})
	}
}

// A 503 without Retry-After is not tried again: the server gave no time to
// wait (it is often a proxy in front of the api, not the api itself). It
// keeps the old path: the fall back, and in the end the warning that the
// cluster continues without kubehz.
func TestRegister503WithoutRetryAfterIsNotRetried(t *testing.T) {
	h, w := retryHarness(t, "bearer")
	scriptRegister(h, answer{503, "", `upstream connect error`})
	mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
	if n := registerCalls(h); n != 2 {
		t.Fatalf("register calls = %d, want 2 (direct claim, then the legacy announce, one try each)", n)
	}
	assertWaits(t, w)
	// The fall back warnings name the refusal (status and code).
	mustContain(t, h.errOut.String(), "[warn] kubehz direct claim failed for test.kubehz.dev (HTTP 503); falling back to the anonymous registration flow\n")
	mustContain(t, h.errOut.String(), "[warn] kubehz API request failed for test.kubehz.dev (HTTP 503), cluster will continue without kubehz integration\n")
	mustNotContain(t, h.output(), "[error]")
}

// A refusal with a code names both; the claim key mode names it too, and a
// transport failure names nothing.
func TestRegisterFallBackWarningsNameTheRefusal(t *testing.T) {
	h, _ := retryHarness(t, "claim-key")
	scriptRegister(h, answer{400, "", `{"data":{"code":"BAD_REQUEST"}}`})
	mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
	mustContain(t, h.errOut.String(), "[warn] kubehz claim-key registration failed for test.kubehz.dev (HTTP 400 BAD_REQUEST); falling back to the legacy fingerprint announce\n")
	mustContain(t, h.errOut.String(), "[warn] kubehz API request failed for test.kubehz.dev (HTTP 400 BAD_REQUEST), cluster will continue without kubehz integration\n")

	// A refusal from the Hetzner Cloud api (the key upload) is no register
	// refusal: the bash twin cannot see its status, so neither names it.
	h, _ = retryHarness(t, "claim-key")
	scriptRegister(h, answer{200, "", registeredBody})
	h.handle("POST /v1/ssh_keys", 403, `{"error":{"code":"forbidden"}}`)
	mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
	mustContain(t, h.errOut.String(), "[warn] kubehz claim-key registration failed for test.kubehz.dev; falling back to the legacy fingerprint announce\n")

	// A transport failure has no answer to name.
	h, _ = retryHarness(t, "legacy")
	h.srv.Close()
	mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
	mustContain(t, h.errOut.String(), "[warn] kubehz API request failed for test.kubehz.dev, cluster will continue without kubehz integration\n")
}

// The bash twin (kubehz::register_post in the embedded tree) holds the same
// bound, cap, overflow guard and code shape as the Go constants.
func TestRegisterRetryBoundMatchesBash(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "libs", "kubehz", "main"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "kubehz::register_post() (")
	if start < 0 {
		t.Fatal("kubehz::register_post not found in libs/kubehz/main")
	}
	body := src[start:]
	body = body[:strings.Index(body, "\n)\n")]
	find := func(re string) string {
		t.Helper()
		m := regexp.MustCompile(re).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s not found in kubehz::register_post", re)
		}
		return m[1]
	}
	for _, c := range []struct{ name, bash, goVal string }{
		{"tries", find(`local -r tries=(\d+) `), strconv.Itoa(registerTries)},
		{"max_wait", find(`max_wait=(\d+)\n`), strconv.Itoa(int(registerMaxWait / time.Second))},
		{"max digits", find(`\(\( \$\{#ra\} <= (\d+) \)\)`), strconv.Itoa(retryAfterMaxDigits)},
		{"clamp", find(`\|\| ra=(\d+)\n`), retryAfterClamp},
		{"code shape", find(`\[\[ "\$\{code\}" =~ (\S+) \]\]`), problemCodeRe.String()},
	} {
		if c.bash != c.goVal {
			t.Errorf("%s: bash %q != Go %q", c.name, c.bash, c.goVal)
		}
	}
}

// A huge Retry-After is capped at registerMaxWait for each wait, so a call
// waits at most two minutes; a value that is not delta-seconds reads as no
// header.
func TestRegisterRetryAfterIsCapped(t *testing.T) {
	h, w := retryHarness(t, "legacy")
	scriptRegister(h, answer{503, "86400", unavailableBody})
	mustErr(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)))
	assertWaits(t, w, registerMaxWait, registerMaxWait)
	mustContain(t, h.output(), "Try 2 of 3 in 60s.")

	for _, tc := range []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"30", 30 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		{"0", 0, true},
		{"99999999999999999999999", 999999 * time.Second, true},
		{"", 0, false},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0, false},
	} {
		hdr := http.Header{}
		if tc.header != "" {
			hdr.Set("Retry-After", tc.header)
		}
		got, ok := retryAfter(hdr)
		if got != tc.want || ok != tc.ok {
			t.Errorf("retryAfter(%q) = %v, %v; want %v, %v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

// The code comes from the server and goes to the terminal: a value that is
// not a code shape is dropped, the status stays.
func TestRegisterStopDropsAHostileCode(t *testing.T) {
	h, _ := retryHarness(t, "legacy")
	scriptRegister(h, answer{503, "1", `{"data":{"code":"\u001b[2JPWNED"}}`})
	mustErr(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)))
	mustContain(t, h.output(), "(HTTP 503, 3 tries)")
	mustNotContain(t, h.output(), "\x1b")
	mustNotContain(t, h.output(), "PWNED")
}

// On a terminal the error and the recovery command are two lines (the
// internal/ui error shape); piped, one [error] line carries both (above).
func TestRegisterStopOnATerminalPrintsANextLine(t *testing.T) {
	t.Cleanup(ui.ForceTTY(true))
	t.Cleanup(ui.ForceColor(false))
	h, _ := retryHarness(t, "legacy")
	scriptRegister(h, answer{503, "30", unavailableBody})
	mustErr(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)))
	want := "error: kubehz: the platform did not register test.kubehz.dev (HTTP 503 REGISTRATION_UNAVAILABLE, 3 tries).\n" +
		"next: lo kubehz register --domain test.kubehz.dev   # when the platform is available again\n"
	mustContain(t, h.errOut.String(), want)
	mustNotContain(t, h.errOut.String(), "[error]")
}

// The provision hook stops the dispatch before its bootstrap, so its
// recovery command is `lo provision --bootstrap`: that registers and then
// bootstraps.
func TestRegisterHookNamesProvisionBootstrap(t *testing.T) {
	h, _ := retryHarness(t, "legacy")
	scriptRegister(h, answer{503, "30", unavailableBody})
	spec := h.writeSpec("reg.dev", "kind: Lo\nspec:\n  cluster:\n    domain: reg.dev\n  kubehz:\n    access: registered\n    apiUrl: "+h.apiURL()+"\n")
	err := h.ctx.RegisterHook()(t.Context(), "reg.dev", spec)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want a handled error that stops the dispatch", err)
	}
	mustContain(t, h.output(), "Run 'lo provision --bootstrap --domain reg.dev' when the platform is available again.")
}

package toolchain

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// B410: the b download is retried on a transient failure, and only then.

// recordSleeps replaces the backoff wait with a recorder, so the tests run
// instantly and can assert the waits.
func recordSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	saved := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = saved })
	return &waits
}

// timeoutErr is a net.Error that reports a timeout, like a dial i/o timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// flakyTransport fails the first `fail` round trips with a dial timeout,
// then hands the request to next.
type flakyTransport struct {
	fail  int32
	calls atomic.Int32
	next  http.RoundTripper
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.calls.Add(1) <= f.fail {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
	}
	return f.next.RoundTrip(r)
}

// bootstrapOnce runs Bootstrap against baseURL with the given client and
// returns the stderr text, the bin dir, the runner and Bootstrap's error.
func bootstrapOnce(t *testing.T, client *http.Client, baseURL, sum string) (string, string, *recRunner, error) {
	t.Helper()
	rel := &Release{Version: "9.9.9", BaseURL: baseURL, Assets: map[string]string{"b-linux-amd64.tar.gz": sum}}
	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	var out, errOut bytes.Buffer
	r := &recRunner{}
	err := Bootstrap(t.Context(), BootstrapOptions{
		Base: base, Bin: bin, Out: &out, Stderr: &errOut, Runner: r, Client: client, Release: rel, GOOS: "linux", GOARCH: "amd64",
	})
	return errOut.String(), bin, r, err
}

func assertNoLeftovers(t *testing.T, bin string) {
	t.Helper()
	if leftovers, _ := filepath.Glob(filepath.Join(bin, ".b-download-*")); len(leftovers) != 0 {
		t.Fatalf("temp download left behind: %v", leftovers)
	}
}

// The crowd's case (B410): two dial timeouts, then the release answers.
func TestBootstrapRetriesATransientDialFailure(t *testing.T) {
	waits := recordSleeps(t)
	tb := fakeTarball(t, map[string]string{"b": "#!/bin/sh\n"})
	var hits int32
	srv := server(t, "b-linux-amd64.tar.gz", tb, &hits)
	flaky := &flakyTransport{fail: 2, next: srv.Client().Transport}
	client := &http.Client{Transport: flaky, CheckRedirect: refuseNonHTTPSRedirect}

	stderr, bin, r, err := bootstrapOnce(t, client, srv.URL, sha256hex(tb))
	if err != nil {
		t.Fatalf("Bootstrap after two transient failures: %v\n%s", err, stderr)
	}
	if got := flaky.calls.Load(); got != 3 {
		t.Fatalf("round trips = %d, want 3 (2 failed + 1 ok)", got)
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second}; !slices.Equal(*waits, want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	for _, line := range []string{"retrying in 2s (2/4)", "retrying in 4s (3/4)", "i/o timeout"} {
		if !strings.Contains(stderr, line) {
			t.Fatalf("stderr lacks %q:\n%s", line, stderr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(bin, "b")); statErr != nil || len(r.cmds) != 1 {
		t.Fatalf("b not installed after the retry: %v / %+v", statErr, r.cmds)
	}
	assertNoLeftovers(t, bin)
}

// statusServer answers the asset with the given status codes in order,
// then with the body.
func statusServer(t *testing.T, codes []int, body []byte, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(hits, 1))
		if n <= len(codes) {
			w.WriteHeader(codes[n-1])
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBootstrapRetries500And503And429(t *testing.T) {
	waits := recordSleeps(t)
	tb := fakeTarball(t, map[string]string{"b": "#!/bin/sh\n"})
	var hits int32
	srv := statusServer(t, []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusTooManyRequests}, tb, &hits)
	stderr, bin, _, err := bootstrapOnce(t, srv.Client(), srv.URL, sha256hex(tb))
	if err != nil {
		t.Fatalf("Bootstrap after 500 + 503 + 429: %v\n%s", err, stderr)
	}
	if atomic.LoadInt32(&hits) != 4 || len(*waits) != 3 {
		t.Fatalf("hits = %d, waits = %v; want 4 and three waits", hits, *waits)
	}
	if !strings.Contains(stderr, "HTTP 500") || !strings.Contains(stderr, "HTTP 503") || !strings.Contains(stderr, "HTTP 429") {
		t.Fatalf("stderr lacks the statuses:\n%s", stderr)
	}
	assertNoLeftovers(t, bin)
}

// A body that breaks off mid-stream is retried, and the partial file goes.
func TestBootstrapRetriesABodyThatBreaksOff(t *testing.T) {
	recordSleeps(t)
	tb := fakeTarball(t, map[string]string{"b": "#!/bin/sh\n" + strings.Repeat("x", 4096)})
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(tb)))
			_, _ = w.Write(tb[:len(tb)/2]) // the server closes the connection short
			return
		}
		_, _ = w.Write(tb)
	}))
	t.Cleanup(srv.Close)
	stderr, bin, _, err := bootstrapOnce(t, srv.Client(), srv.URL, sha256hex(tb))
	if err != nil {
		t.Fatalf("Bootstrap after a broken body: %v\n%s", err, stderr)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
	if !strings.Contains(stderr, "retrying in 2s (2/4)") {
		t.Fatalf("no retry line:\n%s", stderr)
	}
	assertNoLeftovers(t, bin)
}

// Four attempts, three waits (2, 4, 8 s), then the last error with the count.
func TestBootstrapGivesUpAfterFourAttempts(t *testing.T) {
	waits := recordSleeps(t)
	var hits int32
	srv := statusServer(t, []int{502, 502, 502, 502, 502}, nil, &hits)
	stderr, bin, r, err := bootstrapOnce(t, srv.Client(), srv.URL, strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "gave up after 4 attempts") {
		t.Fatalf("err = %v", err)
	}
	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("hits = %d, want 4", hits)
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(*waits, want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	if strings.Count(stderr, "retrying in") != 3 {
		t.Fatalf("want 3 retry lines:\n%s", stderr)
	}
	if _, statErr := os.Stat(filepath.Join(bin, "b")); statErr == nil || len(r.cmds) != 0 {
		t.Fatal("b installed or b install ran after the download failed")
	}
	assertNoLeftovers(t, bin)
}

// A 404 is a wrong pin, not a blip: one request, no wait.
func TestBootstrapDoesNotRetryA404(t *testing.T) {
	waits := recordSleeps(t)
	var hits int32
	srv := statusServer(t, []int{http.StatusNotFound}, nil, &hits)
	stderr, _, _, err := bootstrapOnce(t, srv.Client(), srv.URL, strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "gave up") {
		t.Fatalf("err = %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 || len(*waits) != 0 || stderr != "" {
		t.Fatalf("a 404 was retried: hits = %d, waits = %v, stderr = %q", hits, *waits, stderr)
	}
}

// A checksum mismatch is never retried: the download arrived whole.
func TestBootstrapDoesNotRetryAChecksumMismatch(t *testing.T) {
	waits := recordSleeps(t)
	tb := fakeTarball(t, map[string]string{"b": "#!/bin/sh\n"})
	var hits int32
	srv := server(t, "b-linux-amd64.tar.gz", tb, &hits)
	_, bin, _, err := bootstrapOnce(t, srv.Client(), srv.URL, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "checksum MISMATCH") {
		t.Fatalf("err = %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 || len(*waits) != 0 {
		t.Fatalf("a checksum mismatch was retried: hits = %d, waits = %v", hits, *waits)
	}
	assertNoLeftovers(t, bin)
}

// An oversized download is refused once, not retried, and leaves no file.
func TestBootstrapDoesNotRetryAnOversizedDownload(t *testing.T) {
	waits := recordSleeps(t)
	saved := maxTarballBytes
	maxTarballBytes = 16
	t.Cleanup(func() { maxTarballBytes = saved })
	var hits int32
	srv := statusServer(t, nil, bytes.Repeat([]byte("x"), 64), &hits)
	_, bin, _, err := bootstrapOnce(t, srv.Client(), srv.URL, strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "larger than 16 bytes") {
		t.Fatalf("err = %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 || len(*waits) != 0 {
		t.Fatalf("an oversized download was retried: hits = %d, waits = %v", hits, *waits)
	}
	assertNoLeftovers(t, bin)
}

// A server whose certificate the client does not trust: the handshake
// fails the same way every time, so it is not retried.
func TestBootstrapDoesNotRetryAnUntrustedCertificate(t *testing.T) {
	waits := recordSleeps(t)
	var hits int32
	srv := statusServer(t, nil, []byte("x"), &hits)
	client := &http.Client{CheckRedirect: refuseNonHTTPSRedirect} // no test CA
	_, _, _, err := bootstrapOnce(t, client, srv.URL, strings.Repeat("a", 64))
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) {
		t.Fatalf("err = %v, want a certificate verification error", err)
	}
	if len(*waits) != 0 || atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("an untrusted certificate was retried: waits = %v, hits = %d", *waits, hits)
	}
}

// A cancelled caller stops the retries at once.
func TestBootstrapStopsRetryingWhenCancelled(t *testing.T) {
	tb := fakeTarball(t, map[string]string{"b": "#!/bin/sh\n"})
	var hits int32
	srv := server(t, "b-linux-amd64.tar.gz", tb, &hits)
	flaky := &flakyTransport{fail: 10, next: srv.Client().Transport}
	ctx, cancel := context.WithCancel(t.Context())
	saved := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		cancel() // the user pressed ^C during the first wait
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = saved })
	rel := &Release{Version: "9.9.9", BaseURL: srv.URL, Assets: map[string]string{"b-linux-amd64.tar.gz": sha256hex(tb)}}
	base := t.TempDir()
	err := Bootstrap(ctx, BootstrapOptions{
		Base: base, Bin: filepath.Join(base, ".bin"), Out: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Runner: &recRunner{},
		Client: &http.Client{Transport: flaky}, Release: rel, GOOS: "linux", GOARCH: "amd64",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := flaky.calls.Load(); got != 1 {
		t.Fatalf("round trips = %d, want 1 (no attempt after the cancel)", got)
	}
}

// The redirect refusals and a certificate failure are not transient: the
// server would answer the same way again.
func TestTransientTransportClassification(t *testing.T) {
	ctx := t.Context()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"dial timeout":       {&net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, true},
		"connection reset":   {errors.New("read: connection reset by peer"), true},
		"redirect refused":   {errRedirectRefused, false},
		"too many redirects": {errTooManyRedirects, false},
		"unknown authority":  {&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false},
		"wrong hostname":     {&tls.CertificateVerificationError{Err: x509.HostnameError{Host: "example.invalid"}}, false},
	} {
		if got := transientTransport(ctx, tc.err); got != tc.want {
			t.Errorf("%s: transientTransport = %v, want %v", name, got, tc.want)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if transientTransport(cancelled, &net.OpError{Op: "dial", Err: timeoutErr{}}) {
		t.Error("a cancelled caller's error counted as transient")
	}
}

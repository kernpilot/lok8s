package toolchain

// installer_retry_test.go: install/lo-install.sh retries a transient
// download failure and nothing else. The test runs the real script with the
// real curl against a TLS test server that plays a GitHub release. The
// server fails the first requests to a path with the status codes or the
// broken connections a test gives, then serves the file. The bats suite
// (tests/unit/lo_install_test.bats) uses a file:// release, which cannot
// fail transiently.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/testutil"
)

const fixtureTag = "v9.9.9"

// Broken connections for fail[path], next to the HTTP status codes there.
const (
	faultEmptyReply = -1 // close the connection before a response
	faultShortBody  = -2 // promise the whole body, send half, then close
	faultReset      = -3 // send half the body, then reset the connection
)

// fakeRelease serves releases/latest (a redirect to the tag), the tag page
// and the release files. fail[path] holds the status codes or the faults of
// the first requests to path; hits counts every request.
type fakeRelease struct {
	mu    sync.Mutex
	hits  map[string]int
	fail  map[string][]int
	files map[string][]byte
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.URL.Path]++
	n := f.hits[r.URL.Path]
	codes := f.fail[r.URL.Path]
	body, ok := f.files[r.URL.Path]
	f.mu.Unlock()
	switch {
	case n <= len(codes) && codes[n-1] < 0:
		breakConnection(w, codes[n-1], body)
	case n <= len(codes):
		w.WriteHeader(codes[n-1])
	case r.URL.Path == "/releases/latest":
		http.Redirect(w, r, "/releases/tag/"+fixtureTag, http.StatusFound)
	case r.URL.Path == "/releases/tag/"+fixtureTag:
		w.WriteHeader(http.StatusOK)
	case ok:
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

// breakConnection takes the connection from the server and breaks it the
// way fault says. A failed takeover answers 418, which no test retries.
func breakConnection(w http.ResponseWriter, fault int, body []byte) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
		return
	}
	if fault != faultEmptyReply {
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(body))
		_, _ = buf.Write(body[:len(body)/2])
		_ = buf.Flush()
	}
	if tc, ok := conn.(*tls.Conn); ok && fault == faultReset {
		// SO_LINGER 0: the close sends a TCP RST, not a TLS close_notify.
		if raw, ok := tc.NetConn().(*net.TCPConn); ok {
			_ = raw.SetLinger(0)
			_ = raw.Close()
			return
		}
	}
	_ = conn.Close()
}

func (f *fakeRelease) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// installerFixture is one fake release and the paths a run uses.
type installerFixture struct {
	release   *fakeRelease
	assetPath string // the archive's URL path
	sumsPath  string // checksums.txt's URL path
	url       string // the server base URL
	caFile    string // the server certificate, for CURL_CA_BUNDLE
	home      string
	dir       string // --dir
	pathDir   string // put first on PATH, if set
}

// newInstallerFixture starts the server with a core archive for this host
// (a `lo` that prints a fixed version) and its checksums.txt.
func newInstallerFixture(t *testing.T) *installerFixture {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("the installer supports linux and darwin, not %s", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("the installer supports amd64 and arm64, not %s", runtime.GOARCH)
	}
	asset := fmt.Sprintf("lo-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := fixtureArchive(t, "lo", "#!/bin/sh\necho \"lo version "+fixtureTag+" (fixture)\"\n")
	sum := sha256.Sum256(archive)
	base := "/releases/download/" + fixtureTag + "/"
	f := &installerFixture{
		release: &fakeRelease{
			hits: map[string]int{},
			fail: map[string][]int{},
			files: map[string][]byte{
				base + asset:           archive,
				base + "checksums.txt": []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n"),
			},
		},
		assetPath: base + asset,
		sumsPath:  base + "checksums.txt",
		home:      t.TempDir(),
	}
	f.dir = filepath.Join(f.home, "bin")
	srv := httptest.NewTLSServer(f.release)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	f.caFile = filepath.Join(f.home, "ca.pem")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(f.caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// fixtureArchive is a tar.gz with one executable member.
func fixtureArchive(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// run executes the installer with a clean environment: no host curlrc,
// proxy or LO_* variable reaches it.
func (f *installerFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	script := filepath.Join(testutil.RepoRoot(t), "install", "lo-install.sh")
	cmd := exec.CommandContext(ctx, "bash", append([]string{script, "--dir", f.dir}, args...)...)
	cmd.Dir = f.home
	path := os.Getenv("PATH")
	if f.pathDir != "" {
		path = f.pathDir + string(os.PathListSeparator) + path
	}
	cmd.Env = []string{
		"PATH=" + path,
		"HOME=" + f.home,
		"CURL_HOME=" + f.home,
		"XDG_CONFIG_HOME=" + f.home,
		"CURL_CA_BUNDLE=" + f.caFile,
		"LO_INSTALL_BASE_URL=" + f.url,
		"LO_INSTALL_REPO=fixture/lok8s",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallerRetriesTransientFailures(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	// One failure of each transient kind, on each of the three requests.
	f.release.fail["/releases/latest"] = []int{http.StatusBadGateway}
	f.release.fail[f.assetPath] = []int{http.StatusServiceUnavailable, http.StatusInternalServerError}
	f.release.fail[f.sumsPath] = []int{http.StatusTooManyRequests}

	out, err := f.run(t) // no --version: the latest tag is resolved too
	if err != nil {
		t.Fatalf("the installer failed on transient errors (want: retried, then installed): %v\n%s", err, out)
	}
	for path, want := range map[string]int{"/releases/latest": 2, f.assetPath: 3, f.sumsPath: 2} {
		if got := f.release.count(path); got != want {
			t.Errorf("%s: %d requests, want %d (each failure retried once)", path, got, want)
		}
	}
	if got := strings.Count(out, "retrying "); got != 4 {
		t.Errorf("%d retry lines, want 4 (one for each retry):\n%s", got, out)
	}
	f.checkInstalled(t, out)
}

// The transport errors that curl's own --retry does not retry: an empty
// reply (curl exit 52), a short body (18) and a connection reset (56).
func TestInstallerRetriesBrokenConnections(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	f.release.fail["/releases/latest"] = []int{faultEmptyReply}
	f.release.fail[f.assetPath] = []int{faultShortBody, faultReset}
	f.release.fail[f.sumsPath] = []int{faultReset}

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("the installer failed on a broken connection (want: retried, then installed): %v\n%s", err, out)
	}
	for path, want := range map[string]int{"/releases/latest": 2, f.assetPath: 3, f.sumsPath: 2} {
		if got := f.release.count(path); got != want {
			t.Errorf("%s: %d requests, want %d (each failure retried once)", path, got, want)
		}
	}
	f.checkInstalled(t, out)
}

// checkInstalled fails the test unless the fixture lo is installed and runs.
func (f *installerFixture) checkInstalled(t *testing.T, out string) {
	t.Helper()
	got, err := exec.Command(filepath.Join(f.dir, "lo"), "--version").Output()
	if err != nil {
		t.Fatalf("installed lo does not run: %v\n%s", err, out)
	}
	if want := "lo version " + fixtureTag + " (fixture)"; strings.TrimSpace(string(got)) != want {
		t.Errorf("installed lo prints %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

// A refused connection (curl exit 7) is retried. B410 was a connect that
// failed on a new machine.
func TestInstallerRetriesARefusedConnection(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.url = "https://" + l.Addr().String()
	_ = l.Close() // nothing listens on the port now

	out, err := f.run(t, "--version", fixtureTag)
	if err == nil {
		t.Fatalf("the installer passed without a server:\n%s", out)
	}
	if got := strings.Count(out, "curl: (7)"); got != 4 {
		t.Errorf("%d refused connects, want 4 (1 attempt and 3 retries):\n%s", got, out)
	}
	if !strings.Contains(out, "(4/4)") || !strings.Contains(out, "download failed") {
		t.Errorf("output does not show the last retry and the failure:\n%s", out)
	}
}

// Each attempt has a time limit: --connect-timeout ends a connect that gets
// no answer, and --speed-limit with --speed-time ends a transfer that
// stalls. Without them, curl waits for the kernel (about 2 minutes on
// Linux) or forever. Both take too long to cause here, so a curl wrapper
// records the arguments of each call and then runs the real curl.
func TestInstallerLimitsEachAttempt(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	f.pathDir = filepath.Join(f.home, "wrap")
	argsLog := filepath.Join(f.home, "curl-args.log")
	wrapper := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\nexec '%s' \"$@\"\n", argsLog, curl)
	if err := os.MkdirAll(f.pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.pathDir, "curl"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := f.run(t) // the latest tag, the archive and checksums.txt
	if err != nil {
		t.Fatalf("the installer failed: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(calls) != 3 {
		t.Fatalf("%d curl calls, want 3:\n%s", len(calls), logged)
	}
	for _, call := range calls {
		for _, limit := range []string{" --connect-timeout 30 ", " --speed-limit 1 --speed-time 60 "} {
			if !strings.Contains(" "+call+" ", limit) {
				t.Errorf("curl call without%s: %s", limit, call)
			}
		}
	}
}

func TestInstallerGivesUpAfterFourAttempts(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	f.release.fail[f.assetPath] = []int{503, 503, 503, 503, 503, 503, 503, 503}

	out, err := f.run(t, "--version", fixtureTag)
	if err == nil {
		t.Fatalf("the installer passed on a release that answers 503 every time:\n%s", out)
	}
	if got := f.release.count(f.assetPath); got != 4 {
		t.Errorf("%s: %d requests, want 4 (1 attempt and 3 retries)", f.assetPath, got)
	}
	if got := strings.Count(out, "retrying "); got != 3 {
		t.Errorf("%d retry lines, want 3:\n%s", got, out)
	}
	if !strings.Contains(out, "download failed") {
		t.Errorf("output does not say the download failed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "lo")); err == nil {
		t.Error("lo was installed after a failed download")
	}
}

// A timeout (curl exit 28) and no DNS answer (6) are retried, after 1 s and
// then 2 s. A real timeout takes 30 s and a real DNS failure needs a
// resolver, so a curl wrapper fails the first two calls with these codes and
// then runs the real curl.
func TestInstallerRetriesATimeoutAndNoDNSAnswer(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	f.pathDir = filepath.Join(f.home, "wrap")
	counter := filepath.Join(f.home, "curl-calls")
	wrapper := fmt.Sprintf(`#!/bin/sh
n=$(( $(cat '%[1]s' 2>/dev/null || echo 0) + 1 ))
echo "${n}" > '%[1]s'
case "${n}" in
  1) echo 'curl: (28) fake timeout' >&2; exit 28 ;;
  2) echo 'curl: (6) fake DNS failure' >&2; exit 6 ;;
esac
exec '%[2]s' "$@"
`, counter, curl)
	if err := os.MkdirAll(f.pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.pathDir, "curl"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := f.run(t) // the first call resolves the latest tag
	if err != nil {
		t.Fatalf("the installer failed on a timeout and a DNS failure (want: retried, then installed): %v\n%s", err, out)
	}
	if got := f.release.count("/releases/latest"); got != 1 {
		t.Errorf("/releases/latest: %d requests reached the server, want 1 (the two failed calls never left the wrapper)", got)
	}
	for _, line := range []string{"in 1 s (2/4)", "in 2 s (3/4)"} {
		if !strings.Contains(out, line) {
			t.Errorf("output has no retry line %q (the delay doubles):\n%s", line, out)
		}
	}
	f.checkInstalled(t, out)
}

func TestInstallerDoesNotRetryANotFound(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	delete(f.release.files, f.assetPath) // a tag without a build for this host

	out, err := f.run(t, "--version", fixtureTag)
	if err == nil {
		t.Fatalf("the installer passed without its archive:\n%s", out)
	}
	if got := f.release.count(f.assetPath); got != 1 {
		t.Errorf("%s: %d requests, want 1 (a 404 is not transient)", f.assetPath, got)
	}
	if !strings.Contains(out, "download failed") {
		t.Errorf("output does not say the download failed:\n%s", out)
	}
}

// A TLS certificate error (curl exit 60) is final: the server sends the same
// certificate again. The test certificate does not name localhost.
func TestInstallerDoesNotRetryACertificateError(t *testing.T) {
	t.Parallel()
	f := newInstallerFixture(t)
	f.url = strings.Replace(f.url, "127.0.0.1", "localhost", 1)

	out, err := f.run(t, "--version", fixtureTag)
	if err == nil {
		t.Fatalf("the installer passed with a certificate for another host:\n%s", out)
	}
	if got := strings.Count(out, "curl: (60)"); got != 1 {
		t.Errorf("%d certificate errors, want 1 (not retried):\n%s", got, out)
	}
	if strings.Contains(out, "retrying ") {
		t.Errorf("the installer retried a certificate error:\n%s", out)
	}
}

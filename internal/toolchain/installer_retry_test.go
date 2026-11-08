package toolchain

// installer_retry_test.go: install/lo-install.sh retries a transient
// download failure and nothing else. The test runs the real script with the
// real curl against a TLS test server that plays a GitHub release. The
// server fails the first requests to a path with the status codes a test
// gives, then serves the file. The bats suite (tests/unit/lo_install_test.bats)
// uses a file:// release, which cannot fail transiently.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
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

// fakeRelease serves releases/latest (a redirect to the tag), the tag page
// and the release files. fail[path] holds the status codes of the first
// requests to path; hits counts every request.
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
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
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
	got, err := exec.Command(filepath.Join(f.dir, "lo"), "--version").Output()
	if err != nil {
		t.Fatalf("installed lo does not run: %v\n%s", err, out)
	}
	if want := "lo version " + fixtureTag + " (fixture)"; strings.TrimSpace(string(got)) != want {
		t.Errorf("installed lo prints %q, want %q", strings.TrimSpace(string(got)), want)
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
	if !strings.Contains(out, "download failed") {
		t.Errorf("output does not say the download failed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "lo")); err == nil {
		t.Error("lo was installed after a failed download")
	}
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

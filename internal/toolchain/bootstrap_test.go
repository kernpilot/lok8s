package toolchain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

// fakeTarball builds a b-style release tarball: LICENSE + a `b` script.
func fakeTarball(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range members {
		mode := int64(0o644)
		if name == "b" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type recRunner struct {
	cmds []execx.Cmd
}

func (r *recRunner) Run(_ context.Context, c execx.Cmd) error {
	r.cmds = append(r.cmds, c)
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte("b: installed\n"))
	}
	return nil
}

// server serves one asset; hits counts requests.
func server(t *testing.T, asset string, body []byte, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if strings.HasSuffix(r.URL.Path, "/"+asset) {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBootstrapDownloadsVerifiesExtractsAndInstalls(t *testing.T) {
	tb := fakeTarball(t, map[string]string{"LICENSE": "MIT", "b": "#!/bin/sh\necho v9.9.9\n"})
	var hits int32
	srv := server(t, "b-linux-amd64.tar.gz", tb, &hits)
	rel := &Release{Version: "9.9.9", BaseURL: srv.URL, Assets: map[string]string{"b-linux-amd64.tar.gz": sha256hex(tb)}}

	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	var out bytes.Buffer
	r := &recRunner{}
	err := Bootstrap(context.Background(), BootstrapOptions{
		Base: base, Bin: bin, Out: &out, Stderr: &out, Runner: r, Client: srv.Client(), Release: rel, GOOS: "linux", GOARCH: "amd64",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v\n%s", err, out.String())
	}
	bPath := filepath.Join(bin, "b")
	if !isExecutable(bPath) {
		t.Fatalf(".bin/b not installed executable:\n%s", out.String())
	}
	raw, _ := os.ReadFile(bPath)
	if !strings.HasPrefix(string(raw), "#!/bin/sh") {
		t.Fatalf("wrong member extracted: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(bin, "LICENSE")); err == nil {
		t.Fatal("LICENSE member must not be extracted")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(bin, ".b-download-*")); len(leftovers) != 0 {
		t.Fatalf("temp download left behind: %v", leftovers)
	}
	if len(r.cmds) != 1 || r.cmds[0].Name != bPath || strings.Join(r.cmds[0].Args, " ") != "install" || r.cmds[0].Dir != base {
		t.Fatalf("b install not run as expected: %+v", r.cmds)
	}
	if strings.Join(r.cmds[0].Env, ",") != "PATH_BIN="+bin {
		t.Fatalf("b install env = %v", r.cmds[0].Env)
	}
	if !strings.Contains(out.String(), "verified sha256 "+sha256hex(tb)[:12]) {
		t.Fatalf("no verification line:\n%s", out.String())
	}

	// Second run: b present → no download, install still runs.
	hits = 0
	r = &recRunner{}
	out.Reset()
	if err := Bootstrap(context.Background(), BootstrapOptions{Base: base, Bin: bin, Out: &out, Runner: r, Client: srv.Client(), Release: rel, GOOS: "linux", GOARCH: "amd64"}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("re-downloaded an existing .bin/b")
	}
	if !strings.Contains(out.String(), "b present: .bin/b") || len(r.cmds) != 1 {
		t.Fatalf("present path: %s / %+v", out.String(), r.cmds)
	}
}

func TestBootstrapChecksumMismatchInstallsNothing(t *testing.T) {
	tb := fakeTarball(t, map[string]string{"b": "#!/bin/sh\n"})
	var hits int32
	srv := server(t, "b-linux-arm64.tar.gz", tb, &hits)
	rel := &Release{Version: "1.0.0", BaseURL: srv.URL, Assets: map[string]string{"b-linux-arm64.tar.gz": strings.Repeat("0", 64)}}
	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	r := &recRunner{}
	var out bytes.Buffer
	err := Bootstrap(context.Background(), BootstrapOptions{Base: base, Bin: bin, Out: &out, Runner: r, Client: srv.Client(), Release: rel, GOOS: "linux", GOARCH: "arm64"})
	if err == nil || !strings.Contains(err.Error(), "checksum MISMATCH") {
		t.Fatalf("mismatch not reported: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(bin, "b")); statErr == nil {
		t.Fatal("b installed despite the mismatch")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(bin, ".b-download-*")); len(leftovers) != 0 {
		t.Fatalf("temp download left behind: %v", leftovers)
	}
	if len(r.cmds) != 0 {
		t.Fatal("b install ran after a failed verification")
	}
}

func TestBootstrapTarballWithoutBFails(t *testing.T) {
	tb := fakeTarball(t, map[string]string{"README.md": "x", "bin/b": "#!/bin/sh\n"}) // nested, not root
	var hits int32
	srv := server(t, "b-linux-amd64.tar.gz", tb, &hits)
	rel := &Release{Version: "1.0.0", BaseURL: srv.URL, Assets: map[string]string{"b-linux-amd64.tar.gz": sha256hex(tb)}}
	base := t.TempDir()
	err := Bootstrap(context.Background(), BootstrapOptions{Base: base, Bin: filepath.Join(base, ".bin"), Out: &bytes.Buffer{}, Runner: &recRunner{}, Client: srv.Client(), Release: rel, GOOS: "linux", GOARCH: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "no `b` member") {
		t.Fatalf("want member error, got %v", err)
	}
}

func TestBootstrapDryRunTouchesNothing(t *testing.T) {
	var hits int32
	srv := server(t, "never", nil, &hits)
	rel := &Release{Version: "4.18.7", BaseURL: srv.URL, Assets: map[string]string{"b-linux-amd64.tar.gz": strings.Repeat("a", 64)}}
	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	r := &recRunner{}
	var out bytes.Buffer
	if err := Bootstrap(context.Background(), BootstrapOptions{Base: base, Bin: bin, Out: &out, DryRun: true, Runner: r, Client: srv.Client(), Release: rel, GOOS: "linux", GOARCH: "amd64"}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 0 || len(r.cmds) != 0 {
		t.Fatal("dry run downloaded or ran something")
	}
	if _, err := os.Stat(bin); err == nil {
		t.Fatal("dry run created .bin")
	}
	for _, s := range []string{"would download " + srv.URL + "/v4.18.7/b-linux-amd64.tar.gz", "would verify   sha256 " + strings.Repeat("a", 64), "would install  .bin/b", "would run      .bin/b install  [PATH_BIN=.bin]"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("dry run output lacks %q:\n%s", s, out.String())
		}
	}
}

func TestBootstrapDarwinPointsAtManualInstall(t *testing.T) {
	base := t.TempDir()
	err := Bootstrap(context.Background(), BootstrapOptions{Base: base, Bin: filepath.Join(base, ".bin"), Out: &bytes.Buffer{}, Runner: &recRunner{}, GOOS: "darwin", GOARCH: "arm64"})
	if err == nil || !strings.Contains(err.Error(), "binary.help") || !strings.Contains(err.Error(), "no darwin build") {
		t.Fatalf("darwin: %v", err)
	}
}

func TestBootstrapRefusesPlainHTTP(t *testing.T) {
	rel := &Release{Version: "1.0.0", BaseURL: "http://example.invalid", Assets: map[string]string{"b-linux-amd64.tar.gz": strings.Repeat("a", 64)}}
	base := t.TempDir()
	err := Bootstrap(context.Background(), BootstrapOptions{Base: base, Bin: filepath.Join(base, ".bin"), Out: &bytes.Buffer{}, Runner: &recRunner{}, Release: rel, GOOS: "linux", GOARCH: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("plain http accepted: %v", err)
	}
}

// TestPinnedReleaseShape: the production pin is well-formed — a version,
// an https base, 64-hex sums for both linux architectures lo ships.
func TestPinnedReleaseShape(t *testing.T) {
	if !strings.HasPrefix(BRelease.BaseURL, "https://github.com/fentas/b/") {
		t.Fatalf("BaseURL = %s", BRelease.BaseURL)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		name, sum, err := BRelease.Asset("linux", arch)
		if err != nil || name != "b-linux-"+arch+".tar.gz" || len(sum) != 64 {
			t.Fatalf("linux/%s: %s %s %v", arch, name, sum, err)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			t.Fatalf("sum for %s is not hex: %s", name, sum)
		}
	}
	if BRelease.URL("b-linux-amd64.tar.gz") != "https://github.com/fentas/b/releases/download/v"+BRelease.Version+"/b-linux-amd64.tar.gz" {
		t.Fatalf("URL = %s", BRelease.URL("b-linux-amd64.tar.gz"))
	}
}

package toolchain

// bootstrap.go — install b itself into a project's .bin/ and run
// `.bin/b install`. b's official self-install is its release tarball
// (b-<os>-<arch>.tar.gz containing the `b` binary; install.sh and the
// `b` preset inside b both resolve exactly that asset). This is that
// mechanism WITHOUT `curl | sh`: the release is pinned, the tarball is
// downloaded to a temp file over https only, its SHA-256 is verified
// against the sum the release publishes in checksums.txt (recorded here
// at pin time), and only then is `b` extracted. The same pin + sum pattern
// as .github/workflows/ci.yml.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// Release is one b release: the tag and the SHA-256 of each asset lo can
// install, transcribed from the checksums.txt that release publishes.
type Release struct {
	// Version without the leading v (b tags are v<Version>).
	Version string
	// BaseURL is the release download root; the asset is BaseURL/v<Version>/<asset>.
	BaseURL string
	// Assets maps asset name (b-<os>-<arch>.tar.gz) to its sha256 hex.
	Assets map[string]string
}

// BRelease is the pinned b. Verified 2026-09-03: v4.18.7 is the latest
// release (published 2026-08-30); the sums are from its checksums.txt
// (https://github.com/fentas/b/releases/download/v4.18.7/checksums.txt,
// GPG-signed by the maintainer as checksums.txt.sig). b ships linux,
// freebsd and windows builds — no darwin — so only the two linux
// architectures `lo` itself is released for are listed; macOS users
// install b by hand (see Bootstrap).
var BRelease = Release{
	Version: "4.18.7",
	BaseURL: "https://github.com/fentas/b/releases/download",
	Assets: map[string]string{
		"b-linux-amd64.tar.gz": "decfaac19baa348349e568ae620323f95dacb31c1194abefe0ff7f5265f2822a",
		"b-linux-arm64.tar.gz": "1e70ae1a1f5a6aafa39e3374e2d5824a14e72f78d04eb5b5fe84a666e5599c34",
	},
}

// Asset picks the release asset for a platform and its expected sum.
func (r Release) Asset(goos, goarch string) (name, sum string, err error) {
	name = fmt.Sprintf("b-%s-%s.tar.gz", goos, goarch)
	sum, ok := r.Assets[name]
	if !ok {
		if goos == "darwin" {
			return "", "", fmt.Errorf("b v%s publishes no darwin build (linux, freebsd and windows only) — install b by hand from https://binary.help (or `brew install fentas/tap/b` if offered), put it on PATH or copy it to .bin/b, then re-run lo init toolchain", r.Version)
		}
		return "", "", fmt.Errorf("b v%s has no pinned asset for %s/%s (%s)", r.Version, goos, goarch, name)
	}
	return name, sum, nil
}

// URL is the https download location of one asset.
func (r Release) URL(asset string) string {
	return fmt.Sprintf("%s/v%s/%s", strings.TrimRight(r.BaseURL, "/"), r.Version, asset)
}

// BootstrapOptions shapes one bootstrap.
type BootstrapOptions struct {
	// Base is the project root (the cwd for `b install`).
	Base string
	// Bin is the toolchain dir (<Base>/.bin) — where b lands and what
	// PATH_BIN is set to for `b install`.
	Bin string
	// Out receives the progress lines; Stderr the child's stderr.
	Out    io.Writer
	Stderr io.Writer
	// DryRun prints what would happen and touches nothing.
	DryRun bool
	// SkipInstall bootstraps b but does not run `b install`.
	SkipInstall bool
	// Runner runs `.bin/b install` (the hermetic seam). Nil = execx over
	// Paths{Base, Bin}.
	Runner execx.Runner
	// Client performs the download. Nil = a 5-minute-timeout client that
	// refuses to follow a redirect off https.
	Client *http.Client
	// Release overrides the pin (tests). Nil = BRelease.
	Release *Release
	// GOOS/GOARCH override the host (tests). "" = runtime.
	GOOS, GOARCH string
}

func (o *BootstrapOptions) out() io.Writer {
	if o.Out != nil {
		return o.Out
	}
	return os.Stdout
}

func (o *BootstrapOptions) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

func (o *BootstrapOptions) release() Release {
	if o.Release != nil {
		return *o.Release
	}
	return BRelease
}

func (o *BootstrapOptions) platform() (string, string) {
	goos, goarch := o.GOOS, o.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// Bootstrap makes `<Bin>/b` exist (download + verify + extract, unless it
// already does) and runs `<Bin>/b install` in Base with PATH_BIN=<Bin>, so
// b reads <Bin>/b.yaml and installs next to itself. GITHUB_TOKEN, when
// set, reaches b through the inherited environment (b works token-free
// for public sources; the token only raises the API rate limit or reaches
// private repos).
func Bootstrap(ctx context.Context, o BootstrapOptions) error {
	if o.Base == "" || o.Bin == "" {
		return errors.New("toolchain: Base and Bin are required")
	}
	out := o.out()
	bPath := filepath.Join(o.Bin, "b")
	rel := o.release()

	if isExecutable(bPath) {
		v, _ := probe(ctx, bPath, "--version")
		fmt.Fprintf(out, "  b present: %s %s\n", relOrAbs(o.Base, bPath), strings.TrimSpace(v))
	} else {
		goos, goarch := o.platform()
		asset, sum, err := rel.Asset(goos, goarch)
		if err != nil {
			return err
		}
		url := rel.URL(asset)
		if !strings.HasPrefix(url, "https://") {
			return fmt.Errorf("toolchain: refusing a non-https download: %s", url)
		}
		if o.DryRun {
			fmt.Fprintf(out, "  would download %s\n", url)
			fmt.Fprintf(out, "  would verify   sha256 %s (from the v%s checksums.txt)\n", sum, rel.Version)
			fmt.Fprintf(out, "  would install  %s\n", relOrAbs(o.Base, bPath))
		} else {
			fmt.Fprintf(out, "  downloading %s\n", url)
			if err := downloadVerifyExtract(ctx, o.client(), url, sum, o.Bin, bPath); err != nil {
				return err
			}
			fmt.Fprintf(out, "  verified sha256 %s…, installed %s\n", sum[:12], relOrAbs(o.Base, bPath))
		}
	}

	if o.SkipInstall {
		return nil
	}
	token := ""
	if os.Getenv("GITHUB_TOKEN") != "" {
		token = " (GITHUB_TOKEN set — passed through)" // #nosec G101 -- a status suffix, not the token
	}
	if o.DryRun {
		fmt.Fprintf(out, "  would run      %s install  [PATH_BIN=%s]%s\n", relOrAbs(o.Base, bPath), relOrAbs(o.Base, o.Bin), token)
		return nil
	}
	fmt.Fprintf(out, "  running %s install%s\n", relOrAbs(o.Base, bPath), token)
	runner := o.Runner
	if runner == nil {
		runner = execx.NewRunner(&config.Paths{Base: o.Base, Bin: o.Bin})
	}
	err := runner.Run(ctx, execx.Cmd{
		Name:   bPath,
		Args:   []string{"install"},
		Dir:    o.Base,
		Env:    []string{"PATH_BIN=" + o.Bin},
		Stdout: out,
		Stderr: o.stderr(),
	})
	if err != nil {
		return fmt.Errorf("b install: %w", err)
	}
	return nil
}

func (o *BootstrapOptions) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to a non-https URL refused: %s", req.URL)
			}
			return nil
		},
	}
}

// maxTarballBytes bounds the download (b's tarball is ~5 MB; the binary
// inside ~15 MB).
const maxTarballBytes = 256 << 20

// downloadVerifyExtract fetches url into a temp file under bin, checks its
// sha256 against want BEFORE reading it as an archive, then extracts the
// single `b` member to dst (staged next to it and renamed, so an
// interrupted run never leaves a half-written b).
func downloadVerifyExtract(ctx context.Context, client *http.Client, url, want, bin, dst string) error {
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(bin, ".b-download-*.tar.gz")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxTarballBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if n > maxTarballBytes {
		return fmt.Errorf("download %s: larger than %d bytes, refusing", url, maxTarballBytes)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum MISMATCH for %s\n      expected %s\n      got      %s\n    The download is corrupt or tampered with — nothing was installed", url, want, got)
	}
	return extractB(tmpName, dst)
}

// extractB copies the `b` member of a verified tar.gz to dst (0755).
func extractB(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("b tarball: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errors.New("b tarball: no `b` member found")
		}
		if err != nil {
			return fmt.Errorf("b tarball: %w", err)
		}
		// Only the binary named exactly `b` at the archive root is taken;
		// LICENSE/README and anything path-shaped are skipped.
		if filepath.Clean(hdr.Name) != "b" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		stage := dst + ".tmp"
		w, err := os.OpenFile(stage, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, io.LimitReader(tr, maxTarballBytes)); err != nil {
			_ = w.Close()
			_ = os.Remove(stage)
			return fmt.Errorf("b tarball: %w", err)
		}
		if err := w.Close(); err != nil {
			_ = os.Remove(stage)
			return err
		}
		if err := os.Chmod(stage, 0o755); err != nil {
			_ = os.Remove(stage)
			return err
		}
		return os.Rename(stage, dst)
	}
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// relOrAbs prints p relative to base when it is inside it.
func relOrAbs(base, p string) string {
	if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

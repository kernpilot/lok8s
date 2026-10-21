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
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/clock"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
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
			return "", "", fmt.Errorf("b v%s has no darwin build (linux, freebsd and windows only). Install b by hand from https://binary.help (brew install fentas/tap/b if offered), put it on PATH or copy it to .bin/b, then re-run lo toolchain install", r.Version)
		}
		return "", "", fmt.Errorf("b v%s has no pinned asset for %s/%s (%s). Add the asset to the pinned release in internal/toolchain/pins.go", r.Version, goos, goarch, name)
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
	// Client performs the download. Nil = a client with a 2-minute timeout
	// per attempt that refuses to follow a redirect off https.
	Client *http.Client
	// Release overrides the pin (tests). Nil = BRelease.
	Release *Release
	// GOOS/GOARCH override the host (tests). "" = runtime.
	GOOS, GOARCH string
	// Sleep is the wait between download attempts. Nil = clock.Sleep,
	// which ends with the context.
	Sleep clock.SleepFunc
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

	if fsutil.IsExecutable(bPath) {
		v, _ := probe(ctx, execx.NewRunner(nil), bPath, "--version")
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
			if err := downloadVerifyExtract(ctx, o.client(), url, sum, o.Bin, bPath, o.stderr(), o.sleep()); err != nil {
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

func (o *BootstrapOptions) sleep() clock.SleepFunc {
	if o.Sleep != nil {
		return o.Sleep
	}
	return clock.Sleep
}

func (o *BootstrapOptions) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	// The timeout is per attempt: b's tarball is ~5 MB, and with the
	// retries (downloadBackoff) the worst case stays near 8 minutes.
	return &http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: refuseNonHTTPSRedirect,
	}
}

// The two redirect refusals. They are policy, not a network blip, so the
// download never retries them.
var (
	errTooManyRedirects = errors.New("too many redirects")
	errRedirectRefused  = errors.New("redirect to a non-https URL refused")
)

// refuseNonHTTPSRedirect is the download client's redirect policy: a hop
// off https (a downgrade to plain http) is refused, and so is a chain of
// ten or more redirects. Named so the guard is testable on its own.
func refuseNonHTTPSRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errTooManyRedirects
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w: %s", errRedirectRefused, req.URL)
	}
	return nil
}

// maxTarballBytes bounds the download and the extracted `b` member (b's
// tarball is ~5 MB; the binary inside ~15 MB). A var so a test can lower it.
var maxTarballBytes int64 = 256 << 20

// The download retries a transient failure (B410: one dial timeout on a
// fresh VM failed a whole `lo toolchain install`). downloadBackoff is the
// wait before each retry, so there are len(downloadBackoff)+1 attempts.
var downloadBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// retryableError marks a download failure that another attempt can fix: a
// transport error, a body that broke off, HTTP 429 or a 5xx.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// downloadVerifyExtract fetches url into a temp file under bin (retrying a
// transient failure, see downloadAttempts), checks its sha256 against want
// BEFORE reading it as an archive, then extracts the single `b` member to
// dst (staged next to it and renamed, so an interrupted run never leaves a
// half-written b). A checksum mismatch is never retried. Each retry prints
// one line to stderr.
func downloadVerifyExtract(ctx context.Context, client *http.Client, url, want, bin, dst string, stderr io.Writer, sleep clock.SleepFunc) error {
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	attempts := len(downloadBackoff) + 1
	var tmpName, got string
	for attempt := 1; ; attempt++ {
		var err error
		tmpName, got, err = download(ctx, client, url, bin)
		if err == nil {
			break
		}
		var transient *retryableError
		if !errors.As(err, &transient) || ctx.Err() != nil {
			return err
		}
		if attempt >= attempts {
			return fmt.Errorf("%w (gave up after %d attempts)", transient.err, attempt)
		}
		wait := downloadBackoff[attempt-1]
		fmt.Fprintf(stderr, "  %v; retrying in %s (%d/%d)\n", transient.err, wait, attempt+1, attempts)
		if err := sleep(ctx, wait); err != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
	}
	defer func() { _ = os.Remove(tmpName) }()
	if got != want {
		return fmt.Errorf("checksum MISMATCH for %s: expected %s, got %s. The download is corrupt or tampered with; nothing was installed", url, want, got)
	}
	return extractB(tmpName, dst)
}

// download makes one attempt: GET url into a temp file under bin. It
// returns the file and its sha256; on any error it leaves no file behind.
// A transient failure comes back as a *retryableError.
func download(ctx context.Context, client *http.Client, url, bin string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("download %s: %w", url, err)
		if transientTransport(ctx, err) {
			return "", "", &retryableError{err}
		}
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("download %s: HTTP %s", url, resp.Status)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return "", "", &retryableError{err}
		}
		return "", "", err
	}
	tmp, err := os.CreateTemp(bin, ".b-download-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxTarballBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpName)
		err := fmt.Errorf("download %s: %w", url, copyErr)
		// A failed write to the temp file (disk full, EIO) is local and
		// permanent; a read that broke off (reset, unexpected EOF) is not.
		var pathErr *fs.PathError
		if ctx.Err() == nil && !errors.As(copyErr, &pathErr) {
			return "", "", &retryableError{err}
		}
		return "", "", err
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		return "", "", closeErr
	}
	if n > maxTarballBytes {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("download %s: larger than %d bytes, refusing", url, maxTarballBytes)
	}
	return tmpName, hex.EncodeToString(h.Sum(nil)), nil
}

// transientTransport reports whether a client.Do error is worth another
// attempt: everything except a cancelled caller, a refused redirect and a
// TLS certificate the server will present again unchanged (Go wraps every
// verification failure in tls.CertificateVerificationError).
func transientTransport(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, errRedirectRefused) || errors.Is(err, errTooManyRedirects) {
		return false
	}
	var certErr *tls.CertificateVerificationError
	return !errors.As(err, &certErr)
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
		if errors.Is(err, io.EOF) {
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
		// The member is bounded like the download: a header claiming more,
		// or a body that keeps going past the cap, is refused outright — a
		// silently truncated binary would be a broken b, not a safe one.
		if hdr.Size > maxTarballBytes {
			return fmt.Errorf("b tarball: `b` member is %d bytes, larger than the %d-byte cap, refusing", hdr.Size, maxTarballBytes)
		}
		stage := dst + ".tmp"
		w, err := os.OpenFile(stage, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) // #nosec G302 -- the b binary must be executable
		if err != nil {
			return err
		}
		n, err := io.Copy(w, io.LimitReader(tr, maxTarballBytes+1))
		if err != nil {
			_ = w.Close()
			_ = os.Remove(stage)
			return fmt.Errorf("b tarball: %w", err)
		}
		if n > maxTarballBytes {
			_ = w.Close()
			_ = os.Remove(stage)
			return fmt.Errorf("b tarball: `b` member larger than the %d-byte cap, refusing", maxTarballBytes)
		}
		if err := w.Close(); err != nil {
			_ = os.Remove(stage)
			return err
		}
		if err := os.Chmod(stage, 0o755); err != nil { // #nosec G302 -- the b binary must be executable
			_ = os.Remove(stage)
			return err
		}
		return os.Rename(stage, dst)
	}
}

// relOrAbs prints p relative to base when it is inside it (config.RelTo).
func relOrAbs(base, p string) string { return config.RelTo(base, p) }

// Package kubehz is the Go port of the kubehz platform integration
// (.lok8s/libs/kubehz/{main,shared,hosted,node,handover,deploy} and the
// vendored manifests/ tree): the spec.kubehz config reader + validator, the
// platform-api client (register / deregister / status / assess / re-enroll /
// claim), the hosted-control-plane flows the kubeone and capi drivers call
// through their Hooks, Spaces on the shared control plane (hosting: shared —
// the kubehz driver), the static-pool node verbs, the control-plane handover
// target side, and the in-cluster agent deploy.
//
// Every user-visible string is verbatim from the bash. External processes
// (kubectl, kubeadm, ssh/scp, etcdutl, hcloud, ssh-keygen, …) run through
// the execx.Runner seam; the platform api is reached with net/http through
// an injectable *http.Client, so the whole package tests hermetically
// (httptest + a fake Runner + t.TempDir).
//
// TOKEN CONTAINMENT (matches the bash exactly): KUBEHZ_TOKEN and
// HCLOUD_TOKEN are read from the environment at call time, travel ONLY in
// an Authorization header (or, for the opt-in credential connect, the
// request body) on an https:// URL, and are never printed, logged or
// embedded in an error. The in-cluster agent token is read from the
// cluster, hashed, and discarded — the plaintext never leaves the process.
package kubehz

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/kernpilot/lok8s/internal/credentials"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/kernpilot/lok8s/internal/clock"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose message was already printed in the bash
// implementation's own format ([error] … on stderr, or a plain echo). The
// caller exits non-zero without printing anything further.
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// Context binds the library to its streams, paths, and seams.
type Context struct {
	Paths  *config.Paths
	Runner execx.Runner
	Out    io.Writer
	ErrOut io.Writer

	// HTTP is the platform-api transport (nil = a plain http.Client). Tests
	// point it at an httptest server; the https-only gates run BEFORE any
	// request regardless of the client.
	HTTP *http.Client

	// Env overrides the process environment for the token/tunable reads
	// (KUBEHZ_TOKEN, HCLOUD_TOKEN, HCLOUD_API_BASE, KUBEHZ_HANDOVER_*, …).
	// nil = os.Getenv. A key present with "" reads as unset, like bash.
	Env map[string]string

	// Sleep is the wait seam (nil = clock.Sleep, which ends with the
	// context): the hosted/space wait loops and the deploy drains all wait
	// through it.
	Sleep clock.SleepFunc
	// Now is the clock seam (nil = time.Now) — the claim-nonce stamp.
	Now func() time.Time
	// Hostname is `hostname` (nil = os.Hostname).
	Hostname func() (string, error)
	// IsRoot is node::is_root (nil = euid == 0).
	IsRoot func() bool
	// Euid is the effective user id (nil = os.Geteuid): the owner a link
	// and its target must have before the agent kubeconfig writes through
	// the link (agent.go kubeconfigTarget).
	Euid func() int
	// LookPath is `command -v <tool>` (nil = execx.Look over Paths).
	LookPath func(tool string) bool
	// IsTTY reports whether stdout is a terminal (nil = false) — the
	// assessment printer's colour switch.
	IsTTY func() bool
	// ProviderOutput is the loaded provider's inventory JSON
	// (provider::output on PROVIDER_CONFIG_FILE), consulted by the kubeone
	// fingerprint reader when set. nil = no provider loaded → the spec's
	// sshPublicKeyFile is used.
	ProviderOutput func(ctx context.Context) ([]byte, error)
}

func (c *Context) out() io.Writer {
	if c.Out != nil {
		return c.Out
	}
	return os.Stdout
}

func (c *Context) errOut() io.Writer {
	if c.ErrOut != nil {
		return c.ErrOut
	}
	return os.Stderr
}

func (c *Context) getenv(key string) string {
	if c.Env != nil {
		return c.Env[key]
	}
	return os.Getenv(key)
}

func (c *Context) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	return clock.Sleep(ctx, d)
}

func (c *Context) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Context) hostname() (string, error) {
	if c.Hostname != nil {
		return c.Hostname()
	}
	return os.Hostname()
}

func (c *Context) euid() int {
	if c.Euid != nil {
		return c.Euid()
	}
	return os.Geteuid()
}

func (c *Context) isRoot() bool {
	if c.IsRoot != nil {
		return c.IsRoot()
	}
	return os.Geteuid() == 0
}

func (c *Context) lookPath(tool string) bool {
	if c.LookPath != nil {
		return c.LookPath(tool)
	}
	_, ok := execx.Look(c.Paths, tool)
	return ok
}

func (c *Context) isTTY() bool {
	return c.IsTTY != nil && c.IsTTY()
}

func (c *Context) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// Bound the HANG, not the transfer: http.Client.Timeout would cap the
	// whole body, and the same client downloads the etcd snapshot during a
	// handover (handover.go fetchSnapshot), which can legitimately take
	// longer than any fixed cap. The default transport's dial timeout plus
	// a response-header deadline cover a platform API that never answers.
	// bash curl had no --max-time either; this is a deliberate divergence.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	return &http.Client{
		Transport: transport,
		// Requests carry KUBEHZ_TOKEN as a bearer. net/http drops the header
		// on a cross-host redirect but not on a same-host https→http
		// downgrade — refuse any redirect that leaves https.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-https URL %s://%s", req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
	}
}

// ── verbose.sh helpers, bound to the context's stderr ────

func (c *Context) errorf(format string, a ...any) { ui.ErrorTo(c.errOut(), format, a...) }
func (c *Context) warnf(format string, a ...any)  { ui.WarnTo(c.errOut(), format, a...) }
func (c *Context) debugf(format string, a ...any) { ui.DebugTo(c.errOut(), format, a...) }

// echo writes one stdout line (bash `echo`).
func (c *Context) echo(format string, a ...any) {
	fmt.Fprintf(c.out(), format+"\n", a...)
}

// echoErr writes one stderr line (bash `echo … >&2`).
func (c *Context) echoErr(format string, a ...any) {
	fmt.Fprintf(c.errOut(), format+"\n", a...)
}

// requireHTTPS is http::require_https on the context's stderr.
func (c *Context) requireHTTPS(url, label string) error {
	return credentials.RequireHTTPS(url, label, c.errOut())
}

// ── process seam ─────────────────────────────────────────

// run executes a tool with the context's streams (bash: a plain command
// invocation whose output reaches the terminal).
func (c *Context) run(ctx context.Context, name string, args ...string) error {
	return c.Runner.Run(ctx, execx.Cmd{
		Name: name, Args: args,
		Stdout: c.out(), Stderr: c.errOut(),
	})
}

// runQuiet executes a tool with both streams discarded (bash:
// `>/dev/null 2>&1`).
func (c *Context) runQuiet(ctx context.Context, name string, args ...string) error {
	return c.Runner.Run(ctx, execx.Cmd{
		Name: name, Args: args,
		Stdout: io.Discard, Stderr: io.Discard,
	})
}

// capture executes a tool and returns its stdout (bash: `$(tool …)`);
// stderr goes to the context's stderr unless quiet (bash: `2>/dev/null`).
func (c *Context) capture(ctx context.Context, quiet bool, name string, args ...string) (string, error) {
	var stdout bytes.Buffer
	stderr := c.errOut()
	if quiet {
		stderr = io.Discard
	}
	err := c.Runner.Run(ctx, execx.Cmd{
		Name: name, Args: args,
		Stdout: &stdout, Stderr: stderr,
	})
	return stdout.String(), err
}

// captureBoth executes a tool and returns stdout+stderr merged (bash:
// `$(tool … 2>&1)`) plus the exit status verdict.
func (c *Context) captureBoth(ctx context.Context, name string, args ...string) (string, error) {
	var both bytes.Buffer
	err := c.Runner.Run(ctx, execx.Cmd{
		Name: name, Args: args,
		Stdout: &both, Stderr: &both,
	})
	return both.String(), err
}

// clusterYAMLPath is ${PATH_CLUSTERS}/<domain>/cluster.lok8s.yaml.
func (c *Context) clusterYAMLPath(domain string) string {
	return c.Paths.Clusters + "/" + domain + "/cluster.lok8s.yaml"
}

// bindSecretPath is ${PATH_CLUSTERS}/<domain>/.kubehz-bind — the one-time
// bind secret (B243) a register wrote, that `lo kubehz deploy` hands to the
// in-cluster agent to ADOPT the announced row instead of minting a sibling.
func (c *Context) bindSecretPath(domain string) string {
	return c.Paths.Clusters + "/" + domain + "/.kubehz-bind"
}

// persistBindSecret stores the announce's one-time bind secret (B243) at
// bindSecretPath, mode 0600, with NO trailing newline (the deploy and every
// later register read the exact 64 hex). An empty secret writes nothing: an
// api without bind secrets sends none. The write happens in place under the
// ignored name (createBindSecret), so no state of the write leaves the secret
// in a file that .gitignore does not match. A write failure warns but never
// fails the register. The deploy then finds no bind secret, and the agent
// registers a separate pending row that the user claims by its claim code
// (B288).
func (c *Context) persistBindSecret(domain, secret string) {
	if secret == "" {
		return
	}
	dir := c.Paths.Clusters + "/" + domain
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.warnf("kubehz: could not create %s to store the bind secret: %s", dir, err)
		return
	}
	path := dir + "/.kubehz-bind"
	// A directory, or a link to one, is never replaced (the bash `[[ -d ]]`).
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		c.warnf("kubehz: could not store the bind secret: clusters/%s/.kubehz-bind is a directory", domain)
		return
	}
	if err := createBindSecret(path, secret); err != nil {
		c.warnf("kubehz: could not store the bind secret: %s", err)
	}
}

// syncFile is f.Sync, a variable so a test can fail it.
var syncFile = (*os.File).Sync

// createBindSecret replaces path with content in place. It unlinks the old
// file first (that needs write access to the directory only, so a file that
// cannot be read is replaced too), then creates the file with O_EXCL at 0600,
// writes, syncs and closes it. A failed write removes the new file. A sync
// that the filesystem does not support (syncUnsupported) keeps the file.
// Every state on the way has the ignored name.
func createBindSecret(path, content string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(content)
	serr := syncFile(f)
	if syncUnsupported(serr) {
		serr = nil
	}
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

var bindSecretRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// storedBindSecret is the bind secret a previous register stored at
// bindSecretPath: a readable regular file of exactly 64 lowercase hex
// characters. A missing, unreadable, irregular or malformed file gives "".
// Every register call sends the value as bindSecret (B289), so a re-run
// keeps the same cluster record, and the deploy stages only this value. The
// api refuses a malformed value, so lo never sends one.
//
// The file is opened once, without blocking (openNonblock: a FIFO in the
// slot would block a plain open), checked on the open file as a regular file,
// and read for at most 65 bytes: the count and the anchored pattern each
// refuse a longer file, and the cap bounds the read.
func (c *Context) storedBindSecret(domain string) string {
	f, err := openNonblock(c.bindSecretPath(domain))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	buf := make([]byte, 65)
	n, _ := io.ReadFull(f, buf)
	if n != 64 || !bindSecretRe.Match(buf[:n]) {
		return ""
	}
	return string(buf[:n])
}

// registerBody is the JSON body of a register call: the pairs, then
// bindSecret when storedBindSecret has one.
func (c *Context) registerBody(domain string, pairs ...jsonPair) []byte {
	if secret := c.storedBindSecret(domain); secret != "" {
		pairs = append(pairs, jsonPair{"bindSecret", secret})
	}
	return compactJSON(pairs...)
}

// requireDomainSpec is the shared subcommand preamble: an active domain and
// its cluster.lok8s.yaml.
func (c *Context) requireDomainSpec(domain string) (string, error) {
	if domain == "" {
		c.errorf("No active domain. Use: lo use <domain>")
		return "", ErrHandled
	}
	cy := c.clusterYAMLPath(domain)
	if !fsutil.FileExists(cy) {
		c.errorf("No cluster.lok8s.yaml for domain: %s", domain)
		return "", ErrHandled
	}
	return cy, nil
}

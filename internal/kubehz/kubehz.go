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
	// OverflowUID is the uid an unmapped owner shows in a user namespace
	// (nil = /proc/sys/kernel/overflowuid, else 65534): the owner that "/"
	// alone may have in the temp-dir check of a join file (joinTempDir).
	OverflowUID func() int
	// Lstat is os.Lstat (nil = os.Lstat): the read of each part of the
	// temp dir of a join file (joinTempDir). Tests log the order.
	Lstat func(path string) (fs.FileInfo, error)
	// Chmod is os.Chmod (nil = os.Chmod): the 0700 of the private dirs of
	// a handover (makePrivate) and the modes of the PKI files it places.
	// Tests make it fail, or leave a dir open.
	Chmod func(path string, mode fs.FileMode) error
	// Remove is os.Remove (nil = os.Remove): the removal of a join file
	// and then of its directory (removeJoinFile), after a failed write
	// (writeJoinFile) and after a join (removeJoinConfig).
	Remove func(path string) error
	// LookPath is `command -v <tool>` (nil = execx.Look over Paths).
	LookPath func(tool string) bool
	// IsTTY reports whether stdout is a terminal (nil = false) — the
	// assessment printer's colour switch.
	IsTTY func() bool
	// PrintToken is --print-token of lo kubehz register, lo provision and
	// lo up: the claim-key registration also shows the one-time claim
	// ticket on a stdout that is not a terminal and in a CI run (see
	// claimTicketShown).
	PrintToken bool
	// MkdirTemp makes the fresh private directory of a join file (nil =
	// os.MkdirTemp). Tests return a directory they prepared, to plant a
	// name at the path the file goes to.
	MkdirTemp func(dir, pattern string) (string, error)
	// joinWrite and joinClose are the write and the close of a join file
	// (nil = (*os.File).WriteString and (*os.File).Close). Tests make them
	// fail, to see that the fresh directory goes.
	joinWrite func(f *os.File, s string) error
	joinClose func(f *os.File) error
	// joinMinted runs after the mint of a node join returned a ticket (nil
	// = nothing). Tests send a signal there, between the mint and the
	// next step.
	joinMinted func()
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

func (c *Context) lstat(path string) (fs.FileInfo, error) {
	if c.Lstat != nil {
		return c.Lstat(path)
	}
	return os.Lstat(path)
}

func (c *Context) chmod(path string, mode fs.FileMode) error {
	if c.Chmod != nil {
		return c.Chmod(path, mode)
	}
	return os.Chmod(path, mode)
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
	// bash curl has no --max-time on these calls either. The mint of a node
	// join is the exception in both twins: mintMaxTime here, curl
	// --max-time in bash.
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
// bind secret a register wrote, that `lo kubehz deploy` hands to the
// in-cluster agent to ADOPT the announced row instead of minting a sibling.
func (c *Context) bindSecretPath(domain string) string {
	return c.Paths.Clusters + "/" + domain + "/.kubehz-bind"
}

// persistBindSecret stores the announce's one-time bind secret at
// bindSecretPath, mode 0600, with NO trailing newline (the deploy and every
// later register read the value byte for byte: 64 lowercase hex characters,
// or khzb_ and 64 lowercase hex characters). An empty secret writes nothing: an
// api without bind secrets sends none. The write happens in place under the
// ignored name (createBindSecret), so no state of the write leaves the secret
// in a file that .gitignore does not match. A write failure warns but never
// fails the register. The deploy then finds no bind secret, and the agent
// can register a separate pending row if it has no binding and no staged
// value yet. The user claims that row by its claim code.
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

// persistResponseBindSecret stores the bind secret of the register answer v.
// No value (no key, null, false or "") writes nothing and prints nothing, as
// before. Any other value must be a string that bindSecretRe matches, the
// rule that storedBindSecret applies to the file. Else lo does not change the
// file and warns. The warning names the file and never the value. The answer
// can have rotated the secret, so the warning says that the stored value can
// be one that no longer counts.
func (c *Context) persistResponseBindSecret(domain string, v any) {
	got := jget(v, "bindSecret")
	if jfalsy(got) || got == "" {
		return
	}
	s, ok := got.(string)
	if !ok || !bindSecretRe.MatchString(s) {
		c.warnf("kubehz: the platform answer holds a bind secret that lo cannot use (lo accepts 64 lowercase hex characters, or khzb_ and 64 lowercase hex characters). lo did not change clusters/%s/.kubehz-bind. The platform can have rotated the bind secret with this answer: then the value in the file no longer counts, and lo cannot get the new value again. If the platform hands out a newer form, upgrade lok8s. An agent that has no binding yet can then register a separate pending row: claim that row with the code from 'lo kubehz claim-code'.", domain)
		return
	}
	c.persistBindSecret(domain, s)
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

// A bind secret has one of two forms: 64 lowercase hex characters (64
// bytes), or khzb_ and 64 lowercase hex characters (69 bytes). Without (?m),
// $ is the end of the text, so a newline anywhere fails the match.
const (
	bindSecretHexLen      = 64
	bindSecretPrefixedLen = 69
)

var bindSecretRe = regexp.MustCompile(`^(khzb_)?[0-9a-f]{64}$`)

// readFull is io.ReadFull, a variable so a test can fail the read.
var readFull = io.ReadFull

// storedBindSecret is the bind secret a previous register stored at
// bindSecretPath: a readable regular file of exactly 64 lowercase hex
// characters, or of khzb_ and 64 lowercase hex characters. A missing,
// unreadable, irregular or malformed file gives "". lo changes no byte of
// the value. Every register call sends the value as bindSecret, so a re-run
// keeps the same cluster record. The deploy stages the bytes that this
// function returns and does not read the file again. The api refuses a
// malformed value, so lo never sends one.
//
// The file is opened once, without blocking (openNonblock: a FIFO in the
// slot would block a plain open), checked on the open file as a regular file,
// and read for at most 70 bytes: the count and the anchored pattern each
// refuse a longer file, and the cap bounds the read. A file shorter than the
// buffer ends the read with io.EOF or io.ErrUnexpectedEOF. Any other read
// error gives "".
func (c *Context) storedBindSecret(domain string) string {
	f, err := openNonblock(c.bindSecretPath(domain))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	buf := make([]byte, bindSecretPrefixedLen+1)
	n, err := readFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ""
	}
	if (n != bindSecretHexLen && n != bindSecretPrefixedLen) || !bindSecretRe.Match(buf[:n]) {
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

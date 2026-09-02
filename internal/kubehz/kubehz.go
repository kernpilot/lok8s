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
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose message was already printed in the bash
// implementation's own format ([error] … on stderr, or a plain echo). The
// caller exits non-zero without printing anything further.
var ErrHandled = errors.New("kubehz: handled")

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

	// Sleep is the wait seam (nil = time.Sleep): the hosted/space wait loops
	// and the deploy drains all wait through it.
	Sleep func(time.Duration)
	// Now is the clock seam (nil = time.Now) — the claim-nonce stamp.
	Now func() time.Time
	// Hostname is `hostname` (nil = os.Hostname).
	Hostname func() (string, error)
	// IsRoot is node::is_root (nil = euid == 0).
	IsRoot func() bool
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

func (c *Context) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
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
	return &http.Client{}
}

// ── verbose.sh helpers, bound to the context's stderr ────

func (c *Context) errorf(format string, a ...any) { ui.Errorf(c.errOut(), format, a...) }
func (c *Context) warnf(format string, a ...any)  { ui.Warnf(c.errOut(), format, a...) }
func (c *Context) debugf(format string, a ...any) { ui.Debugf(c.errOut(), format, a...) }

// echo writes one stdout line (bash `echo`).
func (c *Context) echo(format string, a ...any) {
	fmt.Fprintf(c.out(), format+"\n", a...)
}

// echoErr writes one stderr line (bash `echo … >&2`).
func (c *Context) echoErr(format string, a ...any) {
	fmt.Fprintf(c.errOut(), format+"\n", a...)
}

// requireHTTPS ports http::require_https: bearer tokens travel on these
// URLs, never over plain HTTP.
func (c *Context) requireHTTPS(url, label string) error {
	if !strings.HasPrefix(url, "https://") {
		c.errorf("%s must use HTTPS: %s", label, url)
		c.errorf("Plain HTTP is not allowed for security reasons")
		return ErrHandled
	}
	return nil
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

// trimNL mirrors `$(...)` command substitution: trailing newlines dropped.
func trimNL(s string) string { return strings.TrimRight(s, "\n") }

// clusterYAMLPath is ${PATH_CLUSTERS}/<domain>/cluster.lok8s.yaml.
func (c *Context) clusterYAMLPath(domain string) string {
	return c.Paths.Clusters + "/" + domain + "/cluster.lok8s.yaml"
}

// requireDomainSpec is the shared subcommand preamble: an active domain and
// its cluster.lok8s.yaml.
func (c *Context) requireDomainSpec(domain string) (string, error) {
	if domain == "" {
		c.errorf("No active domain. Use: lo use <domain>")
		return "", ErrHandled
	}
	cy := c.clusterYAMLPath(domain)
	if !fileExists(cy) {
		c.errorf("No cluster.lok8s.yaml for domain: %s", domain)
		return "", ErrHandled
	}
	return cy, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

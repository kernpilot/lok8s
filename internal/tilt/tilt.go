// Package tilt is the Go port of .lok8s/libs/tilt — Tilt lifecycle
// management (`lo tilt {up,ci,down,status,restart,preflight}`). Output and
// error strings are byte-identical to the argsh implementation; every
// external process (tilt, kill, pkill, kubectl via kapply) goes through the
// execx.Runner seam so tests never touch a live session.
package tilt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose message was already printed in the bash
// implementation's own [error] format; callers exit non-zero without
// printing anything further.
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// Context carries one tilt-command invocation.
type Context struct {
	Paths  *config.Paths
	Runner execx.Runner
	// Out/ErrOut are the command's stdout/stderr.
	Out    io.Writer
	ErrOut io.Writer
	// Stdin carries the rendered manifest for `lo tilt preflight`.
	Stdin io.Reader

	// StartDetached spawns the backgrounded `tilt up` (bash: `nohup tilt up
	// … > .tilt.nohup 2>&1 &`) and returns its pid. nil → the real detached
	// spawn (setsid, so the session survives our process tree the way nohup's
	// did). Tests inject a fake — a real Tilt must NEVER start under test.
	StartDetached func(port string) (int, error)

	// Preflighter runs the stuck-Terminating sweep (bash: kapply::preflight).
	// nil → kapply.NewApplier(...).Preflight. Tests inject a recorder,
	// mirroring the kapply::preflight stub in tilt_preflight_test.bats.
	Preflighter func(ctx context.Context, manifest string, args ...string) error
}

// exitCode maps a Runner error to the subprocess exit code (nil → 0, an
// *exec.ExitError or anything with ExitCode() → its code, else 1).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		return xe.ExitCode()
	}
	var ce interface{ ExitCode() int }
	if errors.As(err, &ce) {
		return ce.ExitCode()
	}
	return 1
}

// tiltfilePath is "${PATH_BASE}/Tiltfile" — plain concatenation, NOT a
// cleaned filepath.Join, so the pkill pattern built from it stays
// byte-identical to the bash one.
func (c *Context) tiltfilePath() string {
	return c.Paths.Base + "/Tiltfile"
}

// Port derives the deterministic Tilt web port from the active domain name
// (bash: tilt::port). This prevents port collisions when multiple lok8s
// projects run Tilt simultaneously. The port is hashed into the 10351-10499
// range (10350 is the Tilt default, intentionally avoided so a bare
// `tilt up` outside lok8s doesn't collide).
// Override: set TILT_PORT or spec.tilt.port in the cluster spec.
func (c *Context) Port() string {
	// Explicit override takes priority.
	if p := os.Getenv("TILT_PORT"); p != "" {
		return p
	}

	// Check cluster spec for spec.tilt.port. (The bash additionally gated
	// this on `command -v yq`; the Go port always reads the spec.)
	domainName := os.Getenv("DOMAIN_NAME")
	if domainName == "" {
		domainName = "lok8s.dev"
	}
	spec := c.Paths.Clusters + "/" + domainName + "/cluster.lok8s.yaml"
	if p := specTiltPort(spec); p != "" {
		return p
	}

	// Hash the domain name into port range 10351-10499.
	hash := posixCksum([]byte(domainName))
	return strconv.Itoa(int(hash%149) + 10351)
}

// specTiltPort reads .spec.tilt.port ("" when missing/null/unreadable —
// bash: yq -r '.spec.tilt.port // ""' with the "null" guard).
func specTiltPort(specPath string) string {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return ""
	}
	var n yaml.Node
	if yaml.Unmarshal(raw, &n) != nil {
		return ""
	}
	node := yamlPath(&n, "spec", "tilt", "port")
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return ""
	}
	if node.Value == "" || node.Value == "null" {
		return ""
	}
	return node.Value
}

// Running reports whether a Tilt apiserver is already answering on this
// project's port (bash: tilt::running). An authoritative check — it queries
// the live session, so it doesn't trust a possibly-stale .tilt.pid file.
func (c *Context) Running(ctx context.Context, port string) bool {
	err := c.Runner.Run(ctx, execx.Cmd{
		Name: "tilt", Args: []string{"get", "session", "--port", port},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	return err == nil
}

// Reload forces an already-running Tilt to re-execute its Tiltfile (bash:
// tilt::reload), so a re-run of `lo up` picks up new services / addons /
// regenerated artifacts without spawning a duplicate instance. This reloads
// CONFIG only (the (Tiltfile) resource) — workloads keep hot-reloading
// through their own live_update; we never `tilt trigger` a workload.
func (c *Context) Reload(ctx context.Context, port string) error {
	return c.Runner.Run(ctx, execx.Cmd{
		Name: "tilt", Args: []string{"trigger", "(Tiltfile)", "--port", port},
		Stdout: c.Out, Stderr: c.ErrOut,
	})
}

// TriggerResource re-deploys ONE resource through Tilt, so Tilt's own image
// injection applies (bash: tilt::trigger_resource). The rule above — "we
// never `tilt trigger` a workload" — is about Reload; this is the opposite
// case and is deliberate: a `hooks recreate` exists precisely to re-run a
// workload, and it MUST go through Tilt because the rendered artifact
// carries the declared image ref (`lok8s.local/<name>`) while the pushed
// image only exists under Tilt's content-hash tag.
func (c *Context) TriggerResource(ctx context.Context, name, port string) error {
	return c.Runner.Run(ctx, execx.Cmd{
		Name: "tilt", Args: []string{"trigger", name, "--port", port},
		Stdout: io.Discard, Stderr: io.Discard,
	})
}

// HasResource reports whether Tilt knows a resource by this name (bash:
// tilt::has_resource) — used to decide whether a rendered object can be
// re-deployed through Tilt at all.
func (c *Context) HasResource(ctx context.Context, name, port string) bool {
	err := c.Runner.Run(ctx, execx.Cmd{
		Name: "tilt", Args: []string{"get", "uiresource", name, "--port", port},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	return err == nil
}

// doctorKind is the `tilt doctor | grep 'Env: kind'` gate: stdout captured
// for the check, stderr passed through (the bash pipe only redirected
// stdout). Only the presence of the line matters — grep's status is the
// pipeline's.
func (c *Context) doctorKind(ctx context.Context) bool {
	var out strings.Builder
	_ = c.Runner.Run(ctx, execx.Cmd{
		Name: "tilt", Args: []string{"doctor"},
		Stdout: &out, Stderr: c.ErrOut,
	})
	return strings.Contains(out.String(), "Env: kind")
}

// Up spins up tilt, interactive and backgrounded (bash: tilt::up).
func (c *Context) Up(ctx context.Context) error {
	port := c.Port()
	os.Setenv("TILT_PORT", port)

	// Already running for this project? Don't race a duplicate onto the same
	// port — reload the Tiltfile so this `lo up` picks up new services/addons.
	if c.Running(ctx, port) {
		fmt.Fprintf(c.Out, "Tilt already running on http://localhost:%s — reloading Tiltfile\n", port)
		if err := c.Reload(ctx, port); err != nil {
			ui.Warnf(c.ErrOut, "Tiltfile reload trigger failed (Tilt still running on :%s)", port)
		}
		return nil
	}

	if !c.doctorKind(ctx) {
		ui.Errorf(c.ErrOut, "Did not recognize local kind environment.")
		return ErrHandled
	}
	pid := c.Paths.Base + "/.tilt"
	fmt.Fprintf(c.Out, "Tilt UI: http://localhost:%s\n", port)
	p, err := c.startDetached(port)
	if err != nil {
		return err
	}
	return os.WriteFile(pid+".pid", []byte(strconv.Itoa(p)+"\n"), 0o644)
}

// startDetached is the injectable spawn (see Context.StartDetached).
func (c *Context) startDetached(port string) (int, error) {
	if c.StartDetached != nil {
		return c.StartDetached(port)
	}
	tiltPath, ok := execx.Look(c.Paths, "tilt")
	if !ok {
		return 0, fmt.Errorf("tilt: executable not found")
	}
	nohup, err := os.OpenFile(c.Paths.Base+"/.tilt.nohup", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer func() { _ = nohup.Close() }()
	cmd := exec.Command(tiltPath, "up", "--port="+port, "--file="+c.tiltfilePath())
	cmd.Stdout = nohup
	cmd.Stderr = nohup
	// The bash contract is `nohup … &`: Tilt must survive the parent. A new
	// session (setsid) detaches it from our process group AND controlling
	// terminal, so neither our exit nor a SIGHUP to our group reaps it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	p := cmd.Process.Pid
	_ = cmd.Process.Release()
	return p, nil
}

// CI is the headless bring-up for dev/CI/e2e (bash: tilt::ci). Unlike Up
// (which backgrounds an interactive `tilt up` and returns immediately),
// `tilt ci` runs in the foreground: it builds + deploys every resource,
// waits for readiness, then exits 0 on success / non-zero on failure — the
// returned code is the real build+deploy result. timeout is passed straight
// through to `tilt ci` (tilt's default is 30m).
func (c *Context) CI(ctx context.Context, timeout string) (int, error) {
	if !c.doctorKind(ctx) {
		ui.Errorf(c.ErrOut, "Did not recognize local kind environment.")
		return 1, ErrHandled
	}
	port := c.Port()
	os.Setenv("TILT_PORT", port)
	args := []string{"ci", "--port=" + port, "--file=" + c.tiltfilePath()}
	if timeout != "" {
		args = append(args, "--timeout", timeout)
	}
	fmt.Fprintf(c.Out, "Tilt CI (headless) on port %s — building, deploying, waiting for readiness...\n", port)
	// Foreground: tilt ci exits 0 only once all resources are ready,
	// non-zero otherwise. Returning its status lets callers gate on it.
	err := c.Runner.Run(ctx, execx.Cmd{Name: "tilt", Args: args, Stdout: c.Out, Stderr: c.ErrOut})
	return exitCode(err), nil
}

// pkillEscape escapes the path's regex metacharacters exactly like the bash
// `sed 's/[][\\.*^$()+?{}|]/\\&/g'` — the -f pattern is a REGEX, and an
// unlucky path must not broaden the match onto unrelated sessions.
func pkillEscape(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch r {
		case ']', '[', '\\', '.', '*', '^', '$', '(', ')', '+', '?', '{', '}', '|':
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// Down spins down tilt (bash: tilt::down). The pkill fallback is scoped to
// THIS project's tilt (match its --file=): a bare `pkill tilt` kills other
// projects' sessions on the same machine.
func (c *Context) Down(ctx context.Context, force bool) error {
	match := "[t]ilt up.*--file=" + pkillEscape(c.tiltfilePath())
	pidFile := c.Paths.Base + "/.tilt.pid"
	pkill := func() {
		_ = c.Runner.Run(ctx, execx.Cmd{Name: "pkill", Args: []string{"-f", match},
			Stdout: c.Out, Stderr: c.ErrOut})
	}
	if _, err := os.Stat(pidFile); err == nil {
		pid := ""
		if raw, err := os.ReadFile(pidFile); err == nil {
			pid = strings.TrimRight(string(raw), "\n")
		}
		killed := false
		if pid != "" {
			killErr := c.Runner.Run(ctx, execx.Cmd{Name: "kill", Args: []string{pid},
				Stdout: c.Out, Stderr: io.Discard})
			killed = killErr == nil
		}
		if !killed {
			pkill()
		}
	} else if force {
		pkill()
	}
	_ = os.Remove(pidFile)
	return nil
}

// Status runs `tilt doctor` with output passed through (bash: tilt::status)
// and returns its exit code.
func (c *Context) Status(ctx context.Context) int {
	err := c.Runner.Run(ctx, execx.Cmd{Name: "tilt", Args: []string{"doctor"},
		Stdout: c.Out, Stderr: c.ErrOut})
	return exitCode(err)
}

// Restart restarts tilt (bash: tilt::restart — tilt::down; tilt::up).
func (c *Context) Restart(ctx context.Context, force bool) error {
	if err := c.Down(ctx, force); err != nil {
		return err
	}
	return c.Up(ctx)
}

// yamlPath walks nested mapping keys, nil when absent.
func yamlPath(n *yaml.Node, keys ...string) *yaml.Node {
	cur := yamlDeref(n)
	for _, key := range keys {
		cur = yamlMapGet(cur, key)
		if cur == nil {
			return nil
		}
	}
	return cur
}

func yamlDeref(n *yaml.Node) *yaml.Node {
	for n != nil && (n.Kind == yaml.DocumentNode || n.Kind == yaml.AliasNode) {
		if n.Kind == yaml.DocumentNode {
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
			continue
		}
		n = n.Alias
	}
	return n
}

func yamlMapGet(n *yaml.Node, key string) *yaml.Node {
	n = yamlDeref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return yamlDeref(n.Content[i+1])
		}
	}
	return nil
}

// defaultPreflighter builds the kapply sweep bound to this context.
func (c *Context) preflight(ctx context.Context, manifest string, args ...string) error {
	if c.Preflighter != nil {
		return c.Preflighter(ctx, manifest, args...)
	}
	return kapply.NewApplier(c.Runner, c.Out, c.ErrOut).Preflight(ctx, manifest, args...)
}

// PreflightConfig reads the domain's spec.tilt.preflight from
// cluster.lok8s.yaml (bash: tilt::preflight_config; same home as
// spec.tilt.port). Returns enabled, age, crds, crd-allow-csv with "-"
// marking an unset field. Missing spec file (deploy-only domains) or
// unreadable YAML yields all-defaults.
func (c *Context) PreflightConfig(domainName string) (enabled, age, crds, allow string) {
	enabled, age, crds, allow = "true", "-", "-", "-"
	spec := c.Paths.Clusters + "/" + domainName + "/cluster.lok8s.yaml"
	raw, err := os.ReadFile(spec)
	if err != nil {
		return
	}
	var n yaml.Node
	if yaml.Unmarshal(raw, &n) != nil {
		return
	}
	// NOT a `// true` alternative: yq's operator treats an explicit `false`
	// as empty and would read a disable as true. Only a literal false
	// disables — on .enabled, or the scalar shorthand `preflight: false`.
	pf := yamlPath(&n, "spec", "tilt", "preflight")
	en := yamlMapGet(pf, "enabled")
	if scalarIsFalse(en) || scalarIsFalse(pf) {
		enabled = "false"
	}
	if v := scalarValue(yamlMapGet(pf, "age")); v != "" {
		age = v
	}
	if v := scalarValue(yamlMapGet(pf, "crds")); v != "" {
		crds = v
	}
	if list := yamlMapGet(pf, "crdForceAllow"); list != nil && list.Kind == yaml.SequenceNode {
		var items []string
		for _, item := range list.Content {
			it := yamlDeref(item)
			if it != nil && it.Kind == yaml.ScalarNode {
				items = append(items, it.Value)
			}
		}
		if joined := strings.Join(items, ","); joined != "" {
			allow = joined
		}
	}
	return
}

// scalarIsFalse mirrors the bash `grep -cx false` over yq -r output: only a
// node rendering as the bare line "false" counts.
func scalarIsFalse(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Value == "false"
}

// scalarValue is the yq `// "-"` read: "" for missing/null/false and for
// non-scalar shapes (which yq would render multi-line and the TSV read
// would truncate anyway).
func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	if n.Value == "false" || n.Value == "" {
		return ""
	}
	return n.Value
}

// Preflight is called by the Tilt extension right after it renders the
// domain (local('lo tilt preflight', stdin=artifacts)), BEFORE any k8s_yaml
// applies: force-clear manifest objects stuck Terminating so Tilt's apply
// doesn't spin on "currently being deleted" until its upsert timeout and
// fail the build (bash: tilt::preflight). The sweep itself is
// kapply.Preflight; GATING + policy live here, with precedence CLI flag >
// cluster spec > kapply env defaults (the LOK8S_* kill switches outrank
// everything):
//   - LOK8S_PREFLIGHT=0 (env) or spec.tilt.preflight.enabled=false disables
//     the sweep entirely;
//   - only the `lo` (local kind) driver is force-cleared by default — remote
//     clusters need an explicit LOK8S_FORCE_CLEAR_TERMINATING=1. Clearing a
//     finalizer skips its controller's cleanup: fine to lose on a throwaway
//     kind cluster, a data-loss footgun on a real one;
//   - stuck-CRD policy (drain/skip/force + allowlist) comes from the spec
//     unless flags override.
//
// Always returns nil — a preflight must never brick the `tilt up` it
// protects.
func (c *Context) Preflight(ctx context.Context, domainArg, age, crds, crdAllow string) error {
	drain := func() { _, _ = io.Copy(io.Discard, c.Stdin) }

	if v := os.Getenv("LOK8S_PREFLIGHT"); v == "0" || v == "false" {
		drain()
		return nil
	}
	resolved := domain.Resolve(domainArg, c.Paths.Clusters, c.ErrOut)
	driver, err := domain.Driver(c.Paths.Clusters, resolved)
	if err != nil {
		driver = ""
	}
	if driver != "lo" && os.Getenv("LOK8S_FORCE_CLEAR_TERMINATING") == "" {
		label := driver
		if label == "" {
			label = "unknown"
		}
		ui.Warnf(c.ErrOut, "preflight: '%s' uses the '%s' driver — not force-clearing stuck objects on a non-kind cluster (set LOK8S_FORCE_CLEAR_TERMINATING=1 to override)", resolved, label)
		drain()
		return nil
	}

	enabled, specAge, specCrds, specAllow := c.PreflightConfig(resolved)
	if enabled == "false" || enabled == "0" {
		drain()
		return nil
	}

	// CLI flag > spec; "-" is the spec's unset marker. Unset options are NOT
	// passed down — kapply.Preflight owns the real defaults (30 / drain).
	if age == "-" {
		age = ""
	}
	if crds == "-" {
		crds = ""
	}
	if crdAllow == "-" {
		crdAllow = ""
	}
	if specAge != "-" && age == "" {
		age = specAge
	}
	if specCrds != "-" && crds == "" {
		crds = specCrds
	}
	if specAllow != "-" && crdAllow == "" {
		crdAllow = specAllow
	}
	var sweep []string
	if age != "" {
		sweep = append(sweep, "--age", age)
	}
	if crds != "" {
		sweep = append(sweep, "--crds", crds)
	}
	if crdAllow != "" {
		sweep = append(sweep, "--crd-allow", crdAllow)
	}
	manifest, _ := io.ReadAll(c.Stdin)
	return c.preflight(ctx, string(manifest), sweep...)
}

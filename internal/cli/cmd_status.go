package cli

// lo status — cluster and domain status reporting. Go port of
// .lok8s/libs/status (main::status + status::all/cluster/nodes/inventory/
// targets/tilt) and utils/targets.sh targets::discover.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("status", newStatusCommand) }

// statusRequestTimeout is the last argument of each kubectl call of the
// report (bash: status::_kubectl; the -o json report too). After `lo down`
// the kubeconfig can point to an API server that does not answer. Without a
// request timeout, kubectl can wait about two minutes before it stops.
const statusRequestTimeout = "--request-timeout=5s"

// The request timeout bounds one request, not the whole call: a `kubectl
// get` can make discovery requests before the GET and retry them, and an
// exec credential plugin of the kubeconfig runs outside the flag. So the
// report first sends one /readyz probe (statusProbeArgs: no discovery).
// When the probe gets no HTTP answer, the report skips both reads
// (statusReachable). Else each read runs with a bound for the whole call:
// statusReadBound, as a context deadline. The probe has the same bound.
// Each call runs as its own process group (execx.Cmd.ProcessGroup): at the
// bound the group gets SIGTERM, and SIGKILL after statusReadGrace, so a
// credential plugin stops too. The bash twin uses timeout(1) with the same
// 10 s and 2 s (status::_kubectl). statusReadBound is a variable so that a
// test can make it short.
const statusReadGrace = 2 * time.Second

var statusReadBound = 10 * time.Second

// statusProbeArgs is the /readyz probe of the report (bash:
// status::reachable).
var statusProbeArgs = []string{"get", "--raw", "/readyz"}

// statusKubectl runs one read of the report (bash: status::_kubectl):
// statusKubectlCall with stderr discarded.
func statusKubectl(ctx context.Context, r execx.Runner, stdout io.Writer, args ...string) error {
	return statusKubectlCall(ctx, r, stdout, io.Discard, args...)
}

// statusKubectlCall runs one kubectl call of the report (bash:
// status::_kubectl): kubectl <args> --request-timeout=5s, with an empty
// stdin and the bound of statusReadBound. A call that the bound stopped is
// an error, also when kubectl handled the signal and exited 0. The empty
// stdin is there because the call runs as its own process group, which is
// not the foreground group of a terminal: a credential plugin that reads
// the terminal would stop (SIGTTIN) until the bound. With an empty stdin it
// reads end of file at once.
func statusKubectlCall(ctx context.Context, r execx.Runner, stdout, stderr io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, statusReadBound)
	defer cancel()
	err := r.Run(ctx, execx.Cmd{
		Name:         "kubectl",
		Args:         append(slices.Clone(args), statusRequestTimeout),
		Stdin:        strings.NewReader(""),
		Stdout:       stdout,
		Stderr:       stderr,
		WaitDelay:    statusReadGrace,
		ProcessGroup: true,
	})
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// statusKubectlOK is the gate of the kubectl sections (bash: `command -v
// kubectl` and `[[ -f "${KUBECONFIG}" ]]`).
func statusKubectlOK(deps statusDeps) bool {
	hasKubectl := deps.hasKubectl
	if hasKubectl == nil {
		hasKubectl = func() bool { _, ok := execx.Look(deps.paths, "kubectl"); return ok }
	}
	return hasKubectl() && fsutil.FileExists(os.Getenv("KUBECONFIG"))
}

// statusReachable is the /readyz probe (bash: status::reachable): true when
// the API server gives an HTTP answer to it, of any status. A 401, a 403 (a
// cluster that removed the system:public-info-viewer grant), a 404 (an
// older API server, or a proxy in front of it) or a 500 (an API server that
// is not ready) fails the probe, but the server answers, so the reads run,
// each with its bound. Only a probe with no HTTP answer skips the reads: a
// connection, DNS or TLS error, the request timeout, or the bound.
func statusReachable(ctx context.Context, deps statusDeps) bool {
	var stderr bytes.Buffer
	err := statusKubectlCall(ctx, deps.runner, io.Discard, &stderr, statusProbeArgs...)
	return err == nil || statusHTTPAnswer(stderr.String())
}

// statusHTTPAnswer reports whether stderr, the stderr of a kubectl call
// that failed, holds the form that kubectl prints for an HTTP answer that
// is not 2xx (bash: status::_http_answer). kubectl's StandardErrorMessage
// (k8s.io/kubectl/pkg/cmd/util) starts the line with "Error from server"
// for such an answer, and with "error: You must be logged in to the
// server" for a 401. An error with no HTTP answer has other forms:
// "Unable to connect to the server: …" (DNS, TLS, the request timeout) and
// "The connection to the server … was refused …".
func statusHTTPAnswer(stderr string) bool {
	for line := range strings.Lines(stderr) {
		if strings.HasPrefix(line, "Error from server") || strings.HasPrefix(line, "error: You must be logged in to the server") {
			return true
		}
	}
	return false
}

// statusDeps are the report's seams.
type statusDeps struct {
	paths  *config.Paths
	runner execx.Runner
	// dispatchStatus is provision::dispatch_status with BOTH streams on the
	// report (bash: `2>&1 || true`).
	dispatchStatus func(ctx context.Context, out io.Writer, domainName string) error
	// hasKubectl is `command -v kubectl` (nil = execx.Look).
	hasKubectl func() bool
	// pidAlive is `kill -0 <pid>` (nil = syscall.Kill(pid, 0)).
	pidAlive func(pid string) bool
}

func newStatusCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var format func() (string, error)
	cmd := argshFlagErrors(&cobra.Command{
		Use:          "status",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         argshNoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			disp := newDispatcher(cmd, paths)
			deps := statusDeps{
				paths:  paths,
				runner: disp.Runner,
				dispatchStatus: func(ctx context.Context, out io.Writer, domainName string) error {
					disp.Stdout, disp.Stderr = out, out
					return disp.DispatchStatus(ctx, domainName)
				},
			}
			f, err := format()
			if err != nil {
				return err
			}
			if f != outputText {
				return writeOutput(cmd.OutOrStdout(), f, gatherStatus(cmd.Context(), deps, d))
			}
			runStatus(cmd.Context(), cmd.OutOrStdout(), deps, d)
			return nil
		},
	})
	format = addOutputFlag(cmd)
	return cmd
}

// runStatus is status::all: every section in order, each fail-soft.
func runStatus(ctx context.Context, out io.Writer, deps statusDeps, domainName string) {
	ui.Title(out, "=== Domain: "+domainName+" ===")
	fmt.Fprintln(out)

	// Resolve the driver kind so the Tilt section only runs for `lo`
	// clusters.
	kind := ""
	if k, err := domain.SpecDriver(filepath.Join(deps.paths.Clusters, domainName, "cluster.lok8s.yaml"), ""); err == nil {
		kind = k
	}

	// ── Cluster ──
	ui.Section(out, "Cluster")
	_ = deps.dispatchStatus(ctx, out, domainName)
	fmt.Fprintln(out)

	kubectlOK := statusKubectlOK(deps)
	// One /readyz probe for both kubectl reads.
	reachable := kubectlOK && statusReachable(ctx, deps)

	// ── Nodes ── kubectl writes into a buffer, not into out: a process
	// group that is not the foreground group of a terminal can stop when it
	// writes to it (stty tostop). The bytes are the same as the bash twin
	// prints, also those of a call that failed.
	if kubectlOK {
		ui.Section(out, "Nodes")
		read := false
		var nodes bytes.Buffer
		if reachable {
			read = statusKubectl(ctx, deps.runner, &nodes, "get", "nodes", "-o", "wide") == nil
		}
		_, _ = out.Write(nodes.Bytes())
		if !read {
			fmt.Fprintln(out, "  (not reachable)")
		}
		fmt.Fprintln(out)
	}

	// ── ClusterInventory ── read-only, fail-soft: silently skipped unless
	// the cluster is reachable and the CR exists.
	if reachable {
		var inv strings.Builder
		err := statusKubectl(ctx, deps.runner, &inv, "get", "clusterinventories.lok8s.dev", "cluster", "-o", "json")
		if err == nil && strings.TrimSpace(inv.String()) != "" {
			ui.Section(out, "Inventory (ClusterInventory/cluster)")
			if lines, ok := renderInventory(inv.String()); ok {
				for _, l := range lines {
					fmt.Fprintln(out, l)
				}
			} else {
				fmt.Fprintln(out, "  (unreadable inventory)")
			}
			fmt.Fprintln(out)
		}
	}

	// ── Targets ── enumeration only; `lo build` composes the domain into
	// ONE clusters/<domain>/artifacts.yaml.
	ui.Section(out, "Targets")
	domainDir := filepath.Join(deps.paths.Clusters, domainName)
	targets := discoverTargets(domainDir)
	if len(targets) > 0 {
		for _, t := range targets {
			fmt.Fprintf(out, "  %s\n", t)
		}
	} else {
		fmt.Fprintln(out, "  No targets directory")
	}
	if fsutil.FileExists(filepath.Join(domainDir, "artifacts.yaml")) {
		fmt.Fprintln(out, "  artifacts.yaml: built")
	} else {
		fmt.Fprintln(out, "  artifacts.yaml: not built (run 'lo build')")
	}
	fmt.Fprintln(out)

	// ── Tilt ── only meaningful for `lo` (kind) clusters; liveness comes
	// from lok8s's own PID file (written by tilt up), not a tilt API call.
	if kind == "lo" {
		ui.Section(out, "Tilt")
		pid := ""
		if raw, err := os.ReadFile(filepath.Join(deps.paths.Base, ".tilt.pid")); err == nil {
			pid = strings.TrimRight(string(raw), "\n")
		}
		alive := deps.pidAlive
		if alive == nil {
			alive = pidAlive
		}
		if pid != "" && alive(pid) {
			fmt.Fprintf(out, "  running (pid %s)\n", pid)
		} else {
			fmt.Fprintln(out, "  (not running)")
		}
	}
}

// pidAlive is `kill -0 <pid>`: true only for a numeric pid the kernel
// accepts a signal for.
func pidAlive(pid string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(pid))
	if err != nil || n <= 0 {
		return false
	}
	return syscall.Kill(n, 0) == nil
}

// discoverTargets is targets::discover without a request list: the
// directory names under clusters/<domain>/targets/, glob order.
func discoverTargets(domainDir string) []string {
	entries, err := os.ReadDir(filepath.Join(domainDir, "targets"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			// `*/` also matches a symlink to a directory.
			if info, err := os.Stat(filepath.Join(domainDir, "targets", e.Name())); err != nil || !info.IsDir() {
				continue
			}
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// renderInventory is the jq program in status::inventory, line for line.
// ok=false stands for a jq error (→ "(unreadable inventory)").
func renderInventory(raw string) ([]string, bool) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, false
	}
	spec, _ := doc["spec"].(map[string]any)
	status, _ := doc["status"].(map[string]any)
	get := func(m map[string]any, k string) any {
		if m == nil {
			return nil
		}
		return m[k]
	}
	// jq: `x // "?"` — null/false → "?"; string interpolation prints
	// scalars jq-style.
	alt := func(v any, a string) string {
		if v == nil || v == false {
			return a
		}
		return jqString(v)
	}
	truthy := func(v any) bool { return v != nil && v != false }

	var lines []string
	lines = append(lines, "  lok8s:      "+alt(get(spec, "lok8sVersion"), "?"))
	driverLine := "  driver:     " + alt(get(spec, "kind"), "?")
	if p := get(spec, "provider"); truthy(p) {
		ps, ok := p.(string)
		if !ok {
			return nil, false // jq: string + non-string → error
		}
		driverLine += " · " + ps
	}
	lines = append(lines, driverLine)
	if v := get(spec, "kubernetesVersion"); truthy(v) {
		lines = append(lines, "  kubernetes: "+jqString(v))
	}
	hash := get(spec, "specHash")
	hashStr := "?"
	if truthy(hash) {
		s, ok := hash.(string)
		if !ok {
			return nil, false // jq: .[0:12] on a non-string/array → error
		}
		hashStr = s
	}
	if r := []rune(hashStr); len(r) > 12 {
		hashStr = string(r[:12])
	}
	lines = append(lines, "  specHash:   "+hashStr+"…")
	lines = append(lines, "  renderedAt: "+alt(get(spec, "renderedAt"), "?"))

	var addons []any
	if a := get(spec, "addons"); truthy(a) {
		list, ok := a.([]any)
		if !ok {
			return nil, false
		}
		addons = list
	}
	lines = append(lines, "  addons:     "+strconv.Itoa(len(addons)))
	for _, a := range addons {
		m, ok := a.(map[string]any)
		if !ok {
			return nil, false // jq: .name on a non-object → error
		}
		l := "    " + jqString(m["name"])
		if cv := m["chartVersion"]; truthy(cv) {
			s, ok := cv.(string)
			if !ok {
				return nil, false
			}
			l += " " + s
		}
		if c := m["category"]; truthy(c) {
			s, ok := c.(string)
			if !ok {
				return nil, false
			}
			l += " (" + s + ")"
		}
		lines = append(lines, l)
	}
	if lr := get(status, "lastReported"); truthy(lr) {
		lines = append(lines, "  agent:      last reported "+jqString(lr))
	}
	return lines, true
}

// jqString prints a decoded JSON value the way jq's string interpolation
// does: strings raw, null as "null", everything else as compact JSON.
func jqString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

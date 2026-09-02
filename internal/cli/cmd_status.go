package cli

// lo status — cluster and domain status reporting. Go port of
// .lok8s/libs/status (main::status + status::all/cluster/nodes/inventory/
// targets/tilt) and utils/targets.sh targets::discover.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
)

func init() { registerPorted("status", newStatusCommand) }

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
	return argshFlagErrors(&cobra.Command{
		Use:          "status",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "too many arguments: %s", args[0])
			}
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
			runStatus(cmd.Context(), cmd.OutOrStdout(), deps, d)
			return nil
		},
	})
}

// runStatus is status::all: every section in order, each fail-soft.
func runStatus(ctx context.Context, out io.Writer, deps statusDeps, domainName string) {
	fmt.Fprintf(out, "=== Domain: %s ===\n\n", domainName)

	// Resolve the driver kind so the Tilt section only runs for `lo`
	// clusters.
	kind := ""
	if k, err := domain.SpecDriver(filepath.Join(deps.paths.Clusters, domainName, "cluster.lok8s.yaml"), ""); err == nil {
		kind = k
	}

	// ── Cluster ──
	fmt.Fprintln(out, "--- Cluster ---")
	_ = deps.dispatchStatus(ctx, out, domainName)
	fmt.Fprintln(out)

	hasKubectl := deps.hasKubectl
	if hasKubectl == nil {
		hasKubectl = func() bool { _, ok := execx.Look(deps.paths, "kubectl"); return ok }
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	kubectlOK := hasKubectl() && fileExists(kubeconfig)

	// ── Nodes ──
	if kubectlOK {
		fmt.Fprintln(out, "--- Nodes ---")
		if err := deps.runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"get", "nodes", "-o", "wide"}, Stdout: out, Stderr: io.Discard}); err != nil {
			fmt.Fprintln(out, "  (not reachable)")
		}
		fmt.Fprintln(out)
	}

	// ── ClusterInventory ── read-only, fail-soft: silently skipped unless
	// the cluster is reachable and the CR exists.
	if kubectlOK {
		var inv strings.Builder
		err := deps.runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: []string{"get", "clusterinventories.lok8s.dev", "cluster", "-o", "json"}, Stdout: &inv, Stderr: io.Discard})
		if err == nil && strings.TrimSpace(inv.String()) != "" {
			fmt.Fprintln(out, "--- Inventory (ClusterInventory/cluster) ---")
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
	fmt.Fprintln(out, "--- Targets ---")
	domainDir := filepath.Join(deps.paths.Clusters, domainName)
	targets := discoverTargets(domainDir)
	if len(targets) > 0 {
		for _, t := range targets {
			fmt.Fprintf(out, "  %s\n", t)
		}
	} else {
		fmt.Fprintln(out, "  No targets directory")
	}
	if fileExists(filepath.Join(domainDir, "artifacts.yaml")) {
		fmt.Fprintln(out, "  artifacts.yaml: built")
	} else {
		fmt.Fprintln(out, "  artifacts.yaml: not built (run 'lo build')")
	}
	fmt.Fprintln(out)

	// ── Tilt ── only meaningful for `lo` (kind) clusters; liveness comes
	// from lok8s's own PID file (written by tilt up), not a tilt API call.
	if kind == "lo" {
		fmt.Fprintln(out, "--- Tilt ---")
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

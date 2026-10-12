package bootstrap

// dryrun.go — `--dry-run`, `--render-dir`, `--no-diff` and `--only` for
// `lo bootstrap` and `lo provision --bootstrap` (bash: bootstrap::_run_dry,
// bootstrap::_dry_run_one, bootstrap::_select_only).
//
// A real run re-applies every spec.bootstrap entry with `kubectl apply
// --server-side --force-conflicts`, which silently overwrites live drift in
// unrelated addons. The dry run renders every entry exactly as the real run
// would (the same value stack, envsubst, khelm, patches and ordering) and
// applies NOTHING: it writes the manifests into the render dir and asks the
// API server for a server-side diff (`kubectl diff --server-side
// --force-conflicts`, a read-only dry run on the server) per entry.
//
// The entries run SEQUENTIALLY in a deterministic topological order (the
// lowest spec index among the ready entries first), not through the
// parallel scheduler: piped output is a contract, so the report must come
// out in the same order on every run and on both implementations.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Options are the run modes a CLI invocation selects (the flags of
// `lo bootstrap` and `lo provision --bootstrap`). The zero value is the
// plain apply.
type Options struct {
	// DryRun renders every entry and applies nothing. The exit code is 0
	// when nothing would change, 1 when something would, 2 on an error.
	DryRun bool
	// RenderDir receives one file per entry: NN-<entry>.yaml with the
	// rendered manifests and, unless NoDiff, NN-<entry>.diff with the
	// server-side diff. Created when missing; existing files are
	// overwritten, nothing else in it is touched. Dry run only.
	RenderDir string
	// NoDiff skips the kubectl diff of a dry run: render only, no cluster
	// access at all (the hosted-cluster probe is skipped too).
	NoDiff bool
	// Only restricts the run (real or dry) to the named entries, matched
	// against the entry name (the `name:` override when set) or the addon
	// name. dependsOn is NOT expanded: a dependency that is not named is
	// not applied. An unknown name is an error that lists the valid names.
	Only []string
}

// ErrDryRunDiffers is the dry run's "something would change" exit (rc 1).
// The per-entry lines already said what, so it prints nothing.
var ErrDryRunDiffers = ui.Handled(fmt.Errorf("bootstrap dry-run: entries would change"))

// dryRunStatus is one entry's verdict, the status word of its report line.
type dryRunStatus string

const (
	dryUnchanged dryRunStatus = "unchanged" // diff rc 0
	dryDiffers   dryRunStatus = "differs"   // diff rc 1
	dryRendered  dryRunStatus = "rendered"  // --no-diff: rendered, not diffed
	drySkipped   dryRunStatus = "skipped"   // the real run would skip it too
	dryError     dryRunStatus = "error"     // render or diff failed
)

// dryResult is one entry's dry-run outcome.
type dryResult struct {
	status dryRunStatus
	// changed and added are the objects the diff reports as modified and
	// as new; counted is false when the diff text carried no file headers
	// (an external diff tool), so the counts are not printed.
	changed, added int
	counted        bool
}

// selectOnly keeps the entries named in only (bash: bootstrap::_select_only).
// A name matches the entry identity (the `name:` override when set, else
// the basename / map key) or the addon directory's name, so a renamed
// framework addon answers to both. The sub-DAG keeps only the edges among
// the kept entries: dependsOn is not expanded.
func selectOnly(stderr io.Writer, nodes []*node, only []string) ([]*node, error) {
	keep := make([]bool, len(nodes))
	for _, want := range only {
		found := false
		for i, nd := range nodes {
			if nd.entry.Name == want || filepath.Base(nd.entry.Dir) == want {
				keep[i] = true
				found = true
			}
		}
		if !found {
			return nil, planError(stderr, "bootstrap: --only: unknown entry '%s' (valid: %s)", want, strings.Join(entryNames(nodes), ", "))
		}
	}
	return subset(nodes, keep), nil
}

// entryNames lists the entry identities in spec order, each once.
func entryNames(nodes []*node) []string {
	var names []string
	seen := map[string]bool{}
	for _, nd := range nodes {
		if !seen[nd.entry.Name] {
			seen[nd.entry.Name] = true
			names = append(names, nd.entry.Name)
		}
	}
	return names
}

// subset builds fresh scheduler nodes for the kept entries, with the
// effective edges restricted to the kept set, and re-derives the per-entry
// scheduler fields. A subset of a DAG is a DAG, so no cycle check.
func subset(nodes []*node, keep []bool) []*node {
	remap := map[int]int{}
	var out []*node
	for i, nd := range nodes {
		if keep[i] {
			remap[i] = len(out)
			out = append(out, &node{entry: nd.entry, deps: map[int]bool{}})
		}
	}
	for i, nd := range nodes {
		if !keep[i] {
			continue
		}
		for pi := range nd.deps {
			if j, ok := remap[pi]; ok {
				out[remap[i]].deps[j] = true
			}
		}
	}
	deriveEdges(out)
	return out
}

// topoOrder is the sequential dry-run order: the lowest spec index among
// the entries whose effective deps are done, repeated (the order the
// scheduler's launch loop takes with one slot and no failures).
func topoOrder(nodes []*node) []int {
	done := make([]bool, len(nodes))
	order := make([]int, 0, len(nodes))
	for len(order) < len(nodes) {
		picked := -1
		for i, nd := range nodes {
			if done[i] {
				continue
			}
			ready := true
			for _, d := range nd.edeps {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				picked = i
				break
			}
		}
		if picked < 0 {
			break // unreachable after planDAG's cycle check
		}
		done[picked] = true
		order = append(order, picked)
	}
	return order
}

// runDryRun renders (and diffs) every entry in topological order and
// prints one report line per entry plus a summary (bash:
// bootstrap::_run_dry). Off a terminal the lines are the plain contract;
// on a terminal the status word is painted.
func (e *Engine) runDryRun(ctx context.Context, nodes []*node, base Job) error {
	stdout, stderr := e.stdout(), e.stderr()
	order := topoOrder(nodes)
	width := max(len(strconv.Itoa(len(order))), 2)
	if e.Options.RenderDir != "" {
		if err := os.MkdirAll(e.Options.RenderDir, 0o750); err != nil {
			return e.errorf("bootstrap: --render-dir: %v", err)
		}
	}
	counts := map[dryRunStatus]int{}
	for pos, i := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		nd := nodes[i]
		job := base
		job.Name, job.Dir, job.Inline, job.EnvLines = nd.entry.Name, nd.entry.Dir, nd.entry.Inline, nd.entry.EnvLines
		// Never a wait, never a heal: a dry run reads, so Force is dropped
		// on purpose even when the caller passed --force.
		job.Wait, job.Force, job.NonInteractive = false, false, true
		stem := fmt.Sprintf("%0*d-%s", width, pos+1, nd.entry.Name)
		res := e.dryRunOne(ctx, job, stem)
		counts[res.status]++
		fmt.Fprintf(stdout, "bootstrap dry-run: %s: %s\n", stem, res.render(ui.For(stdout)))
	}
	fmt.Fprintf(stdout, "bootstrap dry-run: entries=%d unchanged=%d differs=%d rendered=%d skipped=%d errors=%d\n",
		len(order), counts[dryUnchanged], counts[dryDiffers], counts[dryRendered], counts[drySkipped], counts[dryError])
	if e.Options.RenderDir != "" {
		fmt.Fprintf(stderr, "bootstrap dry-run: files under %s\n", e.Options.RenderDir)
	}
	switch {
	case counts[dryError] > 0:
		return &driver.ExitError{Code: 2, Err: ui.Handled(fmt.Errorf("bootstrap dry-run: %d entries failed", counts[dryError]))}
	case counts[dryDiffers] > 0:
		return ErrDryRunDiffers
	}
	return nil
}

// render is the report's status field: the word, then `changed=N new=M`
// for a diff that was counted.
func (r dryResult) render(s ui.Style) string {
	word := string(r.status)
	switch r.status {
	case dryUnchanged:
		word = s.Dim(word)
	case dryDiffers:
		word = s.Warn(word)
	case dryError:
		word = s.Bold(word)
	}
	if r.status == dryDiffers && r.counted {
		return fmt.Sprintf("%s changed=%d new=%d", word, r.changed, r.added)
	}
	return word
}

// dryRunOne renders ONE entry and diffs it against the cluster (bash:
// bootstrap::_dry_run_one). The skips are the real run's (the hosted
// platform-owned addons, the KubeOne cilium/ccm defer), so the report
// names what the real run would not touch either.
func (e *Engine) dryRunOne(ctx context.Context, job Job, stem string) dryResult {
	stderr := e.stderr()
	if reason, skip := e.skipReason(job); skip {
		fmt.Fprintln(stderr, reason)
		return dryResult{status: drySkipped}
	}
	ui.DebugTo(stderr, "bootstrap: rendering %s", job.Name)
	rendered, err := addons.Render(ctx, e.Paths, e.Runner, stderr, job.Dir, job.Kind, job.Provider, job.Inline, jobEnv(job))
	if err != nil {
		ui.ErrorTo(stderr, "bootstrap: render failed for %s", job.Name)
		return dryResult{status: dryError}
	}
	// One trailing newline, like the bash `printf '%s\n' "${rendered}"`
	// of a command substitution: the files must match byte for byte.
	rendered = strings.TrimRight(rendered, "\n") + "\n"
	if e.Options.RenderDir != "" {
		if err := os.WriteFile(filepath.Join(e.Options.RenderDir, stem+".yaml"), []byte(rendered), 0o600); err != nil {
			ui.ErrorTo(stderr, "bootstrap: --render-dir: %v", err)
			return dryResult{status: dryError}
		}
	}
	if e.Options.NoDiff {
		return dryResult{status: dryRendered}
	}

	var out, errOut strings.Builder
	err = e.Runner.Run(ctx, execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", job.Kubeconfig, "diff", "--server-side", "--force-conflicts", "-f", "-"},
		Stdin:  strings.NewReader(rendered),
		Stdout: &out, Stderr: &errOut,
	})
	rc := execx.ExitCode(err)
	if msg := strings.TrimRight(errOut.String(), "\n"); msg != "" {
		fmt.Fprintln(stderr, msg)
	}
	diff := out.String()
	if e.Options.RenderDir != "" {
		text := ""
		if strings.TrimRight(diff, "\n") != "" {
			text = strings.TrimRight(diff, "\n") + "\n"
		}
		if err := os.WriteFile(filepath.Join(e.Options.RenderDir, stem+".diff"), []byte(text), 0o600); err != nil {
			ui.ErrorTo(stderr, "bootstrap: --render-dir: %v", err)
			return dryResult{status: dryError}
		}
	}
	switch rc {
	case 0:
		return dryResult{status: dryUnchanged}
	case 1:
		// kubectl diff: 1 = differences found.
		if os.Getenv("DEBUG") != "" {
			_, _ = io.WriteString(stderr, strings.TrimRight(diff, "\n")+"\n")
		}
		res := dryResult{status: dryDiffers}
		res.changed, res.added, res.counted = summarizeDiff(diff)
		return res
	}
	// > 1: kubectl or diff failed. The diff's stderr went out above.
	ui.ErrorTo(stderr, "bootstrap: kubectl diff failed for %s (rc=%d)", job.Name, rc)
	return dryResult{status: dryError}
}

// summarizeDiff counts the objects of a `kubectl diff` (GNU `diff -u -N`
// over the LIVE and MERGED trees): one `--- ` header per object that
// differs; an object the live side lacks shows an empty old side, the
// hunk `@@ -0,0 `. counted is false when no header was found (an
// external diff tool with another format).
func summarizeDiff(diff string) (changed, added int, counted bool) {
	headers := 0
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			headers++
		case strings.HasPrefix(line, "@@ -0,0 "):
			added++
		}
	}
	if headers == 0 {
		return 0, 0, false
	}
	return max(headers-added, 0), added, true
}

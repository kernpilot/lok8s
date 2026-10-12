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
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
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
	// AllowGenerators lets a dry run execute a `bash:` secrets generator
	// (default off): a dry run otherwise refuses one outright — it can run
	// kubectl/lo against the live cluster, which is not read-only no matter
	// what the rendered diff says. No effect outside --dry-run.
	AllowGenerators bool
}

// ErrDryRunDiffers is the dry run's "something would change" exit (rc 1).
// The per-entry lines already said what, so it prints nothing.
var ErrDryRunDiffers = ui.Handled(fmt.Errorf("bootstrap dry-run: entries would change"))

// dryRunStatus is one entry's verdict, the status word of its report line.
type dryRunStatus string

const (
	dryUnchanged dryRunStatus = "unchanged" // diff rc 0
	dryDiffers   dryRunStatus = "differs"   // diff rc 1, a completed diff
	dryNeedsCRD  dryRunStatus = "needs-crd" // diff rc 1, but the race-class
	// error raceRe matches (applyone.go): an earlier entry's CRD/webhook
	// has not landed in THIS read-only probe yet — not a real diff. A
	// sequential dry run never establishes CRDs, so a later entry can show
	// this even when the real (parallel, retrying) apply would not.
	dryRendered dryRunStatus = "rendered" // --no-diff: rendered, not diffed
	drySkipped  dryRunStatus = "skipped"  // the real run would skip it too
	dryError    dryRunStatus = "error"    // render or diff failed
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
	warnOmittedPrereqs(stderr, nodes, keep)
	return subset(nodes, keep), nil
}

// warnOmittedPrereqs names, before anything applies, every prerequisite a
// --only selection left out: dependsOn is deliberately NOT expanded (the
// selected entry runs without waiting for it), but applying it blind — with
// no word that its dependency never ran — hides exactly the ordering bug
// --only exists to let an operator rule out by hand.
func warnOmittedPrereqs(stderr io.Writer, nodes []*node, keep []bool) {
	for i, nd := range nodes {
		if !keep[i] {
			continue
		}
		for _, dep := range nd.entry.Deps {
			if dep == "" {
				continue
			}
			for j, other := range nodes {
				if other.entry.Name == dep && !keep[j] {
					ui.WarnTo(stderr, "bootstrap: --only: %s depends on %s, which is not selected — dependsOn is not expanded, applying only the named entries", nd.entry.Name, dep)
				}
			}
		}
	}
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
// on a terminal the status word is painted. restoreD is the already-computed
// restore.d pseudo-entry (nil when there was nothing to restore, or --only
// narrowed the run — restore.d is out of scope either way; see Apply).
func (e *Engine) runDryRun(ctx context.Context, nodes []*node, base Job, restoreD *dryResult) error {
	stdout, stderr := e.stdout(), e.stderr()
	order := topoOrder(nodes)
	if e.Options.RenderDir != "" {
		if err := os.MkdirAll(e.Options.RenderDir, 0o750); err != nil {
			return e.errorf("bootstrap: --render-dir: %v", err)
		}
		if err := clearOwnedRenderFiles(e.Options.RenderDir); err != nil {
			return e.errorf("bootstrap: --render-dir: %v", err)
		}
	}
	width := max(len(strconv.Itoa(len(order))), 2)
	counts := map[dryRunStatus]int{}
	total := 0
	if restoreD != nil {
		total++
		counts[restoreD.status]++
		fmt.Fprintf(stdout, "bootstrap dry-run: restore.d: %s\n", restoreD.render(ui.For(stdout)))
	}
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
		total++
		counts[res.status]++
		fmt.Fprintf(stdout, "bootstrap dry-run: %s: %s\n", stem, res.render(ui.For(stdout)))
	}
	// needs-crd folds into the "differs" count here: it still means
	// "something here needs a second look", and a new summary field would
	// ripple into every existing assertion of this line for no reader
	// benefit the per-entry word above doesn't already give. The per-entry
	// line keeps the honest word.
	differsOrNeedsCRD := counts[dryDiffers] + counts[dryNeedsCRD]
	fmt.Fprintf(stdout, "bootstrap dry-run: entries=%d unchanged=%d differs=%d rendered=%d skipped=%d errors=%d\n",
		total, counts[dryUnchanged], differsOrNeedsCRD, counts[dryRendered], counts[drySkipped], counts[dryError])
	if e.Options.RenderDir != "" {
		fmt.Fprintf(stderr, "bootstrap dry-run: files under %s\n", e.Options.RenderDir)
	}
	switch {
	case counts[dryError] > 0:
		return &driver.ExitError{Code: 2, Err: ui.Handled(fmt.Errorf("bootstrap dry-run: %d entries failed", counts[dryError]))}
	case differsOrNeedsCRD > 0:
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
	case dryDiffers, dryNeedsCRD:
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
	// RenderIsolated, never Render: khelm's own chart-dependency resolution
	// must not write Chart.lock/charts into the real addon directory either
	// — "read-only" covers the render, not just the apply.
	rendered, err := addons.RenderIsolated(ctx, e.Paths, e.Runner, stderr, job.Dir, job.Kind, job.Provider, job.Inline, jobEnv(job))
	if err != nil {
		// %v, not a bare "render failed": a refused bash: generator or a
		// now-unreachable cross-tree valueFiles ref (RenderIsolated's
		// documented limitation) both need to say WHY, not just that it
		// failed.
		ui.ErrorTo(stderr, "bootstrap: render failed for %s: %v", job.Name, err)
		return dryResult{status: dryError}
	}
	// One trailing newline, like the bash `printf '%s\n' "${rendered}"`
	// of a command substitution: the files must match byte for byte.
	rendered = strings.TrimRight(rendered, "\n") + "\n"
	return e.diffManifest(ctx, job.Kubeconfig, rendered, stem, job.Name)
}

// diffManifest writes one rendered manifest's dry-run outcome: <stem>.yaml
// to --render-dir (unless already written for a NoDiff caller — see below),
// then (unless --no-diff) a server-side `kubectl diff` against the live
// cluster, classified into the report's status vocabulary. Shared by
// dryRunOne (an addon) and dryRunRestoreD (the restore.d pseudo-entry) so
// both go through the exact same write-then-diff mechanics.
// label is the plain addon name (no "NN-" render-dir prefix) used in the
// error message, so it reads the same as the bash twin's — stem stays the
// render-dir file stem (the --render-dir path and nothing else).
func (e *Engine) diffManifest(ctx context.Context, kubeconfig, rendered, stem, label string) dryResult {
	stderr := e.stderr()
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
	err := e.Runner.Run(ctx, execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", kubeconfig, "diff", "--server-side", "--force-conflicts", "-f", "-"},
		Stdin:  strings.NewReader(rendered),
		Stdout: &out, Stderr: &errOut,
	})
	rc := execx.ExitCode(err)
	errMsg := strings.TrimRight(errOut.String(), "\n")
	if errMsg != "" {
		fmt.Fprintln(stderr, errMsg)
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
	// kubectl diff's rc 1 means "differences found" by convention, but a
	// generic failure can land on the same rc — classify by what's
	// actually on stderr/stdout rather than trusting the bare code:
	//   • the race-class pattern (raceRe, applyone.go) → needs-crd: an
	//     earlier entry's CRD/webhook has not landed in this read-only
	//     probe, not a real diff.
	//   • otherwise, a COMPLETED diff needs at least one object header
	//     (`--- `) on stdout — no header at all alongside a non-empty
	//     stderr is an error that happened to also exit 1, not "no diff
	//     text to show".
	switch {
	case rc == 0:
		return dryResult{status: dryUnchanged}
	case rc == 1 && raceRe.MatchString(errMsg):
		return dryResult{status: dryNeedsCRD}
	case rc == 1 && strings.Contains(diff, "--- "):
		if os.Getenv("DEBUG") != "" {
			_, _ = io.WriteString(stderr, strings.TrimRight(diff, "\n")+"\n")
		}
		res := dryResult{status: dryDiffers}
		res.changed, res.added, res.counted = summarizeDiff(diff)
		return res
	}
	// Everything else: kubectl/diff genuinely failed. The stderr went out
	// above.
	ui.ErrorTo(stderr, "bootstrap: kubectl diff failed for %s (rc=%d)", label, rc)
	return dryResult{status: dryError}
}

// clearOwnedRenderFiles removes only the files a dry run itself would write
// — NN-<entry>.{yaml,diff} and restore.d.{yaml,diff} — from an existing
// --render-dir before a run, so a STALE file from an earlier (larger or
// renamed) selection never survives alongside the new set and gets mistaken
// for current. Anything else in the directory is the operator's and is left
// alone.
func clearOwnedRenderFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, en := range entries {
		if en.IsDir() || !renderOwnedRe.MatchString(en.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, en.Name())); err != nil {
			return err
		}
	}
	return nil
}

var renderOwnedRe = regexp.MustCompile(`^(restore\.d|[0-9]+-.+)\.(yaml|diff)$`)

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

// dryRunRestoreD computes restore.d's own dry-run report line (bash:
// bootstrap::_dry_run_restore_d). A real run applies restore.d BEFORE any
// addon, so a dry run must say what THAT would do too — "restore.d not
// applied" alone hides a Secret a typo'd --only, or an out-of-date backup,
// would silently overwrite on the next real run. Reads every
// restore.d/*.sops.yaml exactly like restoreD (plaintext working copy
// first, else sops-decrypt in memory — never writes, never applies),
// concatenates them into ONE manifest, and diffs it like any other entry.
// Returns nil when there is nothing under restore.d to report (no
// directory, or every *.sops.yaml failed to decrypt) — the caller prints no
// line for it then, same as a real run skipping an empty restore.d.
func (e *Engine) dryRunRestoreD(ctx context.Context, domain, kubeconfig string) *dryResult {
	stderr := e.stderr()
	rdir := e.Paths.Clusters + "/" + domain + "/restore.d"
	if !fsutil.DirExists(rdir) {
		return nil
	}
	files, _ := filepath.Glob(rdir + "/*.sops.yaml")
	sort.Strings(files)
	var docs []string
	for _, f := range files {
		if !fsutil.FileExists(f) {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(f), ".sops.yaml")
		plain := e.Paths.Clusters + "/" + domain + "/secrets/restore.d/" + base + ".yaml"
		if fsutil.FileExists(plain) {
			if raw, err := os.ReadFile(plain); err == nil {
				docs = append(docs, strings.TrimRight(string(raw), "\n"))
				continue
			}
		}
		dec, err := e.sopsDecrypt(f)
		if err != nil {
			ui.WarnTo(stderr, "bootstrap: dry-run: restore.d could not decrypt %s — excluded from the diff (its owner would re-create/re-issue on a real run)", base)
			continue
		}
		docs = append(docs, strings.TrimRight(string(dec), "\n"))
	}
	if len(docs) == 0 {
		return nil
	}
	rendered := strings.Join(docs, "\n---\n") + "\n"
	res := e.diffManifest(ctx, kubeconfig, rendered, "restore.d", "restore.d")
	return &res
}

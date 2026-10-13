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
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"gopkg.in/yaml.v3"
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
	// AllowGenerators runs the addon's own executable code in a dry run
	// (an exec plugin other than lok8s' two built-ins, a KRM exec or
	// container function, a remote base, a `bash:` secrets generator) as
	// trusted, with the side effects of a real run (default off: refused at
	// the render boundary, internal/addons/dryrun.go). lo's own writes stay
	// suppressed either way. No effect outside --dry-run.
	AllowGenerators bool
	// IgnoreNondeterministic diffs a dry run's entries without the objects
	// that differ between the entry's two renders (regenerated on every
	// apply, e.g. Cilium's CA and TLS Secrets): the verdict is 0 when only
	// they differ. They are still listed. No effect outside --dry-run.
	IgnoreNondeterministic bool
	// WithRestore includes restore.d in an --only run (default off:
	// --only skips restore.d and, on a real run, warns loudly when it is
	// non-empty — restoring a wildcard TLS Secret matters for more than
	// the addon named, e.g. keeping cert-manager from ACME-reissuing and
	// hitting the Let's Encrypt rate limit). No effect without --only.
	WithRestore bool
}

// ErrDryRunDiffers is the dry run's "something would change" exit (rc 1).
// The per-entry lines already said what, so it prints nothing.
var ErrDryRunDiffers = ui.Handled(fmt.Errorf("bootstrap dry-run: entries would change"))

// dryRunStatus is one entry's verdict, the status word of its report line.
type dryRunStatus string

const (
	dryUnchanged dryRunStatus = "unchanged" // diff rc 0
	dryDiffers   dryRunStatus = "differs"   // diff rc 1, a completed diff
	// dryInconclusive: the dry run could not assess the entry. Printed
	// with its reason: "inconclusive (needs-crd)" — the race-class stderr
	// raceRe matches (applyone.go), an earlier entry's CRD/webhook has not
	// landed in THIS read-only probe yet (a sequential dry run never
	// establishes CRDs); "inconclusive (chart dependencies not vendored)" —
	// the ChartRenderer gate refused a local chart khelm would build
	// dependencies for (internal/render/chartgate.go). It does NOT count as
	// "differs": it is its own summary field, and it forces the run's exit
	// to 2 (an assessment that could not be completed), like an error.
	dryInconclusive dryRunStatus = "inconclusive"
	dryRendered     dryRunStatus = "rendered" // --no-diff: rendered, not diffed
	drySkipped      dryRunStatus = "skipped"  // the real run would skip it too
	dryError        dryRunStatus = "error"    // render or diff failed
)

// dryResult is one entry's dry-run outcome.
type dryResult struct {
	status dryRunStatus
	// changed and added are the objects the diff reports as modified and
	// as new; counted is false when the diff text carried no file headers
	// (an external diff tool), so the counts are not printed.
	changed, added int
	counted        bool
	// nonRepeatable marks an entry whose render generated secret material
	// in memory (the secrets plugin's report: a value the store lacks, a CA
	// CAROOT lacks): the next REAL run generates different bytes, so this
	// entry's rendered manifest — and whatever its diff reported —
	// describes a result the real run will not reproduce verbatim.
	nonRepeatable bool
	// reason is the inconclusive status's reason (needs-crd, chart
	// dependencies not vendored).
	reason string
	// nondeterministic lists the objects whose content differed between the
	// entry's two renders (a chart that mints a CA or TLS material on every
	// render): every apply regenerates them.
	nondeterministic []string
	// partial marks an entry whose YAML declares a KRM function: kustomize
	// skips an exec function in a dry run without an error (a hint).
	partial bool
}

// Inconclusive reasons.
const (
	reasonNeedsCRD        = "needs-crd"
	reasonChartDepsVendor = "chart dependencies not vendored"
)

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
	// --render-dir is prepared earlier now (applyIsolated, BEFORE the
	// restore.d preview — item G): restore.d's own diffManifest call
	// needs a writable, already-owned directory too, and used to run
	// before this ever created one.
	stdout, stderr := e.stdout(), e.stderr()
	order := topoOrder(nodes)
	width := max(len(strconv.Itoa(len(order))), 2)
	counts := map[dryRunStatus]int{}
	total, nondeterministic := 0, 0
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
		nondeterministic += len(res.nondeterministic)
		fmt.Fprintf(stdout, "bootstrap dry-run: %s: %s\n", stem, res.render(ui.For(stdout)))
		for _, obj := range res.nondeterministic {
			fmt.Fprintf(stdout, "bootstrap dry-run: %s: non-deterministic (regenerated on every apply): %s\n", stem, obj)
		}
	}
	// Review round 2, item F: inconclusive (needs-crd) is counted
	// SEPARATELY from differs now — it proves nothing either way, so
	// folding it into "differs" would overstate what the run actually
	// found. Both it and errors move the exit to 2 (below).
	fmt.Fprintf(stdout, "bootstrap dry-run: entries=%d unchanged=%d differs=%d rendered=%d skipped=%d inconclusive=%d errors=%d nondeterministic=%d\n",
		total, counts[dryUnchanged], counts[dryDiffers], counts[dryRendered], counts[drySkipped], counts[dryInconclusive], counts[dryError], nondeterministic)
	if e.Options.RenderDir != "" {
		fmt.Fprintf(stderr, "bootstrap dry-run: files under %s\n", e.Options.RenderDir)
	}
	switch {
	case counts[dryError] > 0 || counts[dryInconclusive] > 0:
		return &driver.ExitError{Code: 2, Err: ui.Handled(fmt.Errorf("bootstrap dry-run: %d entries failed or inconclusive", counts[dryError]+counts[dryInconclusive]))}
	case counts[dryDiffers] > 0:
		return ErrDryRunDiffers
	}
	return nil
}

// render is the report's status field: the word (an inconclusive one with
// its reason), then `changed=N new=M` for a diff that was counted, then the
// marks: `(non-deterministic: N)` (objects regenerated on every apply,
// listed on their own lines), `(non-repeatable)` (the render generated
// secret material), `(partial: KRM function skipped)`.
func (r dryResult) render(s ui.Style) string {
	word := string(r.status)
	if r.status == dryInconclusive {
		reason := r.reason
		if reason == "" {
			reason = reasonNeedsCRD
		}
		word = "inconclusive (" + reason + ")"
	}
	switch r.status {
	case dryUnchanged:
		word = s.Dim(word)
	case dryDiffers, dryInconclusive:
		word = s.Warn(word)
	case dryError:
		word = s.Bold(word)
	}
	if r.status == dryDiffers && r.counted {
		word = fmt.Sprintf("%s changed=%d new=%d", word, r.changed, r.added)
	}
	if n := len(r.nondeterministic); n > 0 {
		word += fmt.Sprintf(" (non-deterministic: %d)", n)
	}
	if r.nonRepeatable {
		word += " (non-repeatable)"
	}
	if r.partial {
		word += " (partial: KRM function skipped)"
	}
	return word
}

// dryRunOne renders ONE entry and diffs it against the cluster (bash:
// bootstrap::_dry_run_one). The skips are the real run's (the hosted
// platform-owned addons, the KubeOne cilium/ccm defer), so the report names
// what the real run would not touch either; then the entry's env is
// checked: it may not set lo's own variables (addons.IsProtectedEnv).
//
// The entry renders TWICE: an object whose content differs between the two
// renders is regenerated on every apply (a chart that mints a CA or TLS
// material per render, Cilium's for example), so the report lists it as
// non-deterministic. The diff is honest by default (those objects differ on
// the cluster, every apply rotates them); --ignore-nondeterministic diffs
// without them. The secrets plugin keeps what it generated in the run store,
// so its values are the same in both renders.
func (e *Engine) dryRunOne(ctx context.Context, job Job, stem string) dryResult {
	stderr := e.stderr()
	if reason, skip := e.skipReason(job); skip {
		fmt.Fprintln(stderr, reason)
		return dryResult{status: drySkipped}
	}
	env := jobEnv(job)
	if v := addons.ProtectedEnvOverride(env); v != "" {
		ui.ErrorTo(stderr, "bootstrap: %s: env: may not set %s (reserved for lo)", job.Name, v)
		return dryResult{status: dryError}
	}
	ui.DebugTo(stderr, "bootstrap: rendering %s", job.Name)
	// The secrets plugin reports what it generated or updated in the run
	// store, and the chart gate the charts it refused, to the boundary's
	// report file.
	if err := e.boundary.ResetReport(); err != nil {
		ui.ErrorTo(stderr, "bootstrap: dry-run report: %v", err)
		return dryResult{status: dryError}
	}
	partial := !e.Options.AllowGenerators && addons.KRMFunctionDeclared(job.Dir, e.Paths.Base, e.Paths.Lok8s)
	renderOnce := func() (string, error) {
		return addons.RenderDryRun(ctx, e.Paths, e.Runner, stderr, job.Dir, job.Kind, job.Provider, job.Inline, env, e.boundary, e.Options.AllowGenerators)
	}
	rendered, err := renderOnce()
	var second string
	if err == nil {
		second, err = renderOnce()
	}
	report := e.boundary.ReadReport()
	for _, m := range report.Generated {
		ui.DebugTo(stderr, "bootstrap: %s: generated in the dry run: %s", job.Name, m)
	}
	nonRepeatable := len(report.Generated) > 0
	if len(report.UnvendoredCharts) > 0 {
		return dryResult{status: dryInconclusive, reason: reasonChartDepsVendor, nonRepeatable: nonRepeatable, partial: partial}
	}
	if err != nil {
		// The render (or kustomize, or a plugin) already said WHY on stderr;
		// the cause stays on a debug line (bash prints the same lines).
		ui.DebugTo(stderr, "bootstrap: %s: %v", job.Name, err)
		ui.ErrorTo(stderr, "bootstrap: render failed for %s", job.Name)
		return dryResult{status: dryError, nonRepeatable: nonRepeatable, partial: partial}
	}
	nondeterministic, err := nondeterministicObjects(rendered, second)
	if err != nil {
		ui.ErrorTo(stderr, "bootstrap: %s: cannot compare the two renders: %v", job.Name, err)
		return dryResult{status: dryError, nonRepeatable: nonRepeatable, partial: partial}
	}
	// One trailing newline, like the bash `printf '%s\n' "${rendered}"`
	// of a command substitution: the files must match byte for byte.
	rendered = strings.TrimRight(rendered, "\n") + "\n"
	diffInput := rendered
	if e.Options.IgnoreNondeterministic && len(nondeterministic) > 0 {
		if diffInput, err = withoutObjects(rendered, nondeterministic); err != nil {
			ui.ErrorTo(stderr, "bootstrap: %s: %v", job.Name, err)
			return dryResult{status: dryError, nonRepeatable: nonRepeatable, partial: partial}
		}
	}
	res := e.diffManifest(ctx, job.Kubeconfig, rendered, diffInput, stem, job.Name)
	res.nonRepeatable, res.partial, res.nondeterministic = nonRepeatable, partial, nondeterministic
	return res
}

// objectLabel names a rendered object: `<apiVersion> <kind> <namespace>/<name>`,
// or `<apiVersion> <kind> <name>` for a cluster-scoped one (bash:
// bootstrap::_nd_objects prints the same).
func objectLabel(doc map[string]any) string {
	str := func(v any) string {
		s, _ := v.(string)
		return s
	}
	meta, _ := doc["metadata"].(map[string]any)
	name, ns := str(meta["name"]), str(meta["namespace"])
	if ns != "" {
		name = ns + "/" + name
	}
	return str(doc["apiVersion"]) + " " + str(doc["kind"]) + " " + name
}

// splitObjects decodes a rendered YAML stream into its objects, in order,
// each with its label and its canonical (re-encoded) content.
func splitObjects(stream string) (labels []string, content map[string]string, docs map[string]*yaml.Node, err error) {
	content, docs = map[string]string{}, map[string]*yaml.Node{}
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				return labels, content, docs, nil
			}
			return nil, nil, nil, err
		}
		var m map[string]any
		if node.Decode(&m) != nil || m == nil {
			continue
		}
		raw, err := yaml.Marshal(m)
		if err != nil {
			return nil, nil, nil, err
		}
		label := objectLabel(m)
		if _, seen := content[label]; !seen {
			labels = append(labels, label)
		}
		content[label] = string(raw)
		n := node
		docs[label] = &n
	}
}

// nondeterministicObjects lists the objects whose content differs between
// two renders of one entry, over the union of the objects of both renders:
// in the first render's order, then the objects only the second render has
// (in its order). An object only one render has is listed too.
func nondeterministicObjects(first, second string) ([]string, error) {
	labelsA, a, _, err := splitObjects(first)
	if err != nil {
		return nil, err
	}
	labelsB, b, _, err := splitObjects(second)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range labelsA {
		if other, ok := b[l]; !ok || other != a[l] {
			out = append(out, l)
		}
	}
	for _, l := range labelsB {
		if _, ok := a[l]; !ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// withoutObjects is the rendered stream without the named objects (what
// --ignore-nondeterministic diffs). Empty when nothing is left.
func withoutObjects(stream string, drop []string) (string, error) {
	skip := map[string]bool{}
	for _, l := range drop {
		skip[l] = true
	}
	labels, _, docs, err := splitObjects(stream)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, l := range labels {
		if skip[l] {
			continue
		}
		raw, err := marshalYAML(docs[l])
		if err != nil {
			return "", err
		}
		parts = append(parts, strings.TrimRight(raw, "\n"))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n---\n") + "\n", nil
}

func marshalYAML(n *yaml.Node) (string, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
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
func (e *Engine) diffManifest(ctx context.Context, kubeconfig, rendered, diffInput, stem, label string) dryResult {
	stderr := e.stderr()
	if e.Options.RenderDir != "" {
		if err := writeGuarded(filepath.Join(e.Options.RenderDir, stem+".yaml"), []byte(rendered), 0o600); err != nil {
			ui.ErrorTo(stderr, "bootstrap: --render-dir: %v", err)
			return dryResult{status: dryError}
		}
	}
	if e.Options.NoDiff {
		return dryResult{status: dryRendered}
	}
	// --ignore-nondeterministic left nothing to diff.
	if strings.TrimSpace(diffInput) == "" {
		if e.Options.RenderDir != "" {
			if err := writeGuarded(filepath.Join(e.Options.RenderDir, stem+".diff"), nil, 0o600); err != nil {
				ui.ErrorTo(stderr, "bootstrap: --render-dir: %v", err)
				return dryResult{status: dryError}
			}
		}
		return dryResult{status: dryUnchanged}
	}

	var out, errOut strings.Builder
	err := e.Runner.Run(ctx, execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", kubeconfig, "diff", "--server-side", "--force-conflicts", "-f", "-"},
		Stdin:  strings.NewReader(diffInput),
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
		if err := writeGuarded(filepath.Join(e.Options.RenderDir, stem+".diff"), []byte(text), 0o600); err != nil {
			ui.ErrorTo(stderr, "bootstrap: --render-dir: %v", err)
			return dryResult{status: dryError}
		}
	}
	// Review round 2, item F: classify the PROCESS RESULT directly, no
	// stdout-shape heuristic — the old "`--- ` header or it's an error"
	// rule broke KUBECTL_EXTERNAL_DIFF (a `diff -q` exits 1 on a real
	// difference with NO header at all, and was misclassified as error).
	//   • a launch failure (the binary was never found, the context ended
	//     before it started — execx.LaunchFailed) → error, always: there is
	//     no exit code to read a verdict from.
	//   • the race-class pattern on stderr (raceRe, applyone.go) → an
	//     earlier entry's CRD/webhook has not landed in this read-only
	//     probe, at ANY exit code — not a real diff, not a real error
	//     either: inconclusive.
	//   • rc 0 → unchanged; rc 1 → differs (whatever the diff tool printed,
	//     or didn't); anything else → a genuine kubectl/diff error.
	switch {
	case execx.LaunchFailed(err):
		ui.ErrorTo(stderr, "bootstrap: kubectl diff failed to start for %s: %v", label, err)
		return dryResult{status: dryError}
	case raceRe.MatchString(errMsg):
		return dryResult{status: dryInconclusive, reason: reasonNeedsCRD}
	case rc == 0:
		return dryResult{status: dryUnchanged}
	case rc == 1:
		if os.Getenv("DEBUG") != "" && diff != "" {
			_, _ = io.WriteString(stderr, strings.TrimRight(diff, "\n")+"\n")
		}
		res := dryResult{status: dryDiffers}
		res.changed, res.added, res.counted = summarizeDiff(diff)
		return res
	}
	// rc > 1, no race pattern: kubectl/diff genuinely failed. The stderr
	// went out above.
	ui.ErrorTo(stderr, "bootstrap: kubectl diff failed for %s (rc=%d)", label, rc)
	return dryResult{status: dryError}
}

// renderDirMarker names the file a dry run writes into a fresh/emptied
// --render-dir, so a LATER prepareRenderDir call (this same run's
// restore.d step, or a future run reusing the directory) can tell "a dry
// run's own output" apart from "whatever else lives here" (review round 2,
// item G).
const renderDirMarker = ".lo-dry-run"

// prepareRenderDir prepares dir for a dry run's writes (bash:
// bootstrap::_prepare_render_dir). The path may not contain a symlink (a
// rendered Secret must land where the operator pointed, never somewhere a
// link leads). A missing dir is created 0700 with the marker; an existing
// one that group or others may read is tightened to 0700 (and stderr says
// so); an EXISTING,
// non-empty dir must carry renderDirMarker as a regular file — a prior dry
// run's own — or the call is refused, so a dry run never scribbles into a
// directory an operator keeps something else in. A dir that does carry the
// marker is cleared of only the files it owns (NN-<entry>.{yaml,diff},
// restore.d.{yaml,diff}): a stale entry from an earlier,
// differently-numbered or differently-selected run never survives alongside
// the new set.
func prepareRenderDir(dir string, stderr io.Writer) error {
	if err := refuseSymlinkedPath(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return writeGuarded(filepath.Join(dir, renderDirMarker), []byte(renderDirMarkerText), 0o600)
	case err != nil:
		return err
	}
	if len(entries) > 0 {
		if fi, err := os.Lstat(filepath.Join(dir, renderDirMarker)); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not empty and does not carry %s as a regular file (not a dry run's own output directory) — point --render-dir at an empty or lok8s-owned directory", dir, renderDirMarker)
		}
	}
	// An existing directory holds rendered Secrets in plaintext too: one
	// that group or others may read is tightened to 0700 (an empty one, or
	// a dry run's own).
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a directory: 0700 is owner-only
			return err
		}
		fmt.Fprintf(stderr, "bootstrap dry-run: %s was %04o: tightened to 0700\n", dir, fi.Mode().Perm())
	}
	if len(entries) == 0 {
		return writeGuarded(filepath.Join(dir, renderDirMarker), []byte(renderDirMarkerText), 0o600)
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

const renderDirMarkerText = "lok8s dry-run output directory — safe to delete\n"

// refuseSymlinkedPath walks dir's components from the root and refuses the
// first one that is a symlink (the lexical path and the resolved path must
// be the same). Components that do not exist yet are created by the caller.
func refuseSymlinkedPath(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	cur := filepath.VolumeName(abs) + string(filepath.Separator)
	for part := range strings.SplitSeq(strings.TrimPrefix(abs, cur), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink — point --render-dir at a path without symlinks", cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}

var renderOwnedRe = regexp.MustCompile(`^(restore\.d|[0-9]+-.+)\.(yaml|diff)$`)

// writeGuarded writes data to path UNLESS path already exists as a
// symlink: a --render-dir is lok8s' own, never meant to hold anything but
// its own output files, so a symlink there is either stale or hostile —
// refuse rather than write through it (item G: "never follow symlinks
// inside it"). This is a check-then-write, not O_NOFOLLOW; good enough for
// a directory nothing else is expected to be racing against.
func writeGuarded(path string, data []byte, perm os.FileMode) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through a symlink: %s", path)
	}
	return os.WriteFile(path, data, perm)
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

// dryRunRestoreD computes restore.d's own dry-run report line (bash:
// bootstrap::_dry_run_restore_d). A real run applies restore.d BEFORE any
// addon, so a dry run must say what THAT would do too — "restore.d not
// applied" alone hides a Secret a typo'd --only, or an out-of-date backup,
// would silently overwrite on the next real run. Reads every
// restore.d/*.sops.yaml exactly like restoreD (plaintext working copy
// first, else a sops decrypt in memory with the normal key discovery —
// never writes, never applies), concatenates them into ONE manifest, and
// diffs it like any other entry. A file that does not decrypt is an error
// (exit 2): the preview of restore.d is incomplete, so the run cannot say
// "nothing would change". Returns nil when there is nothing under restore.d
// to report (no directory, no file) — the caller prints no line for it then,
// same as a real run skipping an empty restore.d.
func (e *Engine) dryRunRestoreD(ctx context.Context, domain, kubeconfig string) *dryResult {
	stderr := e.stderr()
	rdir := e.Paths.Clusters + "/" + domain + "/restore.d"
	if !fsutil.DirExists(rdir) {
		return nil
	}
	files, _ := filepath.Glob(rdir + "/*.sops.yaml")
	sort.Strings(files)
	var docs []string
	failed := false
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
			ui.DebugTo(stderr, "bootstrap: dry-run: restore.d: %s: %s", filepath.Base(f), firstLine(err.Error()))
			ui.ErrorTo(stderr, "bootstrap: dry-run: restore.d: cannot decrypt %s", filepath.Base(f))
			failed = true
			continue
		}
		docs = append(docs, strings.TrimRight(string(dec), "\n"))
	}
	if failed {
		return &dryResult{status: dryError}
	}
	if len(docs) == 0 {
		return nil
	}
	rendered := strings.Join(docs, "\n---\n") + "\n"
	res := e.diffManifest(ctx, kubeconfig, rendered, rendered, "restore.d", "restore.d")
	return &res
}

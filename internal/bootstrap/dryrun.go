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
	"crypto/sha256"
	"encoding/hex"
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
	// AllowGenerators lets a dry run execute a `bash:` secrets generator or
	// a non-builtin exec plugin (default off): a dry run otherwise refuses
	// both outright — either can run kubectl/lo against the live cluster,
	// which is not read-only no matter what the rendered diff says. No
	// effect outside --dry-run.
	AllowGenerators bool
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
	// dryInconclusive is printed as "inconclusive (needs-crd)": the race-
	// class stderr raceRe matches (applyone.go) — an earlier entry's
	// CRD/webhook has not landed in THIS read-only probe yet, so the diff
	// result proves nothing either way. A sequential dry run never
	// establishes CRDs, so a later entry can show this even when the real
	// (parallel, retrying) apply would not. Review round 2, item F: this
	// does NOT count as "differs" — it is its own summary field, and it
	// forces the run's exit to 2 (an assessment that could not be
	// completed), the same as a genuine error.
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
	// nonRepeatable marks an entry whose render minted NEW secret material
	// into the sandboxed store (review round 2, item I): the next REAL run
	// generates different bytes (a fresh random password, a new keypair),
	// so this entry's rendered manifest — and whatever its diff reported —
	// describes a result the real run will not reproduce verbatim.
	nonRepeatable bool
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
	// --render-dir is prepared earlier now (applyIsolated, BEFORE the
	// restore.d preview — item G): restore.d's own diffManifest call
	// needs a writable, already-owned directory too, and used to run
	// before this ever created one.
	stdout, stderr := e.stdout(), e.stderr()
	order := topoOrder(nodes)
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
	// Review round 2, item F: inconclusive (needs-crd) is counted
	// SEPARATELY from differs now — it proves nothing either way, so
	// folding it into "differs" would overstate what the run actually
	// found. Both it and errors move the exit to 2 (below).
	fmt.Fprintf(stdout, "bootstrap dry-run: entries=%d unchanged=%d differs=%d rendered=%d skipped=%d inconclusive=%d errors=%d\n",
		total, counts[dryUnchanged], counts[dryDiffers], counts[dryRendered], counts[drySkipped], counts[dryInconclusive], counts[dryError])
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

// render is the report's status field: the word, then `changed=N new=M`
// for a diff that was counted, then `(non-repeatable)` when the render
// minted new secret material (item I).
func (r dryResult) render(s ui.Style) string {
	word := string(r.status)
	if r.status == dryInconclusive {
		word = "inconclusive (needs-crd)"
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
	if r.nonRepeatable {
		word += " (non-repeatable)"
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
	env := jobEnv(job)
	// Item B: an entry's env: must never be the thing that decides HOME,
	// PATH_SECRETS, KUBECONFIG, … for its own render — report the attempt
	// as this entry's error, naming the variable, before rendering at all.
	if v := addons.ProtectedEnvOverride(env); v != "" {
		ui.ErrorTo(stderr, "bootstrap: %s: env: may not set %s (protected — see the sandbox docs)", job.Name, v)
		return dryResult{status: dryError}
	}
	// A snapshot of the sandboxed secrets store BEFORE this entry renders,
	// compared against one taken after (item I): a generator that mints
	// NEW material — a fresh random password, a keypair that didn't exist
	// yet — changes the store even inside the sandbox, and the NEXT real
	// run mints different bytes. The entry's report says so, rather than
	// implying the diff just shown is exactly what a real run would do.
	secretsDir := os.Getenv("PATH_SECRETS")
	before := secretsFingerprint(secretsDir)
	// RenderIsolated, never Render: khelm's own chart-dependency resolution
	// must not write Chart.lock/charts into the real addon directory either
	// — "read-only" covers the render, not just the apply.
	rendered, err := addons.RenderIsolated(ctx, e.Paths, e.Runner, stderr, job.Dir, job.Kind, job.Provider, job.Inline, env, e.Options.AllowGenerators)
	nonRepeatable := secretsFingerprint(secretsDir) != before
	if err != nil {
		// %v, not a bare "render failed": a refused bash: generator or a
		// now-unreachable cross-tree valueFiles ref (RenderIsolated's
		// documented limitation) both need to say WHY, not just that it
		// failed.
		ui.ErrorTo(stderr, "bootstrap: render failed for %s: %v", job.Name, err)
		return dryResult{status: dryError, nonRepeatable: nonRepeatable}
	}
	// One trailing newline, like the bash `printf '%s\n' "${rendered}"`
	// of a command substitution: the files must match byte for byte.
	rendered = strings.TrimRight(rendered, "\n") + "\n"
	res := e.diffManifest(ctx, job.Kubeconfig, rendered, stem, job.Name)
	res.nonRepeatable = nonRepeatable
	return res
}

// secretsFingerprint is a cheap content fingerprint of dir (name, size,
// mtime of every file under it, recursively) — "" when dir does not exist
// or holds nothing. Two calls around a render that differ mean the render
// wrote something new into the sandboxed secrets store (item I).
func secretsFingerprint(dir string) string {
	if dir == "" {
		return ""
	}
	var parts []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = path
		}
		parts = append(parts, fmt.Sprintf("%s:%d:%d", rel, info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
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
		if err := writeGuarded(filepath.Join(e.Options.RenderDir, stem+".yaml"), []byte(rendered), 0o600); err != nil {
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
		return dryResult{status: dryInconclusive}
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

// prepareRenderDir prepares dir for a dry run's writes (item G): created
// when missing. An EXISTING, non-empty dir must already carry
// renderDirMarker — a prior dry run's own — or the call is refused
// outright, so a dry run never scribbles into a directory an operator
// keeps something else in. A dir that does carry the marker is cleared of
// only the files it owns (NN-<entry>.{yaml,diff}, restore.d.{yaml,diff}) —
// a stale entry from an earlier, differently-numbered or
// differently-selected run never survives alongside the new set.
func prepareRenderDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		return writeGuarded(filepath.Join(dir, renderDirMarker), []byte("lok8s dry-run output directory — safe to delete\n"), 0o600)
	case err != nil:
		return err
	}
	if len(entries) == 0 {
		return writeGuarded(filepath.Join(dir, renderDirMarker), []byte("lok8s dry-run output directory — safe to delete\n"), 0o600)
	}
	hasMarker := false
	for _, en := range entries {
		if en.Name() == renderDirMarker {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		return fmt.Errorf("%s is not empty and does not carry %s (not a dry run's own output directory) — point --render-dir at an empty or lok8s-owned directory", dir, renderDirMarker)
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

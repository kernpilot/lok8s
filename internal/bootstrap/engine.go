package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/ui"
	"gopkg.in/yaml.v3"
)

// Job is one scheduled bootstrap entry as handed to the per-entry apply
// (bash: the argv of bootstrap::_apply_one).
type Job struct {
	Name     string
	Dir      string
	Kind     string
	Provider string
	// Kubeconfig is the --kubeconfig path threaded through every kubectl.
	Kubeconfig string
	// Inline is the merged inline helm values ("" when none).
	Inline string
	// WaitFlag is non-empty ("wait") when the scheduler wants the post-apply
	// readiness wait: a dep-target or a wait-gate. A pure leaf gets "".
	WaitFlag string
	// EnvLines is the newline-separated KEY=value envsubst overrides.
	EnvLines string
	// Force re-applies under LOK8S_FORCE_RECREATE=1 semantics (the
	// resolve-parked foreground heal).
	Force bool
	// NonInteractive marks a backgrounded apply (bash: the exported
	// LOK8S_NONINTERACTIVE=1 on every concurrent subshell — no shared
	// /dev/tty, so kapply never prompts and never draws).
	NonInteractive bool
}

// Engine drives the bootstrap addon system. Zero-value fields fall back to
// the same env/tty defaults the bash implementation read.
type Engine struct {
	Paths  *config.Paths
	Runner execx.Runner
	Stdout io.Writer
	Stderr io.Writer

	// ApplyOne overrides the per-entry apply (tests stub it exactly like
	// the bats redefine bootstrap::_apply_one). nil → the real applyOne.
	// Returns the entry's exit code (0 ok, non-zero failed).
	ApplyOne func(ctx context.Context, job Job, stdout, stderr io.Writer) int

	// Interactive/Ask are the batched-recreate prompt's tty seams (bash:
	// bootstrap::_interactive / bootstrap::_ask). One place touches
	// /dev/tty, which is what makes the prompt TEXT assertable in a test.
	Interactive func() bool
	Ask         func(prompt string) bool

	// Sleep is the retry/backoff seam (tests make waits instant).
	Sleep func(time.Duration)

	// SopsDecrypt decrypts one restore.d/*.sops.yaml in memory (nil → the
	// secrets package's sops library decrypt — NEVER a sops|kubectl pipe).
	SopsDecrypt func(path string) ([]byte, error)

	// hosted/bootstrapOnly are resolved per Apply run (also settable
	// directly when tests drive applyOne standalone, mirroring the bats
	// exports of LOK8S_BOOTSTRAP_HOSTED / LOK8S_BOOTSTRAP_ONLY).
	Hosted        bool
	BootstrapOnly string
}

func (e *Engine) stdout() io.Writer {
	if e.Stdout != nil {
		return e.Stdout
	}
	return os.Stdout
}

func (e *Engine) stderr() io.Writer {
	if e.Stderr != nil {
		return e.Stderr
	}
	return os.Stderr
}

func (e *Engine) sleep(d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (e *Engine) applyOneFn() func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
	if e.ApplyOne != nil {
		return e.ApplyOne
	}
	return e.applyOne
}

// engineError prints the bash error() line and returns it as the error.
func (e *Engine) errorf(format string, a ...any) error {
	ui.Errorf(e.stderr(), format, a...)
	return fmt.Errorf(format, a...)
}

var parallelRe = regexp.MustCompile(`^[0-9]+$`)

func maxParallel() int {
	v := os.Getenv("LOK8S_BOOTSTRAP_PARALLEL")
	if parallelRe.MatchString(v) {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return 8
}

// node is one DAG entry's scheduler state.
type node struct {
	entry      *Entry
	edeps      []int // effective dependency parents
	deps       map[int]bool
	dependents []int
	isTarget   bool // something depends on this entry → readiness-wait it
	started    bool
	completed  bool
	skipped    bool
	buf        *bytes.Buffer
}

// Apply reads spec.bootstrap from the cluster YAML and runs the entries as
// a topological-parallel DAG capped at LOK8S_BOOTSTRAP_PARALLEL (default 8)
// — bash: bootstrap::apply. Edge semantics:
//
//   - `dependsOn: [name, …]` — an explicit edge: wait for those entries'
//     READINESS.
//   - `wait: true` — a GLOBAL gate: it depends on ALL entries before it,
//     and every entry after it depends on it.
//
// An entry waits for its own workloads to be Ready IFF something depends on
// it OR it is a wait-gate; a pure leaf just applies and is done.
//
// FAILURE is isolated to the failed entry's SUBTREE — not the whole run:
// only its TRANSITIVE dependents are skipped; every unrelated entry still
// applies. A failed wait-gate skips everything after it automatically. This
// is deliberate bootstrap policy: apply as much as possible, then report
// non-zero if anything failed or was skipped (so the caller re-runs `lo up`
// to converge).
func (e *Engine) Apply(ctx context.Context, domain, clusterYAML, kubeconfig string) error {
	stderr := e.stderr()

	// lo provision drives the bootstrap directly (not via `lo build`), so
	// the per-domain secrets store isn't exported yet. The
	// secrets.lok8s.dev generators read $PATH_SECRETS — without it a target
	// carrying a passwd Secret fails to render. Mirror
	// libs/build::_export_secrets_path.
	if dirExists(e.Paths.Clusters + "/" + domain + "/secrets") {
		os.Setenv("PATH_SECRETS", e.Paths.Clusters+"/"+domain+"/secrets")
	}

	if !fileExists(kubeconfig) {
		return e.errorf("bootstrap: kubeconfig not found: %s", kubeconfig)
	}

	// Detect driver kind and provider for default policy + values resolution.
	kind := specDriverOrEmpty(clusterYAML)
	providerName, hosting := readProviderHosting(clusterYAML)
	e.Hosted = hosting == "hosted"

	// LOK8S_BOOTSTRAP_ONLY gates the KubeOne cilium/ccm skip in applyOne.
	// Both entry points set it EXPLICITLY before calling us; the default
	// here is a pure safety floor for a direct caller that sets nothing:
	// 0 (defer to the driver) so it can never trigger a spurious re-apply
	// that races `kubeone apply` for SSA field ownership.
	e.BootstrapOnly = os.Getenv("LOK8S_BOOTSTRAP_ONLY")
	if e.BootstrapOnly == "" {
		e.BootstrapOnly = "0"
	}
	os.Setenv("LOK8S_BOOTSTRAP_ONLY", e.BootstrapOnly)

	entries, err := ResolveEntries(clusterYAML, kind)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		ui.Debugf(stderr, "%s", nothingToApplyDebug(domain, kind))
		return nil
	}

	// ── Hosted gate ─────────────────────────────────────────────────
	// AFTER the empty-entries return on purpose: a spec with no bootstrap
	// entries must not probe the cluster at all. Both skip WITHOUT failing
	// the provision (the cluster itself is fine); re-running later is
	// idempotent.
	if e.Hosted {
		proceed, err := e.hostedGate(ctx, domain, kubeconfig)
		if err != nil || !proceed {
			return err
		}
	}

	// Export the API server endpoint (host + port) parsed from the
	// kubeconfig so addons can target the real control-plane LB endpoint
	// via envsubst — e.g. cilium k8sServiceHost on KubeOne, where the
	// in-cluster ClusterIP is unreachable from a bare-metal worker across
	// the Hetzner vSwitch. Picked up by the envsubst whitelist
	// (LOK8S_USER_* vars).
	exportAPIEndpoint(stderr, kubeconfig)

	// ── restore.d: pre-bootstrap DR restore (sops-encrypted manifests) ──
	// Applied BEFORE any addon so restored state exists when the addons'
	// reconcilers first look for it (canonical use: issued wildcard TLS
	// Secrets — cert-manager REUSES instead of re-issuing via ACME, keeping
	// clear of the Let's Encrypt duplicate-cert limit).
	e.restoreD(ctx, domain, kubeconfig)

	// ── Plan the DAG: parse every entry, resolve edges, detect cycles ──
	cap_ := maxParallel()

	var nodes []*node
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		parsed, err := ParseEntry(e.Paths, stderr, domain, entry)
		if err != nil {
			return err
		}
		if parsed.Builtin {
			// First use of a framework addon: eject it into the project
			// (default policy) so what this cluster applies is pinned on
			// disk, or serve the embedded copy under --no-eject.
			// The unit is the addon DIR's name, not parsed.Name (an
			// explicit `name:` override renames the entry, not the addon).
			dir, _, err := assets.Resolve(e.Paths, "addons/"+filepath.Base(parsed.Dir))
			if err != nil {
				return e.errorf("bootstrap: %v", err)
			}
			parsed.Dir = dir
		}
		if !dirExists(parsed.Dir) {
			return e.errorf("bootstrap: addon not found: %s (resolved to %s)", entry, parsed.Dir)
		}
		nodes = append(nodes, &node{entry: parsed, deps: map[int]bool{}})
	}
	n := len(nodes)
	if n == 0 {
		ui.Debugf(stderr, "%s", nothingToApplyDebug(domain, kind))
		return nil
	}

	// name → indices sharing it (a multimap). Handling depends on WHY it
	// collides and whether anything references it:
	//   • an explicit name: on a colliding entry               → hard error
	//   • a basename/map-key collision referenced by dependsOn → hard error
	//   • a basename/map-key collision NOTHING dependsOn       → warn
	//     (last-wins), so a barrier-only config with colliding basenames
	//     still applies.
	name2idxs := map[string][]int{}
	var nameOrder []string
	for i, nd := range nodes {
		if _, seen := name2idxs[nd.entry.Name]; !seen {
			nameOrder = append(nameOrder, nd.entry.Name)
		}
		name2idxs[nd.entry.Name] = append(name2idxs[nd.entry.Name], i)
	}
	for _, nd := range nodes {
		if !nd.entry.Explicit {
			continue
		}
		if share := name2idxs[nd.entry.Name]; len(share) > 1 {
			return e.errorf("bootstrap: duplicate entry name '%s' — name: must be unique (%d entries share it)", nd.entry.Name, len(share))
		}
	}

	// Effective dependency edges (child depends on parent). Three unioned
	// sources: explicit dependsOn; every wait-gate positionally before i;
	// and, for a wait-gate itself, ALL entries before it.
	for i, nd := range nodes {
		for _, dname := range nd.entry.Deps {
			if dname == "" {
				continue
			}
			cand := name2idxs[dname]
			if len(cand) == 0 {
				return e.errorf("bootstrap: '%s': dependsOn: unknown entry '%s'", nd.entry.Name, dname)
			}
			// A reference that resolves to a COLLIDED name is ambiguous —
			// error (the warn-only path is reserved for collisions nobody
			// dependsOn).
			if len(cand) > 1 {
				return e.errorf("bootstrap: '%s': dependsOn: ambiguous entry '%s' (%d entries share it — set an explicit name:)", nd.entry.Name, dname, len(cand))
			}
			nd.deps[cand[0]] = true
		}
		for g := 0; g < i; g++ {
			if nodes[g].entry.Wait {
				nd.deps[g] = true
			}
			if nd.entry.Wait {
				nd.deps[g] = true
			}
		}
	}

	// Collisions still standing here are basename/map-key clashes that
	// NOTHING dependsOn — warn (last-wins), it must NOT break a
	// barrier-only config that happens to reuse a basename.
	for _, cn := range nameOrder {
		if ids := name2idxs[cn]; len(ids) > 1 {
			ui.Warnf(stderr, "bootstrap: duplicate entry name '%s' — %d entries share it; a dependsOn on it would be ambiguous (set an explicit name:)", cn, len(ids))
		}
	}

	// Derive per-entry effective deps, the dep-target flag, in-degree and
	// reverse adjacency — used by cycle-check + runner.
	indeg := make([]int, n)
	for i, nd := range nodes {
		for pi := range nd.deps {
			nd.edeps = append(nd.edeps, pi)
			nodes[pi].isTarget = true
			nodes[pi].dependents = append(nodes[pi].dependents, i)
			indeg[i]++
		}
	}

	// Cycle detection (Kahn): peel off in-degree-0 nodes; anything left
	// sits in (or behind) a cycle — which would deadlock the runner. Fail
	// fast with the offending names.
	kahn := append([]int{}, indeg...)
	var queue []int
	for i := 0; i < n; i++ {
		if kahn[i] == 0 {
			queue = append(queue, i)
		}
	}
	processed := 0
	for head := 0; head < len(queue); head++ {
		u := queue[head]
		processed++
		for _, v := range nodes[u].dependents {
			kahn[v]--
			if kahn[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
	if processed < n {
		var cyc []string
		for i := 0; i < n; i++ {
			if kahn[i] > 0 {
				cyc = append(cyc, nodes[i].entry.Name)
			}
		}
		return e.errorf("bootstrap: dependsOn: cycle detected (%s)", strings.Join(cyc, " "))
	}

	// ── Run the DAG: launch every entry whose deps are done, up to the cap ──
	// TERMINATION: every iteration either launches an entry, reaps an
	// in-flight one, resolves the parked set, or (nothing in flight after a
	// full launch pass, nothing parked) breaks — so it can't hang.
	sched := &schedule{nodes: nodes, forceEnv: os.Getenv("LOK8S_FORCE_RECREATE") != ""}
	type finished struct {
		idx int
		rc  int
	}
	done := make(chan finished, n)
	inflight := 0
	apply := e.applyOneFn()
	debug := os.Getenv("DEBUG") != ""

	for {
		// Launch phase: every not-yet-started, not-skipped entry whose
		// effective deps have all COMPLETED, capped at cap_ concurrent.
		for i := 0; i < n && inflight < cap_; i++ {
			nd := nodes[i]
			if nd.started || nd.skipped {
				continue
			}
			ready := true
			for _, d := range nd.edeps {
				if !nodes[d].completed {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			nd.started = true
			nd.buf = &bytes.Buffer{}
			// wait: true GATES run through the SAME background path as
			// everything else — the barrier is enforced by DAG edges, NOT
			// by foreground execution. Immutable/terminating heal is
			// uniform: kapply errors under the non-interactive job, the
			// reap PARKS the entry, and the drain point batch-heals.
			job := Job{
				Name: nd.entry.Name, Dir: nd.entry.Dir, Kind: kind,
				Provider: providerName, Kubeconfig: kubeconfig,
				Inline: nd.entry.Inline, WaitFlag: sched.waitFlag(nodes, i),
				EnvLines: nd.entry.EnvLines, Force: sched.forceEnv,
				NonInteractive: true,
			}
			idx := i
			buf := nd.buf
			go func() {
				rc := apply(ctx, job, buf, buf)
				done <- finished{idx, rc}
			}()
			inflight++
		}

		// Nothing in flight after a full launch pass → everything left is
		// completed or skipped — UNLESS entries parked on an
		// immutable/terminating conflict. Resolve those (batch heal) at
		// this quiescent drain point, where the scheduler owns the tty for
		// a single prompt, then loop: resolving a parked entry completes
		// it, unblocking dependents the next launch pass picks up.
		if inflight == 0 {
			if len(sched.parked) == 0 {
				break
			}
			e.resolveParked(ctx, sched, nodes, kind, providerName, kubeconfig)
			continue
		}

		f := <-done
		inflight--
		nd := nodes[f.idx]
		out := nd.buf.String()
		// PARK check FIRST. A background kapply runs non-interactive, so on
		// an immutable/terminating conflict it can't prompt — it errors and
		// the buffered output names the conflict. Detect that pair and PARK
		// the entry: record it for the drain-point batch heal and DON'T
		// complete it / DON'T OR overall rc.
		if f.rc != 0 && (kapply.ImmutableRe.MatchString(out) || kapply.TerminatingRe.MatchString(out)) {
			sched.parked = append(sched.parked, f.idx)
			// Harvest the namespaces this conflict would force-finalize
			// BEFORE the buffered output is discarded — the drain prompt
			// has to name them. Same extractor the terminating heal acts
			// on, so the prompt cannot name a different set than the heal
			// destroys.
			for _, ns := range kapply.TerminatingNamespaces(out) {
				seen := false
				for _, known := range sched.parkedNS {
					if known == ns {
						seen = true
						break
					}
				}
				if !seen {
					sched.parkedNS = append(sched.parkedNS, ns)
				}
			}
			// Surface the conflict detail NOW (we own the foreground):
			// parking would otherwise discard the buffered kubectl error,
			// and on a fail-fast / declined heal the user needs to see
			// WHICH object/field is blocked to resolve by hand.
			kapply.RenderCaptured(e.stdout(), nd.entry.Name, 1, strings.NewReader(out))
			fmt.Fprintf(stderr, "\033[33m⏸\033[0m %s \033[2m· needs recreate (immutable/terminating) — will confirm at the end\033[0m\n", nd.entry.Name)
			nd.buf = nil
			continue
		}
		nd.completed = true
		sched.done++
		jrc := 0
		if f.rc != 0 {
			sched.overallRC = 1
			jrc = 1
		}
		// Flush this entry's buffered output as ONE de-interleaved block,
		// now that we own the foreground: collapsed on success, errors
		// surfaced on failure. Under DEBUG (lo -v): honor kapply's "print
		// everything, don't aggregate" contract — still de-interleaved,
		// but verbatim.
		if debug {
			_, _ = io.Copy(e.stdout(), strings.NewReader(out))
		} else {
			kapply.RenderCaptured(e.stdout(), nd.entry.Name, jrc, strings.NewReader(out))
		}
		nd.buf = nil
		// iff this entry FAILED, skip only its transitive dependents (they
		// would fail behind the broken dep). A failure no longer stops the
		// world.
		if jrc != 0 {
			e.skipDependents(nodes, f.idx)
		}
	}

	// Non-zero if anything failed OR was skipped behind a failure; 0 only
	// when every entry completed cleanly (skips only ever come from
	// skipDependents, whose both call sites set overallRC first). Bash
	// returns a bare 1 here — the per-entry errors already told the story,
	// so the error carries no extra output.
	if sched.overallRC != 0 {
		return ErrEntriesFailed
	}
	return nil
}

// ErrEntriesFailed is Apply's bare non-zero exit (bash: `return 1` at the
// bottom of bootstrap::apply — the per-entry errors were already printed).
var ErrEntriesFailed = fmt.Errorf("bootstrap: one or more entries failed or were skipped")

// schedule is the run-scoped scheduler state (bash: the _BS_* globals,
// reset at the top of every run — Go scopes them per call instead).
type schedule struct {
	nodes     []*node
	done      int
	overallRC int
	parked    []int
	parkedNS  []string
	forceEnv  bool
}

// waitFlag: readiness wait IFF something needs this entry Ready — a
// dep-target or a wait-gate. A pure leaf passes "" → applyOne skips
// WaitReady (the point of the scheduler refactor).
func (s *schedule) waitFlag(nodes []*node, i int) string {
	if nodes[i].isTarget || nodes[i].entry.Wait {
		return "wait"
	}
	return ""
}

// skipDependents BFSes the reverse-adjacency (parent → children) from the
// failed entry and adds every not-yet-accounted dependent to the skip set,
// logging the cause (bash: bootstrap::_skip_dependents). The cause reported
// is the ORIGINAL failed entry's name, propagated to the whole invalidated
// subtree.
func (e *Engine) skipDependents(nodes []*node, failed int) {
	cause := nodes[failed].entry.Name
	queue := []int{failed}
	for head := 0; head < len(queue); head++ {
		u := queue[head]
		for _, c := range nodes[u].dependents {
			if nodes[c].skipped { // already skipped (visited)
				continue
			}
			if nodes[c].completed { // already completed — leave it
				continue
			}
			nodes[c].skipped = true
			ui.Warnf(e.stderr(), "bootstrap: skipping '%s' — a dependency failed (%s)", nodes[c].entry.Name, cause)
			queue = append(queue, c)
		}
	}
}

// interactive is the batched-prompt tty gate (bash: bootstrap::_interactive
// — interactive iff both std fds are terminals AND /dev/tty is usable).
func (e *Engine) interactive() bool {
	if e.Interactive != nil {
		return e.Interactive()
	}
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

func (e *Engine) askPrompt(prompt string) bool {
	if e.Ask != nil {
		return e.Ask(prompt)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer func() { _ = tty.Close() }()
	fmt.Fprint(tty, prompt)
	buf := make([]byte, 64)
	nRead, err := tty.Read(buf)
	if err != nil || nRead == 0 {
		return false
	}
	ans := strings.TrimSpace(string(buf[:nRead]))
	return strings.HasPrefix(ans, "y") || strings.HasPrefix(ans, "Y")
}

// RecreatePrompt composes the batched recreate prompt (bash:
// bootstrap::_recreate_prompt). Split out because this text IS the safety
// control — the single consent gate for every heal in the batch — so it has
// to be assertable rather than buried in a tty write.
//
// The prompt REPLACES kapply's pointed per-object confirms for accepted
// entries (they re-apply under force, which returns 0 from both the
// sealed-Secret and the ns-finalize confirm without asking). Everything
// those confirms would have said must therefore be said here, or it is not
// said at all.
func RecreatePrompt(count int, list string, stuckNS []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\033[33m?\033[0m bootstrap: %d addon(s) need recreate to reconcile (immutable/terminating):%s\n", count, list)
	b.WriteString(" recreate them? deletes + recreates the blocked object(s) (restarts pods);\n")
	b.WriteString(" \033[31m!\033[0m any SEALED Secret among them is RE-KEYED by this.\n")
	// The namespace case is strictly worse than a re-key: accepting
	// force-finalizes these via the raw /finalize API, which COMPLETES
	// their deletion — everything still inside dies with them. One accept
	// at an earlier prompt that did NOT name the namespaces destroyed live
	// cluster data, hence the explicit list. Only printed when there ARE
	// such namespaces: a warning that shows up every time is one nobody
	// reads by the third time.
	if len(stuckNS) > 0 {
		fmt.Fprintf(&b, " \033[31m!!\033[0m %d namespace(s) stuck Terminating will be FORCE-FINALIZED:\n", len(stuckNS))
		for _, ns := range stuckNS {
			fmt.Fprintf(&b, "      %s\n", ns)
		}
		b.WriteString(" \033[31m!!\033[0m that COMPLETES their deletion — EVERY object still in them is\n")
		b.WriteString("      DESTROYED IRREVERSIBLY (volume data, runtime-minted Secrets, buckets).\n")
	}
	b.WriteString(" [y/N] ")
	return b.String()
}

// resolveParked batch-heals the entries parked on an immutable/terminating
// conflict (bash: bootstrap::_resolve_parked). Called at the quiescent
// drain point (nothing in flight), so the scheduler owns /dev/tty for a
// SINGLE prompt covering ALL parked entries.
//
// Decision: --force / --force-recreate auto-accepts; else an interactive
// tty gets ONE y/N; else fail-fast per entry with actionable guidance.
// Accepted entries are re-applied FOREGROUND with force so kapply deletes +
// recreates the blocked object; each is then marked completed (unblocking
// dependents on the next launch pass) or, on failure, fails + skips its
// dependents.
func (e *Engine) resolveParked(ctx context.Context, s *schedule, nodes []*node, kind, providerName, kubeconfig string) {
	parked := s.parked
	stuckNS := s.parkedNS
	s.parked = nil
	s.parkedNS = nil

	var list strings.Builder
	for _, pi := range parked {
		list.WriteString(" " + nodes[pi].entry.Name)
	}

	accept := false
	if s.forceEnv {
		accept = true // --force / --force-recreate → no prompt, just heal
	} else if e.interactive() {
		accept = e.askPrompt(RecreatePrompt(len(parked), list.String(), stuckNS))
	}

	apply := e.applyOneFn()
	for _, pi := range parked {
		nd := nodes[pi]
		// Same readiness rule as the launch loop: wait IFF a dep-target or
		// a wait-gate.
		wflag := ""
		if nd.isTarget || nd.entry.Wait {
			wflag = "wait"
		}
		if accept {
			job := Job{
				Name: nd.entry.Name, Dir: nd.entry.Dir, Kind: kind,
				Provider: providerName, Kubeconfig: kubeconfig,
				Inline: nd.entry.Inline, WaitFlag: wflag,
				EnvLines: nd.entry.EnvLines, Force: true,
			}
			rc := apply(ctx, job, e.stdout(), e.stderr())
			nd.completed = true
			s.done++
			if rc != 0 {
				s.overallRC = 1
				e.skipDependents(nodes, pi)
			} else {
				fmt.Fprintf(e.stderr(), "\033[32m✓\033[0m %s \033[2m· recreated\033[0m\n", nd.entry.Name)
			}
		} else {
			// Declined (or non-interactive with no --force): announce WHY +
			// how to fix, then mark completed + skip its dependents (a
			// failed heal fails the entry).
			nd.completed = true
			s.done++
			s.overallRC = 1
			ui.Errorf(e.stderr(), "bootstrap: %s needs recreate (immutable/terminating) — re-run with --force (or --force-recreate) to auto-recreate, or resolve by hand", nd.entry.Name)
			e.skipDependents(nodes, pi)
		}
	}
}

// hostedGate probes a hosted cluster before applying (bash: the hosted gate
// block in bootstrap::apply). Two things make a hosted apply pointless
// right now, and they need DIFFERENT messages (conflating them sends users
// chasing the wrong fix):
//  1. The default hosted kubeconfig is OIDC/kubelogin — headless kubectl
//     fails outright. That is an AUTH/reachability problem, not "no
//     workers".
//  2. A CP-only cluster (no Ready workers yet) can't run addon workloads
//     and wait-gated entries would hang.
//
// Both skip WITHOUT failing the provision (proceed=false, err=nil).
func (e *Engine) hostedGate(ctx context.Context, domain, kubeconfig string) (bool, error) {
	stderr := e.stderr()
	var out strings.Builder
	// Bounded: kubectl's default request timeout is unlimited — a transient
	// API problem must not stall the provisioning path.
	err := e.Runner.Run(ctx, execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", kubeconfig, "--request-timeout=15s", "get", "nodes", "--no-headers"},
		Stdout: &out, Stderr: &out,
	})
	if err != nil {
		fmt.Fprintf(stderr, "[bootstrap] hosted %s: cannot reach the cluster with %s\n", domain, kubeconfig)
		fmt.Fprintln(stderr, "[bootstrap] the default hosted kubeconfig is OIDC (kubelogin + a browser sign-in) — install the")
		fmt.Fprintln(stderr, "[bootstrap] kubelogin plugin and run interactively, then re-run 'lo provision' or 'lo bootstrap'.")
		fmt.Fprintf(stderr, "[bootstrap] bootstrap addons NOT applied. (kubectl: %s)\n", firstLine(out.String()))
		return false, nil
	}
	// Ready may read "Ready" or "Ready,SchedulingDisabled" — match the
	// prefix. Control-plane-ROLE nodes don't count as workers (KKP-hosted
	// user clusters register none — the CP runs as pods on the platform
	// seed — but other hosted shapes might, and the message says "workers").
	ready := 0
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		status, roles := fields[1], fields[2]
		if (status == "Ready" || strings.HasPrefix(status, "Ready,")) &&
			!strings.Contains(roles, "control-plane") && !strings.Contains(roles, "master") {
			ready++
		}
	}
	if ready == 0 {
		fmt.Fprintf(stderr, "[bootstrap] hosted %s: control plane is up but has no Ready workers yet —\n", domain)
		fmt.Fprintln(stderr, "[bootstrap] skipping bootstrap addons; re-run 'lo provision' (or 'lo bootstrap') after workers join.")
		return false, nil
	}
	fmt.Fprintf(stderr, "[bootstrap] hosted %s: %d Ready worker(s) — applying bootstrap addons\n", domain, ready)
	return true, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// specDriverOrEmpty mirrors `kind=$(domain::spec_driver …) || kind=""` —
// the apply path tolerates a missing/malformed kind (it only selects the
// values overlay + default policy here; dispatch validates it hard).
func specDriverOrEmpty(clusterYAML string) string {
	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		return ""
	}
	var doc struct {
		Kind string `yaml:"kind"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	kind := strings.ToLower(doc.Kind)
	if !regexp.MustCompile(`^[a-z][a-z0-9]*$`).MatchString(kind) {
		return ""
	}
	return kind
}

func readProviderHosting(clusterYAML string) (provider, hosting string) {
	hosting = "self"
	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		return "", hosting
	}
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return "", hosting
	}
	spec := lookupMap(derefNode(&root), "spec")
	if pn := lookupMap(lookupMap(spec, "provider"), "name"); pn != nil && pn.Kind == yaml.ScalarNode && pn.Tag != "!!null" {
		provider = pn.Value
	}
	if hn := lookupMap(lookupMap(spec, "kubehz"), "hosting"); hn != nil && hn.Kind == yaml.ScalarNode && hn.Tag != "!!null" && hn.Value != "" {
		hosting = hn.Value
	}
	return provider, hosting
}

// exportAPIEndpoint parses the kubeconfig's first cluster server URL into
// LOK8S_USER_API_HOST/PORT (bash: the API-endpoint export block).
func exportAPIEndpoint(stderr io.Writer, kubeconfig string) {
	raw, err := os.ReadFile(kubeconfig)
	if err != nil {
		return
	}
	var doc struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Clusters) == 0 {
		return
	}
	hostport := doc.Clusters[0].Cluster.Server
	hostport = strings.TrimPrefix(hostport, "https://")
	hostport = strings.TrimPrefix(hostport, "http://")
	if hostport == "" {
		return
	}
	host, port, found := strings.Cut(hostport, ":")
	if !found || port == "" {
		port = "6443"
	}
	os.Setenv("LOK8S_USER_API_HOST", host)
	os.Setenv("LOK8S_USER_API_PORT", port)
	ui.Debugf(stderr, "bootstrap: API endpoint %s:%s (for addon envsubst)", host, port)
}

// apply.go — the Go port of kapply::apply and its healing half
// (.lok8s/utils/kapply.sh): server-side kubectl apply with bounded,
// interactive-or-opt-in self-healing.
//
// Two cluster states a plain `kubectl apply` can NEVER reconcile on its own,
// and which a naive retry/Tilt loop spins on forever:
//  1. an IMMUTABLE field changed (spec.selector, a Job's spec.template, a
//     Service's clusterIP, …). The object can only change by recreation.
//  2. an object is stuck TERMINATING — deletionTimestamp set but a finalizer
//     never clears. It blocks recreation until the finalizer goes.
//
// On either state Apply heals just the affected objects (recreate / clear
// finalizers) and re-applies ONCE. The decision is:
//   - ForceRecreate (--force-recreate / LOK8S_FORCE_RECREATE) → heal, no
//     questions asked;
//   - an interactive terminal → PROMPT [y/N];
//   - no tty / CI / LOK8S_NONINTERACTIVE → fail fast with a remediation hint.
//
// Force-finalizing a stuck-Terminating NAMESPACE is the one heal destructive
// enough to get a SECOND, pointed confirm in the interactive path;
// ForceRecreate still skips it. Every OTHER apply error is returned unchanged
// so existing retry logic (CRD-not-established, webhook-not-ready) keeps
// working.
//
// Display: the same documented simplification as Run — off a terminal the
// full kubectl output is printed (CI/Tilt logs, byte-identical to bash); on
// a terminal the identical FINAL state is rendered (summary line / surfaced
// errors) without the in-flight animation.
package kapply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
	"gopkg.in/yaml.v3"
)

// immutableObjRe extracts the objects kubectl reported as having an
// immutable-field conflict. kubectl phrases this two ways, both handled:
//
//	client-side : `The <Kind> "<name>" is invalid: <field>: ... immutable`
//	server-side : `Error from server (Invalid): <Kind>.<group> "<name>" is invalid: ...`
//
// (core resources have no `.<group>`).
var immutableObjRe = regexp.MustCompile(`[A-Z][A-Za-z]+(\.[a-z0-9.-]+)? "[^"]+" is invalid`)

// Applier is the configured apply/heal pipeline. The zero value is not
// usable — construct with NewApplier (which reads the same env the bash
// implementation did) or fill the fields explicitly in tests.
type Applier struct {
	Runner execx.Runner
	// Stdout receives the apply output (off-tty passthrough) and summaries.
	Stdout io.Writer
	// Stderr receives warn/error lines and surfaced apply errors.
	Stderr io.Writer

	// ForceRecreate is LOK8S_FORCE_RECREATE: heal without prompting.
	ForceRecreate bool
	// NonInteractive is LOK8S_NONINTERACTIVE/CI: never prompt, never draw.
	NonInteractive bool

	// Interactive overrides the tty detection (bash: kapply::_interactive —
	// tests stub it, since the harness has no /dev/tty to type into).
	Interactive func() bool
	// Ask asks one [y/N] question on the terminal (bash: kapply::_ask).
	Ask func(prompt string) bool

	// NsWait bounds the post-finalize disappearance poll (KAPPLY_NS_WAIT,
	// default 20 iterations).
	NsWait int
	// PollInterval is the poll sleep (KAPPLY_POLL_INTERVAL, default 1s).
	PollInterval time.Duration
	// Sleep is the sleep seam (tests make waits instant).
	Sleep func(time.Duration)
}

// NewApplier builds an Applier over the runner, reading the decision env
// exactly like the bash implementation (LOK8S_FORCE_RECREATE,
// LOK8S_NONINTERACTIVE, CI, KAPPLY_NS_WAIT, KAPPLY_POLL_INTERVAL).
func NewApplier(r execx.Runner, stdout, stderr io.Writer) *Applier {
	return &Applier{
		Runner:         r,
		Stdout:         stdout,
		Stderr:         stderr,
		ForceRecreate:  os.Getenv("LOK8S_FORCE_RECREATE") != "",
		NonInteractive: os.Getenv("LOK8S_NONINTERACTIVE") != "" || os.Getenv("CI") != "",
		NsWait:         envInt("KAPPLY_NS_WAIT", 20),
		PollInterval:   envInterval(),
	}
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envInterval() time.Duration {
	v := os.Getenv("KAPPLY_POLL_INTERVAL")
	if v == "" {
		return time.Second
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return time.Second
	}
	return time.Duration(f * float64(time.Second))
}

func (a *Applier) sleep(d time.Duration) {
	if a.Sleep != nil {
		a.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (a *Applier) pollInterval() time.Duration {
	if a.PollInterval > 0 {
		return a.PollInterval
	}
	return time.Second
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

// kubectl runs one kubectl invocation with combined stdout+stderr captured
// (bash: `kubectl … 2>&1`), returning the output and the exit code.
func (a *Applier) kubectl(ctx context.Context, stdin string, args ...string) (string, int) {
	var buf strings.Builder
	c := execx.Cmd{Name: "kubectl", Args: args, Stdout: &buf, Stderr: &buf}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	err := a.Runner.Run(ctx, c)
	return buf.String(), exitCode(err)
}

// kubectlQuiet runs kubectl with all output discarded (bash: &>/dev/null).
func (a *Applier) kubectlQuiet(ctx context.Context, stdin string, args ...string) int {
	c := execx.Cmd{Name: "kubectl", Args: args, Stdout: io.Discard, Stderr: io.Discard}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	return exitCode(a.Runner.Run(ctx, c))
}

// tty is the per-applier UI gate: NonInteractive (bash: the exported
// LOK8S_NONINTERACTIVE the bootstrap scheduler sets on its background
// subshells) forces the off-tty passthrough regardless of the process env.
func (a *Applier) tty() bool {
	if a.NonInteractive {
		return false
	}
	return ttyUI()
}

// applyPass is one server-side apply (bash: kapply::_apply_pass): collapse
// the output on a tty (full off-tty), surface ONLY error lines on failure,
// and return (raw output, kubectl's exit). Used for BOTH the initial apply
// and the post-heal re-apply, so the re-apply renders the same named
// progress block.
func (a *Applier) applyPass(ctx context.Context, label, manifest string, kubectlFlags []string) (string, int) {
	args := append(append([]string{}, kubectlFlags...),
		"apply", "--server-side", "--force-conflicts", "-f", "-")
	out, rc := a.kubectl(ctx, manifest, args...)

	if !ttyUI() {
		// bash: out=$(kubectl … 2>&1); printf '%s\n' "${out}" — the command
		// substitution strips trailing newlines, so exactly ONE follows.
		fmt.Fprintf(a.Stdout, "%s\n", strings.TrimRight(out, "\n"))
		return out, rc
	}
	// tty: render the FINAL collapsed state (documented simplification — no
	// in-flight spinner; the summary/aggregated errors match bash's end state).
	lines := splitLines(out)
	okCount := 0
	var rest []string
	for _, l := range lines {
		if OKRe.MatchString(l) {
			okCount++
		} else if l != "" {
			rest = append(rest, l)
		}
	}
	if okCount > 0 {
		noun := "resources"
		if okCount == 1 {
			noun = "resource"
		}
		fmt.Fprintf(a.Stdout, "\033[32m✓\033[0m %s · %d %s\n", label, okCount, noun)
	}
	if rc != 0 {
		for _, l := range Aggregate(rest) {
			fmt.Fprintln(a.Stderr, l)
		}
	}
	return out, rc
}

// Apply reads a manifest and applies it server-side with the bounded heal
// (bash: kapply::apply). label names the progress block; kubectlFlags (e.g.
// --kubeconfig <path>) thread through every kubectl call. Returns the raw
// kubectl output of the LAST pass (bash: KAPPLY_LAST_OUTPUT) and its exit
// status.
func (a *Applier) Apply(ctx context.Context, label, manifest string, kubectlFlags ...string) (string, int) {
	if label == "" {
		label = "resources"
	}
	out, rc := a.applyPass(ctx, label, manifest, kubectlFlags)
	if rc == 0 {
		return out, 0
	}

	immutable := ImmutableRe.MatchString(out)
	terminating := TerminatingRe.MatchString(out)
	if !immutable && !terminating {
		return out, rc
	}

	if !a.confirmHeal() {
		return out, rc
	}

	ui.Warnf(a.Stderr, "healing blocked objects, then re-applying once")
	if immutable {
		a.healImmutable(ctx, manifest, out, kubectlFlags)
	}
	if terminating {
		a.healTerminating(ctx, manifest, out, kubectlFlags)
	}

	// Re-apply through the SAME display pass — collapses like the first apply.
	return a.applyPass(ctx, label, manifest, kubectlFlags)
}

// interactive reports whether a REAL interactive terminal is available for
// the prompt (bash: kapply::_interactive). Its stdin must be a usable tty —
// with no controlling terminal, opening /dev/tty fails in background jobs /
// CI; treat that as non-interactive.
func (a *Applier) interactive() bool {
	if a.Interactive != nil {
		return a.Interactive()
	}
	if a.NonInteractive {
		return false
	}
	if !writerIsTTY(os.Stdout) || !fileIsTTY(os.Stdin) {
		return false
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func fileIsTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ask prints the (pre-formatted) question on /dev/tty and reads the answer
// from it (bash: kapply::_ask). Returns true on y/Y.
func (a *Applier) ask(prompt string) bool {
	if a.Ask != nil {
		return a.Ask(prompt)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	buf := make([]byte, 64)
	n, err := tty.Read(buf)
	if err != nil || n == 0 {
		return false
	}
	ans := strings.TrimSpace(string(buf[:n]))
	return strings.HasPrefix(ans, "y") || strings.HasPrefix(ans, "Y")
}

// confirmHeal decides whether to heal: explicit flag → yes; interactive →
// prompt; else no (bash: kapply::_confirm_heal).
func (a *Applier) confirmHeal() bool {
	if a.ForceRecreate {
		return true
	}
	if !a.interactive() {
		ui.Errorf(a.Stderr, "apply blocked by an unrecoverable state (immutable field / stuck Terminating).")
		ui.Errorf(a.Stderr, "  re-run with --force-recreate to recreate the affected objects (restarts their pods),")
		ui.Errorf(a.Stderr, "  or resolve the conflict by hand. Not retrying — that would loop.")
		return false
	}
	return a.ask("\033[33m?\033[0m kapply: recreate the blocked object(s) above to recover? this deletes + recreates them (restarts their pods); a one-time fix. [y/N] ")
}

// confirmSecretRecreate is the SECOND, pointed confirm just for recreating a
// SEALED Secret (bash: kapply::_confirm_secret_recreate). Recreating it is a
// RE-KEY: exactly the event the seal exists to make deliberate.
// ForceRecreate still proceeds (it IS the documented rotation path) but logs
// a pointed warning per Secret instead of recreating silently.
func (a *Applier) confirmSecretRecreate(name string) bool {
	if a.ForceRecreate {
		ui.Warnf(a.Stderr, "  RE-KEYING sealed Secret/%s (--force-recreate): pods keep the old value until restarted; state encrypted under it may be orphaned", name)
		return true
	}
	if !a.interactive() {
		return false
	}
	return a.ask(fmt.Sprintf("\033[31m!\033[0m kapply: Secret/%s is SEALED (immutable). Recreating it RE-KEYS the credential — pods keep the old value until restarted, and state encrypted under it may be orphaned. Really re-key? [y/N] ", name))
}

// confirmNsFinalize is the SECOND, pointed confirm just for force-finalizing
// a namespace — the most destructive heal (bash:
// kapply::_confirm_ns_finalize). ForceRecreate skips it (the override must
// stay usable non-interactively); no flag + no tty → refuse (don't nuke a
// namespace unattended).
func (a *Applier) confirmNsFinalize(name string) bool {
	if a.ForceRecreate {
		return true
	}
	if !a.interactive() {
		return false
	}
	return a.ask(fmt.Sprintf("\033[31m!\033[0m kapply: namespace/%s is stuck Terminating. Force-remove its finalizers via the API? this COMPLETES its deletion — every object still in it is destroyed, irreversibly. [y/N] ", name))
}

// manifestDoc is one parsed document of the applied manifest.
type manifestDoc struct {
	node      *yaml.Node
	kind      string
	name      string
	namespace string
}

func parseManifestDocs(manifest string) []manifestDoc {
	var docs []manifestDoc
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			break
		}
		d := manifestDoc{node: &n}
		d.kind = docScalar(&n, "kind")
		d.name = docScalar(&n, "metadata", "name")
		d.namespace = docScalar(&n, "metadata", "namespace")
		docs = append(docs, d)
	}
	return docs
}

func deref(n *yaml.Node) *yaml.Node {
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

func mapGet(n *yaml.Node, key string) *yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return deref(n.Content[i+1])
		}
	}
	return nil
}

func docScalar(n *yaml.Node, path ...string) string {
	cur := deref(n)
	for _, key := range path {
		cur = mapGet(cur, key)
		if cur == nil {
			return ""
		}
	}
	if cur.Kind != yaml.ScalarNode {
		return ""
	}
	return cur.Value
}

func marshalDoc(d manifestDoc) string {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d.node); err != nil {
		return ""
	}
	enc.Close()
	return buf.String()
}

// healImmutable recreates the objects kubectl reported as having an
// immutable-field conflict, by kind+name from the same manifest —
// `replace --force` = delete + create, which bypasses immutability (bash:
// kapply::_heal_immutable).
func (a *Applier) healImmutable(ctx context.Context, manifest, out string, kubectlFlags []string) {
	docs := parseManifestDocs(manifest)
	type obj struct{ kind, name string }
	seen := map[obj]bool{}
	var objs []obj
	for _, m := range immutableObjRe.FindAllString(out, -1) {
		// `<Kind>[.<group>] "<name>" is invalid` → kind, name.
		head, rest, _ := strings.Cut(m, ` "`)
		name := strings.TrimSuffix(rest, `" is invalid`)
		kind, _, _ := strings.Cut(head, ".")
		if kind == "" || name == "" {
			continue
		}
		o := obj{kind, name}
		if !seen[o] {
			seen[o] = true
			objs = append(objs, o)
		}
	}
	// bash pipes through `sort -u`, so healing order is sorted.
	sort.Slice(objs, func(i, j int) bool {
		if objs[i].kind != objs[j].kind {
			return objs[i].kind < objs[j].kind
		}
		return objs[i].name < objs[j].name
	})
	for _, o := range objs {
		// The crown-jewel confirm applies only to Secrets the manifest
		// actually SEALS (immutable: true) — a plain Secret can hit "field
		// is immutable" too (e.g. a type: change) and gets the generic heal.
		if o.kind == "Secret" {
			sealed := false
			for _, d := range docs {
				if d.kind == "Secret" && d.name == o.name {
					if imm := docScalar(d.node, "immutable"); imm == "true" {
						sealed = true
					}
				}
			}
			if sealed && !a.confirmSecretRecreate(o.name) {
				ui.Warnf(a.Stderr, "  keeping sealed Secret/%s (re-key declined)", o.name)
				continue
			}
		}
		ui.Warnf(a.Stderr, "  recreating immutable %s/%s", o.kind, o.name)
		var sel strings.Builder
		for _, d := range docs {
			if d.kind == o.kind && d.name == o.name {
				sel.WriteString("---\n")
				sel.WriteString(marshalDoc(d))
			}
		}
		args := append(append([]string{}, kubectlFlags...), "replace", "--force", "-f", "-")
		if rc := a.kubectlQuiet(ctx, sel.String(), args...); rc != 0 {
			ui.Warnf(a.Stderr, "  could not recreate %s/%s", o.kind, o.name)
		}
	}
}

// finalizeNamespace force-completes a stuck-Terminating namespace by
// dropping its spec.finalizers via the /finalize subresource (bash:
// kapply::_finalize_namespace). No-op unless the namespace really is
// terminating. Then waits (bounded) for it to actually disappear.
func (a *Applier) finalizeNamespace(ctx context.Context, name string, kubectlFlags []string) {
	args := append(append([]string{}, kubectlFlags...),
		"get", "ns", name, "-o", "jsonpath={.metadata.deletionTimestamp}")
	var buf strings.Builder
	if err := a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: args, Stdout: &buf, Stderr: io.Discard}); err != nil {
		return
	}
	if strings.TrimSpace(buf.String()) == "" {
		return
	}
	if !a.confirmNsFinalize(name) {
		ui.Warnf(a.Stderr, "  skipped namespace/%s — force-finalize declined (re-apply will retry)", name)
		return
	}
	ui.Warnf(a.Stderr, "  force-finalizing stuck-terminating namespace/%s", name)
	getArgs := append(append([]string{}, kubectlFlags...), "get", "ns", name, "-o", "json")
	var nsJSON strings.Builder
	if err := a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: getArgs, Stdout: &nsJSON, Stderr: io.Discard}); err != nil {
		ui.Warnf(a.Stderr, "  could not finalize namespace/%s", name)
		return
	}
	payload := deleteSpecFinalizers(nsJSON.String())
	repArgs := append(append([]string{}, kubectlFlags...),
		"replace", "--raw", "/api/v1/namespaces/"+name+"/finalize", "-f", "-")
	if rc := a.kubectlQuiet(ctx, payload, repArgs...); rc != 0 {
		ui.Warnf(a.Stderr, "  could not finalize namespace/%s", name)
		return
	}
	waitN := a.NsWait
	for i := 0; i < waitN; i++ {
		probe := append(append([]string{}, kubectlFlags...), "get", "ns", name)
		if rc := a.kubectlQuiet(ctx, "", probe...); rc != 0 {
			return
		}
		a.sleep(a.pollInterval())
	}
}

// deleteSpecFinalizers is jq's `del(.spec.finalizers)` over the namespace
// JSON. YAML is a JSON superset, so the yaml decoder handles it; the result
// is re-emitted as YAML (kubectl accepts either on -f -).
func deleteSpecFinalizers(nsJSON string) string {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(nsJSON), &n); err != nil {
		return nsJSON
	}
	root := deref(&n)
	if spec := mapGet(root, "spec"); spec != nil && spec.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(spec.Content); i += 2 {
			if spec.Content[i].Value == "finalizers" {
				spec.Content = append(spec.Content[:i], spec.Content[i+2:]...)
				break
			}
		}
	}
	out, err := yaml.Marshal(root)
	if err != nil {
		return nsJSON
	}
	return string(out)
}

// healTerminating heals objects stuck Terminating so the delete completes
// and the re-apply can recreate them (bash: kapply::_heal_terminating). The
// apiserver reports the block two ways:
//
//	(a) a 403 on writes INTO a terminating namespace — force-finalize it.
//	(b) a manifest object that is itself mid-delete (deletionTimestamp set,
//	    finalizer not clearing). Namespaces finalize via /finalize;
//	    everything else via metadata.finalizers.
//
// CRDs stuck mid-delete are deliberately NOT force-removed here — that would
// cascade-delete every CR of that kind cluster-wide; the CRD-settle retry in
// the caller handles that race instead.
func (a *Applier) healTerminating(ctx context.Context, manifest, out string, kubectlFlags []string) {
	// (a) namespaces named in "because it is being terminated" 403s
	for _, ns := range TerminatingNamespaces(out) {
		a.finalizeNamespace(ctx, ns, kubectlFlags)
	}

	// (b) manifest objects carrying their own stuck deletionTimestamp
	for _, d := range parseManifestDocs(manifest) {
		if d.kind == "" || d.name == "" {
			continue
		}
		lk := strings.ToLower(d.kind)
		if lk == "namespace" || lk == "ns" {
			a.finalizeNamespace(ctx, d.name, kubectlFlags)
			continue
		}
		args := append(append([]string{}, kubectlFlags...), "get", d.kind, d.name)
		if d.namespace != "" {
			args = append(args, "-n", d.namespace)
		}
		args = append(args, "-o", "jsonpath={.metadata.deletionTimestamp}")
		var buf strings.Builder
		if err := a.Runner.Run(ctx, execx.Cmd{Name: "kubectl", Args: args, Stdout: &buf, Stderr: io.Discard}); err != nil {
			continue
		}
		if strings.TrimSpace(buf.String()) == "" {
			continue
		}
		ui.Warnf(a.Stderr, "  clearing finalizers on stuck-terminating %s/%s", d.kind, d.name)
		patch := append(append([]string{}, kubectlFlags...), "patch", d.kind, d.name)
		if d.namespace != "" {
			patch = append(patch, "-n", d.namespace)
		}
		patch = append(patch, "--type", "merge", "-p", `{"metadata":{"finalizers":null}}`)
		if rc := a.kubectlQuiet(ctx, "", patch...); rc != 0 {
			ui.Warnf(a.Stderr, "  could not clear finalizers on %s/%s", d.kind, d.name)
		}
	}
}

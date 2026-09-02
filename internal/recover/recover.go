// Package recover is the Go port of `lo recover <domain>` — rebuild a
// cluster from bare metal (disaster recovery); .lok8s/libs/recover, bash
// wins on any divergence.
//
// It orchestrates already-merged pieces into a single, hands-off
// bare-metal-up recovery:
//
//	resolve → doctor → consent → rebuild → provision → verify
//
// It is a THIN orchestrator: every step reuses an existing, tested
// primitive — provision.ResolveSpec / provision::dispatch (= `lo
// provision`), the provider contract's OPTIONAL provider::rebuild
// (destructive node reset, atomic preflight, honors CLOUD_DRY_RUN) and
// provider::doctor (read-only advisory diagnosis). It adds only the glue:
// transparency (doctor BEFORE anything destructive), the destructive-consent
// prompt (the guard lives HERE, not in the provider hooks), per-phase timing,
// and a Ready-vs-inventory verify.
//
// DESIGN (converged with the operator):
//   - doctor ADVISES (never blocks); provider::rebuild ENFORCES (its own
//     atomic preflight refuses to touch anything if any node fails
//     validation).
//   - CONSENT lives here. `--force` / LOK8S_NONINTERACTIVE opt out of the
//     prompt. NOTE the asymmetry with the provision gate: LOK8S_NONINTERACTIVE
//     means REFUSE there (provision.Dispatcher.interactive) while it means
//     CONSENT here — deliberate: recover is an explicit DR command whose
//     entry already demanded a literal yes; up/provision are ambient and
//     fail closed.
//   - `--dry-run` is genuinely safe: it exports CLOUD_DRY_RUN so
//     provider::rebuild takes its dry-run branches (prints the plan, reimages
//     NOTHING), then STOPS before any real provision/verify.
//
// Providers and the provision dispatch are BASH plugins today (no Go
// hetzner provider; the Go provision.Dispatcher has no provider loader and
// the KubeOne driver's inventory/pre-apply hooks are not wired) — so the
// provider hooks and the provision phase are delegated to bash children
// running the ORIGINAL libs (bridge.go), the same way `lo doctor` runs its
// provider section. Every child goes through execx.Runner, so the whole
// sequence is hermetic under a fake runner.
package recover

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a failure whose diagnostic was already printed.
var ErrHandled = errors.New("recover: handled")

// Provider is the slice of the provider contract recover consumes
// (utils/provider.sh): the OPTIONAL rebuild + doctor hooks and the
// inventory output. Implemented by the bash bridge, and by fakes in tests.
type Provider interface {
	// HasRebuild is `declare -F provider::rebuild`.
	HasRebuild() bool
	// HasDoctor is `declare -F provider::doctor`.
	HasDoctor() bool
	// Doctor is `provider::doctor <config> 2>/dev/null`: the stdout report
	// (one `<status>\t<message>` line per check). Its exit is ignored.
	Doctor(ctx context.Context, configFile string) string
	// Rebuild is `provider::rebuild <config> <work_dir>` — DESTRUCTIVE,
	// does not prompt (the caller owns consent); under CLOUD_DRY_RUN it
	// reimages nothing. Output passes through.
	Rebuild(ctx context.Context, configFile, workDir string) error
	// Output is `provider::output <config> 2>/dev/null`: the inventory JSON.
	Output(ctx context.Context, configFile string) ([]byte, error)
}

// Runner drives one recovery. Zero-value seams fall back to the real
// implementations (bash: the primitives the bats replace with fakes).
type Runner struct {
	Paths  *config.Paths
	Exec   execx.Runner
	Stdout io.Writer
	Stderr io.Writer
	// In is the consent prompt's input (bash: `read -r ans` on stdin).
	In io.Reader

	// Force is the global --force|-f (bash: the dynamic-scope ${force}).
	Force bool

	// ResolveSpec overrides provision.ResolveSpec (bats: provision::resolve_spec).
	ResolveSpec func(domainName string) (*provision.Spec, error)
	// LoadProvider overrides the provider load (bats: recover::_load_provider):
	// returns the loaded provider + the resolved config path. nil → the real
	// resolution (spec.provider.name → config → bash bridge).
	LoadProvider func(ctx context.Context, specFile string) (name, configFile string, cleanup func(), prov Provider, err error)
	// NewProvider overrides only the provider CONSTRUCTION inside the real
	// LoadProvider (nil → the bash bridge).
	NewProvider func(ctx context.Context, name string) (Provider, error)
	// Provision overrides the provision phase (bats: provision::dispatch);
	// nil → the bash bridge running provision::dispatch under force=1.
	Provision func(ctx context.Context, domainName string) error
	// DriverKubeconfig overrides driver::kubeconfig (nil → the Go driver
	// registry, looked up by the spec's kind).
	DriverKubeconfig func(ctx context.Context, domainName string) (string, error)
	// KubectlAvailable overrides `command -v kubectl` (nil → execx.Look).
	KubectlAvailable func() bool
	// Now overrides the clock (timings).
	Now func() time.Time

	// Module state (bash: the _RECOVER_* globals), set by resolve and
	// consumed by the phases.
	domain      string
	spec        string
	config      string
	provider    string
	clusterName string
	workDir     string
	prov        Provider
	timings     []string
	// configCleanup removes an inline-config temp file (bash: the EXIT trap).
	configCleanup func()

	promptReader *bufio.Reader
}

func (r *Runner) out() io.Writer {
	if r.Stdout != nil {
		return r.Stdout
	}
	return os.Stdout
}

func (r *Runner) errOut() io.Writer {
	if r.Stderr != nil {
		return r.Stderr
	}
	return os.Stderr
}

func (r *Runner) in() io.Reader {
	if r.In != nil {
		return r.In
	}
	return os.Stdin
}

func (r *Runner) exec() execx.Runner {
	if r.Exec != nil {
		return r.Exec
	}
	return execx.NewRunner(r.Paths)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run is the phase orchestrator (bash: recover::_run). Honors the global
// --force and LOK8S_NONINTERACTIVE for the confirm. Every failure prints
// its diagnostic and returns ErrHandled (bash: return 1).
func (r *Runner) Run(ctx context.Context, domainName string, skipRebuild, dryRun bool) error {
	stderr := r.errOut()

	// Reset module state for this run.
	r.timings = nil
	r.domain = domainName
	r.spec, r.config, r.provider, r.clusterName, r.prov = "", "", "", "", nil
	r.configCleanup = nil
	defer func() {
		if r.configCleanup != nil {
			r.configCleanup()
		}
	}()
	// A work dir is required before anything destructive; abort cleanly if
	// none can be made (its diagnostic is already printed). Nothing has been
	// touched yet.
	wd, err := r.workdir(domainName)
	if err != nil {
		return err
	}
	r.workDir = wd

	t0 := r.now()
	fmt.Fprintf(stderr, "\n\033[1;36m━━━ recover %s ━━━\033[0m\n", domainName)

	// 1. resolve — spec + provider + rebuild-capability. FATAL on failure
	//    (before anything is touched).
	if err := r.timed("resolve", func() error { return r.resolve(ctx) }); err != nil {
		return err
	}

	// 2. doctor — transparent, READ-ONLY infrastructure diagnosis. The
	//    operator sees the infra state + advice BEFORE any destructive
	//    action. Advisory: it never blocks.
	_ = r.timed("doctor", func() error { r.doctor(ctx); return nil })

	// 3. decide / consent.
	if dryRun {
		// Genuinely safe preview: CLOUD_DRY_RUN flows into provider::rebuild's
		// dry-run branches — it prints the exact per-node plan and reimages
		// NOTHING.
		os.Setenv("CLOUD_DRY_RUN", "1")
		os.Setenv("CLOUD_DRY_RUN_PATH", filepath.Join(r.workDir, "dry-run"))
		err := r.timed("rebuild", func() error { return r.rebuild(ctx) })
		// Tidy up: don't leave CLOUD_DRY_RUN in the environment behind us.
		os.Unsetenv("CLOUD_DRY_RUN")
		os.Unsetenv("CLOUD_DRY_RUN_PATH")
		if err != nil {
			return ErrHandled
		}
		fmt.Fprintf(stderr, "\n  \033[2mlo provision WOULD run next (fresh install incl. #wipe-devices), then verify.\033[0m\n")
		r.summary(t0)
		fmt.Fprintf(stderr, "\n\033[1;33m━━━ DRY RUN — nothing changed ━━━\033[0m\n")
		return nil
	}

	// Destructive from here. The confirm is THE guard (honors --force /
	// LOK8S_NONINTERACTIVE). Decline → abort, touch nothing. skipRebuild
	// shapes the prompt wording to what actually happens.
	if !r.confirm(ctx, skipRebuild) {
		ui.Warnf(stderr, "recover: aborted by operator — nothing was changed")
		return ErrHandled
	}

	// 4. rebuild — DESTRUCTIVE node reset. On failure, do NOT continue to
	//    provision on a half-reset cluster.
	if skipRebuild {
		ui.Warnf(stderr, "recover: --skip-rebuild — skipping the node rebuild (provision + verify only)")
		r.timings = append(r.timings, "rebuild=skipped")
	} else if err := r.timed("rebuild", func() error { return r.rebuild(ctx) }); err != nil {
		ui.Errorf(stderr, "recover: rebuild failed — NOT provisioning on a half-reset cluster")
		return ErrHandled
	}

	// 5. provision — fresh install (`lo provision`; the bare-metal path
	//    applies #wipe-devices). On failure, stop.
	if err := r.timed("provision", func() error { return r.provision(ctx) }); err != nil {
		ui.Errorf(stderr, "recover: provision failed")
		return ErrHandled
	}

	// 6. verify — node-Ready count vs inventory count. Advisory (never fatal).
	_ = r.timed("verify", func() error { r.verify(ctx); return nil })

	r.summary(t0)
	return nil
}

// ── phase: resolve ─────────────────────────────────────────
// Resolve the spec, require a cluster domain, load the provider, and require
// the provider implements the rebuild hook (bash: recover::_resolve).
func (r *Runner) resolve(ctx context.Context) error {
	stderr := r.errOut()
	var spec *provision.Spec
	var err error
	if r.ResolveSpec != nil {
		spec, err = r.ResolveSpec(r.domain)
	} else {
		spec, err = provision.ResolveSpec(r.Paths, r.domain, stderr)
	}
	if err != nil {
		return ErrHandled
	}

	// Load provider creds NOW — the bare-metal provider::doctor +
	// provider::rebuild phases (which run BEFORE provision) need
	// HCLOUD_TOKEN/HROBOT_* to resolve node IPs. provision::dispatch loads
	// them itself, but only after rebuild — too late for recover, whose
	// dry-run otherwise aborts "no resolvable public IP".
	_ = provision.LoadProviderCreds(r.Paths, r.domain)

	if spec.Kind != provision.SpecKindCluster {
		kind := spec.Kind
		if kind == "" {
			kind = "none"
		}
		ui.Errorf(stderr, "recover: '%s' is not a cluster domain (kind=%s) — recover rebuilds a cluster from bare metal; a deploy domain has nothing to reset", r.domain, kind)
		return ErrHandled
	}
	r.spec = spec.File

	if err := r.loadProvider(ctx); err != nil {
		return err
	}

	if !r.prov.HasRebuild() {
		name := r.provider
		if name == "" {
			name = "unknown"
		}
		ui.Errorf(stderr, "provider '%s' does not support recover (no provider::rebuild)", name)
		return ErrHandled
	}

	r.clusterName = r.resolveClusterName()
	ui.Debugf(stderr, "recover: resolved %s → provider=%s cluster=%s", r.domain, r.provider, r.clusterName)
	return nil
}

// providerNameRe is provider::read_name's allowlist.
var providerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// readProviderName is provider::read_name WITH its diagnostic: the invalid-
// name error surfaces on stderr (recover deliberately does not suppress it,
// so an invalid name stays distinguishable from a missing one). "" + false
// when missing or invalid.
func readProviderName(stderr io.Writer, specFile string) (string, bool) {
	var doc struct {
		Spec struct {
			Provider struct {
				Name any `yaml:"name"`
			} `yaml:"provider"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(specFile)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil || doc.Spec.Provider.Name == nil {
		return "", false
	}
	name := fmt.Sprint(doc.Spec.Provider.Name)
	if name == "" || name == "false" {
		return "", false
	}
	if !providerNameRe.MatchString(name) {
		ui.Errorf(stderr, "provider name '%s' is invalid (must be alphanumeric + hyphens/underscores)", name)
		return "", false
	}
	return name, true
}

// loadProvider resolves + loads the cluster's infrastructure provider
// (bash: recover::_load_provider), setting provider + config.
func (r *Runner) loadProvider(ctx context.Context) error {
	stderr := r.errOut()
	if r.LoadProvider != nil {
		name, cfg, cleanup, prov, err := r.LoadProvider(ctx, r.spec)
		if err != nil {
			return err
		}
		r.configCleanup = cleanup
		r.provider, r.config, r.prov = name, cfg, prov
		return nil
	}

	pname, ok := readProviderName(stderr, r.spec)
	if !ok {
		ui.Errorf(stderr, "recover: cluster '%s' has no usable spec.provider (recover needs a provider that supports rebuild)", r.domain)
		return ErrHandled
	}
	cfg, cleanup, err := provision.WriteProviderConfig(r.spec, stderr)
	if err != nil {
		ui.Errorf(stderr, "recover: could not resolve provider config for '%s'", pname)
		return ErrHandled
	}
	// bash: the inline temp config lives until process exit (EXIT trap) —
	// every later phase reads it. Released when Run returns.
	r.configCleanup = cleanup
	newProv := r.NewProvider
	if newProv == nil {
		newProv = r.bashProvider
	}
	prov, err := newProv(ctx, pname)
	if err != nil {
		ui.Errorf(stderr, "recover: provider '%s' failed to load", pname)
		return ErrHandled
	}
	r.provider, r.config, r.prov = pname, cfg, prov
	return nil
}

// ── phase: doctor ──────────────────────────────────────────
// Render the provider's read-only diagnosis, mirroring `lo doctor`'s
// "provider / infrastructure" section (bash: recover::_doctor). Advisory:
// never mutates.
func (r *Runner) doctor(ctx context.Context) {
	out := r.out()
	fmt.Fprintln(out)
	fmt.Fprintf(out, "--- provider / infrastructure (%s) ---\n", r.provider)
	if !r.prov.HasDoctor() {
		fmt.Fprintf(out, "  \033[33m!\033[0m provider has no doctor hook — infrastructure diagnosis unavailable\n")
		return
	}
	report := r.prov.Doctor(ctx, r.config)
	for _, line := range strings.Split(strings.TrimSuffix(report, "\n"), "\n") {
		// bash: IFS=$'\t' read -r status msg — tab-split; leading/trailing
		// tabs are IFS whitespace and stripped from both fields.
		line = strings.Trim(line, "\t")
		status, msg, _ := strings.Cut(line, "\t")
		msg = strings.Trim(msg, "\t")
		switch status {
		case "ok":
			fmt.Fprintf(out, "  \033[32m✓\033[0m %s\n", msg)
		case "warn":
			fmt.Fprintf(out, "  \033[33m!\033[0m %s\n", msg)
		case "summary":
			fmt.Fprintf(out, "    %s\n", msg)
		case "":
		default:
			if msg != "" {
				msg = " " + msg
			}
			fmt.Fprintf(out, "    %s%s\n", status, msg)
		}
	}
}

// ── phase: rebuild ─────────────────────────────────────────
// Reset the cluster's existing nodes from bare metal (provider::rebuild).
// DESTRUCTIVE but does not prompt — consent already happened.
func (r *Runner) rebuild(ctx context.Context) error {
	return r.prov.Rebuild(ctx, r.config, r.workDir)
}

// ── phase: provision ───────────────────────────────────────
// Fresh install on the reset nodes (= `lo provision`). Consent already
// happened at recover's entry — the dispatch's real-infrastructure gate is
// pre-authorized (force=1) so DR never stalls on a prompt mid-recovery.
func (r *Runner) provision(ctx context.Context) error {
	if r.Provision != nil {
		return r.Provision(ctx, r.domain)
	}
	return r.bashProvision(ctx, r.domain)
}

// ── phase: verify ──────────────────────────────────────────
// Compare Ready nodes (kubectl) to the inventory count (provider::output).
// KUBECONFIG is PINNED to the RECOVERED cluster's OWN kubeconfig — NEVER the
// ambient one — so a false green against a DIFFERENT >=N-Ready cluster is
// impossible; a missing kubeconfig reports the shortfall, it does NOT fall
// back. Advisory (bash: recover::_verify).
func (r *Runner) verify(ctx context.Context) {
	out, stderr := r.out(), r.errOut()
	want, known := r.inventoryCount(ctx)
	// An UNRESOLVED inventory is the sentinel, NOT a real 0 — show it as
	// "unknown" and NEVER declare success against it.
	wantDisp := "unknown"
	if known {
		wantDisp = fmt.Sprint(want)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "--- verify ---")
	kubeconfig := r.kubeconfigPath(ctx)
	if !fileExists(kubeconfig) {
		fmt.Fprintf(stderr, "  \033[33m!\033[0m recovered kubeconfig not found (%s) — cannot confirm node readiness\n", kubeconfig)
	}
	ready := r.readyNodes(ctx, kubeconfig)
	fmt.Fprintf(out, "  nodes Ready: %d/%s\n", ready, wantDisp)
	if known && want > 0 && ready >= want {
		fmt.Fprintf(out, "  \033[32m✓\033[0m all %d node(s) Ready — cluster back from bare metal\n", want)
	} else {
		fmt.Fprintf(out, "  \033[33m!\033[0m only %d/%s node(s) Ready — run: lo status %s (app/data restore is a separate step)\n", ready, wantDisp, r.domain)
	}
}

// inventoryCount is the number of nodes the provider resolves (bash:
// recover::_inventory_count). known=false is the EMPTY sentinel: a failed
// provider::output (missing creds / API error) or non-JSON output. Callers
// MUST distinguish it from a genuine 0.
func (r *Runner) inventoryCount(ctx context.Context) (int, bool) {
	raw, err := r.prov.Output(ctx, r.config)
	if err != nil {
		return 0, false
	}
	// jq '(.nodes // []) | length'
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return 0, false
	}
	nodes, present := top["nodes"]
	if !present || string(nodes) == "null" || string(nodes) == "false" {
		return 0, true
	}
	var arr []json.RawMessage
	if json.Unmarshal(nodes, &arr) == nil {
		return len(arr), true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(nodes, &obj) == nil {
		return len(obj), true
	}
	var s string
	if json.Unmarshal(nodes, &s) == nil {
		return len([]rune(s)), true
	}
	return 0, false
}

// readyNodes counts Ready nodes and prints per-node detail to stderr (bash:
// recover::_ready_nodes). Reads the PINNED kubeconfig. No kubectl / no such
// kubeconfig → 0. A cordoned-but-Ready node shows `Ready,SchedulingDisabled`
// — it IS Ready, so any status STARTING with `Ready` counts.
func (r *Runner) readyNodes(ctx context.Context, kubeconfig string) int {
	if !r.kubectlAvailable() {
		return 0
	}
	if kubeconfig == "" || !fileExists(kubeconfig) {
		return 0
	}
	var buf strings.Builder
	err := r.exec().Run(ctx, execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"get", "nodes", "--no-headers"},
		Env:    []string{"KUBECONFIG=" + kubeconfig},
		Stdout: &buf,
		Stderr: io.Discard,
	})
	if err != nil {
		return 0
	}
	stderr := r.errOut()
	count := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name, status := fields[0], ""
		if len(fields) > 1 {
			status = fields[1]
		}
		if strings.HasPrefix(status, "Ready") {
			fmt.Fprintf(stderr, "  \033[32m✓\033[0m %s %s\n", name, status)
			count++
		} else {
			if status == "" {
				status = "Unknown"
			}
			fmt.Fprintf(stderr, "  \033[33m!\033[0m %s %s\n", name, status)
		}
	}
	return count
}

func (r *Runner) kubectlAvailable() bool {
	if r.KubectlAvailable != nil {
		return r.KubectlAvailable()
	}
	_, ok := execx.Look(r.Paths, "kubectl")
	return ok
}

// ── consent ────────────────────────────────────────────────
// The destructive guard (bash: recover::_confirm). `--force` (global) or
// LOK8S_NONINTERACTIVE opt out of the prompt; otherwise require a literal
// "yes" read from In.
//   - The node count comes from inventoryCount; an unresolved inventory
//     says "an unknown number of nodes" rather than a misleading "0".
//   - Under --skip-rebuild the prompt describes the re-provision (still
//     destructive) — NOT a bare-metal reset. Consent is still required.
func (r *Runner) confirm(ctx context.Context, skipRebuild bool) bool {
	if r.Force || os.Getenv("LOK8S_NONINTERACTIVE") != "" {
		return true
	}
	cluster := r.clusterName
	if cluster == "" {
		cluster = r.domain
	}
	count := "an unknown number of nodes"
	if n, ok := r.inventoryCount(ctx); ok {
		count = fmt.Sprintf("%d node(s)", n)
	}
	var action string
	if skipRebuild {
		action = fmt.Sprintf("re-provision %s of cluster %s (reinstalling Kubernetes, may wipe data disks) — the bare-metal node reset is SKIPPED", count, cluster)
	} else {
		action = fmt.Sprintf("reset %s of cluster %s from bare metal and reinstall them", count, cluster)
	}
	fmt.Fprintf(r.errOut(), "\033[31m!\033[0m recover: this will %s — continue? [type yes to continue] ", action)
	ans, err := r.readAnswer()
	if err != nil {
		return false
	}
	return ans == "yes"
}

// readAnswer is `read -r ans`: one line, IFS whitespace trimmed; EOF with
// no data is a failed read.
func (r *Runner) readAnswer() (string, error) {
	if r.promptReader == nil {
		r.promptReader = bufio.NewReader(r.in())
	}
	line, err := r.promptReader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.Trim(line, " \t\n"), nil
}

// ── helpers ────────────────────────────────────────────────

// workdir is the provider scratch dir (bash: recover::_workdir): the
// per-domain .provider/ (already gitignored) when the domain passes the
// SAME guard resolve_spec uses — this runs BEFORE resolve, so an
// unvalidated domain must not drive a mkdir (path traversal). A bad — or
// unwritable — domain falls back to a private temp dir; if even that fails
// it errors (NO shared /tmp last resort).
func (r *Runner) workdir(domainName string) (string, error) {
	if domain.NameRe.MatchString(domainName) {
		wd := filepath.Join(r.Paths.Clusters, domainName, ".provider")
		if err := os.MkdirAll(wd, 0o755); err == nil {
			return wd, nil
		}
	}
	tmp, err := os.MkdirTemp("", "tmp.")
	if err != nil {
		ui.Errorf(r.errOut(), "recover: could not create a work directory (mktemp failed)")
		return "", ErrHandled
	}
	return tmp, nil
}

// kubeconfigPath is the RECOVERED cluster's OWN kubeconfig (bash:
// recover::_kubeconfig_path). Prefers the driver's own resolver so the path
// stays in LOCKSTEP with wherever the driver writes it; falls back to the
// local reimplementation (PATH_BASE/.kubeconfig/<metadata.name>.yaml,
// defaulting to the cluster name / domain).
func (r *Runner) kubeconfigPath(ctx context.Context) string {
	if path, err := r.driverKubeconfig(ctx); err == nil && path != "" {
		return path
	}
	name := ""
	if fileExists(r.spec) {
		name = specMetadataName(r.spec)
	}
	if name == "" || name == "null" {
		name = r.clusterName
		if name == "" {
			name = r.domain
		}
	}
	return filepath.Join(r.Paths.Base, ".kubeconfig", name+".yaml")
}

func (r *Runner) driverKubeconfig(ctx context.Context) (string, error) {
	if r.DriverKubeconfig != nil {
		return r.DriverKubeconfig(ctx, r.domain)
	}
	kind, err := domain.SpecDriver(r.spec, "")
	if err != nil {
		return "", err
	}
	factory, ok := driver.Get(kind)
	if !ok {
		return "", fmt.Errorf("no driver %s", kind)
	}
	drv, err := factory(&driver.Deps{Paths: r.Paths, Runner: r.exec(), Stderr: io.Discard})
	if err != nil {
		return "", err
	}
	return drv.Kubeconfig(ctx, r.domain)
}

// resolveClusterName is the human name for the consent prompt (bash:
// recover::_cluster_name): provider descriptor .cluster_name, else spec
// metadata.name, else the domain.
func (r *Runner) resolveClusterName() string {
	n := ""
	if fileExists(r.config) {
		n = yamlScalar(r.config, "cluster_name")
	}
	if (n == "" || n == "null") && fileExists(r.spec) {
		n = specMetadataName(r.spec)
	}
	if n == "" || n == "null" {
		n = r.domain
	}
	return n
}

// timed runs one phase, prints + records "<label>=XmYs", and returns the
// phase's error so the caller can abort the sequence (bash: recover::_timed).
func (r *Runner) timed(label string, fn func() error) error {
	s := r.now()
	err := fn()
	d := int(r.now().Unix() - s.Unix())
	fmt.Fprintf(r.errOut(), "\n\033[1;35m━━━ ⏱ %s took %dm %ds ━━━\033[0m\n", label, d/60, d%60)
	r.timings = append(r.timings, fmt.Sprintf("%s=%dm%ds", label, d/60, d%60))
	return err
}

// summary prints the wall-clock total + per-phase breakdown.
func (r *Runner) summary(t0 time.Time) {
	d := int(r.now().Unix() - t0.Unix())
	fmt.Fprintf(r.errOut(), "\n\033[1;35m━━━ DONE in %dm %ds — phases: %s ━━━\033[0m\n", d/60, d%60, strings.Join(r.timings, " "))
}

// PickDomain: the positional outranks --domain/active (bash:
// recover::_pick_domain). A second positional is a hard error — on a command
// that reimages a fleet a mistyped flag must fail loudly, never be
// swallowed. An empty positional falls through to the fallback.
func PickDomain(stderr io.Writer, fallback string, positionals []string) (string, error) {
	if len(positionals) > 1 {
		ui.Errorf(stderr, "too many arguments: %s", strings.Join(positionals[1:], " "))
		return "", ErrHandled
	}
	if len(positionals) == 1 && positionals[0] != "" {
		return positionals[0], nil
	}
	return fallback, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// specMetadataName is `yq -r '.metadata.name // ""'` ("" when unreadable).
func specMetadataName(path string) string {
	var doc struct {
		Metadata struct {
			Name any `yaml:"name"`
		} `yaml:"metadata"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil || doc.Metadata.Name == nil {
		return ""
	}
	return fmt.Sprint(doc.Metadata.Name)
}

// yamlScalar is `yq -r '.<key> // ""'` on a top-level key of a YAML/JSON
// file ("" on any failure).
func yamlScalar(path, key string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]any
	if yaml.Unmarshal(raw, &doc) != nil || doc[key] == nil {
		return ""
	}
	return fmt.Sprint(doc[key])
}

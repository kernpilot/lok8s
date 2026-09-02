package bootstrap

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/ui"
)

// raceRe: transient race between an addon's controllers and the resources
// they own. Three symptoms: "ensure CRDs are installed first" and "no
// matches for kind" / "resource mapping not found" (a CR applied before its
// CRD is Established), and "failed calling webhook" (a CR applied before
// its validating/mutating webhook is serving) — bash: the _race regex in
// bootstrap::_apply_one.
var raceRe = regexp.MustCompile(`ensure CRDs are installed first|failed calling webhook|no matches for kind|resource mapping not found`)

// applyOne applies ONE resolved bootstrap entry (bash:
// bootstrap::_apply_one): the hosted platform-owned skip, the KubeOne
// cilium/ccm skip, addons.Render (value-stacking + kustomize build +
// envsubst), the kapply apply, the immutable/terminating fail-fast, the
// CRD/webhook race-retry (the 6-retry class), and — only when job.WaitFlag
// is non-empty — the post-apply WaitReady. Returns 0 on success, 1 on an
// unrecoverable render/apply error.
//
// The bash MUST-run-in-a-subshell rule (KAPPLY_LAST_OUTPUT isolation across
// concurrent jobs) maps to the Applier being function-local here: each
// invocation owns its own output state, so concurrent goroutines never race.
func (e *Engine) applyOne(ctx context.Context, job Job, stdout, stderr io.Writer) int {
	// job env overrides (bash: exported per-subshell before addons::render,
	// never leaking across entries). Name the keys LOK8S_USER_*/LOK8S_SPEC_*
	// — the same convention the addons reference; other names are passed to
	// the render process but not envsubst-whitelisted.
	env := map[string]string{}
	if job.EnvLines != "" {
		for _, kv := range strings.Split(job.EnvLines, "\n") {
			if kv == "" {
				continue
			}
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
	}

	// HOSTED clusters: the CNI + cloud integration belong to the PLATFORM —
	// the hosted control plane manages cilium as a system application in
	// the user cluster and runs the cloud integration on its side.
	// Unconditional (not existence-gated): even mid-install, bootstrapping
	// our own copy would fight the platform for ownership of the datapath.
	// The list lives in PlatformOwned — one place to extend.
	adBase := filepath.Base(job.Dir)
	if e.Hosted {
		for _, po := range PlatformOwned {
			if adBase == po {
				fmt.Fprintf(stderr, "[bootstrap] %s is platform-owned on a hosted cluster (the platform manages the CNI/cloud integration) — skipping\n", adBase)
				return 0
			}
		}
	}

	// KubeOne applies cilium + the hcloud CCM itself during `kubeone apply`
	// (rendered custom addons) to break the cni:external deadlock. On a
	// FULL provision that reconcile has just run — re-applying here would
	// steal SSA field ownership and the next `kubeone apply` fights back,
	// so defer to the driver. On `lo provision --bootstrap` the infra
	// reconcile is SKIPPED — bootstrap is the ONLY applier and MUST
	// reconcile cilium/ccm from spec.bootstrap. So gate on the PATH —
	// LOK8S_BOOTSTRAP_ONLY — NOT on "does the DaemonSet exist": that check
	// can never tell a fresh driver-apply from a weeks-old DS, so it
	// silently blocked EVERY cilium/ccm reconcile through --bootstrap.
	// Keyed on the addon DIRECTORY basename: a `name:` override changes
	// job.Name, not job.Dir, so a renamed cilium/ccm can't defeat the skip
	// and double-apply the driver's own.
	if job.Kind == "kubeone" && e.BootstrapOnly != "1" && (adBase == "cilium" || adBase == "ccm") {
		fmt.Fprintf(stderr, "[bootstrap] %s is applied by the KubeOne driver on a full provision — skipping (use 'lo provision --bootstrap' to reconcile it from spec.bootstrap)\n", adBase)
		return 0
	}

	// Render via the canonical khelm path (value-stacking + kustomize build
	// + envsubst) — the SAME addons.Render the KubeOne driver stages addons
	// with.
	ui.Debugf(stderr, "bootstrap: rendering %s", job.Name)
	rendered, err := addons.Render(ctx, e.Runner, stderr, job.Dir, job.Kind, job.Provider, job.Inline, env)
	if err != nil {
		ui.Errorf(stderr, "bootstrap: render failed for %s", job.Name)
		return 1
	}

	applier := e.newApplier(job, stdout, stderr)
	kcFlags := []string{"--kubeconfig", job.Kubeconfig}

	// Apply with server-side + CRD retry. The applier heals immutable /
	// stuck-Terminating conflicts (interactive prompt, or force-recreate),
	// and passes CRD-not-installed / webhook-not-ready errors through
	// unchanged so the transient-race retry below still fires.
	out, rc := applier.Apply(ctx, job.Name, rendered, kcFlags...)

	// Unrecoverable conflict that wasn't healed (declined the prompt, or
	// non-interactive without force-recreate) — fail fast rather than
	// "succeeding" and letting the next `lo up` / Tilt loop hit the same
	// wall.
	if rc != 0 && (kapply.ImmutableRe.MatchString(out) || kapply.TerminatingRe.MatchString(out)) {
		ui.Errorf(stderr, "bootstrap: %s has objects blocked by an immutable/terminating conflict — see above (try: lo up --force-recreate)", job.Name)
		return 1
	}

	// Generic apply failure: non-zero and the output matches NEITHER an
	// immutable/terminating conflict (handled above) NOR the transient
	// CRD/webhook race (retried below). Fail rather than silently
	// "succeeding".
	if rc != 0 && !raceRe.MatchString(out) {
		ui.Errorf(stderr, "bootstrap: %s apply failed (rc=%d) — see above", job.Name, rc)
		return 1
	}

	if raceRe.MatchString(out) {
		ui.Debugf(stderr, "bootstrap: %s hit a CRD/webhook race — settling deps before retry", job.Name)
		// (1) CRDs must be Established before their CRs resolve.
		_ = kapply.Run("CRDs established", stdout, stderr, func(o, eo io.Writer) error {
			return e.Runner.Run(ctx, execx.Cmd{
				Name: "kubectl",
				Args: append(append([]string{}, kcFlags...),
					"wait", "--for=condition=Established", "crd", "--all", "--timeout=60s"),
				Stdout: o, Stderr: eo,
			})
		})
		// (2) The KEY wait the old single-retry skipped: this addon's own
		// webhook/controller Deployments must be Available before the
		// resources they validate will apply. Wait on THIS addon's
		// rendered workloads.
		_ = applier.WaitReady(ctx, job.Name, 120, rendered, kcFlags...)
		// (3) Retry with backoff to absorb the brief gap between a webhook
		// Deployment going Available and its Service endpoints being Ready.
		const max = 6
		delay := 3
		retryRC := 1
		for attempt := 1; attempt <= max; attempt++ {
			label := fmt.Sprintf("%s (retry %d/%d)", job.Name, attempt, max)
			out, retryRC = applier.Apply(ctx, label, rendered, kcFlags...)
			if retryRC == 0 && !raceRe.MatchString(out) {
				break
			}
			e.sleep(time.Duration(delay) * time.Second)
			if delay < 15 {
				delay += 3
			}
		}
		if retryRC != 0 || raceRe.MatchString(out) {
			ui.Errorf(stderr, "bootstrap: %s still failing after %d retries — CRDs/webhook not ready (see above)", job.Name, max)
			return 1
		}
	}

	// Post-apply health wait — run it only when the scheduler asks
	// (WaitFlag set), i.e. when something depends on THIS entry being
	// Ready: a dep-target or a wait-gate. A pure leaf skips it (that
	// serial wait was the whole point of the refactor). Best-effort: a
	// timeout is a ⚠, not fatal — the caller decides whether to care.
	if job.WaitFlag != "" {
		ui.Debugf(stderr, "bootstrap: waiting for %s workloads to become ready", job.Name)
		_ = applier.WaitReady(ctx, job.Name, 180, rendered, kcFlags...)
	}
	return 0
}

// newApplier builds the per-job kapply Applier: force from the job (the
// resolve-parked re-apply, or an inherited --force), non-interactive for
// backgrounded jobs (bash: the exported LOK8S_NONINTERACTIVE=1 on every
// concurrent subshell).
func (e *Engine) newApplier(job Job, stdout, stderr io.Writer) *kapply.Applier {
	a := kapply.NewApplier(e.Runner, stdout, stderr)
	if job.Force {
		a.ForceRecreate = true
	}
	if job.NonInteractive {
		a.NonInteractive = true
	}
	if e.Sleep != nil {
		a.Sleep = e.Sleep
	}
	return a
}

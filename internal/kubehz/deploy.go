package kubehz

// deploy.go — libs/kubehz/deploy: render and apply the in-cluster kubehz
// agent. TWO AGENTS, ONE HEARTBEAT: the CronJob (manifests/agent) owns the
// identity and beats every five minutes; the Go live agent
// (manifests/live-agent) beats within seconds. EXACTLY ONE may beat — the
// platform stores the snapshot latest-wins, so two producers erase each
// other. The lever is KUBEHZ_HEARTBEAT_OWNER in ConfigMap
// kubehz-agent-config (one-directional: it silences the CronJob; only
// deleting its Deployment silences the live agent), which is why the apply
// ORDER differs per direction and every step that could arm a second
// producer FAILS rather than continues.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The three waits, all in seconds, all overridable from the environment.
func (c *Context) envSeconds(name string, def int) int {
	if v := c.getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (c *Context) heartbeatDrainSeconds() int {
	return c.envSeconds("KUBEHZ_HEARTBEAT_DRAIN_SECONDS", 130)
}
func (c *Context) liveAgentRolloutSeconds() int {
	return c.envSeconds("KUBEHZ_LIVE_AGENT_ROLLOUT_SECONDS", 120)
}
func (c *Context) liveAgentDrainSeconds() int {
	return c.envSeconds("KUBEHZ_LIVE_AGENT_DRAIN_SECONDS", 120)
}

// Deploy is `lo kubehz deploy [--dry-run]` for the active domain. Applies
// against the AMBIENT kubeconfig.
func (c *Context) Deploy(ctx context.Context, domain string, dryRun bool) error {
	cy, err := c.requireDomainSpec(domain)
	if err != nil {
		return err
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return err
	}
	if err := c.Validate(cfg, cy); err != nil {
		return err
	}
	return c.DeployAgent(ctx, cfg, domain, dryRun)
}

var (
	deployDomainRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	deployAPIURLRe = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]+$`)
)

// DeployAgent ports kubehz::deploy_agent (the :args-free orchestration).
func (c *Context) DeployAgent(ctx context.Context, cfg *Config, domain string, dryRun bool) error {
	apiURL := cfg.APIURL
	owner := cfg.Agent
	if owner == "" {
		owner = "cronjob"
	}
	access := cfg.Access
	if access == "" {
		access = "none"
	}
	if cfg.Hosting == "shared" {
		c.errorf("hosting: shared has no in-cluster agent to deploy (a Space's nodes enroll with 'lo kubehz join')")
		return ErrHandled
	}
	if access == "none" {
		c.errorf("spec.kubehz.access is 'none' — there is no agent to deploy. Set access: registered (or managed) first.")
		return ErrHandled
	}
	// The agent's bearer token travels on this URL, and it is about to be
	// baked into a ConfigMap that outlives the command.
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		return err
	}
	// A spec is still input — pin the shapes before they reach a manifest.
	if !deployDomainRe.MatchString(domain) {
		c.errorf("refusing to template an implausible cluster domain: %s", domain)
		return ErrHandled
	}
	if !deployAPIURLRe.MatchString(apiURL) {
		c.errorf("refusing to template an implausible spec.kubehz.apiUrl: %s", apiURL)
		return ErrHandled
	}
	if owner != "cronjob" && owner != "operator" {
		c.errorf("refusing to template spec.kubehz.agent '%s' — must be 'cronjob' or 'operator'", owner)
		return ErrHandled
	}

	workdir, err := os.MkdirTemp("", "")
	if err != nil {
		c.errorf("could not create a render directory")
		return ErrHandled
	}
	defer func() { _ = os.RemoveAll(workdir) }()

	if err := c.RenderAgent(workdir, domain, apiURL, owner, access); err != nil {
		return err
	}
	if dryRun {
		return c.deployPrint(ctx, workdir, owner, access)
	}
	if err := c.deployApply(ctx, workdir, owner, access); err != nil {
		return err
	}
	c.deploySummary(domain, owner, access)
	return nil
}

// RenderAgent ports kubehz::render_agent: copy the shipped manifests into
// <workdir> and substitute the placeholder tokens. BOTH trees are always
// rendered (cronjob mode still needs the live-agent tree so the delete has
// the full object set). Nothing may reach a cluster with a placeholder
// still in it.
func (c *Context) RenderAgent(workdir, domain, apiURL, owner, access string) error {
	mfs := Manifests()
	if err := copyFS(mfs, "agent", filepath.Join(workdir, "agent")); err != nil {
		c.errorf("kubehz agent manifests not found at %s", "manifests/agent")
		return ErrHandled
	}
	if err := copyFS(mfs, "live-agent", filepath.Join(workdir, "live-agent")); err != nil {
		c.errorf("kubehz live-agent manifests not found at %s", "manifests/live-agent")
		return ErrHandled
	}
	// One substitution pass over every rendered *.yaml — a literal replace,
	// so a `&` or `|` in the apiUrl lands verbatim.
	rep := strings.NewReplacer(
		"KUBEHZ_API_URL_PLACEHOLDER", apiURL,
		"CLUSTER_ID_PLACEHOLDER", domain,
		"HEARTBEAT_OWNER_PLACEHOLDER", owner,
	)
	err := filepath.WalkDir(workdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(rep.Replace(string(raw))), 0o644) // #nosec G306 -- rendered agent manifests; no secret material
	})
	if err != nil {
		return err
	}
	// The scan itself must succeed — a check that could not run is not a
	// check that passed.
	var leftovers []string
	scanErr := filepath.WalkDir(workdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "PLACEHOLDER") {
			rel, _ := filepath.Rel(workdir, path)
			leftovers = append(leftovers, rel)
		}
		return nil
	})
	if scanErr != nil {
		c.errorf("kubehz: could not scan the rendered manifests for leftover placeholders (grep exited %d: %s) — refusing to apply, because a scan that FAILED is not a scan that passed", 2, scanErr)
		return ErrHandled
	}
	if len(leftovers) > 0 {
		c.errorf("kubehz: rendered manifests still carry a placeholder — refusing to apply:")
		for _, f := range leftovers {
			c.echoErr("  %s", f)
		}
		return ErrHandled
	}
	return nil
}

// copyFS materializes an embedded subtree on disk.
func copyFS(src fs.FS, root, dst string) error {
	if _, err := fs.Stat(src, root); err != nil {
		return err
	}
	return fs.WalkDir(src, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, root)
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644) // #nosec G306 -- a copied agent manifest; no secret material
	})
}

// liveAgentOverlay ports kubehz::live_agent_overlay: the read-only base, or
// managed (a superset that adds the acting RBAC). Anything that is not
// "managed" is read-only.
func liveAgentOverlay(workdir, access string) string {
	if access == "managed" {
		return filepath.Join(workdir, "live-agent", "managed")
	}
	return filepath.Join(workdir, "live-agent", "base")
}

// deployPrint ports kubehz::deploy_print: what would be applied, in apply
// order, rendered with `kubectl kustomize` (the SAME embedded kustomize the
// apply uses).
func (c *Context) deployPrint(ctx context.Context, workdir, owner, access string) error {
	c.echo("# --- CronJob agent (kubehz-heartbeat) — identity + enrollment; heartbeat owner: %s ---", owner)
	if err := c.run(ctx, "kubectl", "kustomize", filepath.Join(workdir, "agent")); err != nil {
		return err
	}
	if owner == "operator" {
		c.echo("---")
		c.echo("# --- Live agent (kubehz-live-agent) — %s tier RBAC ---", access)
		if err := c.run(ctx, "kubectl", "kustomize", liveAgentOverlay(workdir, access)); err != nil {
			return err
		}
	} else {
		c.echo("---")
		c.echo("# The live agent is NOT deployed in cronjob mode; a previous install would be removed.")
	}
	return nil
}

// deployApply ports kubehz::deploy_apply — the load-bearing apply order.
func (c *Context) deployApply(ctx context.Context, workdir, owner, access string) error {
	overlay := liveAgentOverlay(workdir, access)
	agentDir := filepath.Join(workdir, "agent")

	if owner == "operator" {
		// 1. The marker first (the CronJob stops beating).
		if err := c.run(ctx, "kubectl", "apply", "-k", agentDir); err != nil {
			c.errorf("kubehz: could not apply the CronJob agent (identity bootstrap) — nothing else was changed")
			return ErrHandled
		}
		// 2. Wait out an in-flight heartbeat pod (fail-soft).
		c.waitHeartbeatIdle(ctx)
		// 3. The live agent, which becomes the only producer.
		if err := c.run(ctx, "kubectl", "apply", "-k", overlay); err != nil {
			c.errorf("kubehz: could not apply the live agent. The CronJob is no longer beating (KUBEHZ_HEARTBEAT_OWNER=operator), so this cluster is reporting NOTHING until you retry or set spec.kubehz.agent back to cronjob and re-run.")
			return ErrHandled
		}
		// 4. Accepted is not running.
		rollout := c.liveAgentRolloutSeconds()
		if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "rollout", "status", "deployment/kubehz-live-agent",
			"--timeout="+strconv.Itoa(rollout)+"s"); err != nil {
			c.errorf("kubehz: the live agent was accepted but never became Ready within %ds. NOTHING owns the heartbeat right now: the CronJob no longer beats (KUBEHZ_HEARTBEAT_OWNER=operator) and the live agent is not running, so this cluster is reporting nothing. Diagnose with 'kubectl -n kubehz-system describe deployment/kubehz-live-agent' (an unpullable image is the usual cause), or set spec.kubehz.agent: cronjob and re-run 'lo kubehz deploy' to hand the beat straight back.", rollout)
			return ErrHandled
		}
		return nil
	}

	// cronjob mode: remove the live agent BEFORE re-arming the CronJob's beat.
	if err := c.run(ctx, "kubectl", "delete", "-k", filepath.Join(workdir, "live-agent", "managed"), "--ignore-not-found=true"); err != nil {
		c.errorf("kubehz: could not remove the live agent — nothing else was changed. The CronJob's beat was NOT re-armed, so this cluster still has exactly one producer (the live agent, which keeps reporting) and no data was lost. Usual causes: the kubeconfig lacks delete permission in kubehz-system, or the apiserver is unreachable. Fix that and re-run 'lo kubehz deploy'.")
		return ErrHandled
	}
	// Sweep the producing object by its semantic label (survives a rename).
	if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "delete", "deployment",
		"-l", "app.kubernetes.io/part-of=kubehz,app.kubernetes.io/component=live-view", "--ignore-not-found=true"); err != nil {
		c.errorf("kubehz: could not sweep live-agent Deployments by label — nothing else was changed. The CronJob's beat was NOT re-armed, so there is still one producer. Fix the delete and re-run 'lo kubehz deploy'.")
		return ErrHandled
	}
	if err := c.waitLiveAgentGone(ctx); err != nil {
		return err
	}
	if err := c.run(ctx, "kubectl", "apply", "-k", agentDir); err != nil {
		c.errorf("kubehz: could not apply the CronJob agent. The live agent is gone and the marker was not rewritten, so this cluster is reporting NOTHING until you re-run 'lo kubehz deploy'.")
		return ErrHandled
	}
	return nil
}

// podLines keeps only the `pod/<name>` lines of a merged capture — an
// apiserver `Warning:` on stderr is not a pod.
func podLines(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "pod/") {
			return true
		}
	}
	return false
}

func oneLine(s string) string { return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", " ") }

// waitHeartbeatIdle ports kubehz::wait_heartbeat_idle — FAIL-SOFT: an
// unreadable probe or a stuck pod WARNS and lets the deploy continue.
func (c *Context) waitHeartbeatIdle(ctx context.Context) {
	drain := c.heartbeatDrainSeconds()
	for waited := 0; waited < drain; waited += 5 {
		out, err := c.captureBoth(ctx, "kubectl", "-n", "kubehz-system", "get", "pods", "-l", "app=kubehz-heartbeat",
			"--field-selector=status.phase=Running", "-o", "name")
		if err != nil {
			c.warnf("kubehz: could not check for an in-flight heartbeat pod (%s) — continuing; the live agent overwrites a stray schema-1 beat within a minute", oneLine(out))
			return
		}
		if !podLines(out) {
			return
		}
		if waited == 0 {
			c.echo("kubehz: waiting for the in-flight heartbeat pod to finish before starting the live agent…")
		}
		c.sleep(5 * time.Second)
	}
	c.warnf("kubehz: a heartbeat pod is still running after %ds — continuing; it may deliver one last schema-1 beat, which the live agent overwrites within a minute", drain)
}

// waitLiveAgentGone ports kubehz::wait_live_agent_gone — FAIL-HARD: a
// failing probe counts as STILL RUNNING, and a window that ends on a failed
// probe refuses with that reason.
func (c *Context) waitLiveAgentGone(ctx context.Context) error {
	drain := c.liveAgentDrainSeconds()
	probeError := ""
	for waited := 0; waited < drain; waited += 5 {
		out, err := c.captureBoth(ctx, "kubectl", "-n", "kubehz-system", "get", "pods",
			"-l", "app.kubernetes.io/component=live-view", "-o", "name")
		if err == nil {
			probeError = ""
			if !podLines(out) {
				return nil
			}
		} else {
			if probeError == "" {
				c.warnf("kubehz: cannot list the live agent's pods in kubehz-system (%s) — treating that as STILL RUNNING and retrying; the CronJob's beat stays disarmed until the pod is provably gone", oneLine(out))
			}
			probeError = oneLine(out)
		}
		if waited == 0 {
			c.echo("kubehz: waiting for the live agent's pod to terminate before the CronJob beats again…")
		}
		c.sleep(5 * time.Second)
	}
	if probeError != "" {
		c.errorf("kubehz: could not tell whether the live agent's pod is gone — listing pods in kubehz-system kept failing for %ds: %s. The CronJob's beat was NOT re-armed, because re-arming it beside a pod that may still be beating puts two producers on the heartbeat and erases the live view every five minutes. Give the kubeconfig 'list pods' in kubehz-system (or fix the apiserver connection), confirm with 'kubectl -n kubehz-system get pods -l app.kubernetes.io/component=live-view', then re-run 'lo kubehz deploy'.", drain, probeError)
		return ErrHandled
	}
	c.errorf("kubehz: the live agent's pod is still running %ds after its Deployment was deleted. The CronJob's beat was NOT re-armed — doing so now would put two producers on the heartbeat, which erases the live view every five minutes. Remove the pod with 'kubectl -n kubehz-system delete pod -l app.kubernetes.io/component=live-view --force', then re-run 'lo kubehz deploy'.", drain)
	return ErrHandled
}

// deploySummary ports kubehz::deploy_summary.
func (c *Context) deploySummary(domain, owner, access string) {
	c.echo("")
	if owner == "operator" {
		c.echo("kubehz: live agent deployed and Ready for %s (deployment/kubehz-live-agent, %s tier).", domain, access)
		c.echo("  It owns the heartbeat. The CronJob keeps the identity Secret bootstrapped")
		c.echo("  and enrolled, and no longer beats — one producer, always.")
		c.echo("")
		c.echo("  You get: live view (nodes with capacity and instance type, pod phase counts,")
		c.echo("  warning events, machine-controller failures, addon inventory), pushed within")
		c.echo("  seconds of a change instead of every five minutes.")
		if access == "managed" {
			c.echo("")
			c.echo("  Acting RBAC is applied (worker scaling, self-healing, worker upgrades).")
			c.echo("  Nothing acts until the platform authorizes it: the agent polls the desired")
			c.echo("  state and obeys the server's execution flags, which the platform computes")
			c.echo("  from your tier, this cluster's access, and its own kill switches. Your tier")
			c.echo("  must allow it (Supporter+) and the platform must have it switched on.")
			c.echo("  Every write runs with your cluster's own credentials — kubehz holds none.")
		} else {
			c.echo("")
			c.echo("  Acting is NOT enabled: access is '%s', so no acting RBAC was granted", access)
			c.echo("  and the desired-state loop stays report-only. Set access: managed to opt in.")
		}
		c.echo("")
		c.echo("  Not reported while the live agent owns the beat: control-plane component")
		c.echo("  health, certificate expiry, and the 24 h handover assessment ('lo kubehz")
		c.echo("  assess' will go stale). The live agent does not collect them yet. Set")
		c.echo("  spec.kubehz.agent: cronjob and re-run to get them back.")
	} else {
		c.echo("kubehz: heartbeat CronJob deployed for %s (cronjob/kubehz-heartbeat).", domain)
		c.echo("  It owns the heartbeat and beats every five minutes. Any live agent from a")
		c.echo("  previous run was removed, so there is one producer.")
		c.echo("")
		c.echo("  You get: node status, control-plane component health, certificate expiry,")
		c.echo("  and the 24 h handover assessment. For the live view and for worker scaling,")
		c.echo("  self-healing or worker upgrades, set spec.kubehz.agent: operator and re-run.")
	}
	c.echo("")
	c.echo("  Claim this cluster (once): lo kubehz claim-code")
	c.echo("  Check it beat:             lo kubehz status")
}

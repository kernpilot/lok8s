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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
)

// The five waits, all in seconds, all overridable from the environment.
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

// How long the deploy waits for the pods of the CronJob agent's Jobs to end
// after it deleted the Jobs (waitAgentJobsGone). A pod of a deleted Job gets
// its grace period (30 s); a pod of a Job that the delete did not match ends
// within that Job's deadline (120 s). This allows a little more.
func (c *Context) agentJobsDrainSeconds() int {
	return c.envSeconds("KUBEHZ_AGENT_JOBS_DRAIN_SECONDS", 130)
}
func (c *Context) liveAgentRolloutSeconds() int {
	return c.envSeconds("KUBEHZ_LIVE_AGENT_ROLLOUT_SECONDS", 120)
}
func (c *Context) liveAgentDrainSeconds() int {
	return c.envSeconds("KUBEHZ_LIVE_AGENT_DRAIN_SECONDS", 120)
}

// How long a first deploy waits for the one-off identity bootstrap Job to
// complete. The Job's own deadline is 120 s; this allows a little more.
func (c *Context) identityBootstrapSeconds() int {
	return c.envSeconds("KUBEHZ_IDENTITY_BOOTSTRAP_SECONDS", 150)
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
		return c.deployPrint(ctx, workdir, domain, owner, access)
	}
	if err := c.deployApply(ctx, workdir, domain, owner, access); err != nil {
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
// apply uses). Comment lines name the steps around the CronJob agent: the
// Job delete and the wait, the bind Secret (its line names the file and
// never prints the value), and the patch that ends the suspend.
func (c *Context) deployPrint(ctx context.Context, workdir, domain, owner, access string) error {
	c.echo("# --- CronJob agent (kubehz-heartbeat) — identity + enrollment; heartbeat owner: %s ---", owner)
	if err := c.run(ctx, "kubectl", "kustomize", filepath.Join(workdir, "agent")); err != nil {
		return err
	}
	c.echo("---")
	c.echo("# The CronJob agent is applied suspended. The deploy then deletes its Jobs (-l %s) and waits until their pods have ended.", agentJobsSelector)
	if c.storedBindSecret(domain) != "" {
		c.echo("---")
		c.echo("# --- Secret kubehz-agent-bind (the bind secret from clusters/%s/.kubehz-bind) — applied after the CronJob agent; the value is not printed ---", domain)
	}
	c.echo("---")
	c.echo("# The deploy then sets spec.suspend to false on cronjob/kubehz-heartbeat.")
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
//
// No agent pod runs between the CronJob agent apply and the stage of the
// bind secret, in both directions, unless the CronJob controller acts on a
// copy of the CronJob from before the suspended apply and starts a Job
// after the pod wait. The manifest sets spec.suspend: true, so
// the CronJob agent apply leaves the CronJob suspended, and it makes no new
// Job. settleCronJobAgent then deletes the Jobs that it made before, waits
// until their pods have ended, stages the bind secret, and only then sets
// spec.suspend to false. A step that fails stops the deploy with the CronJob
// suspended. An INT or a TERM in these steps stops it too, with one line
// that says so (suspendInterrupted). The CronJob reads the Secret through an
// optional env ref, so its pods start with or without it.
//
// In the operator direction, the CronJob agent apply of step 1 stops the
// CronJob's beat, and only the live agent of step 3 beats again. So the deploy
// reads the bind secret once before step 1 and runs a preflight of its stage,
// a server-side dry run of the same Secret apply with the same bytes
// (proveBindSecret). A stage that the apiserver refuses then stops the deploy
// before it changes an agent.
func (c *Context) deployApply(ctx context.Context, workdir, domain, owner, access string) error {
	overlay := liveAgentOverlay(workdir, access)
	agentDir := filepath.Join(workdir, "agent")

	if owner == "operator" {
		// 0. The bind secret, read once, and the preflight of its stage.
		secret := c.takeBindSecret(domain)
		if secret != "" {
			if err := c.proveBindSecret(ctx, workdir, domain, secret); err != nil {
				return err
			}
		}
		// 1. The marker first (the CronJob stops beating), suspended.
		if err := c.applyPrivate(ctx, "the CronJob agent", nil, "apply", "-k", agentDir); err != nil {
			if ctx.Err() != nil {
				return c.suspendInterrupted(ctx)
			}
			c.errorf("kubehz: could not apply the CronJob agent (identity bootstrap). %s Fix the cause, then run 'lo kubehz deploy' again.", agentApplyMayHaveChanged)
			return ErrHandled
		}
		// 1b. The Jobs, the bind secret with the bytes of step 0, and the
		// end of the suspend, before the identity bootstrap Job of step 2b
		// registers.
		if err := c.settleCronJobAgent(ctx, workdir, domain, secret); err != nil {
			return err
		}
		// 2. Wait out an in-flight heartbeat pod (fail-soft).
		c.waitHeartbeatIdle(ctx)
		// 2b. The identity Secret. The live agent mounts the agent-token of
		// kubehz-agent, which the apply above creates empty and only the
		// CronJob's bootstrap fills, on its next five-minute tick; a first
		// deploy that waits for the rollout alone loses that race about half
		// the time. Run the bootstrap now and wait for its Job to complete.
		if err := c.ensureIdentitySecret(ctx); err != nil {
			return err
		}
		// 3. The live agent, which becomes the only producer.
		if err := c.applyPrivate(ctx, "the live agent", nil, "apply", "-k", overlay); err != nil {
			c.errorf("kubehz: could not apply the live agent. The apply can have applied part of the live agent objects. The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator), so %s. Run 'lo kubehz deploy' again, or set spec.kubehz.agent back to cronjob and run it again.", heartbeatsCanStop)
			return ErrHandled
		}
		// 4. Accepted is not running.
		rollout := c.liveAgentRolloutSeconds()
		if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "rollout", "status", "deployment/kubehz-live-agent",
			"--timeout="+strconv.Itoa(rollout)+"s"); err != nil {
			c.errorf("kubehz: the live agent was accepted but never became Ready within %ds. The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and the live agent is not Ready, so %s. Diagnose with 'kubectl -n kubehz-system describe deployment/kubehz-live-agent' (an image pull that fails or hangs, or an untolerated node taint, is the usual cause), or set spec.kubehz.agent: cronjob and re-run 'lo kubehz deploy' to hand the beat straight back.", rollout, heartbeatsCanStop)
			c.liveAgentPullHint(ctx)
			return ErrHandled
		}
		return nil
	}

	// cronjob mode: remove the live agent BEFORE re-arming the CronJob's beat.
	if err := c.run(ctx, "kubectl", "delete", "-k", filepath.Join(workdir, "live-agent", "managed"), "--ignore-not-found=true"); err != nil {
		c.errorf("kubehz: could not remove the live agent. The delete can have removed part of the live agent objects. The deploy did not apply the CronJob agent, so %s. Usual causes: the kubeconfig lacks delete permission in kubehz-system, or the apiserver is unreachable. Fix that and run 'lo kubehz deploy' again.", heartbeatsCanStop)
		return ErrHandled
	}
	// Sweep the producing object by its semantic label (survives a rename).
	if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "delete", "deployment",
		"-l", "app.kubernetes.io/part-of=kubehz,app.kubernetes.io/component=live-view", "--ignore-not-found=true"); err != nil {
		c.errorf("kubehz: could not sweep live-agent Deployments by label. The live agent delete before it passed, and the sweep can have removed some Deployments. The deploy did not apply the CronJob agent, so %s. Fix the delete and run 'lo kubehz deploy' again.", heartbeatsCanStop)
		return ErrHandled
	}
	if err := c.waitLiveAgentGone(ctx); err != nil {
		return err
	}
	if err := c.applyPrivate(ctx, "the CronJob agent", nil, "apply", "-k", agentDir); err != nil {
		if ctx.Err() != nil {
			return c.suspendInterrupted(ctx)
		}
		c.errorf("kubehz: could not apply the CronJob agent. The live agent is gone. %s Fix the cause, then run 'lo kubehz deploy' again.", agentApplyMayHaveChanged)
		return ErrHandled
	}
	// The Jobs, the bind secret, and the end of the suspend.
	return c.settleCronJobAgent(ctx, workdir, domain, c.takeBindSecret(domain))
}

// The error texts after a step that can have stopped the heartbeat.
const (
	heartbeatsCanStop        = "heartbeats can stop until the deploy runs again"
	agentApplyMayHaveChanged = "The apply can have applied part of the agent objects; an applied CronJob is suspended, so " + heartbeatsCanStop + "."
	agentStaysSuspended      = "The CronJob agent stays suspended, so " + heartbeatsCanStop + "."
	operatorNotLive          = "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and this deploy did not apply the live agent, so " + heartbeatsCanStop + "."
	// After agentStaysSuspended, when the pod wait stops the deploy.
	agentSuspendIsSafe = "That is the safe state: a suspended CronJob starts no Job."
	// The next step after the pod wait. A pod that the Job delete removes
	// gets its grace period (the default of 30 s: the pod spec sets none),
	// and a pod on a node that does not answer stays Terminating, in the
	// phase Running, until the node answers or a force delete removes it.
	agentPodsStuck = "A pod ends within about 30 s (its grace period) after the Job delete, unless its node does not answer: then the pod stays Terminating. Check the node of each pod: kubectl -n kubehz-system get pods -l " + agentJobsSelector + " -o wide. If the node is gone, delete the pod: kubectl -n kubehz-system delete pod <name> --force --grace-period=0. Then run 'lo kubehz deploy' again."
	// The one line of an INT or a TERM from the CronJob agent apply to the
	// end of the suspend (suspendInterrupted).
	agentInterrupted = "kubehz: the deploy was interrupted. The CronJob agent can stay suspended, so " + heartbeatsCanStop + ". Run 'lo kubehz deploy' again: it ends the suspend."
)

// suspendInterrupted ends a deploy that an INT or a TERM (the context
// ended) stopped between the CronJob agent apply and the end of the
// suspend. It prints agentInterrupted in place of the error of the step and
// returns the context's error, so lo prints nothing more and exits 128+n
// (130 for an INT, 143 for a TERM), as the bash twin does.
func (c *Context) suspendInterrupted(ctx context.Context) error {
	c.errorf("%s", agentInterrupted)
	return ctx.Err()
}

// The CronJob agent's Jobs and their pods carry these labels: the
// jobTemplate and the pod template of manifests/agent/cronjob.yaml set both,
// and a Job that `kubectl create job --from=cronjob/...` makes copies them.
// The deploy deletes the Jobs and waits for the pods by this selector.
const agentJobsSelector = "app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent"

// agentResumePatch ends the suspend that the CronJob agent apply set.
const agentResumePatch = `{"spec":{"suspend":false}}`

// settleCronJobAgent runs right after the CronJob agent apply, which left the
// CronJob suspended. It deletes the Jobs of the CronJob and waits until their
// pods have ended (quiesceAgentJobs), stages secret when there is one
// (stageBindSecretBytes), and then sets spec.suspend to false
// (resumeCronJobAgent). Each step that fails stops the deploy, and the
// CronJob stays suspended.
func (c *Context) settleCronJobAgent(ctx context.Context, workdir, domain, secret string) error {
	if err := c.quiesceAgentJobs(ctx); err != nil {
		return err
	}
	if secret != "" {
		if err := c.stageBindSecretBytes(ctx, workdir, domain, secret); err != nil {
			return err
		}
	}
	return c.resumeCronJobAgent(ctx)
}

// quiesceAgentJobs deletes the Jobs of the CronJob agent by their labels and
// waits until their pods have ended (waitAgentJobsGone).
func (c *Context) quiesceAgentJobs(ctx context.Context) error {
	if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "delete", "job", "-l", agentJobsSelector, "--ignore-not-found=true"); err != nil {
		if ctx.Err() != nil {
			return c.suspendInterrupted(ctx)
		}
		c.errorf("kubehz: could not delete the Jobs of the CronJob agent (-l %s). The delete can have removed some of them. %s The kubeconfig needs list and delete on Jobs in kubehz-system. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again.", agentJobsSelector, agentStaysSuspended)
		return ErrHandled
	}
	return c.waitAgentJobsGone(ctx)
}

// waitAgentJobsGone waits until no pod of the CronJob agent's Jobs is left
// that has not ended (phase Succeeded or Failed). FAIL-HARD, like
// waitLiveAgentGone: a probe that fails counts as a pod that has not ended,
// and a window that ends on a failed probe refuses with that reason. The
// poll selects the pods by their own labels, so it also waits for a pod of a
// Job that the delete did not match. The window (agentJobsDrainSeconds) is
// the bound: a pod that never ends, such as a pod on a lost node that stays
// Terminating, stops the deploy at its end with the CronJob suspended, and
// the error names the pods and how to remove them (agentPodsStuck).
func (c *Context) waitAgentJobsGone(ctx context.Context) error {
	drain := c.agentJobsDrainSeconds()
	probeError, running := "", ""
	for waited := 0; waited < drain; waited += 5 {
		out, err := c.captureBoth(ctx, "kubectl", "-n", "kubehz-system", "get", "pods", "-l", agentJobsSelector,
			"--field-selector=status.phase!=Succeeded,status.phase!=Failed", "-o", "name")
		if ctx.Err() != nil {
			return c.suspendInterrupted(ctx)
		}
		if err == nil {
			probeError, running = "", out
			if !podLines(out) {
				return nil
			}
		} else {
			if probeError == "" {
				c.warnf("kubehz: cannot list the pods of the CronJob agent's Jobs in kubehz-system (%s). The deploy counts that as a pod that has not ended, and tries again.", oneLine(out))
			}
			probeError = oneLine(out)
		}
		if waited == 0 {
			c.echo("kubehz: waiting until the pods of the CronJob agent's Jobs have ended, before the stage…")
		}
		if err := c.sleep(ctx, 5*time.Second); err != nil {
			if ctx.Err() != nil {
				return c.suspendInterrupted(ctx)
			}
			return err
		}
	}
	if probeError != "" {
		c.errorf("kubehz: could not list the pods of the CronJob agent's Jobs in kubehz-system for %ds: %s. %s %s The kubeconfig needs list on Pods in kubehz-system. %s", drain, probeError, agentStaysSuspended, agentSuspendIsSafe, agentPodsStuck)
		return ErrHandled
	}
	still := ""
	if names := agentPodNames(running); names != "" {
		still = " The pods that have not ended: " + names + "."
	}
	c.errorf("kubehz: pods of the CronJob agent's Jobs did not end within %ds after the deploy deleted the Jobs.%s %s %s %s", drain, still, agentStaysSuspended, agentSuspendIsSafe, agentPodsStuck)
	return ErrHandled
}

// One `pod/<name>` line of the pod poll whose name the stop text prints: a
// DNS name, ASCII only, as in the pull hint (liveAgentPodLineRe).
var agentPodNameRe = regexp.MustCompile(`^pod/([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?)$`)

// agentPodNames are the pod names in the `pod/<name>` lines of out, joined
// by ", ". A line with a name of another shape counts as a pod for the wait
// (podLines), but its name is not printed. The NUL bytes go first: the bash
// twin reads the poll with a command substitution, which drops them.
func agentPodNames(out string) string {
	var names []string
	for line := range strings.SplitSeq(strings.ReplaceAll(out, "\x00", ""), "\n") {
		if m := agentPodNameRe.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	return strings.Join(names, ", ")
}

// resumeCronJobAgent sets spec.suspend to false, after the stage. The deploy
// owns the field: the next CronJob agent apply sets it to true again.
func (c *Context) resumeCronJobAgent(ctx context.Context) error {
	if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "patch", "cronjob", "kubehz-heartbeat", "--type=merge", "-p", agentResumePatch); err != nil {
		if ctx.Err() != nil {
			return c.suspendInterrupted(ctx)
		}
		c.errorf("kubehz: could not set spec.suspend to false on cronjob/kubehz-heartbeat. The CronJob agent can stay suspended, so %s. The kubeconfig needs patch on CronJobs in kubehz-system. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again. The deploy sets it with: kubectl -n kubehz-system patch cronjob kubehz-heartbeat --type=merge -p '%s'.", heartbeatsCanStop, agentResumePatch)
		return ErrHandled
	}
	return nil
}

// The live agent's container name and pod selector in the vendored
// manifests (live-agent/base/deployment.yaml). The pull hint reads only this
// container; TestLiveAgentPullHintFollowsTheVendoredDeployment fails when a
// re-vendor changes either.
const (
	liveAgentContainer = "agent"
	liveAgentSelector  = "app.kubernetes.io/name=kubehz-live-agent"
)

// The pod read after a Ready timeout: one line per live-agent pod, four
// fields joined by '|': pod name, node name, the agent container's waiting
// reason, the agent container's image. A field the pod does not have yet
// (no node, no container status) is empty.
const liveAgentPodsJSONPath = `jsonpath={range .items[*]}{.metadata.name}|{.spec.nodeName}|{.status.containerStatuses[?(@.name=="` +
	liveAgentContainer + `")].state.waiting.reason}|{.spec.containers[?(@.name=="` + liveAgentContainer + `")].image}{"\n"}{end}`

// One line of that read. Every field comes from the cluster and is printed,
// so the shape is pinned: DNS names for the pod and the node, letters for
// the reason, an image reference without spaces, quotes or escapes. All
// classes are ASCII only (the bash twin matches with LC_ALL=C). A NUL byte
// fails the match too. Bash command substitution drops NUL bytes, so the
// bash twin changes each NUL to a byte that fails its match.
var liveAgentPodLineRe = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?)[|]([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?)[|]([A-Za-z]{1,64})[|]([A-Za-z0-9][A-Za-z0-9._/:@-]*)$`)

// The longest image reference that the pull hint prints, in bytes. This is
// a separate check and not a bound in the regexp: POSIX guarantees bounds
// only up to 255 (RE_DUP_MAX), and the bash twin's regex must compile on
// every libc.
const liveAgentImageMaxLen = 511

// liveAgentPullHint runs after a Ready timeout. It reads the live agent's
// pods and, when one of them failed to pull its image or is still pulling
// it, says so and names the IPv6 path to the registry as a cause to check:
// a pull that opens an IPv6 connection which then carries no data hangs on
// that connection and never tries IPv4. It prints nothing when no pod is in
// such a state, or when the pods cannot be read: the error line above
// already names the usual causes.
func (c *Context) liveAgentPullHint(ctx context.Context) {
	out, err := c.capture(ctx, true, "kubectl", "-n", "kubehz-system", "get", "pods",
		"-l", liveAgentSelector, "--request-timeout=10s", "-o", liveAgentPodsJSONPath)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(out, "\n") {
		m := liveAgentPodLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pod, node, reason, image := m[1], m[3], m[5], m[6]
		if len(image) > liveAgentImageMaxLen {
			continue
		}
		switch reason {
		case "ErrImagePull", "ImagePullBackOff":
			c.echoErr("  Pod %s on node %s cannot pull its image %s (%s). Read why: kubectl -n kubehz-system describe pod %s", pod, node, image, reason, pod)
		case "ContainerCreating":
			if !c.imagePullInProgress(ctx, pod) {
				continue
			}
			c.echoErr("  Pod %s on node %s started to pull its image %s, and the pull did not finish.", pod, node, image)
		default:
			continue
		}
		registry := imageRegistry(image)
		c.echoErr("  A pull that hangs or times out can mean that the IPv6 path from the node to %s is broken.", registry)
		c.echoErr("  On that path, the connection opens but carries no data, and the pull does not try IPv4.")
		c.echoErr("  To check, run these two commands on node %s. If only the IPv6 command times out, the IPv6 path is broken:", node)
		c.echoErr("    curl -6 -sS -o /dev/null --max-time 15 https://%s/v2/", registry)
		c.echoErr("    curl -4 -sS -o /dev/null --max-time 15 https://%s/v2/", registry)
		if registry == "ghcr.io" {
			c.echoErr("  ghcr.io sends the image layers from another host. Run the two commands against https://pkg-containers.githubusercontent.com/ too.")
		}
		c.echoErr("  To work around it, make the node reach %s over IPv4. Repair its IPv6 route, or run the node without IPv6.", registry)
		c.echoErr("  Then delete the pod, so that a new pod pulls the image again: kubectl -n kubehz-system delete pod %s", pod)
		return
	}
}

// imagePullInProgress reports whether the kubelet started to pull the agent
// container's image and has not finished: a Pulling event and no Pulled
// event, both for that container (the kubelet sets the event's
// involvedObject.fieldPath to spec.containers{<name>}). A pod in
// ContainerCreating can also wait for a volume, and then the events show no
// pull, or a finished one. An events read that fails reports false.
func (c *Context) imagePullInProgress(ctx context.Context, pod string) bool {
	out, err := c.capture(ctx, true, "kubectl", "-n", "kubehz-system", "get", "events",
		"--field-selector", "involvedObject.kind=Pod,involvedObject.name="+pod,
		"--request-timeout=10s", "-o", `jsonpath={range .items[*]}{.involvedObject.fieldPath}|{.reason}{"\n"}{end}`)
	if err != nil {
		return false
	}
	events := strings.Split(out, "\n")
	agent := "spec.containers{" + liveAgentContainer + "}|"
	return slices.Contains(events, agent+"Pulling") && !slices.Contains(events, agent+"Pulled")
}

// imageRegistry is the host that the container runtime pulls an image
// from: the first path component when it names a host (a dot, a port or
// localhost), else Docker Hub.
func imageRegistry(image string) string {
	host, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(host, ".:") && host != "localhost") || host == "docker.io" {
		return "registry-1.docker.io"
	}
	return host
}

// The bind secret: lo kubehz register stores it in
// clusters/<domain>/.kubehz-bind, and the deploy stages it into the Secret
// kubehz-system/kubehz-agent-bind. The CronJob's bootstrap reads it (the
// optional env BIND_SECRET) and sends it on agent-register. The api adopts
// the announced row only with this secret: an agent that has no binding yet
// and registers without it gets a separate pending row, and every later tick
// keeps that row. The deploy reads the file once (takeBindSecret) and stages
// those bytes (stageBindSecretBytes) while the CronJob is suspended
// (settleCronJobAgent).

// takeBindSecret reads clusters/<domain>/.kubehz-bind once for the stage and
// gives the bytes that storedBindSecret checked. No file, or a file that
// storedBindSecret refuses: it warns and gives "", and the deploy continues
// without the Secret. A register in another checkout leaves no file here.
func (c *Context) takeBindSecret(domain string) string {
	// Any stat failure reads as "no file", like the bash `[[ -e ]]`.
	if _, err := os.Stat(c.bindSecretPath(domain)); err != nil {
		c.warnf("kubehz: no bind secret at clusters/%s/.kubehz-bind. If 'lo kubehz register' ran in another checkout, %s. The announced row then stays as it is and gets no heartbeats. Claim the new row with the code from 'lo kubehz claim-code'.", domain, pendingRowIfUnbound)
		return ""
	}
	secret := c.storedBindSecret(domain)
	if secret == "" {
		c.warnf("kubehz: clusters/%s/.kubehz-bind holds no bind secret (a readable file of 64 lowercase hex characters, or khzb_ and 64 lowercase hex characters). The deploy does not stage it, and a Secret kubehz-agent-bind from an earlier deploy stays as it is. Then %s. The announced row then stays as it is and gets no heartbeats. Claim the new row with the code from 'lo kubehz claim-code'. If the file holds a newer form of bind secret, upgrade lok8s (the lo binary and, for a bash install, the synced tree), then run 'lo kubehz deploy' again.", domain, pendingRowIfUnbound)
		return ""
	}
	return secret
}

// The texts of the warnings and errors about the bind secret.
const (
	pendingRowIfUnbound = "the agent can register a separate pending row if it has no binding and no staged value yet"
	bindRBACNeeded      = "The kubeconfig needs patch on Secrets in kubehz-system, and create when the Secret is new. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again."
	deployWithoutSecret = "To deploy without the secret, remove the file and the Secret (kubectl -n kubehz-system delete secret kubehz-agent-bind --ignore-not-found), then claim the new row with the code from 'lo kubehz claim-code'."
)

// proveBindSecret is the preflight of the stage of secret, before the
// operator direction changes an agent. It applies the namespace for real (a
// first deploy has none yet, and a server-side dry run of a Secret in a
// missing namespace fails; the stage applies it too), then runs the Secret
// apply of the stage with --dry-run=server and the same bytes
// (bindSecretUpsert). A failure stops the deploy before the CronJob agent
// apply.
func (c *Context) proveBindSecret(ctx context.Context, workdir, domain, secret string) error {
	changed := "The namespace apply can have changed the namespace kubehz-system. The deploy applied no other object."
	err := c.applyPrivate(ctx, "the namespace kubehz-system", nil, "apply", "-f", filepath.Join(workdir, "agent", "namespace.yaml"))
	if err == nil {
		changed = "The deploy applied only the namespace kubehz-system."
		err = c.bindSecretUpsert(ctx, secret, "--dry-run=server")
	}
	if err != nil {
		c.errorf("kubehz: the preflight of the bind secret stage failed (a server-side dry run of Secret kubehz-system/kubehz-agent-bind with the value from clusters/%s/.kubehz-bind), so the deploy stopped before the CronJob agent apply. %s %s %s", domain, changed, bindRBACNeeded, deployWithoutSecret)
		return ErrHandled
	}
	return nil
}

// stageBindSecretBytes stages secret, the bytes that takeBindSecret read
// and checked. The file is not read again, so a change after the check does
// not reach the Secret.
func (c *Context) stageBindSecretBytes(ctx context.Context, workdir, domain, secret string) error {
	if err := c.applyBindSecret(ctx, workdir, secret); err != nil {
		if ctx.Err() != nil {
			return c.suspendInterrupted(ctx)
		}
		c.errorf("kubehz: could not stage the bind secret from clusters/%s/.kubehz-bind into Secret kubehz-system/kubehz-agent-bind. The stage can have applied the namespace or the Secret. %s %s If you deploy without the secret, %s. %s", domain, agentStaysSuspended, bindRBACNeeded, pendingRowIfUnbound, deployWithoutSecret)
		return ErrHandled
	}
	return nil
}

// applyBindSecret applies the namespace (a first deploy has none yet), then
// upserts the Secret with the value secret, and stops at the first failure.
// The kubectl output stays private (applyPrivate). The value goes in on the stdin of the
// dry-run create (--from-file=bind-secret=/dev/stdin), never on argv, so `ps`
// does not show it. The server-side apply changes an existing Secret in
// place (a re-announce rotates the value), so a running CronJob never finds
// the Secret missing. It writes no last-applied annotation, which `kubectl
// describe` would show. RBAC: patch on Secrets in kubehz-system, and create
// when the Secret is new.
func (c *Context) applyBindSecret(ctx context.Context, workdir, secret string) error {
	if err := c.applyPrivate(ctx, "the namespace kubehz-system", nil, "apply", "-f", filepath.Join(workdir, "agent", "namespace.yaml")); err != nil {
		return err
	}
	return c.bindSecretUpsert(ctx, secret)
}

// bindSecretUpsert is the Secret apply of the stage, for applyBindSecret and
// for the preflight (proveBindSecret), so the two cannot drift: the dry-run
// create renders the manifest from the bytes on its stdin, and the
// server-side apply sends it. extra goes before "-f -" (the preflight passes
// --dry-run=server). The render runs with the kubectl user defaults off too:
// a kuberc default such as --append-hash would rename the Secret that the
// apply sends.
func (c *Context) bindSecretUpsert(ctx context.Context, secret string, extra ...string) error {
	var manifest bytes.Buffer
	if err := c.Runner.Run(ctx, execx.Cmd{
		Name: "kubectl", Args: []string{"-n", "kubehz-system", "create", "secret", "generic", "kubehz-agent-bind",
			"--from-file=bind-secret=/dev/stdin", "--dry-run=client", "-o", "yaml"},
		Env:   []string{execx.KubeRCOff},
		Stdin: strings.NewReader(secret), Stdout: &manifest, Stderr: c.errOut(),
	}); err != nil {
		return err
	}
	args := append([]string{"apply", "--server-side", "--force-conflicts"}, extra...)
	return c.applyPrivate(ctx, "the bind secret", &manifest, append(args, "-f", "-")...)
}

// applyPrivate runs one kubectl apply of the deploy (args after kubectl)
// with the kubectl output private (kapply.RunPrivate): the output of an
// apply can quote a value of the manifest, and the bind secret holds a
// credential. label names the objects in the fixed lines. A nil stdin is
// the stdin of lo (bash: kapply::run_private).
func (c *Context) applyPrivate(ctx context.Context, label string, stdin io.Reader, args ...string) error {
	return kapply.RunPrivate(ctx, c.Runner, execx.Cmd{Name: "kubectl", Args: args, Stdin: stdin}, label, c.out(), c.errOut())
}

// podLines keeps only the `pod/<name>` lines of a merged capture — an
// apiserver `Warning:` on stderr is not a pod.
func podLines(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "pod/") {
			return true
		}
	}
	return false
}

func oneLine(s string) string { return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", " ") }

// identityProbeTemplate prints the word agent-token when the identity
// Secret holds a non-empty agent-token, else nothing. The read never gets
// the value: the deploy needs only to know that the key is there.
const identityProbeTemplate = `go-template={{with .data}}{{if index . "agent-token"}}agent-token{{end}}{{end}}`

// identityHasToken reports whether the merged output of the
// identityProbeTemplate read has the line agent-token (a warning line on
// stderr can come before it).
func identityHasToken(out string) bool {
	return slices.Contains(strings.Split(out, "\n"), "agent-token")
}

// ensureIdentitySecret runs the CronJob's bootstrap once when the identity
// Secret kubehz-agent holds no agent-token yet (a first deploy: the apply of
// the agent kustomization, just before this step, created it empty) and
// waits for that Job to COMPLETE, not only for the Secret: the bootstrap
// fills the Secret first and attempts the registration after it
// (best-effort; a refused POST still completes the Job), so a Job cut off at
// the Secret would skip that attempt until the CronJob's next tick. The
// Job's own deadline is 120 s; the wait allows a little more.
func (c *Context) ensureIdentitySecret(ctx context.Context) error {
	probe, err := c.captureBoth(ctx, "kubectl", "-n", "kubehz-system", "get", "secret", "kubehz-agent", "-o", identityProbeTemplate)
	if err == nil && identityHasToken(probe) {
		return nil
	}
	if err != nil {
		reason := strings.TrimSpace(probe)
		if reason == "" {
			reason = "kubectl exited without output (" + err.Error() + ")"
		}
		// The agent cannot create the Secret (its RBAC has no create), and
		// the apply just before this step creates it: an absent Secret
		// stops the deploy too.
		if strings.Contains(probe, "NotFound") {
			c.errorf("kubehz: Secret kubehz-agent does not exist in kubehz-system, but the deploy applied it just before: %s. The agent cannot create it: something deleted it, or an admission webhook refused it. %s Run 'lo kubehz deploy' again.", reason, operatorNotLive)
			return ErrHandled
		}
		// A refused or unreachable read says nothing about the Secret, and
		// a bootstrap on top of an existing identity is not what the
		// operator asked for.
		c.errorf("kubehz: could not read Secret kubehz-agent in kubehz-system: %s. %s Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again.", reason, operatorNotLive)
		return ErrHandled
	}
	// Unix seconds plus four random hex digits: two deploys in the same
	// second, from two machines, get two Jobs.
	job := "kubehz-heartbeat-bootstrap-" + strconv.FormatInt(c.now().Unix(), 10) + "-" + randomHex4()
	c.echo("kubehz: the identity Secret holds no agent-token yet — running the CronJob's bootstrap once (job/%s)…", job)
	if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "create", "job", job, "--from=cronjob/kubehz-heartbeat"); err != nil {
		c.errorf("kubehz: could not start the identity bootstrap (job/%s). The CronJob agent is applied with the live owner: it bootstraps the identity on its next tick but does not beat, and this deploy did not apply the live agent, so %s. Run 'lo kubehz deploy' again after that tick, or after a job by hand: kubectl -n kubehz-system create job kubehz-heartbeat-bootstrap-$(date +%%s) --from=cronjob/kubehz-heartbeat.", job, heartbeatsCanStop)
		return ErrHandled
	}
	wait := c.identityBootstrapSeconds()
	if err := c.run(ctx, "kubectl", "-n", "kubehz-system", "wait", "--for=condition=complete", "job/"+job, "--timeout="+strconv.Itoa(wait)+"s"); err != nil {
		c.errorf("kubehz: the identity bootstrap (job/%s) did not complete within %ds. A pod that cannot pull its image or reach the cluster's apiserver is the usual cause. A Job that failed stays until you delete it, or until the next 'lo kubehz deploy' deletes it. Read its state: kubectl -n kubehz-system describe job %s. Read its log: kubectl -n kubehz-system logs job/%s (after the deadline the pod is gone and this call fails). Delete it after you read them: kubectl -n kubehz-system delete job %s. %s Run 'lo kubehz deploy' again.", job, wait, job, job, job, operatorNotLive)
		return ErrHandled
	}
	if out, err := c.captureBoth(ctx, "kubectl", "-n", "kubehz-system", "get", "secret", "kubehz-agent", "-o", identityProbeTemplate); err != nil || !identityHasToken(out) {
		c.errorf("kubehz: the identity bootstrap (job/%s) completed but left no agent-token in Secret kubehz-agent. Read its log: kubectl -n kubehz-system logs job/%s. The live agent was NOT applied.", job, job)
		return ErrHandled
	}
	// The delete is a write: it runs with the kubectl user defaults off.
	_ = c.Runner.Run(ctx, execx.Cmd{
		Name: "kubectl", Args: []string{"-n", "kubehz-system", "delete", "job", job, "--ignore-not-found=true"},
		Env:    []string{execx.KubeRCOff},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	return nil
}

// randomHex4 is four hex digits from crypto/rand; a name suffix, not a key.
func randomHex4() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}

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
		if c.sleep(ctx, 5*time.Second) != nil {
			return
		}
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
		if err := c.sleep(ctx, 5*time.Second); err != nil {
			return err
		}
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

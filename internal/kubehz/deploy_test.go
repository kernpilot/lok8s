package kubehz

// deploy_test.go ports the render / dry-run / apply-order / wait cases of
// tests/unit/kubehz_live_agent_test.bats. kubectl is the fake Runner; the
// rendered manifests are compared against goldens generated ONCE from the
// bash kubehz::render_agent (testdata/golden/rendered-*.yaml).

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
)

func renderInto(t *testing.T, h *harness, owner, access string) string {
	t.Helper()
	work := filepath.Join(h.base, "render-"+owner+"-"+access)
	_ = os.MkdirAll(work, 0o755)
	mustOK(t, h.ctx.RenderAgent(work, "acme.example.com", "https://api.kubehz.cloud", owner, access), h.output())
	return work
}

func TestRenderAgentSubstitutesBothTrees(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	var leftovers []string
	_ = filepath.WalkDir(work, func(path string, d os.DirEntry, err error) error {
		if !d.IsDir() && strings.Contains(readFile(t, path), "PLACEHOLDER") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if len(leftovers) > 0 {
		t.Fatalf("placeholders survived: %v", leftovers)
	}
	agentCM := readFile(t, filepath.Join(work, "agent", "configmap.yaml"))
	liveCM := readFile(t, filepath.Join(work, "live-agent", "base", "configmap.yaml"))
	mustContain(t, agentCM, `CLUSTER_ID: "acme.example.com"`)
	mustContain(t, liveCM, `CLUSTER_ID: "acme.example.com"`)
	mustContain(t, agentCM, `KUBEHZ_API_URL: "https://api.kubehz.cloud"`)
	mustContain(t, liveCM, `KUBEHZ_API_URL: "https://api.kubehz.cloud"`)
	mustContain(t, agentCM, `KUBEHZ_HEARTBEAT_OWNER: "operator"`)
}

func TestRenderAgentMatchesBashGoldens(t *testing.T) {
	// The golden was rendered by the bash with an apiUrl carrying `&` — the
	// value sed would have expanded — so this pins both the byte-for-byte
	// render and the verbatim ampersand.
	h := newHarness(t)
	work := filepath.Join(h.base, "golden")
	_ = os.MkdirAll(work, 0o755)
	url := "https://api.example.com/heartbeat?cluster=a&mode=push"
	mustOK(t, h.ctx.RenderAgent(work, "acme.example.com", url, "operator", "managed"), h.output())
	for got, golden := range map[string]string{
		filepath.Join(work, "agent", "configmap.yaml"):              "rendered-agent-configmap.yaml",
		filepath.Join(work, "live-agent", "base", "configmap.yaml"): "rendered-live-agent-configmap.yaml",
	} {
		if readFile(t, got) != readFile(t, filepath.Join("testdata", "golden", golden)) {
			t.Fatalf("%s drifted from the bash render", golden)
		}
	}
	mustContain(t, readFile(t, filepath.Join(work, "agent", "configmap.yaml")), url)
}

func TestLiveAgentOverlay(t *testing.T) {
	work := "/w"
	if liveAgentOverlay(work, "registered") != "/w/live-agent/base" || liveAgentOverlay(work, "managed") != "/w/live-agent/managed" {
		t.Fatal("overlay")
	}
	// Anything that is not 'managed' is read-only.
	if liveAgentOverlay(work, "") != "/w/live-agent/base" || liveAgentOverlay(work, "Managed") != "/w/live-agent/base" {
		t.Fatal("unknown tier fell through to the acting overlay")
	}
}

func TestRenderAgentRefusesSurvivingPlaceholder(t *testing.T) {
	h := newHarness(t)
	work := filepath.Join(h.base, "r3")
	_ = os.MkdirAll(work, 0o755)
	// A value that re-introduces the token is the same observable state as a
	// renamed placeholder in a manifest.
	mustErr(t, h.ctx.RenderAgent(work, "CLUSTER_ID_PLACEHOLDER", "https://api.kubehz.cloud", "operator", "managed"))
	mustContain(t, h.output(), "still carry a placeholder")
	mustContain(t, h.output(), "  agent/configmap.yaml")
}

func kubectlLogger(h *harness, override func(c execx.Cmd) (bool, error)) *[]string {
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if override != nil {
			if handled, err := override(c); handled {
				return err
			}
		}
		if strings.Contains(argvLine(c), "get pods") {
			return nil // idle cluster
		}
		log = append(log, argvLine(c))
		return nil
	}
	return &log
}

func TestDeployPrint(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if c.Name != "kubectl" || c.Args[0] != "kustomize" {
			t.Fatalf("dry run must render with kubectl kustomize, got %s", argvLine(c))
		}
		io.WriteString(c.Stdout, "kind: Rendered\ndir: "+c.Args[1]+"\n")
		return nil
	}
	mustOK(t, h.ctx.deployPrint(context.Background(), work, "operator", "managed"), h.output())
	mustContain(t, h.output(), "# --- CronJob agent (kubehz-heartbeat) — identity + enrollment; heartbeat owner: operator ---")
	mustContain(t, h.output(), "# --- Live agent (kubehz-live-agent) — managed tier RBAC ---")
	mustContain(t, h.output(), "dir: "+filepath.Join(work, "live-agent", "managed"))

	h.reset()
	mustOK(t, h.ctx.deployPrint(context.Background(), work, "cronjob", "registered"), h.output())
	mustContain(t, h.output(), "# The live agent is NOT deployed in cronjob mode; a previous install would be removed.")
	mustNotContain(t, h.output(), "live-agent")
}

func TestDeployApplyToOperatorOrder(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, nil)
	mustOK(t, h.ctx.deployApply(context.Background(), work, "operator", "managed"), h.output())
	if len(*log) != 3 {
		t.Fatalf("calls: %v", *log)
	}
	mustContain(t, (*log)[0], "apply -k "+filepath.Join(work, "agent"))
	mustContain(t, (*log)[1], "apply -k "+filepath.Join(work, "live-agent", "managed"))
	mustContain(t, (*log)[2], "rollout status deployment/kubehz-live-agent")
	mustContain(t, (*log)[2], "--timeout=120s")
}

func TestDeployApplyNeverReadyFails(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "rollout status") {
			return true, exitErr(1)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(context.Background(), work, "operator", "managed"))
	mustContain(t, h.output(), "never became Ready")
	mustContain(t, h.output(), "NOTHING owns the heartbeat")
}

func TestDeployApplyRolloutTimeoutFromEnv(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_LIVE_AGENT_ROLLOUT_SECONDS"] = "600"
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, nil)
	mustOK(t, h.ctx.deployApply(context.Background(), work, "operator", "managed"), h.output())
	mustContain(t, strings.Join(*log, "\n"), "--timeout=600s")
}

func TestDeployApplyToCronjobOrder(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "cronjob", "registered")
	log := kubectlLogger(h, nil)
	mustOK(t, h.ctx.deployApply(context.Background(), work, "cronjob", "registered"), h.output())
	if len(*log) != 3 {
		t.Fatalf("calls: %v", *log)
	}
	mustContain(t, (*log)[0], "delete -k "+filepath.Join(work, "live-agent", "managed")+" --ignore-not-found=true")
	mustContain(t, (*log)[1], "delete deployment -l app.kubernetes.io/part-of=kubehz,app.kubernetes.io/component=live-view")
	mustContain(t, (*log)[2], "apply -k "+filepath.Join(work, "agent"))
	mustNotContain(t, strings.Join(*log, "\n"), "apply -k "+filepath.Join(work, "live-agent"))
}

func TestDeployApplyFailedDeleteNeverRearms(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "cronjob", "registered")
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "delete -k") {
			return true, exitErr(1)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(context.Background(), work, "cronjob", "registered"))
	mustContain(t, h.output(), "could not remove the live agent")
	mustNotContain(t, strings.Join(*log, "\n"), "apply -k "+filepath.Join(work, "agent"))
}

func TestDeployApplyPodWontTerminateBlocks(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_LIVE_AGENT_DRAIN_SECONDS"] = "10"
	work := renderInto(t, h, "cronjob", "registered")
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "get pods") {
			io.WriteString(c.Stdout, "pod/kubehz-live-agent-abc123\n")
			return nil
		}
		log = append(log, argvLine(c))
		return nil
	}
	mustErr(t, h.ctx.deployApply(context.Background(), work, "cronjob", "registered"))
	mustContain(t, h.output(), "still running")
	mustNotContain(t, strings.Join(log, "\n"), "apply -k "+filepath.Join(work, "agent"))
}

func TestDeployApplyBlindProbeNeverRearms(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_LIVE_AGENT_DRAIN_SECONDS"] = "10"
	work := renderInto(t, h, "cronjob", "registered")
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "get pods") {
			io.WriteString(c.Stderr, `Error from server (Forbidden): pods is forbidden: User "deployer" cannot list resource "pods" in API group "" in the namespace "kubehz-system"`+"\n")
			return exitErr(1)
		}
		log = append(log, argvLine(c))
		return nil
	}
	mustErr(t, h.ctx.deployApply(context.Background(), work, "cronjob", "registered"))
	mustContain(t, h.output(), "could not tell whether")
	mustContain(t, h.output(), "Forbidden")
	mustNotContain(t, strings.Join(log, "\n"), "apply -k "+filepath.Join(work, "agent"))
}

func TestDeployApplyUnreadableHeartbeatProbeWarnsAndContinues(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "get pods") {
			io.WriteString(c.Stderr, "Error from server (Forbidden): pods is forbidden\n")
			return exitErr(1)
		}
		log = append(log, argvLine(c))
		return nil
	}
	mustOK(t, h.ctx.deployApply(context.Background(), work, "operator", "managed"), h.output())
	mustContain(t, h.output(), "could not check for an in-flight heartbeat pod")
	mustContain(t, log[1], "apply -k "+filepath.Join(work, "live-agent", "managed"))
}

func TestWaitsIgnoreApiserverWarnings(t *testing.T) {
	for _, warning := range []string{
		"Warning: v1 ComponentStatus is deprecated in v1.19+",
		`Warning: metadata.finalizers: "foregroundDeletion": prefer a domain-qualified finalizer name`,
	} {
		h := newHarness(t)
		slept := 0
		h.ctx.Sleep = func(time.Duration) { slept++ }
		h.runner.handler = func(c execx.Cmd, _ string) error {
			io.WriteString(c.Stderr, warning+"\n")
			return nil
		}
		h.ctx.waitHeartbeatIdle(context.Background())
		if h.output() != "" || slept != 0 {
			t.Fatalf("heartbeat wait tripped on a warning: %q slept=%d", h.output(), slept)
		}
		mustOK(t, h.ctx.waitLiveAgentGone(context.Background()), h.output())
		if h.output() != "" || slept != 0 {
			t.Fatalf("live-agent wait tripped on a warning: %q slept=%d", h.output(), slept)
		}
	}
}

func TestDeployAgentRefusals(t *testing.T) {
	h := newHarness(t)
	mustErr(t, h.ctx.DeployAgent(context.Background(), &Config{Hosting: "shared", Access: "none", APIURL: "https://api.kubehz.cloud", Agent: "cronjob"}, "acme.example.com", false))
	mustContain(t, h.output(), "no in-cluster agent to deploy")
	h.reset()
	mustErr(t, h.ctx.DeployAgent(context.Background(), &Config{Hosting: "self", Access: "none", APIURL: "https://api.kubehz.cloud", Agent: "cronjob"}, "acme.example.com", false))
	mustContain(t, h.output(), "access is 'none'")
	h.reset()
	mustErr(t, h.ctx.DeployAgent(context.Background(), &Config{Hosting: "self", Access: "registered", APIURL: "http://api.kubehz.cloud", Agent: "cronjob"}, "acme.example.com", false))
	mustContain(t, h.output(), "must use HTTPS")
	for _, bad := range []string{"Operator", "cronjob\"extra", "operator\nbeats"} {
		h.reset()
		mustErr(t, h.ctx.DeployAgent(context.Background(), &Config{Hosting: "self", Access: "registered", APIURL: "https://api.kubehz.cloud", Agent: bad}, "acme.example.com", false))
		mustContain(t, h.output(), "must be 'cronjob' or 'operator'")
	}
	if len(h.runner.calls) != 0 {
		t.Fatal("nothing may reach kubectl")
	}
}

func TestDeploySummary(t *testing.T) {
	h := newHarness(t)
	h.ctx.deploySummary("acme.example.com", "operator", "managed")
	mustContain(t, h.output(), "live agent deployed and Ready for acme.example.com (deployment/kubehz-live-agent, managed tier).")
	mustContain(t, h.output(), "Acting RBAC is applied")
	h.reset()
	h.ctx.deploySummary("acme.example.com", "operator", "registered")
	mustContain(t, h.output(), "Acting is NOT enabled: access is 'registered'")
	h.reset()
	h.ctx.deploySummary("acme.example.com", "cronjob", "registered")
	mustContain(t, h.output(), "heartbeat CronJob deployed for acme.example.com (cronjob/kubehz-heartbeat).")
	mustContain(t, h.output(), "Claim this cluster (once): lo kubehz claim-code")
}

func TestDeploySubcommandDryRun(t *testing.T) {
	h := newHarness(t)
	h.writeSpec("acme.example.com", specYAML("KubeOne", "    access: registered\n    apiUrl: https://api.kubehz.cloud\n"))
	h.runner.handler = func(c execx.Cmd, _ string) error {
		io.WriteString(c.Stdout, "kind: CronJob\n")
		return nil
	}
	mustOK(t, h.ctx.Deploy(context.Background(), "acme.example.com", true), h.output())
	mustContain(t, h.output(), "kind: CronJob")
	for _, l := range h.runner.lines() {
		if !strings.HasPrefix(l, "kubectl kustomize ") {
			t.Fatalf("dry run must apply nothing: %s", l)
		}
	}
}

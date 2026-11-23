package kubehz

// kuberc_test.go: every kubectl apply of the agent deploy (the namespace,
// the bind Secret, its server-side dry run, the CronJob agent and the live
// agent) runs with the kubectl user defaults off (execx.KubeRCOff). The
// applies run through applyPrivate: kapply.RunPrivate sets the override
// on each kubectl it runs. The other kubectl children of Context.run get
// it from execx.KubectlEnv. The bash twin is libs/kubehz/deploy (the
// KUBERC=off prefix). The claim patch of `lo kubehz claim` is a write too:
// it sets the override on its own Cmd (the bash twin: libs/kubehz/main). So
// do the delete of the one-off identity bootstrap Job and the untaint of a
// single-node handover receive (the bash twins: libs/kubehz/deploy and
// libs/kubehz/handover).

import (
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestDeployAppliesRunWithKubeRCOff(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	writeBindSecret(t, h, "acme.example.com", testBindSecret)
	bindStageLogger(h, "")
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	applies := 0
	for _, c := range h.runner.calls {
		if c.Name != "kubectl" || !slices.Contains(c.Args, "apply") {
			continue
		}
		applies++
		if !execx.KubeRCIsOff(c.Env) {
			t.Errorf("%s runs without %s", argvLine(c), execx.KubeRCOff)
		}
	}
	// The preflight (the namespace, the server-side dry run of the bind
	// Secret), the CronJob agent, the stage (the namespace, the bind
	// Secret), the live agent.
	if applies != 6 {
		t.Errorf("applies = %d, want 6:\n%v", applies, h.runner.lines())
	}
}

// The render of the bind Secret (create secret --dry-run=client) feeds the
// apply, so it runs with the kubectl user defaults off too: a kuberc default
// such as --append-hash would rename the Secret that the apply sends. The
// preflight and the stage render it once each.
func TestBindSecretRenderRunsWithKubeRCOff(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	writeBindSecret(t, h, "acme.example.com", testBindSecret)
	bindStageLogger(h, "")
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	renders := 0
	for _, c := range h.runner.calls {
		if c.Name != "kubectl" || !slices.Contains(c.Args, "--dry-run=client") {
			continue
		}
		renders++
		if !execx.KubeRCIsOff(c.Env) {
			t.Errorf("the render %s runs with Env %q, want %s as the last KUBERC entry", argvLine(c), c.Env, execx.KubeRCOff)
		}
	}
	if renders != 2 {
		t.Errorf("renders = %d, want 2:\n%v", renders, h.runner.lines())
	}
}

// The claim patch runs with KUBERC=off; the read of the ConfigMap before it
// keeps the kuberc of the user.
func TestClaimPatchRunsWithKubeRCOff(t *testing.T) {
	h := newHarness(t)
	kubectlStub(h, false, false)
	mustOK(t, h.ctx.Claim(t.Context(), "khzn_batsPlacedNonce_43charsBase64urlValue000"), h.output())
	patches := 0
	for _, c := range h.runner.calls {
		if c.Name != "kubectl" || !slices.Contains(c.Args, "patch") {
			if execx.KubeRCIsOff(c.Env) {
				t.Errorf("the read %s runs with %s", argvLine(c), execx.KubeRCOff)
			}
			continue
		}
		patches++
		if !execx.KubeRCIsOff(c.Env) {
			t.Errorf("%s runs without %s", argvLine(c), execx.KubeRCOff)
		}
	}
	if patches != 1 {
		t.Errorf("patches = %d, want 1:\n%v", patches, h.runner.lines())
	}
}

// The deploy deletes the one-off identity bootstrap Job after it completed,
// with the kubectl user defaults off: a kuberc default such as
// --dry-run=client would keep the Job.
func TestIdentityBootstrapJobDeleteRunsWithKubeRCOff(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	// Empty before the bootstrap, filled once the one-off Job completed.
	completed := false
	kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "wait --for=condition=complete") {
			completed = true
			return false, nil
		}
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") && !completed {
			return true, nil
		}
		return false, nil
	})
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	deletes := 0
	for _, c := range h.runner.calls {
		if c.Name != "kubectl" || !slices.Contains(c.Args, "delete") {
			continue
		}
		if !slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "kubehz-heartbeat-bootstrap-") }) {
			continue
		}
		deletes++
		if !execx.KubeRCIsOff(c.Env) {
			t.Errorf("%s runs without %s", argvLine(c), execx.KubeRCOff)
		}
	}
	if deletes != 1 {
		t.Errorf("deletes of the bootstrap Job = %d, want 1:\n%v", deletes, h.runner.lines())
	}
}

// A single-node handover receive removes the control-plane taint, with the
// kubectl user defaults off.
func TestHandoverUntaintRunsWithKubeRCOff(t *testing.T) {
	ho := newHandover(t)
	taints := 0
	ho.mockNodeBinaries(map[string]func(c execx.Cmd) error{
		"kubectl": func(c execx.Cmd) error {
			if slices.Contains(c.Args, "taint") {
				taints++
				if !execx.KubeRCIsOff(c.Env) {
					t.Errorf("%s runs without %s", argvLine(c), execx.KubeRCOff)
				}
			}
			return nil
		},
	})
	mustOK(t, ho.receive(ReceiveOpts{Snapshot: ho.snapshot, SingleNode: true}), ho.output())
	if taints != 1 {
		t.Errorf("taints = %d, want 1:\n%s", taints, strings.Join(ho.calls, "\n"))
	}
}

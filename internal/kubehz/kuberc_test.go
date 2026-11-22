package kubehz

// kuberc_test.go — every kubectl apply of the agent deploy (the namespace,
// the bind Secret, its server-side dry run, the CronJob agent and the live
// agent) runs with the kubectl user defaults off (execx.KubeRCOff). The
// applies run through Context.run, which sets it on each kubectl child
// (execx.KubectlEnv), or set it on their own Cmd (bindSecretUpsert). The
// bash twin is libs/kubehz/deploy (the KUBERC=off prefix).

import (
	"slices"
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
		if !slices.Contains(c.Env, execx.KubeRCOff) {
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

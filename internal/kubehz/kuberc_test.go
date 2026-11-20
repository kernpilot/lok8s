package kubehz

// kuberc_test.go — every kubectl apply of the agent deploy (the namespace,
// the bind Secret, the CronJob agent and the live agent) runs with the
// kubectl user defaults off (execx.KubeRCOff). The bash twin is
// libs/kubehz/deploy (the KUBERC=off prefix).

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
		if !execx.KubectlApplyOrDiff(c.Name, c.Args) {
			if slices.Contains(c.Env, execx.KubeRCOff) {
				t.Errorf("%s: %s set on a call that is not an apply", argvLine(c), execx.KubeRCOff)
			}
			continue
		}
		applies++
		if !slices.Contains(c.Env, execx.KubeRCOff) {
			t.Errorf("%s runs without %s", argvLine(c), execx.KubeRCOff)
		}
	}
	// the namespace, the bind Secret, the CronJob agent, the live agent
	if applies != 4 {
		t.Errorf("applies = %d, want 4:\n%v", applies, h.runner.lines())
	}
}

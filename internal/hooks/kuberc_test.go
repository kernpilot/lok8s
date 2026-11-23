package hooks

// kuberc_test.go: the kubectl writes of the hooks run with the kubectl user
// defaults off (execx.KubeRCOff): the delete of a recreate and the rollout
// restart. A kuberc default such as --dry-run=client would make each one
// report success and change nothing. The apply of a recreate runs through
// kapply, which sets the override itself. The reads (the live image refs)
// keep the kuberc of the user. The bash twin is libs/hooks (the KUBERC=off
// prefix).

import (
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestRecreateDeleteRunsWithKubeRCOff(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeArtifact(t, c, strings.Replace(renderedJob, "name: mig\n", "name: mig\n  labels: {run: mig}\n", 1))
	deletes := 0
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if cmd.Name == "tilt" && cmd.Args[0] == "get" && cmd.Args[1] == "session" {
			return &fakeExit{1} // no session: the kubectl path
		}
		if cmd.Name != "kubectl" {
			return nil
		}
		switch cmd.Args[0] {
		case "get":
			if slices.Contains(cmd.Env, execx.KubeRCOff) {
				t.Errorf("the read %s runs with %s", strings.Join(cmd.Args, " "), execx.KubeRCOff)
			}
		case "delete":
			deletes++
			if !slices.Contains(cmd.Env, execx.KubeRCOff) {
				t.Errorf("%s runs without %s", strings.Join(cmd.Args, " "), execx.KubeRCOff)
			}
		}
		return nil
	}
	if err := c.Recreate(t.Context(), "run=mig"); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Errorf("deletes = %d, want 1: %v", deletes, runner.calls)
	}
}

func TestRestartRolloutRunsWithKubeRCOff(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeArtifact(t, c, `kind: Deployment
metadata:
  name: web
  namespace: apps
  labels: {app: web}
`)
	restarts := 0
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if cmd.Name == "kubectl" && slices.Contains(cmd.Args, "rollout") {
			restarts++
			if !slices.Contains(cmd.Env, execx.KubeRCOff) {
				t.Errorf("%s runs without %s", strings.Join(cmd.Args, " "), execx.KubeRCOff)
			}
		}
		return nil
	}
	if err := c.Restart(t.Context(), "app=web"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Errorf("rollout restarts = %d, want 1: %v", restarts, runner.calls)
	}
}

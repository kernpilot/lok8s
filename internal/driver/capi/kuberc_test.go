package capi

// kuberc_test.go — every kubectl apply of the capi driver runs with the
// kubectl user defaults off (execx.KubeRCOff): a kuberc of the operator
// cannot change the apply. The bash twin is drivers/capi/{main,generate}
// (the KUBERC=off prefix).

import (
	"slices"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

// applyCallsWithoutKubeRCOff returns the argv of each kubectl apply or
// diff in calls that runs without KUBERC=off, and the number of such
// calls.
func applyCallsWithoutKubeRCOff(calls []execx.Cmd) ([]string, int) {
	var bad []string
	n := 0
	for _, c := range calls {
		if !execx.KubectlApplyOrDiff(c.Name, c.Args) {
			continue
		}
		n++
		if !slices.Contains(c.Env, execx.KubeRCOff) {
			bad = append(bad, argvLine(c))
		}
	}
	return bad, n
}

func TestKubectlApplyRunsWithKubeRCOff(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "test-token")
	t.Setenv("HROBOT_USER", "")
	t.Setenv("HROBOT_PASSWORD", "")
	for name, run := range map[string]func(t *testing.T) []execx.Cmd{
		// capi.go: the namespace and the resources on the management
		// cluster, and the credentials Secret (generate.go).
		"provision": func(t *testing.T) []execx.Cmd {
			d, runner, _ := provisionSetup(t)
			if err := d.Provision(t.Context(), "test.dev"); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			return runner.calls
		},
		// capi.go: the resources on the bootstrap cluster and the deploy
		// dir on the management cluster.
		"bootstrap": func(t *testing.T) []execx.Cmd {
			d, runner, _ := testDriver(t)
			bootstrapSpec(t, d)
			runner.handler = happyHandler
			if err := d.Provision(t.Context(), "mgmt.lok8s.dev"); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			return runner.calls
		},
		"credentials": func(t *testing.T) []execx.Cmd {
			d, runner, _ := testDriver(t)
			if err := d.EnsureCredentialsSecret(t.Context(), hetznerFixture(t), "hetzner", "/tmp/kubeconfig.yaml"); err != nil {
				t.Fatal(err)
			}
			return runner.calls
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad, n := applyCallsWithoutKubeRCOff(run(t))
			if n == 0 {
				t.Fatal("no kubectl apply ran")
			}
			for _, argv := range bad {
				t.Errorf("%s runs without %s", argv, execx.KubeRCOff)
			}
		})
	}
}

package capi

// kuberc_test.go: every kubectl apply of the capi driver, and the delete
// of the workload Cluster, runs with the kubectl user defaults off
// (execx.KubeRCOff): a kuberc of the operator cannot change the write. The
// bash twin is drivers/capi/{main,generate} (the KUBERC=off prefix).

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
		if c.Name != "kubectl" || (!slices.Contains(c.Args, "apply") && !slices.Contains(c.Args, "diff")) {
			continue
		}
		n++
		if !execx.KubeRCIsOff(c.Env) {
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

// Each render (create --dry-run=client) feeds an apply, so it runs with the
// kubectl user defaults off too: a kuberc default such as --append-hash
// would rename the object that lo applies. The provision renders the
// namespace (capi.go) and the credentials Secret (generate.go).
func TestKubectlRenderRunsWithKubeRCOff(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "test-token")
	t.Setenv("HROBOT_USER", "")
	t.Setenv("HROBOT_PASSWORD", "")
	for name, tc := range map[string]struct {
		run  func(t *testing.T) []execx.Cmd
		want []string // the object kind of each render, in order
	}{
		"provision": {func(t *testing.T) []execx.Cmd {
			d, runner, _ := provisionSetup(t)
			if err := d.Provision(t.Context(), "test.dev"); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			return runner.calls
		}, []string{"namespace", "secret"}},
		"credentials": {func(t *testing.T) []execx.Cmd {
			d, runner, _ := testDriver(t)
			if err := d.EnsureCredentialsSecret(t.Context(), hetznerFixture(t), "hetzner", "/tmp/kubeconfig.yaml"); err != nil {
				t.Fatal(err)
			}
			return runner.calls
		}, []string{"secret"}},
	} {
		t.Run(name, func(t *testing.T) {
			var kinds []string
			for _, c := range tc.run(t) {
				if c.Name != "kubectl" || !slices.Contains(c.Args, "--dry-run=client") {
					continue
				}
				if i := slices.Index(c.Args, "create"); i >= 0 && i+1 < len(c.Args) {
					kinds = append(kinds, c.Args[i+1])
				}
				if !execx.KubeRCIsOff(c.Env) {
					t.Errorf("the render %s runs with Env %q, want %s as the last KUBERC entry", argvLine(c), c.Env, execx.KubeRCOff)
				}
			}
			if !slices.Equal(kinds, tc.want) {
				t.Errorf("renders = %q, want %q", kinds, tc.want)
			}
		})
	}
}

// The destroy deletes the workload Cluster with the kubectl user defaults
// off: a kuberc default such as --dry-run=client would report the delete
// and keep the Hetzner servers.
func TestDestroyDeleteRunsWithKubeRCOff(t *testing.T) {
	d, runner, _ := destroySetup(t, "false")
	if err := d.Destroy(t.Context(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	for _, c := range runner.calls {
		if c.Name != "kubectl" || !slices.Contains(c.Args, "delete") {
			continue
		}
		deletes++
		if !execx.KubeRCIsOff(c.Env) {
			t.Errorf("%s runs without %s", argvLine(c), execx.KubeRCOff)
		}
	}
	if deletes != 1 {
		t.Errorf("kubectl deletes = %d, want 1:\n%s", deletes, callDump(runner))
	}
}

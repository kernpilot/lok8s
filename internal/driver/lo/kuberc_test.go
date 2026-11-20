package lo

// kuberc_test.go — the kubectl applies of the Lo driver (the registry
// manifests, the CoreDNS files and the coredns-custom ConfigMap) run through
// runInput and runOut, and run with the kubectl user defaults off
// (execx.KubeRCOff). Any other command keeps the environment of lo. The
// bash twin is drivers/lo/utils/{registries,services}.sh (the KUBERC=off
// prefix).

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestKubectlApplyRunsWithKubeRCOff(t *testing.T) {
	d, runner, _, _ := testDriver(t)
	envs := map[string][]string{}
	runner.handler = func(c execx.Cmd) error {
		envs[c.Name+" "+strings.Join(c.Args, " ")] = c.Env
		return nil
	}
	ctx := t.Context()
	_ = d.runInput(ctx, "kind: ConfigMap\n", io.Discard, io.Discard, "kubectl", "apply", "--kubeconfig", "/kc", "-f", "-")
	_ = d.runOut(ctx, io.Discard, io.Discard, "kubectl", "apply", "--kubeconfig", "/kc", "-f", "/corefile.yaml")
	_ = d.runOut(ctx, io.Discard, io.Discard, "kubectl", "--kubeconfig", "/kc", "get", "pods")
	_ = d.runInput(ctx, "", io.Discard, io.Discard, "docker", "apply")
	for argv, want := range map[string]bool{
		"kubectl apply --kubeconfig /kc -f -":              true,
		"kubectl apply --kubeconfig /kc -f /corefile.yaml": true,
		"kubectl --kubeconfig /kc get pods":                false,
		"docker apply":                                     false,
	} {
		env, ran := envs[argv]
		if !ran {
			t.Errorf("%s did not run", argv)
			continue
		}
		if got := slices.Contains(env, execx.KubeRCOff); got != want {
			t.Errorf("%s: %s in Env = %v, want %v (Env %q)", argv, execx.KubeRCOff, got, want, env)
		}
	}
}

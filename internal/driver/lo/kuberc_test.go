package lo

// kuberc_test.go: the kubectl children of the Lo driver that run through
// runOut (cmdWith: the CoreDNS annotate and the CoreDNS rollout) run with
// the kubectl user defaults off (execx.KubeRCOff), whatever their
// arguments. Any other tool keeps the environment of lo. The CoreDNS patch
// from a file sets the override on its own Cmd, and the heal deletes the
// Cilium pods through cmdWith. The applies of the driver (the registry
// ConfigMaps, the CoreDNS files and the coredns-custom ConfigMap) run
// through applyPrivate: kapply.RunPrivate sets the override on each kubectl
// it runs. The bash twin is drivers/lo/utils/{registries,services,network}.sh
// (the KUBERC=off prefix).

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestRunOutRunsKubectlWithKubeRCOff(t *testing.T) {
	d, runner, _, _ := testDriver(t)
	envs := map[string][]string{}
	runner.handler = func(c execx.Cmd) error {
		envs[c.Name+" "+strings.Join(c.Args, " ")] = c.Env
		return nil
	}
	ctx := t.Context()
	_ = d.runOut(ctx, io.Discard, io.Discard, "kubectl", "apply", "--kubeconfig", "/kc", "-f", "/corefile.yaml")
	_ = d.runOut(ctx, io.Discard, io.Discard, "kubectl", "--kubeconfig", "/kc", "get", "pods")
	_ = d.runOut(ctx, io.Discard, io.Discard, "kubectl", "--server", "https://203.0.113.1:6443", "annotate", "svc", "coredns-external")
	_ = d.runOut(ctx, io.Discard, io.Discard, "docker", "apply")
	for argv, want := range map[string]bool{
		"kubectl apply --kubeconfig /kc -f /corefile.yaml":                        true,
		"kubectl --kubeconfig /kc get pods":                                       true,
		"kubectl --server https://203.0.113.1:6443 annotate svc coredns-external": true,
		"docker apply": false,
	} {
		env, ran := envs[argv]
		if !ran {
			t.Errorf("%s did not run", argv)
			continue
		}
		if got := execx.KubeRCIsOff(env); got != want {
			t.Errorf("%s: %s in Env = %v, want %v (Env %q)", argv, execx.KubeRCOff, got, want, env)
		}
	}
}

// The CoreDNS deployment patch reads its JSON patch from a file, so it runs
// with the kubectl user defaults off (bash: lo::coredns, `KUBERC=off kubectl
// patch …`).
func TestCorednsPatchRunsWithKubeRCOff(t *testing.T) {
	d, runner, _ := corednsFixture(t, "")
	var patchEnv []string
	patched := false
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" && slices.Contains(c.Args, "patch") && slices.Contains(c.Args, "--patch-file") {
			patchEnv, patched = c.Env, true
		}
		return nil
	}
	if err := d.coredns(t.Context(), io.Discard, io.Discard, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if !patched {
		t.Fatalf("the coredns patch did not run:\n%s", strings.Join(runner.calls, "\n"))
	}
	if !execx.KubeRCIsOff(patchEnv) {
		t.Errorf("coredns patch Env = %q, want %s", patchEnv, execx.KubeRCOff)
	}
}

// The CoreDNS rollout restart is a write: it runs with the kubectl user
// defaults off too (bash: lo::coredns, `KUBERC=off kubectl rollout restart
// …`). The bats twin is "coredns: the rollout restart runs with the kubectl
// user defaults off (KUBERC=off)" in coredns_spec_test.bats.
func TestCorednsRolloutRunsWithKubeRCOff(t *testing.T) {
	d, runner, _ := corednsFixture(t, "")
	var rolloutEnv []string
	rolled := false
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" && slices.Contains(c.Args, "rollout") && slices.Contains(c.Args, "restart") {
			rolloutEnv, rolled = c.Env, true
		}
		return nil
	}
	if err := d.coredns(t.Context(), io.Discard, io.Discard, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if !rolled {
		t.Fatalf("the coredns rollout did not run:\n%s", strings.Join(runner.calls, "\n"))
	}
	if !execx.KubeRCIsOff(rolloutEnv) {
		t.Errorf("coredns rollout Env = %q, want %s", rolloutEnv, execx.KubeRCOff)
	}
}

// The coredns-custom render (create configmap --dry-run=client) feeds the
// apply, so it runs with the kubectl user defaults off too: a kuberc default
// such as --append-hash would rename the ConfigMap (bash: lo::coredns_custom,
// `KUBERC=off kubectl create configmap …`).
func TestCorednsCustomRenderRunsWithKubeRCOff(t *testing.T) {
	d, runner, _ := corednsFixture(t, `  coredns:
    hosts:
      - name: kubehz.dev
        target: gateway
`)
	var renderEnv []string
	rendered := false
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" && strings.Contains(strings.Join(c.Args, " "), "create configmap coredns-custom") {
			renderEnv, rendered = c.Env, true
			writeOut(c, "apiVersion: v1\nkind: ConfigMap\n")
		}
		return nil
	}
	if err := d.coredns(t.Context(), io.Discard, io.Discard, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if !rendered {
		t.Fatalf("the coredns-custom render did not run:\n%s", strings.Join(runner.calls, "\n"))
	}
	if !execx.KubeRCIsOff(renderEnv) {
		t.Errorf("coredns-custom render Env = %q, want %s as the last KUBERC entry", renderEnv, execx.KubeRCOff)
	}
}

// After a repair, the heal deletes the Cilium pods of the node. The delete
// is a write: it runs with the kubectl user defaults off (bash:
// drivers/lo/utils/network.sh, `KUBERC=off kubectl … delete pod`). The
// reads (the node address, /readyz) keep the kuberc of the user.
func TestHealCiliumDeleteRunsWithKubeRCOff(t *testing.T) {
	h := newHealFixture(t)
	inner := h.runner.handler
	deletes := 0
	h.runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" {
			write := slices.Contains(c.Args, "delete")
			if write {
				deletes++
			}
			if got := execx.KubeRCIsOff(c.Env); got != write {
				t.Errorf("%s: %s in Env = %v, want %v", strings.Join(c.Args, " "), execx.KubeRCOff, got, write)
			}
		}
		return inner(c)
	}
	h.heal()
	if deletes != 1 {
		t.Fatalf("Cilium pod deletes = %d, want 1: %q", deletes, h.calls)
	}
}

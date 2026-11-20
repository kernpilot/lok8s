package operator

// kuberc_test.go — the kubectl applies of the hooks (kube.run: the CAPI
// manifests, the status manifests, the Lo resources) run with the kubectl
// user defaults off (execx.KubeRCOff); every other kubectl call keeps the
// environment of the hook.

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestKubeApplyRunsWithKubeRCOff(t *testing.T) {
	envs := map[string][]string{}
	r := &fakeRunner{log: &klog{}, handler: func(c execx.Cmd) error {
		envs[argv(c)] = c.Env
		return nil
	}}
	k := &kube{runner: r, stdout: io.Discard, stderr: io.Discard}
	ctx := t.Context()
	_ = k.run(ctx, strings.NewReader("kind: ConfigMap\n"), nil, nil, "apply", "-f", "-")
	_ = k.run(ctx, nil, nil, nil, "get", "lo", "-o", "json")
	for line, want := range map[string]bool{"kubectl apply -f -": true, "kubectl get lo -o json": false} {
		env, ran := envs[line]
		if !ran {
			t.Errorf("%s did not run", line)
			continue
		}
		if got := slices.Contains(env, execx.KubeRCOff); got != want {
			t.Errorf("%s: %s in Env = %v, want %v", line, execx.KubeRCOff, got, want)
		}
	}
}

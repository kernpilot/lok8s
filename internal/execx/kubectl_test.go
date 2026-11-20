package execx

import (
	"slices"
	"testing"
)

// KubectlEnv turns the kubectl user defaults off for every kubectl child of
// a generic wrapper, whatever its arguments: no guess of the subcommand, so
// a global flag with a separate value before it (--server <url> apply)
// cannot make the override miss. Any other tool gets nil.
func TestKubectlEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"kubectl", []string{"--kubeconfig", "kc.yaml", "apply", "--server-side", "-f", "-"}, true},
		{"kubectl", []string{"--server", "https://203.0.113.1:6443", "apply", "-f", "-"}, true},
		{"kubectl", []string{"--server=https://203.0.113.1:6443", "apply", "-f", "-"}, true},
		{"kubectl", []string{"-s", "https://203.0.113.1:6443", "diff", "-f", "-"}, true},
		{"kubectl", []string{"--kubeconfig", "kc.yaml", "get", "ns"}, true},
		{"kubectl", []string{"replace", "--force", "-f", "-"}, true},
		{"kubectl", nil, true},
		{"/opt/bin/kubectl", []string{"apply", "-f", "-"}, true},
		{"kubeone", []string{"apply", "--manifest", "m.yaml"}, false},
		{"helm", []string{"diff", "upgrade"}, false},
		{"docker", []string{"apply"}, false},
	} {
		got := KubectlEnv(tc.name)
		if tc.want && !slices.Equal(got, []string{KubeRCOff}) {
			t.Errorf("KubectlEnv(%s) for %v = %v, want [%s]", tc.name, tc.args, got, KubeRCOff)
		}
		if !tc.want && got != nil {
			t.Errorf("KubectlEnv(%s) for %v = %v, want nil", tc.name, tc.args, got)
		}
	}
	if KubeRCOff != "KUBERC=off" {
		t.Errorf("KubeRCOff = %q: kubectl reads KUBERC, and off loads no file", KubeRCOff)
	}
}

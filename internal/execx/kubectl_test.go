package execx

import (
	"slices"
	"testing"
)

// KubectlEnv turns the kubectl user defaults off for a kubectl apply and a
// kubectl diff, in each argument order that lo uses, and for no other call.
func TestKubectlEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"kubectl", []string{"--kubeconfig", "kc.yaml", "apply", "--server-side", "-f", "-"}, true},
		{"kubectl", []string{"--kubeconfig", "kc.yaml", "diff", "--server-side", "-f", "-"}, true},
		{"kubectl", []string{"apply", "--kubeconfig", "kc.yaml", "-f", "-"}, true},
		{"kubectl", []string{"apply", "-k", "dir"}, true},
		{"kubectl", []string{"--kubeconfig", "apply", "get", "ns"}, false}, // a kubeconfig named apply
		{"kubectl", []string{"--kubeconfig", "kc.yaml", "get", "ns", "apply"}, false},
		{"kubectl", []string{"--kubeconfig", "kc.yaml", "wait", "--for=condition=Established"}, false},
		{"kubectl", []string{"replace", "--force", "-f", "-"}, false},
		{"kubectl", []string{"-n", "ns", "apply", "-f", "-"}, true},
		{"kubectl", []string{"--namespace", "apply", "get", "cm"}, false}, // a namespace named apply
		{"kubectl", []string{"--context", "c", "diff", "-f", "-"}, true},
		{"kubectl", []string{"-n", "ns", "create", "secret", "generic", "s", "--dry-run=client"}, false},
		{"kubectl", []string{"--namespace=ns", "apply", "-f", "-"}, true},
		{"kubectl", nil, false},
		{"kubeone", []string{"apply", "--manifest", "m.yaml"}, false},
		{"helm", []string{"diff", "upgrade"}, false},
	} {
		got := KubectlEnv(tc.name, tc.args)
		if tc.want && !slices.Equal(got, []string{KubeRCOff}) {
			t.Errorf("KubectlEnv(%s %v) = %v, want [%s]", tc.name, tc.args, got, KubeRCOff)
		}
		if !tc.want && got != nil {
			t.Errorf("KubectlEnv(%s %v) = %v, want nil", tc.name, tc.args, got)
		}
		if KubectlApplyOrDiff(tc.name, tc.args) != tc.want {
			t.Errorf("KubectlApplyOrDiff(%s %v) != %v", tc.name, tc.args, tc.want)
		}
	}
	if KubeRCOff != "KUBERC=off" {
		t.Errorf("KubeRCOff = %q: kubectl reads KUBERC, and off loads no file", KubeRCOff)
	}
}

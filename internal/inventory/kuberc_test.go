package inventory

// kuberc_test.go — each kubectl call of Publish (the server-side applies of
// the CRD and the CR, and the wait between them) runs with the kubectl user
// defaults off (execx.KubeRCOff). The bash twin is libs/inventory/main (the
// KUBERC=off prefix on the applies).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestPublishAppliesRunWithKubeRCOff(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	p := realPaths(t)
	spec := softSpec(t, p, "happy")
	kc := filepath.Join(p.Base, "kc.yaml")
	if err := os.WriteFile(kc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	var errBuf bytes.Buffer
	Publish(t.Context(), p, f, &errBuf, "happy", spec, kc)
	if len(f.calls) != 3 {
		t.Fatalf("calls = %d, want 3:\n%s", len(f.calls), strings.Join(f.calls, "\n"))
	}
	for i, call := range f.calls {
		if !execx.KubeRCIsOff(f.envs[i]) {
			t.Errorf("%s runs without %s (Env %q)", call, execx.KubeRCOff, f.envs[i])
		}
	}
}

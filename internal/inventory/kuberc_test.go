package inventory

// kuberc_test.go — the two server-side applies of Publish (the CRD and the
// CR) run with the kubectl user defaults off (execx.KubeRCOff); the wait
// keeps the environment of lo. The bash twin is libs/inventory/main (the
// KUBERC=off prefix).

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
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
		want := strings.Contains(call, " apply ")
		if got := slices.Contains(f.envs[i], execx.KubeRCOff); got != want {
			t.Errorf("%s: %s in Env = %v, want %v", call, execx.KubeRCOff, got, want)
		}
	}
}

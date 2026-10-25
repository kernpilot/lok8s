package bootstrap

// store_test.go: a domain without its own secrets store, in a project that
// keeps a store per domain, stops the bootstrap before any render or apply.
// The flat store would read and generate values in the wrong place (the
// recreate: a new domain directory would get a new sops-age key and new
// tokens). A dry run stops the same way.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

func TestApplyRefusesADomainWithoutAStore(t *testing.T) {
	for _, dry := range []bool{false, true} {
		e, f, _, errOut, p := testEngine(t)
		writeStackAddon(t, p)
		spec := writeClusterSpec(t, p, "testcni")
		kc := writeKubeconfig(t, p)
		testutil.WriteFile(t, filepath.Join(p.Clusters, "other.dev", "secrets", ".gitignore"), "*\n")
		t.Setenv("PATH_SECRETS", "/ambient/flat")
		e.Options.DryRun = dry

		err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
		if err == nil {
			t.Fatalf("dry=%v: a domain without a store bootstrapped", dry)
		}
		if !dry && !errors.Is(err, ui.ErrHandled) {
			t.Errorf("dry=%v: err = %v, want ui.ErrHandled", dry, err)
		}
		if !strings.Contains(errOut.String(), "[error] domain test.lok8s.dev has no secrets store: "+p.Clusters+"/test.lok8s.dev/secrets does not exist") {
			t.Errorf("dry=%v: stderr = %q", dry, errOut.String())
		}
		if len(f.calls) != 0 {
			t.Errorf("dry=%v: commands ran:\n%s", dry, f.log())
		}
		if got := os.Getenv("PATH_SECRETS"); got != "/ambient/flat" {
			t.Errorf("dry=%v: PATH_SECRETS = %q", dry, got)
		}
	}
}

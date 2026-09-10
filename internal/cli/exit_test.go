package cli

import (
	"os"
	"testing"

	"github.com/kernpilot/lok8s/internal/assets"
)

// exitNow drops the per-run assets temp dir before ending the process with
// the passthrough code; a bare os.Exit leaked /tmp/lo-assets-*.
func TestExitNowCleansUpBeforeExiting(t *testing.T) {
	quietAssets(t)
	p := synthProject(t)
	assets.SetPolicy(assets.PolicyNever)
	dir, _, err := assets.Resolve(p, "addons/cilium")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("--no-eject temp copy not materialized: %v", err)
	}

	saved := osExit
	var got []int
	osExit = func(code int) { got = append(got, code) }
	t.Cleanup(func() { osExit = saved })

	exitNow(7)
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("exit code not passed through: %v", got)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("temp assets dir survived the exit: %s", dir)
	}
}

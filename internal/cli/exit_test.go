package cli

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/driver"
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

// The two rc passthroughs behind the orchestration commands (a dispatch
// error carrying a code, `tilt ci`'s own status) end the process through
// exitNow too; a bare osExit there leaked the same temp dir.
func TestRcPassthroughsCleanUpBeforeExiting(t *testing.T) {
	quietAssets(t)
	p := synthProject(t)
	assets.SetPolicy(assets.PolicyNever)
	saved := osExit
	var got []int
	osExit = func(code int) { got = append(got, code) }
	t.Cleanup(func() { osExit = saved })

	materialize := func() string {
		dir, _, err := assets.Resolve(p, "addons/cilium")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("--no-eject temp copy not materialized: %v", err)
		}
		return dir
	}

	dir := materialize()
	_ = dispatchExit(io.Discard, &driver.ExitError{Code: 42})
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("dispatchExit: temp assets dir survived the exit: %s", dir)
	}

	dir = materialize()
	h := newUpHarness(t, nil)
	_ = runUp(context.Background(), h.p, h.out, h.deps, "lo.dev", true, "10m", false)
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("lo up --ci: temp assets dir survived the exit: %s", dir)
	}
	if len(got) != 1 || got[0] != 42 || len(h.exits) != 1 || h.exits[0] != 7 {
		t.Fatalf("exit codes not passed through: dispatch=%v up=%v", got, h.exits)
	}
}

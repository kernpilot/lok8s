package cli

// dispatch_test.go covers newDispatcher: the seam wiring and the exit
// mapping of dispatchExit.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/driver/capi"
	"github.com/kernpilot/lok8s/internal/driver/kubeone"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Every dispatch-tail hook and driver seam is wired — a nil here is the
// bash "lib not loaded" state that the flip must never ship.
func TestNewDispatcherWiresEverySeam(t *testing.T) {
	p, r, _ := orchestrateProject(t)
	root := NewRoot(p)
	cmd, _, err := root.Find([]string{"provision"})
	if err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(cmd, p)
	if d.Hooks.KubehzRegister == nil || d.Hooks.KubehzDeregister == nil || d.Hooks.BootstrapApply == nil || d.Hooks.InventoryPublish == nil || d.Hooks.GitopsBootstrap == nil {
		t.Errorf("dispatch-tail hooks unwired: %+v", d.Hooks)
	}
	if d.Providers == nil || d.Drivers == nil || d.Runner != r {
		t.Errorf("providers/drivers/runner seams: %+v", d)
	}
	for _, name := range []string{"lo", "kubeone", "capi", "kkp", "kubehz"} {
		f, ok := d.Drivers(name)
		if !ok {
			t.Errorf("driver %q not linked (drivers.go)", name)
			continue
		}
		drv, err := f(&driver.Deps{Paths: p, Runner: r})
		if err != nil {
			t.Fatal(err)
		}
		switch x := drv.(type) {
		case *kubeone.Driver:
			if x.Hooks.ReadKubehzConfig == nil || x.Hooks.ProvisionHosted == nil || x.Hooks.DestroyHosted == nil || x.Hooks.AppendInventory == nil || x.Hooks.PrepareApply == nil {
				t.Errorf("kubeone hooks unwired: %+v", x.Hooks)
			}
		case *capi.Driver:
			if x.Hooks.ReadKubehzConfig == nil || x.Hooks.ProvisionHosted == nil || x.Hooks.DestroyHosted == nil {
				t.Errorf("capi hooks unwired: %+v", x.Hooks)
			}
		case *lodriver.Driver:
			if x.Hooks.KustomizeBuild == nil {
				t.Error("lo KustomizeBuild unwired")
			}
		}
	}
	if _, ok := d.Drivers("nosuch"); ok {
		t.Error("unknown driver must not resolve")
	}
}

func TestDispatchExitMapping(t *testing.T) {
	exits := captureExits(t)
	var stderr bytes.Buffer

	if err := dispatchExit(&stderr, nil); err != nil {
		t.Error(err)
	}
	// An error nobody printed: the mapping prints it as the [error] line
	// the bash would have shown, then exits 1.
	if err := dispatchExit(&stderr, errors.New("plain")); !errors.Is(err, ErrHandled) || len(*exits) != 0 {
		t.Errorf("plain: err=%v exits=%v", err, *exits)
	}
	if got, want := stderr.String(), "\033[0;31m[error]\033[0m plain\n"; got != want {
		t.Errorf("unprinted error: stderr = %q, want %q", got, want)
	}
	// Already printed where it happened (ui.Handled, or the sentinel
	// itself): silent.
	stderr.Reset()
	if err := dispatchExit(&stderr, ui.Handled(errors.New("printed"))); !errors.Is(err, ErrHandled) {
		t.Errorf("handled: err=%v", err)
	}
	if err := dispatchExit(&stderr, fmt.Errorf("wrap: %w", ErrHandled)); !errors.Is(err, ErrHandled) {
		t.Errorf("sentinel: err=%v", err)
	}
	// A failed child streamed its own stderr: silent too.
	childErr := exec.Command("sh", "-c", "exit 1").Run()
	if _, ok := errors.AsType[*exec.ExitError](childErr); !ok {
		t.Fatalf("want an *exec.ExitError, got %v", childErr)
	}
	if err := dispatchExit(&stderr, fmt.Errorf("kubectl: %w", childErr)); !errors.Is(err, ErrHandled) {
		t.Errorf("child: err=%v", err)
	}
	// The interrupt's own trace: main exits 128+n for it, nothing to print.
	if err := dispatchExit(&stderr, fmt.Errorf("wait: %w", context.Canceled)); !errors.Is(err, ErrHandled) {
		t.Errorf("cancel: err=%v", err)
	}
	if stderr.Len() != 0 || len(*exits) != 0 {
		t.Errorf("printed errors must stay silent: stderr=%q exits=%v", stderr.String(), *exits)
	}

	dispatchExit(&stderr, driver.ErrDeclined)
	dispatchExit(&stderr, driver.ErrFullLifecycle)
	dispatchExit(&stderr, &driver.ExitError{Code: 42})
	if len(*exits) != 3 || (*exits)[0] != 3 || (*exits)[1] != 100 || (*exits)[2] != 42 {
		t.Errorf("exits = %v", *exits)
	}
	if stderr.Len() != 0 {
		t.Errorf("rc passthroughs print nothing: %q", stderr.String())
	}
}

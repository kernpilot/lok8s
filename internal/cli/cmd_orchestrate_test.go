package cli

// cmd_orchestrate_test.go — the command-tree wiring of the orchestration
// flip: flag/positional errors in the argsh shape, the dispatch refusals
// that need no driver (deploy domains, missing specs), the registry driver
// gate, and the hook/seam wiring of newDispatcher. The exec seam is a fake
// for every test — nothing here may reach docker/kind/tilt/bash.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/driver/capi"
	"github.com/kernpilot/lok8s/internal/driver/kubeone"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// orchestrateProject is a synthetic project with the routing-axis domains
// and the exec/exit seams faked for the test's lifetime.
func orchestrateProject(t *testing.T) (*config.Paths, *scriptRunner, *[]int) {
	t.Helper()
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "alpha.dev", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: alpha\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "beta.cloud", "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata:\n  name: beta\nspec:\n  kubernetes:\n    version: \"1.31.0\"\n  bootstrap:\n    - name: cilium\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "gamma.app", "deploy.lok8s.yaml"), "kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "nokind.dev", "cluster.lok8s.yaml"), "metadata:\n  name: prod\n")

	r := &scriptRunner{handler: func(c execx.Cmd) error {
		t.Errorf("unexpected exec under test: %s %v", c.Name, c.Args)
		return errors.New("no exec under test")
	}}
	prevRunner := newRunner
	newRunner = func(*config.Paths) execx.Runner { return r }
	exits := captureExits(t)
	t.Cleanup(func() { newRunner = prevRunner })

	t.Setenv("DOMAIN_NAME", "")
	os.Unsetenv("DOMAIN_NAME")
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	t.Setenv("LOK8S_REGISTRY_JSON", "")
	os.Unsetenv("LOK8S_REGISTRY_JSON")
	return p, r, exits
}

func TestOrchestrateArgshShapedParseErrors(t *testing.T) {
	p, _, _ := orchestrateProject(t)
	for _, argv := range [][]string{
		{"provision", "extra"}, {"destroy", "extra"}, {"bootstrap", "extra"}, {"status", "extra"}, {"clean", "extra"}, {"up", "extra"},
	} {
		_, stderr, err := runLo(t, NewRoot(p), argv...)
		if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: too many arguments: extra") || !strings.Contains(stderr, `Run "lo -h" for more information.`) {
			t.Errorf("%v: err=%v stderr=%q", argv, err, stderr)
		}
	}
	for _, argv := range [][]string{{"provision", "--bogus"}, {"up", "--bogus"}, {"clean", "--bogus"}, {"status", "--bogus"}} {
		_, stderr, err := runLo(t, NewRoot(p), argv...)
		if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: unknown flag: --bogus\n\n  Run \"lo -h\"") {
			t.Errorf("%v: err=%v stderr=%q", argv, err, stderr)
		}
	}
	// bash: main::down has no :args — positionals AND unknown flags are
	// dropped; registry::* likewise.
	_, stderr, err := runLo(t, NewRoot(p), "registry", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: Invalid command: bogus") {
		t.Errorf("registry bogus: err=%v stderr=%q", err, stderr)
	}
}

func TestProvisionDeployDomainRefusal(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "provision", "--domain", "gamma.app")
	if !errors.Is(err, ErrHandled) || len(*exits) != 0 {
		t.Fatalf("err=%v exits=%v", err, *exits)
	}
	for _, want := range []string{"lo provision: domain gamma.app\n", "Cannot provision a deployment domain. Use 'lo deploy gamma.app' instead.", "Deployment domains reference a cluster via spec.clusterRef.domain."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q: %q", want, stderr)
		}
	}
	_, stderr, err = runLo(t, NewRoot(p), "p", "--domain", "nope.dev")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "No cluster.lok8s.yaml or deploy.lok8s.yaml found in .lok8s/nope.dev/") {
		t.Errorf("alias/no spec: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "provision", "--domain", "nokind.dev")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "cluster spec has no .kind:") {
		t.Errorf("no kind: err=%v stderr=%q", err, stderr)
	}
}

// The real-infrastructure gate refuses non-interactively with the rc-3
// sentinel — passed through as the process exit (bash: dispatch returns 3,
// main::provision's status IS main's).
func TestProvisionGateDeclineExitsThree(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "provision", "--domain", "beta.cloud")
	if !errors.Is(err, ErrHandled) || len(*exits) != 1 || (*exits)[0] != 3 {
		t.Fatalf("err=%v exits=%v stderr=%q", err, *exits, stderr)
	}
	if !strings.Contains(stderr, "targets \033[1mreal infrastructure\033[0m") || !strings.Contains(stderr, "refusing to reconcile 'beta.cloud' non-interactively — re-run with --force") {
		t.Errorf("stderr = %q", stderr)
	}
	*exits = nil
	_, stderr, _ = runLo(t, NewRoot(p), "provision", "--bootstrap", "--domain", "beta.cloud")
	if len(*exits) != 1 || (*exits)[0] != 3 || !strings.Contains(stderr, "next: re-apply 1 bootstrap addons on the LIVE cluster") {
		t.Errorf("bootstrap gate: exits=%v stderr=%q", *exits, stderr)
	}
	*exits = nil
	_, stderr, _ = runLo(t, NewRoot(p), "destroy", "--domain", "beta.cloud")
	if len(*exits) != 1 || (*exits)[0] != 3 || !strings.Contains(stderr, "refusing to destroy 'beta.cloud' non-interactively") {
		t.Errorf("destroy gate: exits=%v stderr=%q", *exits, stderr)
	}
}

func TestDestroyAndBootstrapRefusals(t *testing.T) {
	p, _, _ := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "destroy", "--domain", "gamma.app")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Cannot destroy a deployment domain. Destroy the cluster domain instead.") {
		t.Errorf("destroy deploy: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "bootstrap", "--domain", "gamma.app")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "cluster spec not found:") {
		t.Errorf("bootstrap deploy: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "bootstrap", "--domain", "../evil")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "invalid domain name: ../evil") {
		t.Errorf("bootstrap traversal: err=%v stderr=%q", err, stderr)
	}
}

func TestRegistryDriverGate(t *testing.T) {
	p, _, _ := orchestrateProject(t)
	cases := map[string]string{
		"beta.cloud": "error: domain 'beta.cloud' uses the 'kubeone' driver — registry management is a 'lo'-driver (local cluster) feature.\n       Pass --domain <a-lo-domain> or switch with 'lo use <domain>'.\n",
		"gamma.app":  "error: domain 'gamma.app' uses the 'deploy' driver — registry management is a 'lo'-driver (local cluster) feature.\n",
		"nope.dev":   "error: domain 'nope.dev' has no readable cluster/deploy spec under clusters/ — cannot run registry management\n",
	}
	for d, want := range cases {
		for _, verb := range []string{"status", "up", "down", "clean"} {
			_, stderr, err := runLo(t, NewRoot(p), "registry", verb, "--domain", d)
			if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, want) {
				t.Errorf("registry %s --domain %s: err=%v stderr=%q", verb, d, err, stderr)
			}
		}
	}
	// A loaded registry JSON (Tilt subshells) skips the gate entirely.
	rj := filepath.Join(p.Base, "reg.json")
	testutil.WriteFile(t, rj, `{"registries":[]}`)
	t.Setenv("LOK8S_REGISTRY_JSON", rj)
	if err := registryGate(p, "beta.cloud", os.Stderr); err != nil {
		t.Errorf("gate must be skipped with LOK8S_REGISTRY_JSON: %v", err)
	}
}

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

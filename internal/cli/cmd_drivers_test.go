package cli

// cmd_drivers_test.go covers `lo drivers`: the list, the error paths and
// the dispatch to a Go driver. The drivers are fakes registered in a test
// registry over a fake runner. Nothing here can touch a real cluster.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// ── lo drivers ─────────────────────────────────────────────────────────

type fakeDriver struct {
	calls *[]string
	err   error
}

func (f *fakeDriver) Provision(_ context.Context, d string) error {
	*f.calls = append(*f.calls, "provision:"+d)
	return f.err
}
func (f *fakeDriver) Destroy(_ context.Context, d string) error {
	*f.calls = append(*f.calls, "destroy:"+d)
	return f.err
}
func (f *fakeDriver) Status(_ context.Context, d string) (string, error) {
	*f.calls = append(*f.calls, "status:"+d)
	return "Fake", f.err
}
func (f *fakeDriver) Kubeconfig(_ context.Context, d string) (string, error) {
	*f.calls = append(*f.calls, "kubeconfig:"+d)
	return "/kc/" + d + ".yaml", f.err
}

type fakeRunner struct{ cmds []execx.Cmd }

func (r *fakeRunner) Run(_ context.Context, c execx.Cmd) error {
	r.cmds = append(r.cmds, c)
	return nil
}

type driversHarness struct {
	calls  []string
	shimed [][]string
	err    error
}

func (h *driversHarness) deps() driversDeps {
	registry := map[string]driver.Factory{
		"fakedrv": func(deps *driver.Deps) (driver.Driver, error) {
			return &fakeDriver{calls: &h.calls, err: h.err}, nil
		},
	}
	return driversDeps{
		names: func() []string { return []string{"fakedrv"} },
		lookup: func(name string) (driver.Factory, bool) {
			f, ok := registry[name]
			return f, ok
		},
		runner: &fakeRunner{},
		shim:   func(argv []string) error { h.shimed = append(h.shimed, argv); return nil },
	}
}

// driversRoot is the real root with the drivers command rebuilt over the
// test registry.
func driversRoot(p *config.Paths, deps driversDeps) *cobra.Command {
	root := NewRoot(p)
	for _, c := range root.Commands() {
		if c.Name() == "drivers" {
			root.RemoveCommand(c)
		}
	}
	var spec commandSpec
	for _, s := range commandTree {
		if s.use == "drivers" {
			spec = s
		}
	}
	root.AddCommand(newDriversCommand(p, spec, deps))
	return root
}

func TestDriversList(t *testing.T) {
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "drivers", "bashonly", "main"), "#!/usr/bin/env argsh\n")
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "drivers", "fakedrv", "main"), "#!/usr/bin/env argsh\n")
	os.MkdirAll(filepath.Join(p.Lok8s, "drivers", ".hidden"), 0o755)
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "drivers", "README.md"), "x")
	h := &driversHarness{}
	stdout, _, err := runLo(t, driversRoot(p, h.deps()), "drivers", "--list")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Available drivers:\n\n- bashonly\n- fakedrv\n"; stdout != want {
		t.Errorf("list = %q, want %q", stdout, want)
	}
	if stdout2, _, _ := runLo(t, driversRoot(p, h.deps()), "drivers", "-l"); stdout2 != stdout {
		t.Errorf("-l differs: %q", stdout2)
	}
}

func TestDriversErrorPaths(t *testing.T) {
	p := synthProject(t)
	h := &driversHarness{}

	_, stderr, err := runLo(t, driversRoot(p, h.deps()), "drivers")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Driver name required — try: lo drivers --list") {
		t.Errorf("bare: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, driversRoot(p, h.deps()), "drivers", "../x")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Invalid driver name: ../x") {
		t.Errorf("invalid: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, driversRoot(p, h.deps()), "drivers", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Driver 'bogus' not found") {
		t.Errorf("unknown: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Invalid command: bogus") {
		t.Errorf("driver bogus cmd: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "status")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "missing required argument: domain") {
		t.Errorf("missing domain: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "status", "a.dev", "extra")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "too many arguments: extra") {
		t.Errorf("too many: err=%v stderr=%q", err, stderr)
	}
	if len(h.calls) != 0 {
		t.Errorf("driver called on an error path: %v", h.calls)
	}

	// A bash-only driver falls back to the argsh implementation, argv
	// verbatim.
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "drivers", "bashonly", "main"), "#!/usr/bin/env argsh\n")
	saved := os.Args
	os.Args = []string{"lo", "drivers", "bashonly", "status", "a.dev"}
	defer func() { os.Args = saved }()
	if _, _, err := runLo(t, driversRoot(p, h.deps()), "drivers", "bashonly", "status", "a.dev"); err != nil {
		t.Fatal(err)
	}
	if len(h.shimed) != 1 || strings.Join(h.shimed[0], " ") != "drivers bashonly status a.dev" {
		t.Errorf("shim argv = %v", h.shimed)
	}
}

func TestDriversDispatchToGoDriver(t *testing.T) {
	p := synthProject(t)
	h := &driversHarness{}
	exits := captureExits(t)

	stdout, _, err := runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "status", "a.dev")
	if err != nil || stdout != "Fake\n" {
		t.Errorf("status: err=%v stdout=%q", err, stdout)
	}
	stdout, _, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "k", "b.dev")
	if err != nil || stdout != "/kc/b.dev.yaml\n" {
		t.Errorf("kubeconfig: err=%v stdout=%q", err, stdout)
	}
	if _, _, err := runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "provision", "c.dev"); err != nil {
		t.Errorf("provision: %v", err)
	}
	if _, _, err := runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "d", "c.dev"); err != nil {
		t.Errorf("destroy: %v", err)
	}
	want := "status:a.dev kubeconfig:b.dev provision:c.dev destroy:c.dev"
	if got := strings.Join(h.calls, " "); got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if len(h.shimed) != 0 || len(*exits) != 0 {
		t.Errorf("unexpected shim/exit: %v %v", h.shimed, *exits)
	}

	// The driver's own rc passes through (gate decline sentinel 3); a plain
	// error is a plain exit 1.
	h.err = driver.ErrDeclined
	_, _, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "provision", "c.dev")
	if !errors.Is(err, ErrHandled) || len(*exits) != 1 || (*exits)[0] != 3 {
		t.Errorf("rc 3 passthrough: err=%v exits=%v", err, *exits)
	}
	h.err = errors.New("boom")
	_, _, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "status", "c.dev")
	if err == nil || errors.Is(err, ErrHandled) || err.Error() != "boom" {
		t.Errorf("plain error: %v", err)
	}
}

func TestDriversHelpTablesCoverRegistry(t *testing.T) {
	t.Parallel()
	for _, n := range driver.Names() {
		if _, ok := driverUsages[n]; !ok {
			t.Errorf("Go driver %q has no verbatim usage table (add its main::driver texts)", n)
		}
	}
}

package operator

// operator_test.go — the `--config` goldens (generated once from the bash
// hooks: `LOK8S_STATE_DIR=$tmp bash operator/hooks/<hook>.sh --config >
// testdata/<hook>.config.yaml`; -update rewrites them), the binding-context
// reader's exit paths, and the runtime env layout.

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// update rewrites the golden files with the current output:
// go test ./internal/operator/ -update
var update = flag.Bool("update", false, "rewrite the golden files")

func TestConfigGoldens(t *testing.T) {
	t.Parallel()
	for name, hook := range map[string]Hook{
		"lo-reconcile":     &LoHook{},
		"capi-reconcile":   &CapiHook{},
		"capi-status-sync": &CapiStatusSyncHook{},
	} {
		testutil.Golden(t, filepath.Join("testdata", name+".config.yaml"), hook.Config(), *update)
	}
}

func TestReadBindingContext(t *testing.T) {
	var stderr bytes.Buffer
	exitCode := func(err error) int {
		if ee, ok := errors.AsType[*ExitError](err); ok {
			return ee.Code
		}
		return -1
	}
	// bash: `set -u` abort (1), jq "Could not open file" (2), jq parse
	// error (5) — measured on the frozen hooks.
	if _, err := ReadBindingContext(&stderr, ""); exitCode(err) != 1 {
		t.Errorf("unset path: err = %v", err)
	}
	if stderr.String() != "error: BINDING_CONTEXT_PATH: unbound variable\n" {
		t.Errorf("unset path stderr = %q", stderr.String())
	}

	stderr.Reset()
	if _, err := ReadBindingContext(&stderr, filepath.Join(t.TempDir(), "missing.json")); exitCode(err) != 2 {
		t.Errorf("missing file: err = %v", err)
	}
	if !strings.HasPrefix(stderr.String(), "jq: error: Could not open file ") {
		t.Errorf("missing file stderr = %q", stderr.String())
	}
	stderr.Reset()
	if _, err := ReadBindingContext(&stderr, bindingFile(t, "not json")); exitCode(err) != 5 {
		t.Errorf("non-JSON: err = %v", err)
	}

	events, err := ReadBindingContext(&stderr, bindingFile(t, `[{"type":"Schedule","binding":"lo-drift"},{"object":{"a":1}},{"type":"Event","filterResult":null}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].EventType() != "Schedule" || events[1].EventType() != "Event" || events[0].Binding != "lo-drift" {
		t.Errorf("events = %+v", events)
	}
	if string(events[1].Object) != `{"a":1}` || string(eventObject(events[0])) != "null" {
		t.Errorf("object decoding: %q / %q", events[1].Object, eventObject(events[0]))
	}
	// An empty batch is a no-op for every hook.
	empty := mustEvents(t, `[]`)
	for _, h := range []Hook{&LoHook{}, &CapiHook{}, &CapiStatusSyncHook{}} {
		if err := h.Trigger(t.Context(), empty); err != nil {
			t.Errorf("%T: empty batch: %v", h, err)
		}
	}
}

func TestEnvLayout(t *testing.T) {
	for _, k := range []string{"PATH_LOK8S", "LOK8S_STATE_DIR", "KUSTOMIZE_PLUGIN_HOME", "PATH_BASE", "PATH_CLUSTERS", "PATH_SECRETS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	e := ResolveEnv()
	if e.HookDir != DefaultHookDir || e.StateDir != DefaultStateDir || e.KustomizePluginHome != DefaultKustomizePluginHome {
		t.Errorf("defaults: %+v", e)
	}
	if e.CapiTemplateDir() != "/hooks/capi-templates" {
		t.Errorf("template dir = %s", e.CapiTemplateDir())
	}

	state := t.TempDir()
	t.Setenv("LOK8S_STATE_DIR", state)
	t.Setenv("PATH_LOK8S", "/opt/hooks")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "/plugins")
	e = ResolveEnv()
	p := e.Paths()
	if p.Base != state || p.Lok8s != "/opt/hooks" || p.Clusters != filepath.Join(state, "clusters") {
		t.Errorf("paths: %+v", p)
	}
	if err := e.Export(); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"PATH_BASE": state, "PATH_CLUSTERS": filepath.Join(state, "clusters"),
		"PATH_SECRETS": filepath.Join(state, ".secrets"), "PATH_LOK8S": "/opt/hooks",
		"KUSTOMIZE_PLUGIN_HOME": "/plugins",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, dir := range []string{"clusters", ".secrets", ".kubeconfig"} {
		if info, err := os.Stat(filepath.Join(state, dir)); err != nil || !info.IsDir() {
			t.Errorf("%s not created", dir)
		}
	}
}

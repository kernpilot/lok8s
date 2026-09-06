package operator

// operator_test.go — the `--config` goldens (generated once from the bash
// hooks: `LOK8S_STATE_DIR=$tmp bash operator/hooks/<hook>.sh --config >
// testdata/<hook>.config.yaml`, read-only since), the binding-context
// reader's exit paths, and the runtime env layout.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigGoldens(t *testing.T) {
	for name, hook := range map[string]Hook{
		"lo-reconcile":     &LoHook{},
		"capi-reconcile":   &CapiHook{},
		"capi-status-sync": &CapiStatusSyncHook{},
	} {
		want := readFileT(t, filepath.Join("testdata", name+".config.yaml"))
		if got := hook.Config(); got != want {
			t.Errorf("%s --config drifted from the bash golden:\n got: %q\nwant: %q", name, got, want)
		}
	}
}

func TestReadBindingContext(t *testing.T) {
	var stderr bytes.Buffer
	exitCode := func(err error) int {
		var ee *ExitError
		if errors.As(err, &ee) {
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
		if err := h.Trigger(context.Background(), empty); err != nil {
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

func TestJqIdioms(t *testing.T) {
	v, _ := decode([]byte(`{"n":3,"f":1.0,"s":"x","b":false,"z":null,"o":{"k":"v"},"a":["p"]}`))
	cases := map[string]string{
		jqR(get(v, "n")):                   "3",
		jqR(get(v, "f")):                   "1.0",
		jqR(get(v, "s")):                   "x",
		jqR(get(v, "b")):                   "false",
		jqR(get(v, "z")):                   "null",
		jqR(get(v, "missing")):             "null",
		jqR(get(v, "s", "deeper")):         "null",
		jqR(get(v, "o")):                   `{"k":"v"}`,
		jqR(alt(get(v, "b"), "d")):         "d",
		jqR(alt(get(v, "z"), "d")):         "d",
		jqEmpty(get(v, "z")):               "",
		jqEmpty(get(v, "b")):               "",
		jqEmpty(get(v, "n")):               "3",
		compact(alt(get(v, "z"), []any{})): "[]",
		compact(alt(get(v, "a"), []any{})): `["p"]`,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if present(get(v, "z")) || present(get(v, "b")) || !present(get(v, "o")) || !present(get(v, "n")) {
		t.Error("present (jq -e) semantics")
	}
	if !contains(get(v, "a"), "p") || contains(get(v, "o"), "v") || contains(nil, "x") {
		t.Error("contains semantics")
	}
}

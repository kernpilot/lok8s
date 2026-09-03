package cli

// Tests for the cluster-free leaf ports: init, crds, addons (--detail),
// drivers, chat, ai. Everything runs against a synthetic project under a
// temp dir; drivers are fakes registered in a test registry over a fake
// runner — nothing here can touch a real cluster, Tilt, or a registry.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

func repoRootDir(t *testing.T) string {
	t.Helper()
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	if _, err := os.Stat(filepath.Join(root, ".lok8s", "lo")); err != nil {
		t.Skip("repo checkout not available")
	}
	return root
}

// synthProject is a synthetic lok8s project: its own .lok8s (empty unless
// the test links the framework tree), clusters/, .bin/.
func synthProject(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	p := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	os.MkdirAll(p.Bin, 0o755)
	os.MkdirAll(p.Lok8s, 0o755)
	os.MkdirAll(p.Clusters, 0o755)
	return p
}

// runLo executes the Go command tree with the given argv (no subprocess).
func runLo(t *testing.T, root *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ── lo init ────────────────────────────────────────────────────────────

func TestInitCommandRouting(t *testing.T) {
	p := synthProject(t)
	root := NewRoot(p)

	_, stderr, err := runLo(t, root, "init", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: Invalid command: bogus") {
		t.Errorf("init bogus: err=%v stderr=%q", err, stderr)
	}

	_, stderr, err = runLo(t, NewRoot(p), "init", "service")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "service name is required") {
		t.Errorf("init service (no name): err=%v stderr=%q", err, stderr)
	}

	svc := filepath.Join(p.Base, "svc")
	stdout, _, err := runLo(t, NewRoot(p), "init", "service", "foo", "--path", svc)
	if err != nil {
		t.Fatalf("init service: %v", err)
	}
	if !strings.Contains(stdout, "Scaffolded "+svc+"/lok8s.yaml\n") || !strings.Contains(stdout, "Wrote canonical Tiltfile") {
		t.Errorf("stdout:\n%s", stdout)
	}
	// The inherited --force|-f reaches the scaffold.
	writeFile(t, svc+"/lok8s.yaml", "build: { context: ., dockerfile: Keep }\n")
	if _, _, err := runLo(t, NewRoot(p), "init", "service", "foo", "-p", svc, "-f"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(svc + "/lok8s.yaml")
	if strings.Contains(string(raw), "Keep") {
		t.Error("-f did not force the overwrite")
	}

	stdout, _, err = runLo(t, NewRoot(p), "init", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Base, "tests", "playwright.config.ts")); err != nil {
		t.Error("init test did not scaffold the suite")
	}
	if !strings.Contains(stdout, "Scaffolded Playwright test suite into "+p.Base+"/tests\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

// ── lo crds ────────────────────────────────────────────────────────────

func crdsSandbox(t *testing.T) *config.Paths {
	t.Helper()
	repo := repoRootDir(t)
	p := synthProject(t)
	schemas, _ := filepath.Glob(filepath.Join(repo, "operator", "crds", "schema", "*.schema.yaml"))
	if len(schemas) == 0 {
		t.Skip("no schema source")
	}
	for _, s := range schemas {
		raw, _ := os.ReadFile(s)
		writeFile(t, filepath.Join(p.Base, "operator", "crds", "schema", filepath.Base(s)), string(raw))
	}
	return p
}

func TestCrdsGenerateThenCheck(t *testing.T) {
	p := crdsSandbox(t)
	stdout, _, err := runLo(t, NewRoot(p), "crds", "generate")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"lo", "capi", "kkp", "kubeone", "deploy", "clusterinventory"} {
		if _, err := os.Stat(filepath.Join(p.Base, "operator", "crds", kind+".yaml")); err != nil {
			t.Errorf("generate skipped %s", kind)
		}
		if !strings.Contains(stdout, "  wrote operator/crds/"+kind+".yaml\n") {
			t.Errorf("no progress line for %s:\n%s", kind, stdout)
		}
	}
	mirror := filepath.Join(p.Lok8s, "libs", "inventory", "manifests", "clusterinventory.crd.yaml")
	if _, err := os.Stat(mirror); err != nil {
		t.Error("inventory mirror not written")
	}
	if !strings.Contains(stdout, "  wrote .lok8s/libs/inventory/manifests/clusterinventory.crd.yaml (mirror for synced .lok8s trees)\n") {
		t.Errorf("mirror progress line:\n%s", stdout)
	}
	head, _ := os.ReadFile(filepath.Join(p.Base, "operator", "crds", "lo.yaml"))
	if !strings.HasPrefix(string(head), "# GENERATED by 'lo crds generate'") {
		t.Error("banner missing")
	}

	stdout, _, err = runLo(t, NewRoot(p), "crds", "check")
	if err != nil || stdout != "  all CRDs up to date.\n" {
		t.Errorf("check after generate: err=%v stdout=%q", err, stdout)
	}
	_, _, err = runLo(t, NewRoot(p), "crds", "c")
	if err != nil {
		t.Errorf("alias c: %v", err)
	}
}

// The drift gate: a hand edit to a generated CRD (or the mirror) must fail
// `lo crds check`.
func TestCrdsCheckDetectsDrift(t *testing.T) {
	p := crdsSandbox(t)
	if _, _, err := runLo(t, NewRoot(p), "crds", "generate"); err != nil {
		t.Fatal(err)
	}
	appendTo := func(path string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("  hand-edited: true\n")
		f.Close()
	}
	appendTo(filepath.Join(p.Base, "operator", "crds", "lo.yaml"))
	_, stderr, err := runLo(t, NewRoot(p), "crds", "check")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "operator/crds/lo.yaml is STALE") {
		t.Errorf("drift not detected: err=%v stderr=%q", err, stderr)
	}

	if _, _, err := runLo(t, NewRoot(p), "crds", "generate"); err != nil {
		t.Fatal(err)
	}
	appendTo(filepath.Join(p.Lok8s, "libs", "inventory", "manifests", "clusterinventory.crd.yaml"))
	_, stderr, err = runLo(t, NewRoot(p), "crds", "check")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, ".lok8s/libs/inventory/manifests/clusterinventory.crd.yaml is STALE") {
		t.Errorf("mirror drift not detected: err=%v stderr=%q", err, stderr)
	}

	_, stderr, err = runLo(t, NewRoot(p), "crds", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Invalid command: bogus") {
		t.Errorf("crds bogus: err=%v stderr=%q", err, stderr)
	}
}

// ── lo addons --detail (port of addons_detail_test.bats' inventory half) ─

func addonsProject(t *testing.T) *config.Paths {
	t.Helper()
	repo := repoRootDir(t)
	p := synthProject(t)
	// The REAL addon tree so category/version come from the shipped labels.
	p.Lok8s = filepath.Join(repo, ".lok8s")
	return p
}

func TestAddonsDetailInventory(t *testing.T) {
	p := addonsProject(t)
	os.MkdirAll(filepath.Join(p.Clusters, "inv", "targets", "networking"), 0o755)
	writeFile(t, filepath.Join(p.Clusters, "inv", "cluster.lok8s.yaml"), `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: inv }
spec:
  bootstrap:
    - cilium: { wait: true }
    - cert-manager
    - ./targets/networking: { dependsOn: [cert-manager] }
`)
	stdout, _, err := runLo(t, NewRoot(p), "addons", "--detail", "--domain", "inv")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Addons deployed by inv (kind=kubeone)",
		"cilium", "networking", "policyAuditMode",
		"cert-manager", "infrastructure",
		"per-cluster glue in clusters/inv/targets/networking",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}

	// Map-form entry stays ONE addon (no shattering into reserved keys).
	writeFile(t, filepath.Join(p.Clusters, "mapform", "cluster.lok8s.yaml"), `kind: KubeOne
metadata: { name: m }
spec:
  bootstrap:
    - ccm:
        values:
          env:
            ROBOT_ENABLED: { value: "true" }
        wait: true
`)
	stdout, _, err = runLo(t, NewRoot(p), "a", "--detail", "--domain", "mapform")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "ccm") || !strings.Contains(stdout, "hcloud CCM") {
		t.Errorf("got:\n%s", stdout)
	}
	if regexp.MustCompile(`(?m)^(values|env|wait|dependsOn)[[:space:]]`).MatchString(stdout) {
		t.Errorf("map entry shattered:\n%s", stdout)
	}

	// Empty bootstrap, missing spec, injected domain, malformed kind.
	writeFile(t, filepath.Join(p.Clusters, "empty", "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata: { name: e }\nspec:\n  bootstrap: []\n")
	stdout, _, _ = runLo(t, NewRoot(p), "addons", "--detail", "--domain", "empty")
	if !strings.Contains(stdout, "deploys no addons") {
		t.Errorf("empty:\n%s", stdout)
	}
	_, stderr, err := runLo(t, NewRoot(p), "addons", "--detail", "--domain", "nonexistent-domain")
	if err != nil || !strings.Contains(stderr, "nothing to inventory") {
		t.Errorf("missing spec: err=%v stderr=%q", err, stderr)
	}
	for _, d := range []string{"../etc", "/abs", "foo/../bar", "a/b", ".hidden"} {
		stdout, stderr, err := runLo(t, NewRoot(p), "addons", "--detail", "--domain", d)
		if err != nil || !strings.Contains(stderr, "Invalid domain") || strings.Contains(stdout, "Addons deployed by") {
			t.Errorf("%q: err=%v out=%q stderr=%q", d, err, stdout, stderr)
		}
	}
	writeFile(t, filepath.Join(p.Clusters, "bad2", "cluster.lok8s.yaml"), "kind: \"a b\"\nmetadata: { name: bad2 }\nspec:\n  bootstrap:\n    - cilium\n")
	stdout, _, err = runLo(t, NewRoot(p), "addons", "--detail", "--domain", "bad2")
	if !errors.Is(err, ErrHandled) || strings.Contains(stdout, "kind=lo") {
		t.Errorf("malformed kind: err=%v out=%q", err, stdout)
	}
}

func TestAddonsShowSeparator(t *testing.T) {
	p := addonsProject(t)
	stdout, _, err := runLo(t, NewRoot(p), "addons", "cilium", "metallb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "\n\n---\nname:    metallb\n") {
		t.Errorf("separator:\n%s", stdout)
	}
	if !strings.HasPrefix(stdout, "name:    cilium\ndriver:  lo\npath:    "+p.Lok8s+"/addons/cilium\ntype:    khelm\n") {
		t.Errorf("show:\n%s", stdout)
	}
}

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
	exits  []int
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
		exit:   func(code int) { h.exits = append(h.exits, code) },
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
	writeFile(t, filepath.Join(p.Lok8s, "drivers", "bashonly", "main"), "#!/usr/bin/env argsh\n")
	writeFile(t, filepath.Join(p.Lok8s, "drivers", "fakedrv", "main"), "#!/usr/bin/env argsh\n")
	os.MkdirAll(filepath.Join(p.Lok8s, "drivers", ".hidden"), 0o755)
	writeFile(t, filepath.Join(p.Lok8s, "drivers", "README.md"), "x")
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
	writeFile(t, filepath.Join(p.Lok8s, "drivers", "bashonly", "main"), "#!/usr/bin/env argsh\n")
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
	if len(h.shimed) != 0 || len(h.exits) != 0 {
		t.Errorf("unexpected shim/exit: %v %v", h.shimed, h.exits)
	}

	// The driver's own rc passes through (gate decline sentinel 3); a plain
	// error is a plain exit 1.
	h.err = driver.ErrDeclined
	_, _, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "provision", "c.dev")
	if !errors.Is(err, ErrHandled) || len(h.exits) != 1 || h.exits[0] != 3 {
		t.Errorf("rc 3 passthrough: err=%v exits=%v", err, h.exits)
	}
	h.err = errors.New("boom")
	_, _, err = runLo(t, driversRoot(p, h.deps()), "drivers", "fakedrv", "status", "c.dev")
	if err == nil || errors.Is(err, ErrHandled) || err.Error() != "boom" {
		t.Errorf("plain error: %v", err)
	}
}

func TestDriversHelpTablesCoverRegistry(t *testing.T) {
	for _, n := range driver.Names() {
		if _, ok := driverUsages[n]; !ok {
			t.Errorf("Go driver %q has no verbatim usage table (add its main::driver texts)", n)
		}
	}
}

// ── lo chat ────────────────────────────────────────────────────────────

func chatProject(t *testing.T) *config.Paths {
	t.Helper()
	p := synthProject(t)
	writeFile(t, filepath.Join(p.Bin, "lochat"), "#!/bin/sh\nexit 0\n")
	os.Chmod(filepath.Join(p.Bin, "lochat"), 0o755)
	writeFile(t, filepath.Join(p.Bin, "argsh.so"), "")
	writeFile(t, filepath.Join(p.Lok8s, "chat", "defaults.json"), "{}\n")
	return p
}

func TestChatExecArgv(t *testing.T) {
	p := chatProject(t)
	t.Setenv("LO_CHAT_CONFIG", "")
	os.Unsetenv("LO_CHAT_CONFIG")

	var gotBin string
	var gotArgv []string
	saved := execProcess
	execProcess = func(bin string, argv, env []string) error {
		gotBin, gotArgv = bin, argv
		return nil
	}
	defer func() { execProcess = saved }()

	if _, _, err := runLo(t, NewRoot(p), "chat", "-p", "hello", "--model", "m", "--help"); err != nil {
		t.Fatal(err)
	}
	if gotBin != filepath.Join(p.Bin, "lochat") {
		t.Errorf("bin = %q", gotBin)
	}
	want := []string{
		filepath.Join(p.Bin, "lochat"),
		"--config", filepath.Join(p.Lok8s, "chat", "defaults.json"),
		"--lo", filepath.Join(p.Lok8s, "lo"),
		"--cwd", p.Base,
		"--base-dir", p.Base,
		"-p", "hello", "--model", "m", "--help",
	}
	if strings.Join(gotArgv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q\nwant  %q", gotArgv, want)
	}

	// A per-project lo-chat.json wins over the shipped defaults; LO_CHAT_CONFIG
	// overrides the project path.
	writeFile(t, filepath.Join(p.Base, "lo-chat.json"), "{}\n")
	runLo(t, NewRoot(p), "chat")
	if gotArgv[2] != filepath.Join(p.Base, "lo-chat.json") {
		t.Errorf("project config not preferred: %q", gotArgv[2])
	}
	custom := filepath.Join(p.Base, "custom.json")
	writeFile(t, custom, "{}\n")
	t.Setenv("LO_CHAT_CONFIG", custom)
	runLo(t, NewRoot(p), "chat")
	if gotArgv[2] != custom {
		t.Errorf("LO_CHAT_CONFIG not honoured: %q", gotArgv[2])
	}
}

func TestChatPreflightErrors(t *testing.T) {
	saved := execProcess
	execProcess = func(string, []string, []string) error { t.Fatal("exec reached"); return nil }
	defer func() { execProcess = saved }()
	t.Setenv("PATH", t.TempDir()) // no lochat/argsh on PATH

	p := synthProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "chat")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "lochat binary not found. Build it once:") ||
		!strings.Contains(stderr, `  go build -C ai/lochat -o "${PATH_BIN}/lochat" .`) {
		t.Errorf("missing lochat: err=%v stderr=%q", err, stderr)
	}

	writeFile(t, filepath.Join(p.Bin, "lochat"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(p.Bin, "lochat"), 0o755)
	// No project config and no local .lok8s/chat: the embedded defaults are
	// ejected on first use (the "no chat config" error is unreachable now),
	// and the preflight moves on to the argsh.so check.
	_, stderr, err = runLo(t, NewRoot(p), "chat")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "argsh.so is missing") || !strings.Contains(stderr, "  argsh builtins install") {
		t.Errorf("missing argsh.so: err=%v stderr=%q", err, stderr)
	}
	ejected, err := os.ReadFile(filepath.Join(p.Lok8s, "chat", "defaults.json"))
	if err != nil {
		t.Fatalf("chat defaults not ejected: %v", err)
	}
	embedded, _ := fs.ReadFile(assets.FS(), "chat/defaults.json")
	if !bytes.Equal(ejected, embedded) {
		t.Error("ejected defaults.json differs from the embedded copy")
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "chat", assets.MarkerFile)); err != nil {
		t.Error("no .lo-origin marker next to the ejected defaults")
	}

	// LO_ASSETS_EJECT=never: a fresh project stays untouched. (chat passes
	// its argv through unparsed, so the env form is the one that reaches
	// it; the --no-eject flag is covered on a flag-parsing command in
	// cmd_assets_test.go.)
	t.Setenv(assets.EnvEject, "never")
	p2 := synthProject(t)
	writeFile(t, filepath.Join(p2.Bin, "lochat"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(p2.Bin, "lochat"), 0o755)
	_, _, _ = runLo(t, NewRoot(p2), "chat")
	if _, err := os.Stat(filepath.Join(p2.Lok8s, "chat")); err == nil {
		t.Error("LO_ASSETS_EJECT=never still wrote .lok8s/chat")
	}
}

// ── lo ai ──────────────────────────────────────────────────────────────

func skillsProject(t *testing.T) *config.Paths {
	t.Helper()
	p := synthProject(t)
	writeFile(t, filepath.Join(p.Base, "skills", "beta", "SKILL.md"), "# beta\n")
	writeFile(t, filepath.Join(p.Base, "skills", "alpha", "SKILL.md"), "# alpha\n")
	writeFile(t, filepath.Join(p.Base, "skills", "alpha", "extra.txt"), "x\n")
	writeFile(t, filepath.Join(p.Base, "skills", "noskill", "README.md"), "not a skill\n")
	return p
}

func TestAiSkillsLinkUnlink(t *testing.T) {
	p := skillsProject(t)
	src := filepath.Join(p.Base, "skills")
	dst := filepath.Join(p.Base, ".claude", "skills")

	stdout, _, err := runLo(t, NewRoot(p), "ai", "skills")
	if err != nil {
		t.Fatal(err)
	}
	want := "Agent skills — " + src + "\n" +
		"  alpha                    lo chat: injected (not linked)\n" +
		"  beta                     lo chat: injected (not linked)\n" +
		"\nLink them natively into Claude:  lo ai link claude\n"
	if stdout != want {
		t.Errorf("skills:\n%s\nwant:\n%s", stdout, want)
	}

	stdout, _, err = runLo(t, NewRoot(p), "ai", "link")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Linked 2 skills into " + dst + " (symlink).\nclaude (run in this project) now loads the lok8s skills natively.\n"; stdout != want {
		t.Errorf("link:\n%s", stdout)
	}
	if link, err := os.Readlink(filepath.Join(dst, "alpha")); err != nil || link != filepath.Join(src, "alpha") {
		t.Errorf("alpha link = %q, %v", link, err)
	}
	stdout, _, _ = runLo(t, NewRoot(p), "ai", "skills")
	if !strings.Contains(stdout, "  alpha                    claude: linked\n") {
		t.Errorf("after link:\n%s", stdout)
	}

	stdout, _, err = runLo(t, NewRoot(p), "ai", "link", "claude", "--copy")
	if err != nil || !strings.Contains(stdout, "Linked 2 skills into "+dst+" (copy).") {
		t.Errorf("copy: err=%v stdout=%q", err, stdout)
	}
	if info, err := os.Lstat(filepath.Join(dst, "alpha")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Error("copy left a symlink")
	}
	if _, err := os.Stat(filepath.Join(dst, "alpha", "extra.txt")); err != nil {
		t.Error("copy is not recursive")
	}
	stdout, _, _ = runLo(t, NewRoot(p), "ai", "skills")
	if !strings.Contains(stdout, "  beta                     claude: copied\n") {
		t.Errorf("after copy:\n%s", stdout)
	}

	// unlink cleans copies matching a current skill AND our symlinks (even
	// dangling ones), leaves foreign entries alone.
	os.Symlink(filepath.Join(src, "gone"), filepath.Join(dst, "gone"))
	os.Symlink("/elsewhere/thing", filepath.Join(dst, "foreign"))
	os.MkdirAll(filepath.Join(dst, "theirs"), 0o755)
	stdout, _, err = runLo(t, NewRoot(p), "ai", "unlink")
	if err != nil || stdout != "Unlinked 3 skills from "+dst+".\n" {
		t.Errorf("unlink: err=%v stdout=%q", err, stdout)
	}
	for _, kept := range []string{"foreign", "theirs"} {
		if _, err := os.Lstat(filepath.Join(dst, kept)); err != nil {
			t.Errorf("%s removed", kept)
		}
	}
	for _, gone := range []string{"alpha", "beta", "gone"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%s not removed", gone)
		}
	}

	_, stderr, err := runLo(t, NewRoot(p), "ai", "link", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "bogus has no native skill dir — it gets skills by injection from `lo chat`, nothing to link.") {
		t.Errorf("link bogus: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "ai", "unlink", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "bogus: no skill dir") {
		t.Errorf("unlink bogus: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "ai", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Invalid command: bogus") {
		t.Errorf("ai bogus: err=%v stderr=%q", err, stderr)
	}

	empty := synthProject(t)
	stdout, _, err = runLo(t, NewRoot(empty), "ai", "unlink")
	if err != nil || stdout != "Nothing linked in "+filepath.Join(empty.Base, ".claude", "skills")+".\n" {
		t.Errorf("unlink nothing: err=%v stdout=%q", err, stdout)
	}
	_, stderr, err = runLo(t, NewRoot(empty), "ai", "skills")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "no skills dir: "+filepath.Join(empty.Base, "skills")) {
		t.Errorf("no skills: err=%v stderr=%q", err, stderr)
	}
}

func TestAiCheckRunsRuntimeCheckThenSkills(t *testing.T) {
	p := chatProject(t)
	writeFile(t, filepath.Join(p.Base, "skills", "alpha", "SKILL.md"), "# alpha\n")
	t.Setenv("LO_CHAT_CONFIG", "")
	os.Unsetenv("LO_CHAT_CONFIG")

	var gotArgv []string
	rc := 0
	savedRun, savedExit := runProcess, exitProcess
	var exits []int
	runProcess = func(bin string, argv, env []string) int { gotArgv = argv; return rc }
	exitProcess = func(code int) { exits = append(exits, code) }
	defer func() { runProcess, exitProcess = savedRun, savedExit }()

	stdout, _, err := runLo(t, NewRoot(p), "ai", "check")
	if err != nil {
		t.Fatal(err)
	}
	if gotArgv[len(gotArgv)-1] != "--check" || gotArgv[0] != filepath.Join(p.Bin, "lochat") {
		t.Errorf("check argv = %q", gotArgv)
	}
	if !strings.HasPrefix(stdout, "\nAgent skills — ") || !strings.Contains(stdout, "  alpha                    lo chat: injected (not linked)\n") {
		t.Errorf("stdout:\n%s", stdout)
	}

	rc = 1
	if _, _, err := runLo(t, NewRoot(p), "ai", "check"); !errors.Is(err, ErrHandled) {
		t.Errorf("rc 1: %v", err)
	}
	rc = 7
	runLo(t, NewRoot(p), "ai", "check")
	if len(exits) != 1 || exits[0] != 7 {
		t.Errorf("rc passthrough: %v", exits)
	}

	// Missing runtime: the chat error prints, then the skills still show.
	os.Remove(filepath.Join(p.Bin, "lochat"))
	t.Setenv("PATH", t.TempDir())
	stdout, stderr, err := runLo(t, NewRoot(p), "ai", "check")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "lochat binary not found") || !strings.Contains(stdout, "Agent skills") {
		t.Errorf("missing runtime: err=%v out=%q stderr=%q", err, stdout, stderr)
	}
}

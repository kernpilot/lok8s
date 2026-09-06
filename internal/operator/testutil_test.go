package operator

// testutil_test.go — hermetic test infrastructure. Every external tool
// (kubectl, clusterctl) runs through the fake runner and the lo driver is a
// fake; no real kubectl/kind/docker is ever invoked (live clusters exist on
// dev machines — hermeticity here is a safety property).
//
// The fake mirrors the bats stubs in tests/operator/hooks_test.bats: every
// call is appended to a log as "kubectl <args>" / "driver::<verb> <domain>"
// (the bats KLOG), so the bats assert_output --partial pins port over
// verbatim.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

// klog is the shared call log (bats: KLOG).
type klog struct {
	lines []string
	// stdins records the stdin handed to each `-f -` call, in order.
	stdins []string
}

func (l *klog) add(line string) { l.lines = append(l.lines, line) }

func (l *klog) text() string { return strings.Join(l.lines, "\n") + "\n" }

func (l *klog) has(sub string) bool { return strings.Contains(l.text(), sub) }

func (l *klog) matching(sub string) []string {
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

type fakeRunner struct {
	log *klog
	// handler answers an invocation after logging; nil = success, no output.
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	r.log.add(strings.Join(append([]string{c.Name}, c.Args...), " "))
	if c.Stdin != nil {
		raw, _ := io.ReadAll(c.Stdin)
		r.log.stdins = append(r.log.stdins, string(raw))
	}
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

func argv(c execx.Cmd) string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

func writeOut(c execx.Cmd, s string) {
	if c.Stdout != nil {
		io.WriteString(c.Stdout, s)
	}
}

// loKubectl is the bats lo_hook_load kubectl stub: the finalizers jsonpath
// answers the teardown finalizer, the re-list answers an empty list.
func loKubectl(c execx.Cmd) error {
	a := argv(c)
	switch {
	case strings.Contains(a, "jsonpath={.metadata.finalizers}"):
		writeOut(c, `["lok8s.dev/lo-teardown"]`)
	case strings.HasPrefix(a, "kubectl get lo -A -o json"):
		writeOut(c, `{"items":[]}`)
	}
	return nil
}

// capiKubectl is the bats capi_hook_load stub.
func capiKubectl(c execx.Cmd) error {
	a := argv(c)
	switch {
	case strings.Contains(a, "jsonpath={.metadata.finalizers}"):
		writeOut(c, `["lok8s.dev/capi-teardown"]`)
	case strings.HasPrefix(a, "kubectl get capi -A -o json"):
		writeOut(c, `{"items":[]}`)
	}
	return nil
}

// fakeDriver is the bats driver::* stub set: every verb logs, status and
// the destroy outcome are settable.
type fakeDriver struct {
	log       *klog
	paths     *config.Paths
	status    string
	provision error
	destroy   error
	// kubeconfigName is the file the stub driver::kubeconfig writes
	// (bats: .kubeconfig/test-lo.yaml).
	kubeconfigName string
}

func (d *fakeDriver) Provision(ctx context.Context, domain string) error {
	d.log.add("driver::provision " + domain)
	return d.provision
}

func (d *fakeDriver) Destroy(ctx context.Context, domain string) error {
	d.log.add("driver::destroy " + domain)
	return d.destroy
}

func (d *fakeDriver) Status(ctx context.Context, domain string) (string, error) {
	d.log.add("driver::status " + domain)
	return d.status, nil
}

func (d *fakeDriver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	d.log.add("driver::kubeconfig " + domain)
	dir := filepath.Join(d.paths.Base, ".kubeconfig")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, d.kubeconfigName+".yaml")
	return path, os.WriteFile(path, []byte("kc\n"), 0o600)
}

// loFixture is one lo-reconcile test's world.
type loFixture struct {
	hook   *LoHook
	log    *klog
	runner *fakeRunner
	drv    *fakeDriver
	paths  *config.Paths
	stderr *bytes.Buffer
	stdout *bytes.Buffer
}

func newLoFixture(t *testing.T) *loFixture {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	env := &Env{HookDir: filepath.Join(t.TempDir(), "hooks"), StateDir: state}
	p := env.Paths()
	log := &klog{}
	runner := &fakeRunner{log: log, handler: loKubectl}
	drv := &fakeDriver{log: log, paths: p, status: "NotFound", kubeconfigName: "test-lo"}
	var stderr, stdout bytes.Buffer
	hook := &LoHook{
		Paths: p, Runner: runner, Stdout: &stdout, Stderr: &stderr,
		Drivers: func(name string) (driver.Factory, bool) {
			if name != "lo" {
				return nil, false
			}
			return func(*driver.Deps) (driver.Driver, error) { return drv, nil }, true
		},
		BootstrapApply: func(ctx context.Context, domain, clusterYAML, kubeconfig string) error {
			log.add("bootstrap::apply " + domain + " " + clusterYAML + " " + kubeconfig)
			return nil
		},
	}
	return &loFixture{hook: hook, log: log, runner: runner, drv: drv, paths: p, stderr: &stderr, stdout: &stdout}
}

// capiFixture is one capi-reconcile test's world.
type capiFixture struct {
	hook   *CapiHook
	log    *klog
	runner *fakeRunner
	stderr *bytes.Buffer
	stdout *bytes.Buffer
	tmpl   string
}

func newCapiFixture(t *testing.T) *capiFixture {
	t.Helper()
	env := &Env{HookDir: filepath.Join(t.TempDir(), "hooks"), StateDir: filepath.Join(t.TempDir(), "state")}
	log := &klog{}
	runner := &fakeRunner{log: log, handler: capiKubectl}
	var stderr, stdout bytes.Buffer
	hook := &CapiHook{
		Paths: env.Paths(), Runner: runner, Stdout: &stdout, Stderr: &stderr,
		TemplateDir: env.CapiTemplateDir(),
	}
	return &capiFixture{hook: hook, log: log, runner: runner, stderr: &stderr, stdout: &stdout, tmpl: env.CapiTemplateDir()}
}

// writeTemplates lays down a synthetic capi-templates tree (the real one
// lives only in the container image, as the bats note says).
func (f *capiFixture) writeTemplates(t *testing.T, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		writeFile(t, filepath.Join(f.tmpl, rel), content)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// bindingFile writes a binding context file and returns its path.
func bindingFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "binding.json")
	writeFile(t, path, content)
	return path
}

func mustEvents(t *testing.T, content string) []Event {
	t.Helper()
	events, err := ReadBindingContext(io.Discard, bindingFile(t, content))
	if err != nil {
		t.Fatalf("binding context: %v", err)
	}
	return events
}

func assertHas(t *testing.T, log *klog, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !log.has(sub) {
			t.Errorf("KLOG missing %q:\n%s", sub, log.text())
		}
	}
}

func refuteHas(t *testing.T, log *klog, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if log.has(sub) {
			t.Errorf("KLOG must not contain %q:\n%s", sub, log.text())
		}
	}
}

func assertStderr(t *testing.T, buf *bytes.Buffer, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(buf.String(), sub) {
			t.Errorf("stderr missing %q:\n%s", sub, buf.String())
		}
	}
}

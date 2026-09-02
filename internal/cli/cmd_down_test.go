package cli

// cmd_down_test.go — the Go twin of tests/unit/lo_down_routing_test.bats
// (`lo down` must not reach driver-destroy on a malformed spec) plus the
// local-path branches and main::clean's routing. Every side effect is
// recorded instead of performed: the dispatch, Tilt and every kind/docker
// invocation go through fakes — live kind clusters exist on dev machines.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

// scriptRunner is a fake execx.Runner with a scripted answer per argv.
type scriptRunner struct {
	calls   []string
	handler func(c execx.Cmd) error
}

func (r *scriptRunner) Run(_ context.Context, c execx.Cmd) error {
	r.calls = append(r.calls, strings.Join(append([]string{c.Name}, c.Args...), " "))
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

func (r *scriptRunner) has(sub string) bool {
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

type downHarness struct {
	deps    downDeps
	runner  *scriptRunner
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	acted   []string
	destroy error
}

func newDownHarness(t *testing.T) *downHarness {
	t.Helper()
	p := synthProject(t)
	h := &downHarness{runner: &scriptRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	h.deps = downDeps{
		paths:  p,
		runner: h.runner,
		out:    h.out,
		stderr: h.errOut,
		dispatchDestroy: func(_ context.Context, d string) error {
			h.acted = append(h.acted, "driver-destroy "+d)
			return h.destroy
		},
		tiltDown: func(context.Context) error { h.acted = append(h.acted, "tilt-down"); return nil },
	}
	return h
}

func (h *downHarness) acted_(s string) bool {
	for _, a := range h.acted {
		if a == s {
			return true
		}
	}
	return false
}

func (h *downHarness) spec(t *testing.T, body string) {
	t.Helper()
	writeFile(t, filepath.Join(h.deps.paths.Clusters, "test.dev", "cluster.lok8s.yaml"), body)
}

// bats: "a cloud spec DOES reach driver-destroy" — the positive control.
func TestDownCloudSpecReachesDriverDestroy(t *testing.T) {
	h := newDownHarness(t)
	h.spec(t, "apiVersion: cluster.lok8s.dev/v1beta1\nkind: KubeOne\nmetadata:\n  name: prod\n")
	if err := runDown(context.Background(), h.deps, "test.dev", "test-down"); err != nil {
		t.Fatal(err)
	}
	if !h.acted_("driver-destroy test.dev") || h.acted_("tilt-down") {
		t.Errorf("acted = %v", h.acted)
	}
	if !strings.Contains(h.out.String(), "• destroying kubeone cluster via its driver") {
		t.Errorf("stdout = %q", h.out.String())
	}
}

// bats: "a spec with no .kind must NOT reach driver-destroy" — THE gate.
// Pre-fix this spec produced kind="null", which passed `[[ -n … && != lo ]]`
// and deprovisioned infrastructure on a malformed file.
func TestDownSpecWithoutKindRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"absent":    "apiVersion: cluster.lok8s.dev/v1beta1\nmetadata:\n  name: prod\nspec:\n  kubernetes:\n    version: \"1.31.0\"\n",
		"traversal": "apiVersion: cluster.lok8s.dev/v1beta1\nkind: ../../evil\nmetadata:\n  name: prod\n",
		"empty":     "apiVersion: cluster.lok8s.dev/v1beta1\nkind: \"\"\nmetadata:\n  name: prod\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newDownHarness(t)
			h.spec(t, body)
			err := runDown(context.Background(), h.deps, "test.dev", "test-down")
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("err = %v", err)
			}
			// Neither teardown is the right one: the spec exists but does
			// not say what it is.
			if len(h.acted) != 0 || len(h.runner.calls) != 0 {
				t.Errorf("acted=%v calls=%v", h.acted, h.runner.calls)
			}
			if !strings.Contains(h.out.String(), "✗ cannot read the driver from") || !strings.Contains(h.out.String(), "refusing to tear down") {
				t.Errorf("stdout = %q", h.out.String())
			}
			if !strings.Contains(h.errOut.String(), "[error]") {
				t.Errorf("read_kind's own diagnostic missing: %q", h.errOut.String())
			}
		})
	}
}

// bats: "a Lo spec takes the local teardown" + the registry branches.
func TestDownLoSpecLocalTeardown(t *testing.T) {
	h := newDownHarness(t)
	h.spec(t, "kind: Lo\nmetadata:\n  name: dev\n")
	writeFile(t, filepath.Join(h.deps.paths.Clusters, "test.dev", ".registries.json"),
		`{"shared": false, "project_network": "devnet", "registries": [{"name": "build"}, {"name": "cache"}, {"name": ""}]}`)
	h.runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && c.Args[0] == "get" {
			c.Stdout.Write([]byte("other\ndev\n"))
		}
		if c.Name == "docker" && strings.HasSuffix(strings.Join(c.Args, " "), "devnet-registry-cache") {
			return errors.New("no such container")
		}
		return nil
	}
	if err := runDown(context.Background(), h.deps, "test.dev", "dev"); err != nil {
		t.Fatal(err)
	}
	if !h.acted_("tilt-down") || h.acted_("driver-destroy test.dev") {
		t.Errorf("acted = %v", h.acted)
	}
	if !h.runner.has("kind delete cluster --name dev") {
		t.Errorf("kind delete missing: %v", h.runner.calls)
	}
	if !h.runner.has("docker rm -f devnet-registry-build") || !h.runner.has("docker rm -f devnet-registry-cache") {
		t.Errorf("registry containers not removed: %v", h.runner.calls)
	}
	out := h.out.String()
	for _, want := range []string{"  \033[1;36mdev\033[0m  \033[2mtearing down\033[0m", "• stopping Tilt", "• deleting kind cluster", "• shut down 1 registry containers (not shared — nothing to reuse; volumes kept)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestDownSharedRegistriesLeftUp(t *testing.T) {
	h := newDownHarness(t)
	h.spec(t, "kind: Lo\nmetadata:\n  name: dev\n")
	writeFile(t, filepath.Join(h.deps.paths.Clusters, "test.dev", ".registries.json"),
		`{"shared": true, "project_network": "devnet", "registries": [{"name": "build"}, {"name": "io-docker"}]}`)
	if err := runDown(context.Background(), h.deps, "test.dev", "dev"); err != nil {
		t.Fatal(err)
	}
	if h.runner.has("docker rm") {
		t.Errorf("shared mirrors must be left up: %v", h.runner.calls)
	}
	out := h.out.String()
	for _, want := range []string{"• kind cluster not running", "ℹ registries left up: 2 containers", "remove:     lo registry down", "+ volumes:  lo registry clean --shared"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// bats: "no cluster spec at all takes the local teardown" — an unprovisioned
// or deploy-only domain has only a kind cluster to remove.
func TestDownNoSpecTakesLocalPath(t *testing.T) {
	h := newDownHarness(t)
	writeFile(t, filepath.Join(h.deps.paths.Clusters, "test.dev", "deploy.lok8s.yaml"), "kind: Deploy\nspec:\n  clusterRef:\n    domain: other\n")
	if err := runDown(context.Background(), h.deps, "test.dev", "local"); err != nil {
		t.Fatal(err)
	}
	if !h.acted_("tilt-down") || h.acted_("driver-destroy test.dev") {
		t.Errorf("acted = %v", h.acted)
	}
	if !h.runner.has("kind get clusters") || h.runner.has("kind delete") {
		t.Errorf("calls = %v", h.runner.calls)
	}
}

// A gate DECLINE is a silent rc 1 (nothing ran, nothing is orphaned); any
// other driver failure prints the orphaned-infra warning.
func TestDownDriverDestroyOutcomes(t *testing.T) {
	h := newDownHarness(t)
	h.spec(t, "kind: Kkp\nmetadata:\n  name: prod\n")
	h.destroy = driver.ErrDeclined
	if err := runDown(context.Background(), h.deps, "test.dev", "prod"); !errors.Is(err, ErrHandled) {
		t.Fatalf("decline: err = %v", err)
	}
	if strings.Contains(h.out.String(), "driver destroy failed") {
		t.Errorf("a decline must not print the orphan warning:\n%s", h.out.String())
	}

	h = newDownHarness(t)
	h.spec(t, "kind: Kkp\nmetadata:\n  name: prod\n")
	h.destroy = &driver.ExitError{Code: 1, Err: errors.New("curl exit 3, remapped")}
	if err := runDown(context.Background(), h.deps, "test.dev", "prod"); !errors.Is(err, ErrHandled) {
		t.Fatalf("failure: err = %v", err)
	}
	if !strings.Contains(h.out.String(), "✗ driver destroy failed — infrastructure may still exist; inspect and re-run") {
		t.Errorf("orphan warning missing:\n%s", h.out.String())
	}
}

// ── lo clean ──────────────────────────────────────────────────────────

func cleanHarness(t *testing.T, h *downHarness) (cleanDeps, *[]string) {
	t.Helper()
	acted := &[]string{}
	return cleanDeps{
		downDeps: h.deps,
		registryClean: func(_ context.Context, d string) error {
			*acted = append(*acted, "registry-clean "+d)
			return nil
		},
	}, acted
}

func TestCleanAllPrunes(t *testing.T) {
	h := newDownHarness(t)
	h.spec(t, "kind: Lo\nmetadata:\n  name: dev\n")
	deps, acted := cleanHarness(t, h)
	if err := runClean(context.Background(), deps, "test.dev", "dev", true); err != nil {
		t.Fatal(err)
	}
	if !h.runner.has("docker system prune -f") || h.runner.has("docker volume") || len(*acted) != 0 {
		t.Errorf("calls=%v acted=%v", h.runner.calls, *acted)
	}
}

func TestCleanVolumesAndRegistriesOnLoDomain(t *testing.T) {
	h := newDownHarness(t)
	h.spec(t, "kind: Lo\nmetadata:\n  name: dev\n")
	h.runner.handler = func(c execx.Cmd) error {
		if c.Name == "docker" && c.Args[0] == "volume" && c.Args[1] == "ls" {
			c.Stdout.Write([]byte("dev-data\ndev-cache\n"))
		}
		return nil
	}
	deps, acted := cleanHarness(t, h)
	if err := runClean(context.Background(), deps, "test.dev", "dev", false); err != nil {
		t.Fatal(err)
	}
	if !h.runner.has("docker volume ls --filter name=^dev- -q") || !h.runner.has("docker volume rm -f dev-data") || !h.runner.has("docker volume rm -f dev-cache") {
		t.Errorf("volume calls: %v", h.runner.calls)
	}
	if len(*acted) != 1 || (*acted)[0] != "registry-clean test.dev" {
		t.Errorf("acted = %v", *acted)
	}
	if h.runner.has("system prune") {
		t.Errorf("--all not given, must not prune: %v", h.runner.calls)
	}
}

func TestCleanSkipsRegistriesOffLoDomains(t *testing.T) {
	h := newDownHarness(t)
	writeFile(t, filepath.Join(h.deps.paths.Clusters, "test.dev", "deploy.lok8s.yaml"), "kind: Deploy\n")
	deps, acted := cleanHarness(t, h)
	if err := runClean(context.Background(), deps, "test.dev", "local", false); err != nil {
		t.Fatal(err)
	}
	if len(*acted) != 0 {
		t.Errorf("registry clean must not run for a deploy domain: %v", *acted)
	}
	if !strings.Contains(h.errOut.String(), "skipping registry cleanup — domain 'test.dev' is not a Lo cluster") {
		t.Errorf("warn missing: %q", h.errOut.String())
	}
}

func TestCleanStopsWhenDownRefuses(t *testing.T) {
	h := newDownHarness(t)
	h.spec(t, "metadata:\n  name: prod\n")
	deps, acted := cleanHarness(t, h)
	if err := runClean(context.Background(), deps, "test.dev", "prod", true); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if len(h.runner.calls) != 0 || len(*acted) != 0 {
		t.Errorf("nothing may run after a refused down: calls=%v acted=%v", h.runner.calls, *acted)
	}
}

func TestKindClusterListedExactMatch(t *testing.T) {
	r := &scriptRunner{handler: func(c execx.Cmd) error {
		c.Stdout.Write([]byte("kubehz-dev\nlocal\n"))
		return nil
	}}
	if !kindClusterListed(context.Background(), r, "local") || kindClusterListed(context.Background(), r, "loc") || kindClusterListed(context.Background(), r, "kubehz") {
		t.Error("grep -qx semantics: whole-line match only")
	}
	r = &scriptRunner{handler: func(c execx.Cmd) error { return errors.New("kind: not found") }}
	if kindClusterListed(context.Background(), r, "local") {
		t.Error("a failed kind lists nothing")
	}
}

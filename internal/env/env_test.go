package env

// env_test.go — hermetic tests for the env port, mirroring
// tests/unit/env_test.bats. yq runs through the fake runner (the bats suite
// stubbed yq/envsubst the same way: eval-all as a passthrough); the
// extraction + image-generation half is native and tested directly.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

type fakeRunner struct {
	calls   []string
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(_ context.Context, c execx.Cmd) error {
	r.calls = append(r.calls, strings.Join(append([]string{c.Name}, c.Args...), " "))
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

// yqPassthrough mirrors the bats mock: every yq invocation copies stdin to
// stdout.
func yqPassthrough(c execx.Cmd) error {
	if c.Name != "yq" {
		return fmt.Errorf("unexpected tool %s", c.Name)
	}
	if c.Stdin != nil && c.Stdout != nil {
		io.Copy(c.Stdout, c.Stdin)
	}
	return nil
}

func testCtx(t *testing.T) (*Context, *fakeRunner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	paths := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	runner := &fakeRunner{handler: yqPassthrough}
	var out, errOut bytes.Buffer
	c := &Context{Paths: paths, Runner: runner, Out: &out, ErrOut: &errOut, Domain: "test.example"}
	for _, v := range []string{"LOK8S_SERVICE_CONFIG", "DEBUG", "DOCKER_REGISTRY", "DOCKER_PROJECT", "DOCKER_TAG"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	return c, runner, &out, &errOut
}

const servicesFixture = `# Test fixture: services config
registry:
  prefix: "lok8s.local"
  branch: "test-project"
  tag: "latest"
services:
  api:
    enabled: true
    build: true
  web:
    enabled: true
    build: true
`

// ── env::services ────────────────────────────────────────

func TestServicesReadsBaseServicesYAML(t *testing.T) {
	c, _, out, _ := testCtx(t)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.yaml"), []byte(servicesFixture), 0o644)
	if err := c.Services(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"registry:", "services:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %q", want, out.String())
		}
	}
}

func TestServicesMergesConfigSpecificOverride(t *testing.T) {
	c, _, out, _ := testCtx(t)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.yaml"), []byte(servicesFixture), 0o644)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.local.yaml"), []byte("services:\n  api:\n    build: false\n"), 0o644)
	t.Setenv("LOK8S_SERVICE_CONFIG", "local")
	if err := c.Services(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	// Passthrough yq shows both parts plus the separator the merge input
	// carries (bats: assert_output --partial "---").
	if !strings.Contains(out.String(), "---") || !strings.Contains(out.String(), "build: false") {
		t.Errorf("output = %q", out.String())
	}
}

func TestServicesFallsBackToLegacyBaseFile(t *testing.T) {
	c, _, out, _ := testCtx(t)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.base.yaml"), []byte(servicesFixture), 0o644)
	if err := c.Services(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "services:") {
		t.Errorf("output = %q", out.String())
	}
}

func TestServicesEmptyConfigWhenNoFileExists(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	if err := c.Services(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{}\n" {
		t.Errorf("output = %q, want {}\\n", out.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("yq must not run for the empty-config path: %v", runner.calls)
	}
}

func TestServicesMergesLegacyDefaultFile(t *testing.T) {
	c, _, out, _ := testCtx(t)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.yaml"), []byte(servicesFixture), 0o644)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.default.yaml"),
		[]byte("services:\n  default-svc:\n    enabled: true\n    build: false\n"), 0o644)
	if err := c.Services(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "---") || !strings.Contains(out.String(), "default-svc") {
		t.Errorf("output = %q", out.String())
	}
}

func TestServicesOnlyFlagsRunTheExtractionStage(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.yaml"), []byte(servicesFixture), 0o644)
	if err := c.Services(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[1] != "yq .services" {
		t.Errorf("calls = %v", runner.calls)
	}
	runner.calls = nil
	if err := c.Services(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[1] != "yq .registry" {
		t.Errorf("calls = %v", runner.calls)
	}
}

func TestServicesAppliesBareEnvsubst(t *testing.T) {
	c, _, out, _ := testCtx(t)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.yaml"),
		[]byte("registry:\n  endpoint: ${MY_REG}/x\n  tag: $UNSET_VAR\n"), 0o644)
	t.Setenv("MY_REG", "ghcr.io/acme")
	if err := c.Services(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "endpoint: ghcr.io/acme/x") {
		t.Errorf("output = %q", out.String())
	}
	if strings.Contains(out.String(), "$UNSET_VAR") || !strings.Contains(out.String(), "tag: \n") {
		t.Errorf("undefined var must substitute to empty: %q", out.String())
	}
}

// ── BareEnvsubst — GNU gettext bare-mode semantics ───────

func TestBareEnvsubstGNUContract(t *testing.T) {
	t.Setenv("FOO", "foo")
	in := "a=${FOO} b=$BAR c=${X:-y} lit=$$ e=${FOO}bar f=${arr[0]}"
	// Verified against `envsubst (GNU gettext-runtime)`:
	want := "a=foo b= c=${X:-y} lit=$$ e=foobar f=${arr[0]}"
	if got := string(BareEnvsubst([]byte(in))); got != want {
		t.Errorf("BareEnvsubst = %q, want %q", got, want)
	}
}

// ── env::kustomization ───────────────────────────────────

// kustomizationFixture mirrors the bats "one builds locally, one uses
// registry, one pins an explicit image" merged env.
const kustomizationFixture = `registry:
  endpoint: ghcr.io/myorg
  branch: test-project
  tag: latest
  prefix: lok8s.local
defaults:
  build: false
services:
  api:
    build: true
  worker:
    build: false
  pinned:
    image: ghcr.io/external/pinned:v1.2.3
`

func kustomizationCtx(t *testing.T, merged string) (*Context, string) {
	t.Helper()
	c, _, _, _ := testCtx(t)
	os.WriteFile(filepath.Join(c.Paths.Base, "services.yaml"), []byte(merged), 0o644)
	c.BuildArtifacts = func() error { return nil }
	domainDir := filepath.Join(c.Paths.Clusters, c.Domain)
	os.MkdirAll(domainDir, 0o755)
	os.WriteFile(filepath.Join(domainDir, "artifacts.yaml"), []byte("# placeholder\n"), 0o644)
	return c, domainDir
}

func TestKustomizationWritesOverlayWithImageSwaps(t *testing.T) {
	c, domainDir := kustomizationCtx(t, kustomizationFixture)
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(domainDir, "artifacts", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"apiVersion: kustomize.config.k8s.io/v1beta1",
		"kind: Kustomization",
		"  - ../artifacts.yaml",
		// worker: build=false → registry swap goes through the on-cluster
		// cache (lok8s.cache), not directly to the remote registry.
		"  - name: lok8s.local/worker\n    newName: lok8s.cache/test-project/worker\n    newTag: \"latest\"",
		// pinned: explicit image bypasses the cache.
		"  - name: lok8s.local/pinned\n    newName: ghcr.io/external/pinned\n    newTag: \"v1.2.3\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kustomization missing %q:\n%s", want, got)
		}
	}
	// api: build=true → must NOT appear.
	if strings.Contains(got, "name: lok8s.local/api") {
		t.Errorf("api must not be swapped:\n%s", got)
	}
	// The remote ref is queued for `lo image cache`.
	queue, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", ".cache-queue"))
	if string(queue) != "worker\tghcr.io/myorg/test-project/worker:latest\ttest-project\tlatest\n" {
		t.Errorf(".cache-queue = %q", queue)
	}
}

func TestKustomizationEmptyImagesBlockWhenNoSwapsNeeded(t *testing.T) {
	c, domainDir := kustomizationCtx(t, `registry:
  endpoint: ghcr.io/myorg
  branch: test-project
  tag: latest
  prefix: lok8s.local
defaults:
  build: true
services:
  api: {}
  web: {}
`)
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", "kustomization.yaml"))
	got := string(raw)
	if !strings.Contains(got, "resources:\n  - ../artifacts.yaml") {
		t.Errorf("kustomization = %q", got)
	}
	if strings.Contains(got, "name: lok8s.local") || strings.Contains(got, "images:") {
		t.Errorf("no swaps expected:\n%s", got)
	}
}

func TestKustomizationEmptyResourcesWithoutDomainArtifact(t *testing.T) {
	c, domainDir := kustomizationCtx(t, kustomizationFixture)
	os.Remove(filepath.Join(domainDir, "artifacts.yaml"))
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", "kustomization.yaml"))
	if !strings.Contains(string(raw), "resources: []") {
		t.Errorf("kustomization = %q", raw)
	}
}

func TestKustomizationSkipsDisabledServices(t *testing.T) {
	c, domainDir := kustomizationCtx(t, `registry:
  endpoint: ghcr.io/myorg
  branch: p
  tag: t
defaults:
  build: false
services:
  off:
    enabled: false
  on: {}
`)
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", "kustomization.yaml"))
	if strings.Contains(string(raw), "off") {
		t.Errorf("disabled service leaked:\n%s", raw)
	}
	if !strings.Contains(string(raw), "name: lok8s.local/on") {
		t.Errorf("enabled service missing:\n%s", raw)
	}
}

func TestKustomizationDigestPin(t *testing.T) {
	c, domainDir := kustomizationCtx(t, `services:
  pinned:
    image: ghcr.io/x/y@sha256:abcdef
`)
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", "kustomization.yaml"))
	want := "  - name: lok8s.local/pinned\n    newName: ghcr.io/x/y\n    digest: \"sha256:abcdef\"\n"
	if !strings.Contains(string(raw), want) {
		t.Errorf("kustomization = %q, want fragment %q", raw, want)
	}
}

func TestKustomizationBarePinEmitsNewNameOnly(t *testing.T) {
	c, domainDir := kustomizationCtx(t, "services:\n  pinned:\n    image: busybox\n")
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", "kustomization.yaml"))
	want := "  - name: lok8s.local/pinned\n    newName: busybox\n"
	if !strings.Contains(string(raw), want) || strings.Contains(string(raw), "newTag") {
		t.Errorf("kustomization = %q", raw)
	}
}

func TestKustomizationMissingEndpointWarnsAndSkips(t *testing.T) {
	c, domainDir := kustomizationCtx(t, "services:\n  lost:\n    build: false\n")
	var errOut bytes.Buffer
	c.ErrOut = &errOut
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	want := "warn: service 'lost' has build:false but no registry.endpoint configured — skipping image swap (define registry.endpoint, set image:, or set build:true)\n"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr = %q", errOut.String())
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", "kustomization.yaml"))
	if strings.Contains(string(raw), "lost") {
		t.Errorf("skipped service leaked:\n%s", raw)
	}
}

func TestKustomizationEndpointDefaultsSubstituteFromEnv(t *testing.T) {
	c, domainDir := kustomizationCtx(t, "services:\n  svc:\n    build: false\n")
	t.Setenv("DOCKER_REGISTRY", "reg.example")
	t.Setenv("DOCKER_PROJECT", "proj")
	t.Setenv("DOCKER_TAG", "v9")
	if err := c.Kustomization(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	queue, _ := os.ReadFile(filepath.Join(domainDir, "artifacts", ".cache-queue"))
	if string(queue) != "svc\treg.example/proj/svc:v9\tproj\tv9\n" {
		t.Errorf(".cache-queue = %q", queue)
	}
}

func TestKustomizationPullDrainsNonEmptyQueue(t *testing.T) {
	c, _ := kustomizationCtx(t, kustomizationFixture)
	pulled := 0
	c.Pull = func() error { pulled++; return nil }
	if err := c.Kustomization(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	if pulled != 1 {
		t.Errorf("pulled = %d, want 1", pulled)
	}
}

func TestKustomizationPullSkipsEmptyQueue(t *testing.T) {
	c, _ := kustomizationCtx(t, "services:\n  api:\n    build: true\n")
	c.Pull = func() error {
		t.Fatal("Pull must not run for an empty queue")
		return nil
	}
	if err := c.Kustomization(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
}

func TestKustomizationPullFailureIsLoud(t *testing.T) {
	c, _ := kustomizationCtx(t, kustomizationFixture)
	var errOut bytes.Buffer
	c.ErrOut = &errOut
	c.Pull = func() error { return fmt.Errorf("boom") }
	if err := c.Kustomization(context.Background(), true, true); err != ErrHandled {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	if !strings.Contains(errOut.String(), "image::cache --all failed; check upstream credentials and network") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestKustomizationBuildFailureAborts(t *testing.T) {
	c, domainDir := kustomizationCtx(t, kustomizationFixture)
	c.BuildArtifacts = func() error { return fmt.Errorf("render failed") }
	if err := c.Kustomization(context.Background(), false, false); err == nil {
		t.Fatal("expected the build failure to abort")
	}
	if _, err := os.Stat(filepath.Join(domainDir, "artifacts", "kustomization.yaml")); !os.IsNotExist(err) {
		t.Error("kustomization must not be written after a failed build")
	}
}

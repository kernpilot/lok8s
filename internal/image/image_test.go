package image

// image_test.go — hermetic tests for the image port. ALL docker/network
// access runs through the fake runner / HTTPGet seam; no real daemon or
// registry is ever touched (live registry containers exist on dev machines —
// hermeticity is a safety property).

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

type fakeExit struct{ code int }

func (e *fakeExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *fakeExit) ExitCode() int { return e.code }

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

func (r *fakeRunner) matching(sub string) []string {
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
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
	runner := &fakeRunner{}
	var out, errOut bytes.Buffer
	c := &Context{Paths: paths, Runner: runner, Out: &out, ErrOut: &errOut, Domain: "dev.test"}
	os.MkdirAll(filepath.Join(paths.Clusters, "dev.test"), 0o755)
	for _, v := range []string{"LOK8S_REGISTRY_IP_CACHE", "LOK8S_REGISTRY_JSON", "DEBUG",
		"DOCKER_REGISTRY", "DOCKER_PROJECT", "DOCKER_TAG"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	return c, runner, &out, &errOut
}

func writeSpec(t *testing.T, c *Context, content string) {
	t.Helper()
	os.WriteFile(filepath.Join(c.Paths.Clusters, c.Domain, "cluster.lok8s.yaml"), []byte(content), 0o644)
}

func writeServices(t *testing.T, c *Context, content string) {
	t.Helper()
	os.WriteFile(filepath.Join(c.Paths.Base, "services.yaml"), []byte(content), 0o644)
}

// yqPassthrough answers env.Services' merge stage (stdin → stdout, like the
// bats yq mock).
func yqPassthrough(c execx.Cmd) error {
	if c.Name == "yq" && c.Stdin != nil && c.Stdout != nil {
		io.Copy(c.Stdout, c.Stdin)
	}
	return nil
}

const loSpec = "kind: Lo\nspec:\n  network:\n    name: devnet\n    cidr: 10.99.7.0/24\n"

// ── image::_registry_tls ─────────────────────────────────

func TestRegistryTLSReadsTheDomainsOwnFile(t *testing.T) {
	c, _, _, _ := testCtx(t)
	// issue #89: `--domain X` must pair X's registries.json, not the other
	// domain's.
	os.MkdirAll(filepath.Join(c.Paths.Clusters, "other.test"), 0o755)
	os.WriteFile(filepath.Join(c.Paths.Clusters, "dev.test", ".registries.json"), []byte(`{"tls": true}`), 0o644)
	os.WriteFile(filepath.Join(c.Paths.Clusters, "other.test", ".registries.json"), []byte(`{"tls": false}`), 0o644)
	if !c.registryTLS("dev.test") {
		t.Error("dev.test must read tls: true")
	}
	if c.registryTLS("other.test") {
		t.Error("other.test must read tls: false")
	}
}

func TestRegistryTLSEnvOverrideAndFallbacks(t *testing.T) {
	c, _, _, _ := testCtx(t)
	if c.registryTLS("dev.test") {
		t.Error("missing file must read as plain HTTP (back-compat default)")
	}
	override := filepath.Join(c.Paths.Base, "custom.json")
	os.WriteFile(override, []byte(`{"tls": true}`), 0o644)
	t.Setenv("LOK8S_REGISTRY_JSON", override)
	if !c.registryTLS("dev.test") {
		t.Error("LOK8S_REGISTRY_JSON override must win")
	}
	os.WriteFile(override, []byte("not json"), 0o644)
	if c.registryTLS("dev.test") {
		t.Error("invalid JSON must read as not-TLS (jq -e failure)")
	}
}

// ── cache IP resolution ──────────────────────────────────

func TestResolveCacheNetFromSpecCIDR(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	net := c.resolveCacheNet()
	if net.ip != "10.99.7.102" {
		t.Errorf("ip = %q, want 10.99.7.102 (base + cache offset 102)", net.ip)
	}
	if net.tls == nil || !*net.tls {
		t.Error("tls must default true when resolved from the spec")
	}
}

func TestResolveCacheNetSlotDefaults(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeSpec(t, c, "kind: Lo\nmetadata:\n  name: slot50\nspec:\n  cluster:\n    domain: 50.lok8s.dev\n")
	net := c.resolveCacheNet()
	if net.ip != "10.125.50.102" {
		t.Errorf("ip = %q, want 10.125.50.102 (slot default)", net.ip)
	}
}

func TestResolveCacheNetEnvOverrideSkipsSpec(t *testing.T) {
	c, _, _, _ := testCtx(t)
	t.Setenv("LOK8S_REGISTRY_IP_CACHE", "127.0.0.1:5001")
	net := c.resolveCacheNet()
	if net.ip != "127.0.0.1:5001" || net.tls != nil {
		t.Errorf("net = %+v", net)
	}
}

func TestResolveCacheNetFailsWithoutNetworkOnNonSlotDomain(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeSpec(t, c, "kind: Lo\n")
	if net := c.resolveCacheNet(); net.ip != "" {
		t.Errorf("ip = %q, want unresolved", net.ip)
	}
}

func TestResolveCacheNetSpecTLSFalse(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeSpec(t, c, loSpec+"  registries:\n    tls: false\n")
	net := c.resolveCacheNet()
	if net.tls == nil || *net.tls {
		t.Errorf("tls = %v, want false", net.tls)
	}
}

func TestResolveCacheNetInvalidTLSFailsLikeConfigGenerate(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeSpec(t, c, loSpec+"  registries:\n    tls: maybe\n")
	if net := c.resolveCacheNet(); net.ip != "" {
		t.Errorf("ip = %q, want unresolved (invalid spec.registries.tls)", net.ip)
	}
}

// ── image::cache — gates + error paths ───────────────────

func TestCacheRefusesNonLoDriver(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	writeSpec(t, c, "kind: KubeOne\n")
	err := c.Cache(context.Background(), "svc", false, false)
	if err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "error: domain 'dev.test' uses the 'kubeone' driver — the image cache is a 'lo'-driver (local cluster) feature.") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestCacheEnvOverrideSkipsTheDriverGate(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeSpec(t, c, "kind: KubeOne\n")
	runner.handler = yqPassthrough
	t.Setenv("LOK8S_REGISTRY_IP_CACHE", "127.0.0.1:5001")
	err := c.Cache(context.Background(), "", false, false)
	if err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	// Reaches the missing-service error, NOT the driver gate (issue #89's
	// documented escape for a shared cache).
	if !strings.Contains(errOut.String(), "image cache: provide a service name or use --all") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestCacheUnresolvableIPNamesTheEscapeHatch(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	writeSpec(t, c, "kind: Lo\n")
	err := c.Cache(context.Background(), "svc", false, false)
	if err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "cannot resolve the cache registry IP for domain 'dev.test' (spec.network unreadable?) — export LOK8S_REGISTRY_IP_CACHE=<ip> to override") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestCachePinnedServiceErrors(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeSpec(t, c, loSpec)
	writeServices(t, c, "services:\n  svc:\n    image: busybox:1.36\n")
	runner.handler = yqPassthrough
	err := c.Cache(context.Background(), "svc", false, false)
	if err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "service 'svc' has an explicit 'image:' pin — there is nothing to cache, kind pulls it directly") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if got := runner.matching("docker"); len(got) != 0 {
		t.Errorf("docker must not run: %v", got)
	}
}

func TestCacheMissingEndpointErrors(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeSpec(t, c, loSpec)
	writeServices(t, c, "services:\n  svc:\n    build: false\n")
	runner.handler = yqPassthrough
	err := c.Cache(context.Background(), "svc", false, false)
	if err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "service 'svc' has no registry.endpoint configured (set spec.registries.endpoint or services.svc.registry.endpoint)") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestCacheInvalidParallelErrors(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeSpec(t, c, loSpec)
	writeServices(t, c, "registry:\n  parallel: many\n")
	runner.handler = yqPassthrough
	err := c.Cache(context.Background(), "svc", false, false)
	if err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "registry.parallel must be a non-negative integer, got: many") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestCacheAllWithNoQueueIsANoop(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	runner.handler = yqPassthrough
	if err := c.Cache(context.Background(), "", false, true); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("docker"); len(got) != 0 {
		t.Errorf("docker must not run: %v", got)
	}
}

// ── image::_cache_one / queue processing ─────────────────

func seedQueue(t *testing.T, c *Context, content string) {
	t.Helper()
	dir := filepath.Join(c.Paths.Clusters, c.Domain, "artifacts")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".cache-queue"), []byte(content), 0o644)
}

func TestCacheAllPullsTagsAndPushes(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	seedQueue(t, c, "svc\thttps://ghcr.io/org/proj/svc:v1\tproj\tv1\n")
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Name == "yq" {
			return yqPassthrough(cmd)
		}
		if cmd.Name == "docker" && cmd.Args[0] == "manifest" {
			return &fakeExit{1} // not cached yet
		}
		return nil
	}
	if err := c.Cache(context.Background(), "", false, true); err != nil {
		t.Fatal(err)
	}
	// The scheme is stripped before docker sees the ref.
	wantCalls := []string{
		"docker manifest inspect 10.99.7.102/proj/svc:v1",
		"docker pull ghcr.io/org/proj/svc:v1",
		"docker tag ghcr.io/org/proj/svc:v1 10.99.7.102/proj/svc:v1",
		"docker push 10.99.7.102/proj/svc:v1",
	}
	got := runner.matching("docker")
	if strings.Join(got, "|") != strings.Join(wantCalls, "|") {
		t.Errorf("docker calls = %v, want %v", got, wantCalls)
	}
	if !strings.Contains(out.String(), ":: [ svc ] caching ghcr.io/org/proj/svc:v1 -> 10.99.7.102/proj/svc:v1") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestCacheOneInsecureInspectWhenPlainHTTP(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeSpec(t, c, loSpec+"  registries:\n    tls: false\n")
	seedQueue(t, c, "svc\tghcr.io/org/proj/svc:v1\tproj\tv1\n")
	runner.handler = yqPassthrough
	if err := c.Cache(context.Background(), "", false, true); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("manifest inspect"); len(got) != 1 || got[0] != "docker manifest inspect --insecure 10.99.7.102/proj/svc:v1" {
		t.Errorf("inspect calls = %v", got)
	}
}

func TestCacheOneSkipsWhenAlreadyCached(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	seedQueue(t, c, "svc\tghcr.io/org/proj/svc:v1\tproj\tv1\n")
	runner.handler = yqPassthrough // manifest inspect succeeds → cached
	if err := c.Cache(context.Background(), "", false, true); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("docker pull"); len(got) != 0 {
		t.Errorf("pull must not run when cached: %v", got)
	}
}

func TestCacheOneForceRepullsDespiteCache(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	seedQueue(t, c, "svc\tghcr.io/org/proj/svc:v1\tproj\tv1\n")
	runner.handler = yqPassthrough
	if err := c.Cache(context.Background(), "", true, true); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("manifest inspect"); len(got) != 0 {
		t.Errorf("--force must skip the idempotence probe: %v", got)
	}
	if got := runner.matching("docker pull"); len(got) != 1 {
		t.Errorf("pull calls = %v", got)
	}
}

func TestCacheQueueFailureAggregation(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeSpec(t, c, loSpec)
	seedQueue(t, c, "good\tr/p/good:v1\tp\tv1\nbad\tr/p/bad:v1\tp\tv1\n")
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Name == "yq" {
			return yqPassthrough(cmd)
		}
		if cmd.Name == "docker" && cmd.Args[0] == "manifest" {
			return &fakeExit{1}
		}
		if cmd.Name == "docker" && cmd.Args[0] == "pull" && strings.Contains(cmd.Args[1], "bad") {
			return &fakeExit{1}
		}
		return nil
	}
	err := c.Cache(context.Background(), "", false, true)
	if err != ErrHandled {
		t.Fatalf("err = %v (a failed pull must fail the run)", err)
	}
	if !strings.Contains(errOut.String(), "image cache: 1/2 images failed: bad:v1") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "[ bad ] failed to pull r/p/bad:v1 (check upstream credentials)") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestCacheQueueDropsUnterminatedFinalLine(t *testing.T) {
	// bash quirk pair: `grep -cv '^$'` counts the unterminated line, the
	// `while read` loop never processes it.
	c, runner, _, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	seedQueue(t, c, "one\tr/p/one:v1\tp\tv1\ntwo\tr/p/two:v1\tp\tv1")
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Name == "yq" {
			return yqPassthrough(cmd)
		}
		if cmd.Name == "docker" && cmd.Args[0] == "manifest" {
			return &fakeExit{1}
		}
		return nil
	}
	if err := c.Cache(context.Background(), "", false, true); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("docker pull"); len(got) != 1 || got[0] != "docker pull r/p/one:v1" {
		t.Errorf("pull calls = %v (the unterminated final line must be dropped)", got)
	}
}

// ── image::list ──────────────────────────────────────────

func TestListRunsTheCurlJqPipeline(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	runner.handler = func(cmd execx.Cmd) error {
		switch cmd.Name {
		case "curl":
			if cmd.Args[0] != "-s" || cmd.Args[1] != "https://10.99.7.102/v2/_catalog" {
				t.Errorf("curl args = %v", cmd.Args)
			}
			fmt.Fprint(cmd.Stdout, `{"repositories":["p/one"]}`)
			return nil
		case "jq":
			// jq's stdin is the curl body; its stdout is the user's terminal.
			buf := make([]byte, 4096)
			n, _ := cmd.Stdin.Read(buf)
			if string(buf[:n]) != `{"repositories":["p/one"]}` {
				t.Errorf("jq stdin = %q", buf[:n])
			}
			fmt.Fprint(cmd.Stdout, "{\n  \"repositories\": [\n    \"p/one\"\n  ]\n}\n")
			return nil
		}
		return nil
	}
	rc, err := c.List(context.Background())
	if err != nil || rc != 0 {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	want := ":: cache registry @ https://10.99.7.102/v2/_catalog\n" +
		"{\n  \"repositories\": [\n    \"p/one\"\n  ]\n}\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if got := runner.matching("curl"); len(got) != 1 {
		t.Errorf("curl calls = %v (success fetches once)", got)
	}
}

func TestListPlainHTTPWithoutTLS(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	writeSpec(t, c, loSpec+"  registries:\n    tls: false\n")
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ":: cache registry @ http://10.99.7.102/v2/_catalog") {
		t.Errorf("stdout = %q", out.String())
	}
	if got := runner.matching("curl -s http://10.99.7.102/v2/_catalog"); len(got) != 1 {
		t.Errorf("curl calls = %v", runner.calls)
	}
}

func TestListDeadEndpointReturnsCurlsCode(t *testing.T) {
	// bash: pipefail makes `curl -s | jq .` fail with curl's code (7 on
	// connection-refused), the raw re-fetch fails the same way, and the
	// function's status is curl's — with NOTHING printed past the header.
	c, runner, out, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Name == "curl" {
			return &fakeExit{7}
		}
		return nil // jq on the empty stream succeeds
	}
	rc, err := c.List(context.Background())
	if err != nil || rc != 7 {
		t.Fatalf("rc=%d err=%v, want curl's 7", rc, err)
	}
	if out.String() != ":: cache registry @ https://10.99.7.102/v2/_catalog\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if got := runner.matching("curl"); len(got) != 2 {
		t.Errorf("curl calls = %v (bash re-fetches on pipeline failure)", got)
	}
}

func TestListNonJSONBodyRefetchesAndPrintsRaw(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	writeSpec(t, c, loSpec)
	runner.handler = func(cmd execx.Cmd) error {
		switch cmd.Name {
		case "curl":
			fmt.Fprint(cmd.Stdout, "<html>404</html>")
			return nil
		case "jq":
			return &fakeExit{5} // not JSON
		}
		return nil
	}
	rc, err := c.List(context.Background())
	if err != nil || rc != 0 {
		t.Fatalf("rc=%d err=%v (the re-fetch succeeded)", rc, err)
	}
	if got := runner.matching("curl"); len(got) != 2 {
		t.Errorf("curl calls = %v, want 2 (the bash curls twice)", got)
	}
	if !strings.Contains(out.String(), "<html>404</html>") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestListRefusesNonLoDriverUnlessOverridden(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	writeSpec(t, c, "kind: KubeOne\n")
	if _, err := c.List(context.Background()); err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "the image cache is a 'lo'-driver (local cluster) feature.") {
		t.Errorf("stderr = %q", errOut.String())
	}
	// The override makes the gate unreachable (issue #89).
	c2, _, out2, _ := testCtx(t)
	writeSpec(t, c2, "kind: KubeOne\n")
	t.Setenv("LOK8S_REGISTRY_IP_CACHE", "127.0.0.1:5001")
	if _, err := c2.List(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out2.String(), ":: cache registry @ http://127.0.0.1:5001/v2/_catalog") {
		t.Errorf("stdout = %q", out2.String())
	}
}

// ── image::clean ─────────────────────────────────────────

func TestCleanDropsContainerAndVolume(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	if err := c.Clean(context.Background(), "devnet"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker rm -f devnet-registry-cache",
		"docker volume rm -f devnet-registry-cache",
	}
	if strings.Join(runner.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v", runner.calls)
	}
	for _, line := range []string{
		":: dropping cache registry volume (devnet-registry-cache)",
		":: run 'lo registry up --cache' or 'lo provision' to recreate",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("stdout missing %q: %q", line, out.String())
		}
	}
}

func TestCleanFailuresAreTolerated(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	runner.handler = func(execx.Cmd) error { return &fakeExit{1} }
	if err := c.Clean(context.Background(), ""); err != nil {
		t.Fatalf("err = %v (bash: `|| true`)", err)
	}
	if runner.calls[0] != "docker rm -f lok8s-registry-cache" {
		t.Errorf("calls = %v (default network lok8s)", runner.calls)
	}
}

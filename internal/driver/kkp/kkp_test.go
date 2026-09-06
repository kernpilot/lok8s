package kkp

// kkp_test.go — the port of tests/unit/kkp_test.bats (URL validation,
// credential validation, the api client incl. CA/redaction/429, the
// health-derived status words, the payload builders pinned against the
// jq-built goldens) and the kkp half of kkp_capi_destroy_guards_test.bats.
//
// GOLDEN PROVENANCE: testdata/kkp_payloads.golden is the byte-exact stdout
// of the BASH payload builders,
//
//	source .lok8s/{utils/{verbose,http,spec,credentials}.sh,
//	       drivers/kkp/{api,main}}
//	HCLOUD_TOKEN=tok-abc
//	_build_cluster_json test-kkp-cluster v1.29.2 hetzner-fsn1 hetzner \
//	    tests/fixtures/kkp-cluster.lok8s.yaml
//	_build_cloud_spec byo … "" ; _build_cloud_spec hetzner … my-preset
//	_build_machinedeployment_json pool-1 3 cpx31 ubuntu hetzner 1 10
//	_build_machinedeployment_json pool-a 2 t3.large flatcar aws 0 0
//
// captured read-only. Regenerate only from the bash — bash wins.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

// ── harness ───────────────────────────────────────────────

type fakeRunner struct {
	calls   []execx.Cmd
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	r.calls = append(r.calls, c)
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

func argvLine(c execx.Cmd) string { return c.Name + " " + strings.Join(c.Args, " ") }

// curlRespond scripts curl's captured stream: body then the write-out line
// (`\n%{http_code}`), both into the shared 2>&1 buffer.
func curlRespond(body string, code string) func(c execx.Cmd) error {
	return func(c execx.Cmd) error {
		fmt.Fprintf(c.Stdout, "%s\n%s", body, code)
		return nil
	}
}

func testDriver(t *testing.T) (*Driver, *fakeRunner, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	runner := &fakeRunner{}
	var stderr bytes.Buffer
	paths := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	d := New(&driver.Deps{Paths: paths, Runner: runner, Stderr: &stderr})
	// Deterministic clock: the wall advances only through the sleep seam
	// (the bash loops measured `date +%s` in real time; the fake keeps the
	// same arithmetic without the waiting).
	clock := time.Unix(0, 0)
	d.now = func() time.Time { return clock }
	d.sleep = func(dur time.Duration) { clock = clock.Add(dur) }
	return d, runner, &stderr
}

func writeSpec(t *testing.T, d *Driver, domain, yaml string) string {
	t.Helper()
	dir := filepath.Join(d.deps.Paths.Clusters, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cluster.lok8s.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func kkpFixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "tests", "fixtures", "kkp-cluster.lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func setKKPEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KKP_TOKEN", "test-kkp-token-abc123")
	t.Setenv("KKP_API_URL", "https://kkp.test.example.com")
	t.Setenv("KKP_CA_CERT", "")
	os.Unsetenv("KKP_CA_CERT")
	t.Setenv("KKP_RETRY_DELAY", "0")
	t.Setenv("KKP_WAIT_INTERVAL", "1")
}

const allUpHealth = `{"apiserver":"HealthStatusUp","etcd":"HealthStatusUp","controller":"HealthStatusUp","scheduler":"HealthStatusUp","machineController":"HealthStatusDown"}`

// ── validate_url ──────────────────────────────────────────

func TestValidateURL(t *testing.T) {
	d, _, stderr := testDriver(t)
	if err := d.validateURL("https://kkp.example.com", stderr); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://kkp.example.com", "ftp://kkp.example.com", ""} {
		stderr.Reset()
		if err := d.validateURL(bad, stderr); err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if !strings.Contains(stderr.String(), "must use HTTPS") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	}
}

// ── validate_credentials ──────────────────────────────────

func TestValidateCredentialsHappy(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("HCLOUD_TOKEN", "test-hcloud-token")
	d, _, _ := testDriver(t)
	if err := d.validateCredentials(kkpFixture(t)); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCredentialsMissingToken(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("KKP_TOKEN", "")
	t.Setenv("HCLOUD_TOKEN", "test-hcloud-token")
	d, _, stderr := testDriver(t)
	if err := d.validateCredentials(kkpFixture(t)); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "KKP_TOKEN env var is required for KKP API authentication") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestValidateCredentialsMissingHcloudToken(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("HCLOUD_TOKEN", "")
	d, _, stderr := testDriver(t)
	if err := d.validateCredentials(kkpFixture(t)); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "required environment variable HCLOUD_TOKEN is not set") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestValidateCredentialsPresetSkipsProviderCheck(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("HCLOUD_TOKEN", "")
	d, _, _ := testDriver(t)
	spec := writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: test-kkp-preset}
spec:
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "test-project-123"
    datacenter: "hetzner-fsn1"
    preset: "hetzner-default"
  provider: hetzner
`)
	if err := d.validateCredentials(spec); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCredentialsRejectsHTTPURL(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("KKP_API_URL", "http://kkp.insecure.example.com")
	t.Setenv("HCLOUD_TOKEN", "x")
	d, _, stderr := testDriver(t)
	if err := d.validateCredentials(kkpFixture(t)); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "HTTPS") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestValidateCredentialsByoNeedsNoCloudCreds(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("HCLOUD_TOKEN", "")
	d, _, _ := testDriver(t)
	spec := writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: test-kkp-byo}
spec:
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "test-project-123"
    datacenter: "byo-local"
  provider: {name: byo}
`)
	if err := d.validateCredentials(spec); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCredentialsScalarProviderShape(t *testing.T) {
	// `provider: hetzner` (bare scalar) must behave like
	// `provider.name: hetzner`.
	setKKPEnv(t)
	t.Setenv("HCLOUD_TOKEN", "")
	d, _, stderr := testDriver(t)
	spec := writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: test-kkp-scalar}
spec:
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "test-project-123"
    datacenter: "hetzner-fsn1"
  provider: hetzner
`)
	if err := d.validateCredentials(spec); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "HCLOUD_TOKEN") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestValidateCredentialsExportsCACertFromSpec(t *testing.T) {
	setKKPEnv(t)
	d, _, _ := testDriver(t)
	dir := filepath.Join(d.deps.Paths.Clusters, "test.dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "myca.crt")
	if err := os.WriteFile(ca, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	spec := writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: t}
spec:
  kkp:
    apiUrl: https://kkp.test.example.com
    caCert: myca.crt
  provider: {name: byo}
`)
	if err := d.validateCredentials(spec); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("KKP_CA_CERT"); got != ca {
		t.Fatalf("KKP_CA_CERT = %q, want %q", got, ca)
	}
}

func TestValidateCredentialsRejectsMissingCACertFile(t *testing.T) {
	setKKPEnv(t)
	d, _, stderr := testDriver(t)
	spec := writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: t}
spec:
  kkp:
    apiUrl: https://kkp.test.example.com
    caCert: /nope/missing-ca.crt
  provider: {name: byo}
`)
	if err := d.validateCredentials(spec); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "spec.kkp.caCert points to a missing file: /nope/missing-ca.crt") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// ── api client ────────────────────────────────────────────

func TestAPIFailsWithoutToken(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("KKP_TOKEN", "")
	d, runner, stderr := testDriver(t)
	if _, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "KKP_TOKEN is not set") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatal("curl must never run without a token")
	}
}

func TestAPIFailsWithoutURL(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("KKP_API_URL", "")
	d, _, stderr := testDriver(t)
	if _, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "KKP_API_URL is not set") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestAPIRejectsHTTPURL(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("KKP_API_URL", "http://kkp.insecure.example.com")
	d, runner, stderr := testDriver(t)
	if _, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "must use HTTPS") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatal("curl must never run against a plain-http URL")
	}
}

func TestAPICurlArgvExact(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{"ok": true}`, "200")
	body, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr)
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"ok": true}` {
		t.Fatalf("body = %q", body)
	}
	want := []string{
		"--silent", "--show-error", "--fail-with-body", "--location",
		"--header", "Authorization: Bearer test-kkp-token-abc123",
		"--header", "Content-Type: application/json",
		"--header", "Accept: application/json",
		"--write-out", "\n%{http_code}",
		"--request", "GET",
		"https://kkp.test.example.com/api/v2/dc",
	}
	got := runner.calls[0]
	if got.Name != "curl" {
		t.Fatalf("name = %q", got.Name)
	}
	if strings.Join(got.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("curl argv:\n got %q\nwant %q", got.Args, want)
	}
}

func TestAPIPassesCACert(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KKP_CA_CERT", ca)
	runner.handler = curlRespond(`{"ok": true}`, "200")
	if _, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(argvLine(runner.calls[0]), "--cacert "+ca) {
		t.Fatalf("argv = %q", argvLine(runner.calls[0]))
	}
}

func TestAPIRejectsMissingCACertFile(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	t.Setenv("KKP_CA_CERT", filepath.Join(t.TempDir(), "does-not-exist.crt"))
	if _, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "not a readable file") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatal("curl must never be reached with a bogus KKP_CA_CERT")
	}
}

func TestAPIDebugRedactsToken(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("DEBUG", "1")
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{"ok": true}`, "200")
	if _, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr); err != nil {
		t.Fatal(err)
	}
	out := stderr.String()
	if strings.Contains(out, "test-kkp-token-abc123") {
		t.Fatal("token leaked into debug output")
	}
	if !strings.Contains(out, "<redacted>") {
		t.Fatalf("stderr = %q", out)
	}
}

func TestAPIRetriesOn429(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	count := 0
	runner.handler = func(c execx.Cmd) error {
		count++
		if count < 2 {
			return curlRespond(`{"error": "rate limited"}`, "429")(c)
		}
		return curlRespond(`{"ok": true}`, "200")(c)
	}
	body, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr)
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"ok": true}` {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(stderr.String(), "KKP API rate limited (429), retrying") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestAPIRateLimitExhaustsRetries(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{"error": "rate limited"}`, "429")
	if _, err := d.api(context.Background(), "GET", "/api/v2/dc", "", stderr); err == nil {
		t.Fatal("expected error")
	}
	if len(runner.calls) != 3 {
		t.Fatalf("curl ran %d times, want KKP_MAX_RETRIES=3", len(runner.calls))
	}
	if !strings.Contains(stderr.String(), "KKP API: max retries (3) exhausted for GET /api/v2/dc") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestAPIErrorsOn4xx(t *testing.T) {
	setKKPEnv(t)
	d, _, stderr := testDriver(t)
	dRunner := d.deps.Runner.(*fakeRunner)
	dRunner.handler = curlRespond(`{"error": "not found"}`, "404")
	if _, err := d.api(context.Background(), "GET", "/api/v2/projects/bad/clusters/bad", "", stderr); err == nil {
		t.Fatal("expected error")
	}
	out := stderr.String()
	if !strings.Contains(out, "KKP API error: GET /api/v2/projects/bad/clusters/bad -> HTTP 404") {
		t.Fatalf("stderr = %q", out)
	}
	if !strings.Contains(out, `Response: {"error": "not found"}`) {
		t.Fatalf("stderr = %q", out)
	}
	if len(dRunner.calls) != 1 {
		t.Fatalf("4xx must not be retried (curl ran %d times)", len(dRunner.calls))
	}
}

// ── create/delete/kubeconfig/md ───────────────────────────

func TestCreateClusterReturnsID(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	runner.handler = curlRespond(`{"id": "abc123cluster"}`, "200")
	id, err := d.createCluster(context.Background(), "project-1", `{"cluster":{"name":"test"}}`)
	if err != nil || id != "abc123cluster" {
		t.Fatalf("got %q, %v", id, err)
	}
	line := argvLine(runner.calls[0])
	if !strings.Contains(line, "--request POST") ||
		!strings.Contains(line, "https://kkp.test.example.com/api/v2/projects/project-1/clusters") ||
		!strings.Contains(line, `--data {"cluster":{"name":"test"}}`) {
		t.Fatalf("argv = %q", line)
	}
}

func TestCreateClusterFailsWithoutID(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{}`, "200")
	if _, err := d.createCluster(context.Background(), "project-1", `{}`); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "no cluster ID") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDeleteClusterSucceeds(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	runner.handler = curlRespond("", "200")
	if err := d.deleteCluster(context.Background(), "project-1", "cluster-abc"); err != nil {
		t.Fatal(err)
	}
	line := argvLine(runner.calls[0])
	if !strings.Contains(line, "--request DELETE") ||
		!strings.Contains(line, "/api/v2/projects/project-1/clusters/cluster-abc") {
		t.Fatalf("argv = %q", line)
	}
}

func TestGetKubeconfigWritesFile(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	runner.handler = curlRespond("apiVersion: v1\nkind: Config\nclusters: []", "200")
	out := filepath.Join(t.TempDir(), "sub", "kubeconfig.yaml")
	if err := d.getKubeconfig(context.Background(), "project-1", "cluster-abc", out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "apiVersion: v1\nkind: Config\nclusters: []\n" {
		t.Fatalf("kubeconfig = %q", raw)
	}
}

func TestCreateMachineDeploymentReturnsID(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	runner.handler = curlRespond(`{"id": "md-pool1-xyz"}`, "200")
	id, err := d.createMachineDeployment(context.Background(), "proj-1", "cluster-1", `{"name":"pool-1"}`)
	if err != nil || id != "md-pool1-xyz" {
		t.Fatalf("got %q, %v", id, err)
	}
	if !strings.Contains(argvLine(runner.calls[0]), "/api/v2/projects/proj-1/clusters/cluster-1/machinedeployments") {
		t.Fatalf("argv = %q", argvLine(runner.calls[0]))
	}
}

// ── core_healthy ──────────────────────────────────────────

func TestCoreHealthy(t *testing.T) {
	cases := []struct {
		name   string
		health string
		want   bool
	}{
		{"core up, provider-dependent down ignored", allUpHealth, true},
		{"etcd provisioning", `{"apiserver":"HealthStatusUp","etcd":"HealthStatusProvisioning","controller":"HealthStatusUp","scheduler":"HealthStatusUp"}`, false},
		{"core component missing", `{"apiserver":"HealthStatusUp"}`, false},
		{"legacy numeric health", `{"apiserver":1,"etcd":1,"controller":1,"scheduler":1}`, true},
		{"invalid json", `nope`, false},
	}
	for _, tc := range cases {
		if got := coreHealthy(tc.health); got != tc.want {
			t.Errorf("%s: coreHealthy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ── wait_ready / wait_components ──────────────────────────

func TestWaitReadyHealthy(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	runner.handler = curlRespond(allUpHealth, "200")
	if err := d.waitReady(context.Background(), "project-1", "cluster-abc", 5); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{"apiserver":"HealthStatusProvisioning","etcd":"HealthStatusProvisioning","controller":"HealthStatusProvisioning","scheduler":"HealthStatusProvisioning"}`, "200")
	if err := d.waitReady(context.Background(), "project-1", "cluster-abc", 1); err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(stderr.String(), "Timed out waiting for KKP cluster cluster-abc to become healthy after 1s") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestWaitComponentsUp(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	runner.handler = curlRespond(`{"machineController":"HealthStatusUp","operatingSystemManager":"HealthStatusUp"}`, "200")
	if err := d.waitComponents(context.Background(), "project-1", "cluster-abc", 5,
		"machineController", "operatingSystemManager"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitComponentsTimesOut(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{"machineController":"HealthStatusUp","operatingSystemManager":"HealthStatusProvisioning"}`, "200")
	if err := d.waitComponents(context.Background(), "project-1", "cluster-abc", 1,
		"machineController", "operatingSystemManager"); err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(stderr.String(), "Timed out waiting for KKP cluster cluster-abc components (machineController operatingSystemManager) after 1s") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// ── payload builders (jq goldens) ─────────────────────────

// goldenSection extracts one `=== name ===` block from the payloads golden.
func goldenSection(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "kkp_payloads.golden"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(raw), "=== ")
	for _, p := range parts {
		if strings.HasPrefix(p, name+" ===\n") {
			body := strings.TrimPrefix(p, name+" ===\n")
			return strings.TrimRight(body, "\n")
		}
	}
	t.Fatalf("golden section %q not found", name)
	return ""
}

func TestBuildClusterJSONMatchesJQGolden(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "tok-abc")
	spec := loadSpec(kkpFixture(t))
	var stderr bytes.Buffer
	got, err := buildClusterJSON("test-kkp-cluster", "v1.29.2", "hetzner-fsn1", "hetzner", spec, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "cluster hetzner"); got != want {
		t.Fatalf("cluster JSON diverges from the jq golden:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildCloudSpecByo(t *testing.T) {
	var stderr bytes.Buffer
	cs, err := buildCloudSpec("byo", "", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalJQ(cs)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "cloud byo"); got != want {
		t.Fatalf("byo cloud spec:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildCloudSpecPreset(t *testing.T) {
	var stderr bytes.Buffer
	cs, err := buildCloudSpec("hetzner", "my-preset", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalJQ(cs)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "cloud preset"); got != want {
		t.Fatalf("preset cloud spec:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildCloudSpecUnsupported(t *testing.T) {
	var stderr bytes.Buffer
	if _, err := buildCloudSpec("gcp", "", &stderr); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "Unsupported KKP provider: gcp") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBuildMachineDeploymentJSONHetznerAutoscaled(t *testing.T) {
	var stderr bytes.Buffer
	got, err := buildMachineDeploymentJSON("pool-1", "3", "cpx31", "ubuntu", "hetzner", "1", "10", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "md hetzner autoscaled"); got != want {
		t.Fatalf("md JSON diverges from the jq golden:\n got: %s\nwant: %s", got, want)
	}
	// REST HetznerNodeSpec field is `type` (machine-controller's rawConfig
	// calls it serverType — do not confuse the two).
	if !strings.Contains(got, `"type": "cpx31"`) || strings.Contains(got, "serverType") {
		t.Fatalf("hetzner node spec field: %s", got)
	}
}

func TestBuildMachineDeploymentJSONAWSPlain(t *testing.T) {
	var stderr bytes.Buffer
	got, err := buildMachineDeploymentJSON("pool-a", "2", "t3.large", "flatcar", "aws", "0", "0", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "md aws plain"); got != want {
		t.Fatalf("md JSON:\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, "minReplicas") {
		t.Fatal("autoscaler bounds added without configuration")
	}
}

// ── driver::status ────────────────────────────────────────

func statusState(t *testing.T, d *Driver) {
	t.Helper()
	raw, err := os.ReadFile(kkpFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	writeSpec(t, d, "test-domain", string(raw))
	work := d.workDir("test-domain")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "cluster_id"), []byte("cluster-abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "project_id"), []byte("project-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatusNotFoundWithoutClusterID(t *testing.T) {
	setKKPEnv(t)
	d, _, _ := testDriver(t)
	raw, _ := os.ReadFile(kkpFixture(t))
	writeSpec(t, d, "test-domain", string(raw))
	got, err := d.Status(context.Background(), "test-domain")
	if err != nil || got != "NotFound" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusRunningWhenCoreHealthy(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	statusState(t, d)
	runner.handler = curlRespond(allUpHealth, "200")
	got, err := d.Status(context.Background(), "test-domain")
	if err != nil || got != "Running" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusProvisioningWhenNotYetHealthy(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	statusState(t, d)
	runner.handler = curlRespond(`{"apiserver":"HealthStatusProvisioning","etcd":"HealthStatusProvisioning","controller":"HealthStatusProvisioning","scheduler":"HealthStatusProvisioning"}`, "200")
	got, err := d.Status(context.Background(), "test-domain")
	if err != nil || got != "Provisioning" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusUnknownWhenAPIErrors(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	statusState(t, d)
	runner.handler = curlRespond(`{"error":"boom"}`, "500")
	got, err := d.Status(context.Background(), "test-domain")
	if err != nil || got != "Unknown" {
		t.Fatalf("got %q, %v", got, err)
	}
	// The bash probe was `>/dev/null 2>&1` — status must stay silent.
	if strings.Contains(stderr.String(), "KKP API error") {
		t.Fatalf("status leaked api diagnostics: %q", stderr.String())
	}
}

// ── driver::kubeconfig / ensure_credentials ───────────────

func TestKubeconfigUsesMetadataName(t *testing.T) {
	d, _, _ := testDriver(t)
	raw, _ := os.ReadFile(kkpFixture(t))
	writeSpec(t, d, "test-domain", string(raw))
	got, err := d.Kubeconfig(context.Background(), "test-domain")
	want := filepath.Join(d.deps.Paths.Base, ".kubeconfig", "test-kkp-cluster.yaml")
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestEnsureCredentialsRejectsHTTPSpecURL(t *testing.T) {
	setKKPEnv(t)
	t.Setenv("KKP_API_URL", "")
	os.Unsetenv("KKP_API_URL")
	d, _, stderr := testDriver(t)
	spec := writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: test-kkp-insecure}
spec:
  kkp:
    apiUrl: "http://kkp.insecure.example.com"
    projectId: "test-project-123"
    datacenter: "hetzner-fsn1"
  provider: hetzner
`)
	if err := d.EnsureCredentials(context.Background(), spec); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "HTTPS") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

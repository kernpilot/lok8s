package kkp

// api_test.go covers api.go, the port of tests/unit/kkp_test.bats: URL
// validation, credential validation, the api client (CA, redaction, 429
// retries), the create, delete, kubeconfig and machine-deployment calls,
// core_healthy and the two waits. curl is the fake Runner.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

// ── validate_url ──────────────────────────────────────────

func TestValidateURL(t *testing.T) {
	t.Parallel()
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr); err == nil {
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr); err == nil {
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr); err == nil {
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
	body, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr)
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"ok": true}` {
		t.Fatalf("body = %q", body)
	}
	want := []string{
		"--silent", "--show-error", "--fail-with-body", "--location",
		"--config", "-",
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
	// The token travels in the config on stdin, never on argv.
	if strings.Contains(strings.Join(got.Args, " "), "test-kkp-token-abc123") {
		t.Fatalf("token on argv: %q", got.Args)
	}
	if want := "header = \"Authorization: Bearer test-kkp-token-abc123\"\n"; runner.stdins[0] != want {
		t.Fatalf("curl config on stdin = %q, want %q", runner.stdins[0], want)
	}
}

func TestCurlConfigQuoteEscapes(t *testing.T) {
	t.Parallel()
	if got, want := curlConfigQuote(`a"b\c`), `"a\"b\\c"`; got != want {
		t.Fatalf("got %s, want %s", got, want)
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr); err != nil {
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr); err == nil {
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr); err != nil {
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
	body, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr)
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/dc", "", stderr); err == nil {
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
	if _, err := d.api(t.Context(), "GET", "/api/v2/projects/bad/clusters/bad", "", stderr); err == nil {
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
	id, err := d.createCluster(t.Context(), "project-1", `{"cluster":{"name":"test"}}`)
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
	if _, err := d.createCluster(t.Context(), "project-1", `{}`); err == nil {
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
	if err := d.deleteCluster(t.Context(), "project-1", "cluster-abc"); err != nil {
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
	if err := d.getKubeconfig(t.Context(), "project-1", "cluster-abc", out); err != nil {
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
	id, err := d.createMachineDeployment(t.Context(), "proj-1", "cluster-1", `{"name":"pool-1"}`)
	if err != nil || id != "md-pool1-xyz" {
		t.Fatalf("got %q, %v", id, err)
	}
	if !strings.Contains(argvLine(runner.calls[0]), "/api/v2/projects/proj-1/clusters/cluster-1/machinedeployments") {
		t.Fatalf("argv = %q", argvLine(runner.calls[0]))
	}
}

// ── core_healthy ──────────────────────────────────────────

func TestCoreHealthy(t *testing.T) {
	t.Parallel()
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
	if err := d.waitReady(t.Context(), "project-1", "cluster-abc", 5); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{"apiserver":"HealthStatusProvisioning","etcd":"HealthStatusProvisioning","controller":"HealthStatusProvisioning","scheduler":"HealthStatusProvisioning"}`, "200")
	if err := d.waitReady(t.Context(), "project-1", "cluster-abc", 1); err == nil {
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
	if err := d.waitComponents(t.Context(), "project-1", "cluster-abc", 5,
		"machineController", "operatingSystemManager"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitComponentsTimesOut(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	runner.handler = curlRespond(`{"machineController":"HealthStatusUp","operatingSystemManager":"HealthStatusProvisioning"}`, "200")
	if err := d.waitComponents(t.Context(), "project-1", "cluster-abc", 1,
		"machineController", "operatingSystemManager"); err == nil {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(stderr.String(), "Timed out waiting for KKP cluster cluster-abc components (machineController operatingSystemManager) after 1s") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

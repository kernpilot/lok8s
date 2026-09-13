package kkp

// kkp_test.go covers kkp.go: the payload builders pinned against the
// jq-built goldens, the health-derived status words, the kubeconfig path,
// the credential gate, and provision and destroy end to end over a
// scripted curl (the kkp half of kkp_capi_destroy_guards_test.bats: a
// FAILED remote delete must not report success and must KEEP cluster_id,
// the only handle a retry has).
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
// captured read-only. Regenerate from the bash (bash wins); -update rewrites
// a section from the Go output for a reviewed, deliberate change.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"errors"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// goldenSection returns one `=== name ===` block of the payloads golden.
// With -update it first rewrites that block with got.
func goldenSection(t *testing.T, name, got string) string {
	t.Helper()
	path := filepath.Join("testdata", "kkp_payloads.golden")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := "=== " + name + " ===\n"
	start := strings.Index(string(raw), head)
	if start < 0 {
		t.Fatalf("golden section %q not found", name)
	}
	bodyStart := start + len(head)
	end := strings.Index(string(raw[bodyStart:]), "\n=== ")
	if end < 0 {
		end = len(raw)
	} else {
		end += bodyStart + 1
	}
	if *update {
		testutil.WriteFile(t, path, string(raw[:bodyStart])+got+"\n"+string(raw[end:]))
		return got
	}
	return strings.TrimRight(string(raw[bodyStart:end]), "\n")
}

func TestBuildClusterJSONMatchesJQGolden(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "tok-abc")
	spec := loadSpec(kkpFixture(t))
	var stderr bytes.Buffer
	got, err := buildClusterJSON("test-kkp-cluster", "v1.29.2", "hetzner-fsn1", "hetzner", spec, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "cluster hetzner", got); got != want {
		t.Fatalf("cluster JSON diverges from the jq golden:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildCloudSpecByo(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	cs, err := buildCloudSpec("byo", "", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalJQ(cs)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "cloud byo", got); got != want {
		t.Fatalf("byo cloud spec:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildCloudSpecPreset(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	cs, err := buildCloudSpec("hetzner", "my-preset", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalJQ(cs)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "cloud preset", got); got != want {
		t.Fatalf("preset cloud spec:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildCloudSpecUnsupported(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if _, err := buildCloudSpec("gcp", "", &stderr); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "Unsupported KKP provider: gcp") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBuildMachineDeploymentJSONHetznerAutoscaled(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	got, err := buildMachineDeploymentJSON("pool-1", "3", "cpx31", "ubuntu", "hetzner", "1", "10", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "md hetzner autoscaled", got); got != want {
		t.Fatalf("md JSON diverges from the jq golden:\n got: %s\nwant: %s", got, want)
	}
	// REST HetznerNodeSpec field is `type` (machine-controller's rawConfig
	// calls it serverType — do not confuse the two).
	if !strings.Contains(got, `"type": "cpx31"`) || strings.Contains(got, "serverType") {
		t.Fatalf("hetzner node spec field: %s", got)
	}
}

func TestBuildMachineDeploymentJSONAWSPlain(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	got, err := buildMachineDeploymentJSON("pool-a", "2", "t3.large", "flatcar", "aws", "0", "0", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenSection(t, "md aws plain", got); got != want {
		t.Fatalf("md JSON:\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, "minReplicas") {
		t.Fatal("autoscaler bounds added without configuration")
	}
}

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
	got, err := d.Status(t.Context(), "test-domain")
	if err != nil || got != "NotFound" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusRunningWhenCoreHealthy(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	statusState(t, d)
	runner.handler = curlRespond(allUpHealth, "200")
	got, err := d.Status(t.Context(), "test-domain")
	if err != nil || got != "Running" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusProvisioningWhenNotYetHealthy(t *testing.T) {
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	statusState(t, d)
	runner.handler = curlRespond(`{"apiserver":"HealthStatusProvisioning","etcd":"HealthStatusProvisioning","controller":"HealthStatusProvisioning","scheduler":"HealthStatusProvisioning"}`, "200")
	got, err := d.Status(t.Context(), "test-domain")
	if err != nil || got != "Provisioning" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestStatusUnknownWhenAPIErrors(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	statusState(t, d)
	runner.handler = curlRespond(`{"error":"boom"}`, "500")
	got, err := d.Status(t.Context(), "test-domain")
	if err != nil || got != "Unknown" {
		t.Fatalf("got %q, %v", got, err)
	}
	// The bash probe was `>/dev/null 2>&1` — status must stay silent.
	if strings.Contains(stderr.String(), "KKP API error") {
		t.Fatalf("status leaked api diagnostics: %q", stderr.String())
	}
}

func TestKubeconfigUsesMetadataName(t *testing.T) {
	t.Parallel()
	d, _, _ := testDriver(t)
	raw, _ := os.ReadFile(kkpFixture(t))
	writeSpec(t, d, "test-domain", string(raw))
	got, err := d.Kubeconfig(t.Context(), "test-domain")
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
	if err := d.EnsureCredentials(t.Context(), spec); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "HTTPS") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// kkpAPI routes a fake curl call by method+path suffix.
type kkpAPI struct {
	t       *testing.T
	created []string // POST bodies seen
	deleted int
	// respond overrides per (method, path-contains) — return handled=true
	// to short-circuit.
	respond func(method, url string, c execx.Cmd) bool
}

func (a *kkpAPI) handler(c execx.Cmd) error {
	if c.Name != "curl" {
		a.t.Fatalf("unexpected exec: %s", argvLine(c))
	}
	method, url, body := "", "", ""
	for i, arg := range c.Args {
		switch arg {
		case "--request":
			method = c.Args[i+1]
		case "--data":
			body = c.Args[i+1]
		}
	}
	url = c.Args[len(c.Args)-1]
	if a.respond != nil && a.respond(method, url, c) {
		return nil
	}
	switch {
	case method == "POST" && strings.HasSuffix(url, "/machinedeployments"):
		a.created = append(a.created, body)
		fmt.Fprint(c.Stdout, `{"id": "md-xyz"}`+"\n200")
	case method == "POST" && strings.HasSuffix(url, "/clusters"):
		a.created = append(a.created, body)
		fmt.Fprint(c.Stdout, `{"id": "new-cluster-id"}`+"\n200")
	case method == "DELETE":
		a.deleted++
		fmt.Fprint(c.Stdout, "\n200")
	case strings.HasSuffix(url, "/health"):
		fmt.Fprint(c.Stdout, `{"apiserver":"HealthStatusUp","etcd":"HealthStatusUp","controller":"HealthStatusUp","scheduler":"HealthStatusUp","machineController":"HealthStatusUp","operatingSystemManager":"HealthStatusUp"}`+"\n200")
	case strings.HasSuffix(url, "/kubeconfig"):
		fmt.Fprint(c.Stdout, "apiVersion: v1\nkind: Config"+"\n200")
	case strings.HasSuffix(url, "/machinedeployments"):
		fmt.Fprint(c.Stdout, "[]\n200")
	default: // get_cluster & friends
		fmt.Fprint(c.Stdout, `{"id": "cluster-abc"}`+"\n200")
	}
	return nil
}

func provisionEnv(t *testing.T) {
	t.Helper()
	setKKPEnv(t)
	t.Setenv("HCLOUD_TOKEN", "tok-abc")
}

func TestProvisionHappyPath(t *testing.T) {
	provisionEnv(t)
	d, runner, _ := testDriver(t)
	raw, _ := os.ReadFile(kkpFixture(t))
	writeSpec(t, d, "test.dev", string(raw))
	api := &kkpAPI{t: t}
	runner.handler = api.handler
	// A stale world-readable kubeconfig under the same name: the write
	// must tighten it, not keep its mode.
	stale := filepath.Join(d.deps.Paths.Base, ".kubeconfig", "test-kkp-cluster.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := d.Provision(t.Context(), "test.dev"); err != nil {
		t.Fatal(err)
	}

	// cluster_id/project_id persisted with the bash trailing newline.
	work := d.workDir("test.dev")
	for file, want := range map[string]string{"cluster_id": "new-cluster-id\n", "project_id": "test-project-123\n"} {
		got, err := os.ReadFile(filepath.Join(work, file))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", file, got, err, want)
		}
	}
	// Kubeconfig under metadata.name, owner-only (D20) — even when a
	// world-readable file already sat there (planted above).
	kc := filepath.Join(d.deps.Paths.Base, ".kubeconfig", "test-kkp-cluster.yaml")
	if raw, err := os.ReadFile(kc); err != nil || string(raw) != "apiVersion: v1\nkind: Config\n" {
		t.Errorf("kubeconfig = %q, %v", raw, err)
	}
	if st, err := os.Stat(kc); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
	// The worker pool MD payload is exactly the jq golden (pool-1 from the
	// fixture: 3× cpx31 ubuntu, autoscaler 1..10).
	if len(api.created) != 2 {
		t.Fatalf("created payloads = %d, want cluster + one MD", len(api.created))
	}
	if want := goldenSection(t, "md hetzner autoscaled", api.created[1]); api.created[1] != want {
		t.Errorf("MD payload diverges from the jq golden:\n got: %s\nwant: %s", api.created[1], want)
	}
	// And the cluster payload is the golden too.
	if want := goldenSection(t, "cluster hetzner", api.created[0]); api.created[0] != want {
		t.Errorf("cluster payload diverges from the jq golden:\n got: %s\nwant: %s", api.created[0], want)
	}
}

func TestProvisionIdempotentReusesExistingCluster(t *testing.T) {
	provisionEnv(t)
	d, runner, _ := testDriver(t)
	raw, _ := os.ReadFile(kkpFixture(t))
	writeSpec(t, d, "test.dev", string(raw))
	work := d.workDir("test.dev")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "cluster_id"), []byte("cl-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &kkpAPI{t: t}
	runner.handler = api.handler

	if err := d.Provision(t.Context(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	for _, body := range api.created {
		if strings.Contains(body, `"cluster"`) {
			t.Fatal("a cluster create was POSTed although cl-123 still exists")
		}
	}
	// The saved ID stays.
	if got, _ := os.ReadFile(filepath.Join(work, "cluster_id")); string(got) != "cl-123\n" {
		t.Fatalf("cluster_id = %q", got)
	}
}

func TestProvisionStaleClusterIDCreatesNew(t *testing.T) {
	provisionEnv(t)
	d, runner, stderr := testDriver(t)
	raw, _ := os.ReadFile(kkpFixture(t))
	writeSpec(t, d, "test.dev", string(raw))
	work := d.workDir("test.dev")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "cluster_id"), []byte("gone-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &kkpAPI{t: t}
	api.respond = func(method, url string, c execx.Cmd) bool {
		if method == "GET" && strings.HasSuffix(url, "/clusters/gone-123") {
			fmt.Fprint(c.Stdout, `{"error":"not found"}`+"\n404")
			return true
		}
		return false
	}
	runner.handler = api.handler

	if err := d.Provision(t.Context(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Saved cluster ID gone-123 no longer exists in KKP — creating a new cluster") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if got, _ := os.ReadFile(filepath.Join(work, "cluster_id")); string(got) != "new-cluster-id\n" {
		t.Fatalf("cluster_id = %q", got)
	}
}

func TestProvisionByoSkipsComponentGate(t *testing.T) {
	// byo clusters have no pools — the machineController/OSM gate (which
	// they never satisfy) must not run.
	provisionEnv(t)
	d, runner, _ := testDriver(t)
	writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: byo-cluster}
spec:
  kubernetes: {version: "1.35.5"}
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "proj-1"
    datacenter: "byo-local"
  provider: {name: byo}
`)
	api := &kkpAPI{t: t}
	runner.handler = api.handler
	if err := d.Provision(t.Context(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	for _, c := range runner.calls {
		if strings.Contains(argvLine(c), "/machinedeployments") {
			t.Fatal("machinedeployments touched for a pool-less byo cluster")
		}
	}
	// The byo cloud spec went over the wire.
	if len(api.created) != 1 || !strings.Contains(api.created[0], `"bringyourown": {}`) {
		t.Fatalf("created = %v", api.created)
	}
}

func TestProvisionSkipsExistingPool(t *testing.T) {
	provisionEnv(t)
	d, runner, stderr := testDriver(t)
	raw, _ := os.ReadFile(kkpFixture(t))
	writeSpec(t, d, "test.dev", string(raw))
	api := &kkpAPI{t: t}
	api.respond = func(method, url string, c execx.Cmd) bool {
		if method == "GET" && strings.HasSuffix(url, "/machinedeployments") {
			fmt.Fprint(c.Stdout, `[{"name":"pool-1"}]`+"\n200")
			return true
		}
		return false
	}
	runner.handler = api.handler
	t.Setenv("DEBUG", "1")
	if err := d.Provision(t.Context(), "test.dev"); err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 1 { // only the cluster create
		t.Fatalf("created = %v", api.created)
	}
	if !strings.Contains(stderr.String(), "Worker pool pool-1 already exists — skipping create") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestProvisionPoolWithoutFlavorFails(t *testing.T) {
	provisionEnv(t)
	d, runner, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: noflavor}
spec:
  kubernetes: {version: v1.29.2}
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "proj-1"
    datacenter: "hetzner-fsn1"
  provider: {name: hetzner}
  workers:
    bare-pool:
      replicas: 1
`)
	api := &kkpAPI{t: t}
	runner.handler = api.handler
	if err := d.Provision(t.Context(), "test.dev"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "Worker pool 'bare-pool' has no flavor/type set") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestProvisionExportsNullSpecURLQuirk(t *testing.T) {
	// The bash read spec.kkp.apiUrl BARE for the export, so a spec without
	// it exported the literal "null" — which validate_credentials had
	// already caught… unless KKP_API_URL validation is reached first with
	// the exported value. Pin the quirk at its observable edge: env unset +
	// spec without apiUrl fails validation before any curl runs.
	setKKPEnv(t)
	t.Setenv("KKP_API_URL", "")
	os.Unsetenv("KKP_API_URL")
	t.Setenv("HCLOUD_TOKEN", "tok")
	d, runner, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: nourl}
spec:
  kkp: {projectId: p, datacenter: dc}
  provider: {name: hetzner}
`)
	if err := d.Provision(t.Context(), "test.dev"); err == nil {
		t.Fatal("expected error")
	}
	if len(runner.calls) != 0 {
		t.Fatal("curl must not run without an API URL")
	}
	if !strings.Contains(stderr.String(), "KKP_API_URL env var or spec.kkp.apiUrl is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func destroySetup(t *testing.T) (*Driver, *fakeRunner, string) {
	t.Helper()
	setKKPEnv(t)
	d, runner, _ := testDriver(t)
	writeSpec(t, d, "test.dev", `kind: Kkp
metadata: {name: destroytest}
spec:
  kkp:
    apiUrl: https://kkp.example.test
    projectId: proj-abc
`)
	work := d.workDir("test.dev")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "cluster_id"), []byte("cl-123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "project_id"), []byte("proj-abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.deps.Paths.Base, ".kubeconfig", "destroytest.yaml"),
		[]byte("apiVersion: v1\nkind: Config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d, runner, work
}

func TestDestroyFailedDeleteDoesNotReportSuccess(t *testing.T) {
	d, runner, _ := destroySetup(t)
	runner.handler = curlRespond(`{"error":"boom"}`, "500")
	if err := d.Destroy(t.Context(), "test.dev"); err == nil {
		t.Fatal("destroy returned success although the KKP delete FAILED — " +
			"the user cluster is still running and still billing (issue #91's class)")
	}
}

func TestDestroyFailedDeleteKeepsClusterID(t *testing.T) {
	// Returning an error is not sufficient. The driver refuses to destroy
	// without a saved cluster_id, so wiping the work dir on a failed delete
	// makes the orphan PERMANENTLY unreachable. The surviving file is the
	// property that can only mean one thing.
	d, runner, work := destroySetup(t)
	runner.handler = curlRespond(`{"error":"boom"}`, "500")
	_ = d.Destroy(t.Context(), "test.dev")
	if _, err := os.Stat(filepath.Join(work, "cluster_id")); err != nil {
		t.Fatal("cluster_id was deleted after a FAILED KKP delete — nothing left can clean this up")
	}
	if _, err := os.Stat(filepath.Join(d.deps.Paths.Base, ".kubeconfig", "destroytest.yaml")); err != nil {
		t.Fatal("the kubeconfig was deleted after a FAILED delete")
	}
}

func TestDestroyHappyPathCleansLocalState(t *testing.T) {
	// Guards against 'fixing' the above by making destroy always fail.
	d, runner, work := destroySetup(t)
	api := &kkpAPI{t: t}
	runner.handler = api.handler
	if err := d.Destroy(t.Context(), "test.dev"); err != nil {
		t.Fatalf("kkp happy path regressed: %v", err)
	}
	if api.deleted != 1 {
		t.Fatalf("DELETE issued %d times", api.deleted)
	}
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a SUCCESSFUL destroy left the work dir behind — stale cluster_id " +
			"would make the next destroy address a cluster that no longer exists")
	}
	if _, err := os.Stat(filepath.Join(d.deps.Paths.Base, ".kubeconfig", "destroytest.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("kubeconfig survived a successful destroy")
	}
}

func TestDestroyWithoutSavedIDRefuses(t *testing.T) {
	setKKPEnv(t)
	d, runner, stderr := testDriver(t)
	writeSpec(t, d, "test.dev", "kind: Kkp\nmetadata: {name: x}\nspec:\n  kkp: {apiUrl: https://kkp.example.test}\n")
	if err := d.Destroy(t.Context(), "test.dev"); err == nil {
		t.Fatal("expected error")
	}
	out := stderr.String()
	if !strings.Contains(out, "No saved cluster ID found in") || !strings.Contains(out, "Cannot destroy cluster without a cluster ID") {
		t.Fatalf("stderr = %q", out)
	}
	if len(runner.calls) != 0 {
		t.Fatal("no curl may run without a cluster ID")
	}
}

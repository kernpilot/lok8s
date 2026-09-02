package kubehz

// hosted_test.go ports tests/unit/kubehz_hosted_test.bats (the library
// half; the driver-branch cases live in the kubeone/capi packages' own
// hook tests and in hooks_test.go here).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildClusterPayloadMatchesBashGolden(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(spec, []byte(`kind: KubeOne
metadata:
  name: hosted
spec:
  cluster:
    domain: test.kubehz.dev
  hcloud:
    region: nbg1
  kubernetes:
    version: v1.31.10
  controlPlane:
    replicas: 3
  kubehz:
    hosting: hosted
    access: managed
    apiUrl: https://api.kubehz.dev
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildClusterPayload(spec)
	golden := readFile(t, filepath.Join("testdata", "golden", "cluster-payload.json"))
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("payload not JSON: %s", got)
	}
	_ = json.Unmarshal([]byte(golden), &w)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("payload drift:\n got %s\nwant %s", got, golden)
	}
	// Key order is the jq object order (bash: jq -n '{domain, kind, …}').
	if string(got) != `{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"nbg1","kubernetesVersion":"v1.31.10","controlPlaneReplicas":3,"hosting":"hosted","access":"managed"}` {
		t.Fatalf("key order: %s", got)
	}
}

func TestBuildClusterPayloadDefaults(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "spec.yaml")
	_ = os.WriteFile(spec, []byte("kind: Capi\nspec:\n  cluster:\n    domain: default.kubehz.dev\n  kubernetes:\n    version: v1.30.0\n"), 0o644)
	var g map[string]any
	_ = json.Unmarshal(buildClusterPayload(spec), &g)
	if g["provider"] != "hetzner" || g["controlPlaneReplicas"] != float64(1) || g["region"] != "fsn1" || g["kind"] != "Capi" {
		t.Fatalf("%v", g)
	}
}

func TestWaitForCluster(t *testing.T) {
	h := newHarness(t)
	h.handle("GET /api/clusters/cl-001", 200, `{"id":"cl-001","status":"Running"}`)
	mustOK(t, h.ctx.waitForCluster(context.Background(), h.apiURL(), "cl-001", 30), h.output())
	h.reset()
	h.handle("GET /api/clusters/cl-002", 200, `{"id":"cl-002","status":"Failed"}`)
	mustErr(t, h.ctx.waitForCluster(context.Background(), h.apiURL(), "cl-002", 30))
	mustContain(t, h.output(), "failed")
	h.reset()
	h.handle("GET /api/clusters/cl-003", 200, `{"data":{"status":"Creating"}}`)
	mustErr(t, h.ctx.waitForCluster(context.Background(), h.apiURL(), "cl-003", 30))
	mustContain(t, h.output(), "Timed out waiting for hosted cluster cl-003 after 30s")
}

func hostedSpec(h *harness, name string) string {
	return h.writeSpec("test.kubehz.dev", "kind: KubeOne\nmetadata:\n  name: "+name+"\nspec:\n  cluster:\n    domain: test.kubehz.dev\n  kubernetes:\n    version: v1.31.10\n  kubehz:\n    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n")
}

func TestProvisionHostedCreatesWaitsDownloads(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_TOKEN"] = "test-token"
	spec := hostedSpec(h, "test.kubehz.dev")
	h.handle("POST /api/clusters", 201, `{"ok":true,"data":{"id":"cl-hosted-001","status":"Creating"}}`)
	h.handle("GET /api/clusters/cl-hosted-001", 200, `{"id":"cl-hosted-001","status":"Running"}`)
	h.handle("GET /api/clusters/cl-hosted-001/kubeconfig", 200, "apiVersion: v1\nkind: Config\n")
	cfg := &Config{APIURL: h.apiURL()}
	mustOK(t, h.ctx.ProvisionHosted(context.Background(), cfg, "test.kubehz.dev", spec), h.output())
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	if readFile(t, kc) != "apiVersion: v1\nkind: Config\n" {
		t.Fatal("kubeconfig not written")
	}
	if info, _ := os.Stat(kc); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if r := h.lastReq("POST", "/api/clusters"); r.Auth != "Bearer test-token" {
		t.Fatalf("create auth: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(h.base, ".kubeconfig", "shortname.yaml")); err == nil {
		t.Fatal("no mirror when metadata.name equals the domain")
	}
}

func TestProvisionHostedMirrorsToMetadataName(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "shortname")
	h.handle("POST /api/clusters", 201, `{"data":{"id":"cl-1","status":"Running"}}`)
	h.handle("GET /api/clusters/cl-1", 200, `{"data":{"status":"Running"}}`)
	h.handle("GET /api/clusters/cl-1/kubeconfig", 200, "kc-bytes\n")
	mustOK(t, h.ctx.ProvisionHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	if readFile(t, filepath.Join(h.base, ".kubeconfig", "shortname.yaml")) != "kc-bytes\n" {
		t.Fatal("metadata.name mirror missing")
	}
}

func TestProvisionHostedNoClusterID(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("POST /api/clusters", 200, `{"ok":true,"data":{}}`)
	mustErr(t, h.ctx.ProvisionHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "did not return a cluster ID")
}

func TestProvisionHostedCapacityEnvelope(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("POST /api/clusters", 503, `{"ok":false,"data":{"code":"AT_CAPACITY","message":"at capacity","detail":{"tier":"dev","used":40,"limit":40,"retryAfter":3600}}}`)
	mustErr(t, h.ctx.ProvisionHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	out := h.output()
	mustContain(t, out, "at capacity for the 'dev' plan")
	mustContain(t, out, "40/40")
	mustContain(t, out, "hosting: self")
	mustContain(t, out, h.apiURL()+"/api/capacity")
	mustContain(t, out, "~60 min")
	mustNotContain(t, out, "~3600s")
	mustNotContain(t, out, "spec.kubehz.plan")

	h.reset()
	h.handle("POST /api/clusters", 503, "{\n  \"ok\": false,\n  \"data\": {\n    \"code\": \"AT_CAPACITY\",\n    \"detail\": {\n      \"tier\": \"starter\",\n      \"used\": 20,\n      \"limit\": 20,\n      \"retryAfter\": 1800\n    }\n  }\n}")
	mustErr(t, h.ctx.ProvisionHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "at capacity for the 'starter' plan")
	mustContain(t, h.output(), "20/20")
	mustContain(t, h.output(), "~30 min")
}

func TestRenderCapacityRejectionHints(t *testing.T) {
	h := newHarness(t)
	cfg := &Config{APIURL: "https://api.kubehz.dev"}
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"tier":"dev","used":3,"limit":3,"retryAfter":45}}}`))
	mustContain(t, h.output(), "~45s")
	mustNotContain(t, h.output(), "min")
	h.reset()
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"tier":"dev"}}}`))
	mustContain(t, h.output(), "at capacity for the 'dev' plan")
	mustNotContain(t, h.output(), "suggested wait")
	mustNotContain(t, h.output(), "spec.kubehz.plan")
}

func TestProvisionHostedSurfacesAPIMessage(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("POST /api/clusters", 502, `{"ok":false,"data":{"code":"HOSTED_BACKEND_ERROR","message":"Failed to schedule the hosted control plane","help":"try later"}}`)
	mustErr(t, h.ctx.ProvisionHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "HTTP 502")
	mustContain(t, h.output(), "Failed to schedule the hosted control plane")
	mustContain(t, h.output(), "  try later")
}

func TestDestroyHostedResolvesOldestAndDeletes(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","createdAt":"2026-01-01T00:00:00Z"}
]}`)
	h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true,"data":{"deleted":true,"id":"cl-old"}}`)
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	_ = os.MkdirAll(filepath.Dir(kc), 0o755)
	_ = os.WriteFile(kc, []byte("kc"), 0o600)
	mustOK(t, h.ctx.DestroyHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	if _, err := os.Stat(kc); err == nil {
		t.Fatal("kubeconfig not cleaned up")
	}
	if h.anyReq("DELETE", "/api/clusters/cl-new") {
		t.Fatal("deleted the NEWEST cluster")
	}
}

func TestDestroyHostedRefusesOnLookupFailure(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("GET /api/clusters", 500, `{}`)
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	_ = os.MkdirAll(filepath.Dir(kc), 0o755)
	_ = os.WriteFile(kc, []byte("kc"), 0o600)
	mustErr(t, h.ctx.DestroyHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "nothing was deleted")
	if h.anyReq("DELETE", "/api/clusters") {
		t.Fatal("DELETE must not run when the lookup failed")
	}
	if _, err := os.Stat(kc); err != nil {
		t.Fatal("kubeconfig must survive the refusal")
	}
}

func TestDestroyHostedNoRowIsIdempotent(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","createdAt":"2026-03-01T00:00:00Z"}]}`)
	mustOK(t, h.ctx.DestroyHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	mustContain(t, h.output(), "nothing to destroy")
}

func TestDestroyHostedRefusedDelete(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-1","domain":"test.kubehz.dev","createdAt":"2026-01-01T00:00:00Z"}]}`)
	h.handle("DELETE /api/clusters/cl-1", 403, `{"ok":false,"data":{"message":"forbidden"}}`)
	mustErr(t, h.ctx.DestroyHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "HTTP 403: forbidden")
}

func TestDestroyHostedRemovesMirror(t *testing.T) {
	h := newHarness(t)
	spec := hostedSpec(h, "shortname")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-1","domain":"test.kubehz.dev","createdAt":"2026-01-01T00:00:00Z"}]}`)
	h.handle("DELETE /api/clusters/cl-1", 200, `{"ok":true}`)
	dir := filepath.Join(h.base, ".kubeconfig")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "test.kubehz.dev.yaml"), []byte("kc"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "shortname.yaml"), []byte("kc"), 0o600)
	mustOK(t, h.ctx.DestroyHosted(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	for _, f := range []string{"test.kubehz.dev.yaml", "shortname.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s leaked", f)
		}
	}
}

func TestDestroyHostedRefusesPlainHTTP(t *testing.T) {
	h := newHarness(t)
	mustErr(t, h.ctx.DestroyHosted(context.Background(), &Config{APIURL: "http://api"}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "must use HTTPS")
}

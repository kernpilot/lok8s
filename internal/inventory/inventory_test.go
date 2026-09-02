package inventory

// inventory_test.go ports tests/unit/inventory_test.bats (the
// ClusterInventory writer): Build is PURE metadata, deterministic under
// SOURCE_DATE_EPOCH; Publish is FAIL-SOFT. The golden in testdata/ was
// generated ONCE from the bash inventory::build_json over the same spec.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
)

// The compile-time pins: Publish's shape IS the no-error hook contract of
// both dispatchers. A signature drift fails the build, not a test.
var (
	_ = provision.Hooks{InventoryPublish: PublishHook(nil, nil, io.Discard)}
	_ = bootstrap.Dispatcher{InventoryPublish: PublishHook(nil, nil, io.Discard)}
)

// projectRoot is the lok8s checkout (the REAL addon tree, like the bats'
// PATH_LOK8S="${_PROJECT_ROOT}/.lok8s").
func projectRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// realPaths is a temp project whose .lok8s is the checkout's framework tree.
func realPaths(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	return &config.Paths{
		Base:     base,
		Bin:      filepath.Join(projectRoot(t), ".bin"),
		Lok8s:    filepath.Join(projectRoot(t), ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
}

func writeSpec(t *testing.T, p *config.Paths, domainName, content string) string {
	t.Helper()
	path := filepath.Join(p.Clusters, domainName, "cluster.lok8s.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fieldString(t *testing.T, out, path string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	var cur any = doc
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %s: not an object at %s", path, seg)
		}
		cur = m[seg]
	}
	if cur == nil {
		return ""
	}
	return cur.(string)
}

func addonsOf(t *testing.T, out string) []map[string]any {
	t.Helper()
	var doc struct {
		Spec struct {
			Addons []map[string]any `json:"addons"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Spec.Addons
}

// The bash-generated golden: the whole CR byte-for-byte (key order, indent,
// the malformed-entry skip, the @sha256 strip, the ./targets source, the
// real chart pins).
func TestBuildJSONMatchesBashGolden(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	p := realPaths(t)
	spec := writeSpec(t, p, "inv.dev", `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: inv }
spec:
  kubernetes: { version: "v1.31.10@sha256:abc" }
  provider: { name: hetzner }
  bootstrap:
    - cilium: { wait: true, values: { encryption: { enabled: true } } }
    - cert-manager
    - ./targets/networking
    - { "bad": 1, "two": 2 }
`)
	if err := os.MkdirAll(filepath.Join(p.Clusters, "inv.dev", "targets", "networking"), 0o755); err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	out, err := BuildJSON(p, &errBuf, "inv.dev", spec)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "build_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(string(golden), "\n"); out != want {
		t.Errorf("golden mismatch\n--- want ---\n%s\n--- got ---\n%s", want, out)
	}
	if want := "\033[0;33m[warn]\033[0m inventory: skipping unparseable bootstrap entry 'bad'\n"; errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
}

// bats: "build_json resolves addons with chart version + category from the real tree"
func TestBuildResolvesAddonsFromRealTree(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "inv", `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: inv }
spec:
  kubernetes: { version: "v1.31.10" }
  provider: { name: hetzner }
  bootstrap:
    - cilium: { wait: true, values: { encryption: { enabled: true } } }
    - cert-manager
`)
	out, err := BuildJSON(p, io.Discard, "inv", spec)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"apiVersion":                       "lok8s.dev/v1alpha1",
		"kind":                             "ClusterInventory",
		"metadata.name":                    "cluster",
		"metadata.labels.lok8s.dev/domain": "inv",
		"spec.kind":                        "kubeone",
		"spec.provider":                    "hetzner",
		"spec.kubernetesVersion":           "v1.31.10",
	} {
		if strings.Contains(path, "lok8s.dev/domain") {
			// The label key contains a dot; read it directly.
			var doc struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			_ = json.Unmarshal([]byte(out), &doc)
			if got := doc.Metadata.Labels["lok8s.dev/domain"]; got != want {
				t.Errorf("label = %q, want %q", got, want)
			}
			continue
		}
		if got := fieldString(t, out, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	addons := addonsOf(t, out)
	if len(addons) != 2 {
		t.Fatalf("addons = %v", addons)
	}
	if addons[0]["name"] != "cilium" || addons[0]["source"] != "addon" || addons[0]["category"] != "networking" {
		t.Errorf("cilium entry = %v", addons[0])
	}
	if cv := chartVersion(filepath.Join(p.Lok8s, "addons", "cilium")); addons[0]["chartVersion"] != cv || cv == "" || cv == "-" {
		t.Errorf("cilium chartVersion = %v, want the real pin %q", addons[0]["chartVersion"], cv)
	}
	if addons[1]["name"] != "cert-manager" {
		t.Errorf("second addon = %v", addons[1])
	}
}

// bats: "build_json emits STRICTLY metadata — inline values/env never leak"
func TestBuildNeverLeaksValuesOrEnv(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "leak", `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: leak }
spec:
  bootstrap:
    - cilium:
        values: { encryption: { enabled: true }, secretString: "hunter2-not-for-export" }
        env: { LOK8S_USER_SECRET_TOKEN: "tok-abc123" }
`)
	out, err := BuildJSON(p, io.Discard, "leak", spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2-not-for-export", "tok-abc123", "encryption"} {
		if strings.Contains(out, leak) {
			t.Errorf("inventory leaked %q", leak)
		}
	}
	allowed := map[string]bool{"name": true, "chartVersion": true, "appVersion": true, "category": true, "source": true}
	for _, a := range addonsOf(t, out) {
		for k := range a {
			if !allowed[k] {
				t.Errorf("addon entry carries non-enumerated key %q", k)
			}
		}
	}
}

// bats: "build_json specHash is the sha256 of the cluster spec bytes and is stable"
func TestBuildSpecHashAndDeterminism(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	p := realPaths(t)
	spec := writeSpec(t, p, "hashme", "apiVersion: cluster.lok8s.dev/v1beta1\nkind: KubeOne\nmetadata: { name: hashme }\nspec:\n  bootstrap: []\n")
	one, err := BuildJSON(p, io.Discard, "hashme", spec)
	if err != nil {
		t.Fatal(err)
	}
	two, _ := BuildJSON(p, io.Discard, "hashme", spec)
	if one != two {
		t.Fatal("not deterministic")
	}
	// sha256 of the exact bytes above.
	if got := fieldString(t, one, "spec.specHash"); len(got) != 64 {
		t.Errorf("specHash = %q", got)
	}
	if got := fieldString(t, one, "spec.renderedAt"); got != "2023-11-14T22:13:20Z" {
		t.Errorf("renderedAt = %q", got)
	}
	// An empty bootstrap list renders as `"addons": []`, never null.
	if !strings.Contains(one, `"addons": []`) {
		t.Errorf("empty addons must render as []:\n%s", one)
	}
}

// bats: "build_json strips a kindest-node @sha256 digest from kubernetesVersion"
func TestBuildStripsDigest(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "digest", "kind: Lo\nmetadata: { name: digest }\nspec:\n  kubernetes: { version: \"v1.31.12@sha256:0f5cc49c\" }\n  bootstrap: []\n")
	out, err := BuildJSON(p, io.Discard, "digest", spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := fieldString(t, out, "spec.kubernetesVersion"); got != "v1.31.12" {
		t.Errorf("kubernetesVersion = %q", got)
	}
}

// bats: "build_json includes the per-driver default (cilium on kind) when bootstrap is absent"
func TestBuildPerDriverDefault(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "defl", "kind: Lo\nmetadata: { name: defl }\nspec:\n  cluster: { domain: defl.lok8s.dev }\n")
	out, err := BuildJSON(p, io.Discard, "defl", spec)
	if err != nil {
		t.Fatal(err)
	}
	addons := addonsOf(t, out)
	if len(addons) != 1 || addons[0]["name"] != "cilium" {
		t.Errorf("addons = %v", addons)
	}
}

// bats: "build_json marks ./targets entries as source: target"
func TestBuildTargetSource(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "tgt", "kind: KubeOne\nmetadata: { name: tgt }\nspec:\n  bootstrap:\n    - ./targets/networking\n")
	if err := os.MkdirAll(filepath.Join(p.Clusters, "tgt", "targets", "networking"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := BuildJSON(p, io.Discard, "tgt", spec)
	if err != nil {
		t.Fatal(err)
	}
	addons := addonsOf(t, out)
	if len(addons) != 1 || addons[0]["name"] != "networking" || addons[0]["source"] != "target" {
		t.Errorf("addons = %v", addons)
	}
	if _, has := addons[0]["chartVersion"]; has {
		t.Errorf("a plain target carries no chartVersion: %v", addons[0])
	}
}

// bats: "build_json reads lok8sVersion from .lok8s/VERSION"
func TestBuildLok8sVersion(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "ver", "kind: KubeOne\nmetadata: { name: ver }\nspec:\n  bootstrap: []\n")
	out, err := BuildJSON(p, io.Discard, "ver", spec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(p.Lok8s, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fieldString(t, out, "spec.lok8sVersion"), strings.TrimRight(string(raw), "\n"); got != want {
		t.Errorf("lok8sVersion = %q, want %q", got, want)
	}
	// No VERSION file → "dev".
	p2 := &config.Paths{Base: t.TempDir(), Lok8s: filepath.Join(t.TempDir(), ".lok8s"), Clusters: p.Clusters}
	out2, _ := BuildJSON(p2, io.Discard, "ver", spec)
	if got := fieldString(t, out2, "spec.lok8sVersion"); got != "dev" {
		t.Errorf("lok8sVersion without VERSION = %q, want dev", got)
	}
}

// bats: "build_json refuses a malformed kind instead of defaulting it to lo"
func TestBuildRefusesMalformedKind(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "bad", "kind: ../../evil\nmetadata: { name: bad }\nspec:\n  kubernetes: { version: \"v1.31.10\" }\n")
	var errBuf bytes.Buffer
	out, err := BuildJSON(p, &errBuf, "bad", spec)
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(out, `"kind": "lo"`) {
		t.Error("malformed kind laundered to lo")
	}
	if want := "\033[0;31m[error]\033[0m inventory: cluster spec declares a malformed kind: " + spec + "\n"; errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
}

// bats: "build_json still defaults to lo when the spec declares no kind"
func TestBuildDefaultsMissingKindToLo(t *testing.T) {
	p := realPaths(t)
	spec := writeSpec(t, p, "nokind", "metadata: { name: nokind }\nspec:\n  kubernetes: { version: \"v1.31.10\" }\n")
	out, err := BuildJSON(p, io.Discard, "nokind", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"kind": "lo"`) {
		t.Errorf("missing kind must default to lo:\n%s", out)
	}
}

func TestBuildMissingSpec(t *testing.T) {
	p := realPaths(t)
	var errBuf bytes.Buffer
	if _, err := BuildJSON(p, &errBuf, "x", filepath.Join(p.Clusters, "x", "cluster.lok8s.yaml")); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "inventory: cluster spec not found: ") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// ── Publish — FAIL-SOFT contract ──────────────────────────

// fakeRunner records kubectl calls; fail makes every call error.
type fakeRunner struct {
	calls []string
	stdin []string
	fail  bool
}

func (f *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	f.calls = append(f.calls, c.Name+" "+strings.Join(c.Args, " "))
	in := ""
	if c.Stdin != nil {
		raw, _ := io.ReadAll(c.Stdin)
		in = string(raw)
	}
	f.stdin = append(f.stdin, in)
	if f.fail {
		return errors.New("exit status 1")
	}
	return nil
}

func softSpec(t *testing.T, p *config.Paths, name string) string {
	t.Helper()
	return writeSpec(t, p, name, "kind: KubeOne\nmetadata: { name: "+name+" }\nspec:\n  bootstrap: []\n")
}

// bats: "publish without a kubeconfig warns and returns 0 (never breaks a deploy)"
func TestPublishWithoutKubeconfigWarns(t *testing.T) {
	p := realPaths(t)
	spec := softSpec(t, p, "soft")
	f := &fakeRunner{}
	var errBuf bytes.Buffer
	Publish(context.Background(), p, f, &errBuf, "soft", spec, filepath.Join(p.Base, ".kubeconfig", "nonexistent.yaml"))
	if want := "\033[0;33m[warn]\033[0m inventory: kubeconfig not found (" + filepath.Join(p.Base, ".kubeconfig", "nonexistent.yaml") + ") — skipping ClusterInventory publish\n"; errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
	if len(f.calls) != 0 {
		t.Errorf("kubectl must not run: %v", f.calls)
	}
}

// bats: "publish skips cleanly when there is no cluster spec (deploy domains)"
func TestPublishSkipsWithoutSpec(t *testing.T) {
	p := realPaths(t)
	f := &fakeRunner{}
	var errBuf bytes.Buffer
	Publish(context.Background(), p, f, &errBuf, "dep", filepath.Join(p.Clusters, "dep", "cluster.lok8s.yaml"), filepath.Join(p.Base, "kc.yaml"))
	if strings.Contains(errBuf.String(), "error") || len(f.calls) != 0 {
		t.Errorf("stderr=%q calls=%v", errBuf.String(), f.calls)
	}
}

// bats: "publish warns and returns 0 when the cluster is unreachable (kubectl fails)"
// — THE fail-soft pin: a failing kubectl yields a warning, never a failure.
func TestPublishUnreachableClusterWarns(t *testing.T) {
	p := realPaths(t)
	spec := softSpec(t, p, "unreach")
	kc := filepath.Join(p.Base, "kc.yaml")
	if err := os.WriteFile(kc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{fail: true}
	var errBuf bytes.Buffer
	Publish(context.Background(), p, f, &errBuf, "unreach", spec, kc)
	if want := "\033[0;33m[warn]\033[0m inventory: could not apply the ClusterInventory CRD (cluster unreachable, RBAC, or a conflicting CRD) — skipping publish\n"; errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
	if len(f.calls) != 1 {
		t.Errorf("must stop after the CRD apply: %v", f.calls)
	}
}

// bats: "publish applies the CRD then the CR via server-side apply (field manager lok8s)"
func TestPublishHappyPathArgv(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	p := realPaths(t)
	spec := softSpec(t, p, "happy")
	kc := filepath.Join(p.Base, "kc.yaml")
	if err := os.WriteFile(kc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	var errBuf bytes.Buffer
	Publish(context.Background(), p, f, &errBuf, "happy", spec, kc)
	crd := filepath.Join(p.Lok8s, "libs", "inventory", "manifests", "clusterinventory.crd.yaml")
	want := []string{
		"kubectl --kubeconfig " + kc + " apply --server-side --field-manager=lok8s -f " + crd,
		"kubectl --kubeconfig " + kc + " wait --for=condition=Established crd/clusterinventories.lok8s.dev --timeout=30s",
		"kubectl --kubeconfig " + kc + " apply --server-side --field-manager=lok8s -f -",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	cr, _ := BuildJSON(p, io.Discard, "happy", spec)
	if f.stdin[2] != cr+"\n" {
		t.Errorf("CR on stdin = %q", f.stdin[2])
	}
	if errBuf.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errBuf.String())
	}
}

// The CR apply failing (CRD fine) is the second fail-soft branch.
func TestPublishCRApplyFailureWarns(t *testing.T) {
	p := realPaths(t)
	spec := softSpec(t, p, "crfail")
	kc := filepath.Join(p.Base, "kc.yaml")
	if err := os.WriteFile(kc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &crFailRunner{}
	var errBuf bytes.Buffer
	Publish(context.Background(), p, f, &errBuf, "crfail", spec, kc)
	if want := "\033[0;33m[warn]\033[0m inventory: failed to publish the ClusterInventory for crfail (provision/deploy unaffected)\n"; errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
}

type crFailRunner struct{ n int }

func (f *crFailRunner) Run(ctx context.Context, c execx.Cmd) error {
	f.n++
	if c.Stdin != nil {
		_, _ = io.Copy(io.Discard, c.Stdin)
		return errors.New("exit status 1")
	}
	return nil
}

// bats: "publish uses the .lok8s CRD mirror (consumer repos vendor only .lok8s/**)"
func TestCRDManifestPrefersLok8sMirror(t *testing.T) {
	base := t.TempDir()
	p := &config.Paths{Base: base, Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	mirror := filepath.Join(p.Lok8s, "libs", "inventory", "manifests", "clusterinventory.crd.yaml")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mirror, []byte("kind: CustomResourceDefinition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := CRDManifest(p)
	if !ok || got != mirror {
		t.Errorf("CRDManifest = %q, %v", got, ok)
	}
	// No manifest anywhere → publish warns and skips.
	p2 := &config.Paths{Base: t.TempDir(), Lok8s: filepath.Join(t.TempDir(), ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	spec := writeSpec(t, p2, "nocrd", "kind: KubeOne\nmetadata: { name: nocrd }\n")
	kc := filepath.Join(p2.Base, "kc.yaml")
	_ = os.WriteFile(kc, nil, 0o644)
	var errBuf bytes.Buffer
	f := &fakeRunner{}
	Publish(context.Background(), p2, f, &errBuf, "nocrd", spec, kc)
	if want := "\033[0;33m[warn]\033[0m inventory: ClusterInventory CRD manifest not found — skipping publish\n"; errBuf.String() != want || len(f.calls) != 0 {
		t.Errorf("stderr=%q calls=%v", errBuf.String(), f.calls)
	}
}

// addons::_category picks the LC_ALL=C-smallest match and strips the
// prefix + whitespace; symlinks below the dir are not followed.
func TestCategoryHelper(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("labels:\n  lok8s.dev/category: zeta\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("x: lok8s.dev/category:\talpha-1 trailing\n"), 0o644)
	if got := category(dir); got != "alpha-1" {
		t.Errorf("category = %q, want alpha-1", got)
	}
	if got := category(filepath.Join(dir, "nope")); got != "-" {
		t.Errorf("missing dir category = %q, want -", got)
	}
	if got := chartVersion(dir); got != "-" {
		t.Errorf("no chart.yaml → %q, want -", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "chart.yaml"), []byte("name: x\nversion: 1.10\n"), 0o644)
	if got := chartVersion(dir); got != "1.10" {
		t.Errorf("chartVersion keeps the raw scalar text: %q", got)
	}
}

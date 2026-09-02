package capi

// generate_test.go — the port of tests/unit/capi_test.bats: provider
// detection, resource generation (pinned against a golden rendered ONCE by
// the bash capi::generate — see the header in
// testdata/generate_hetzner.golden's provenance note below), credential
// Secrets, wait_ready, and the template-variable drift gates.
//
// GOLDEN PROVENANCE: testdata/generate_hetzner.golden is the byte-exact
// stdout of the BASH generator,
//
//	source .lok8s/{utils/{verbose,template,spec,credentials,provider}.sh,
//	       drivers/capi/generate}
//	capi::generate tests/fixtures/capi-cluster.lok8s.yaml hetzner
//
// captured read-only (no cluster commands run). Regenerate only when the
// templates or the bash generator change, from the bash — bash wins.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/execx"
)

func hetznerFixture(t *testing.T) string { return fixturePath(t, "capi-cluster.lok8s.yaml") }

// ── DetectProvider ────────────────────────────────────────

func TestDetectProviderExplicitName(t *testing.T) {
	d, _, _ := testDriver(t)
	got, err := d.DetectProvider(hetznerFixture(t))
	if err != nil || got != "hetzner" {
		t.Fatalf("got %q, %v; want hetzner", got, err)
	}
}

func TestDetectProviderLegacyHcloud(t *testing.T) {
	d, _, _ := testDriver(t)
	spec := writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: x}\nspec:\n  hcloud:\n    region: fsn1\n")
	got, err := d.DetectProvider(spec)
	if err != nil || got != "hetzner" {
		t.Fatalf("got %q, %v; want hetzner", got, err)
	}
}

func TestDetectProviderLegacyAWS(t *testing.T) {
	d, _, _ := testDriver(t)
	spec := writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: x}\nspec:\n  aws:\n    region: eu-central-1\n")
	got, err := d.DetectProvider(spec)
	if err != nil || got != "aws" {
		t.Fatalf("got %q, %v; want aws", got, err)
	}
}

func TestDetectProviderFailsForUnknown(t *testing.T) {
	d, _, stderr := testDriver(t)
	spec := writeSpec(t, d, "test.dev", "kind: Capi\nmetadata: {name: x}\nspec:\n  kubernetes:\n    version: v1.31.10\n")
	if _, err := d.DetectProvider(spec); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "No provider found in cluster spec") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// ── Generate ──────────────────────────────────────────────

func TestGenerateMatchesBashGolden(t *testing.T) {
	d, _, _ := testDriver(t)
	got, err := d.Generate(hetznerFixture(t), "hetzner")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "generate_hetzner.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(golden) {
		t.Fatalf("rendered stream diverges from the bash golden.\nGot %d bytes, want %d.\nFirst divergence: %s",
			len(got), len(golden), firstDiff(got, string(golden)))
	}
	// The bats content pins, kept as belt-and-braces over the byte pin.
	for _, want := range []string{"kind: Cluster", "kind: KubeadmControlPlane", "kind: HetznerCluster", "kind: MachineDeployment"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The rewritten generator targets hcloud only; the hcloud output must
	// never contain a bare-metal template.
	if strings.Contains(got, "HetznerBareMetalMachineTemplate") {
		t.Error("hrobot template rendered (hcloud only)")
	}
}

func firstDiff(a, b string) string {
	max := len(a)
	if len(b) < max {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		if a[i] != b[i] {
			start := i - 40
			if start < 0 {
				start = 0
			}
			return "byte " + strings.TrimSpace(a[start:i]) + " ⇒ got " + a[i:min(i+40, len(a))] + " | want " + b[i:min(i+40, len(b))]
		}
	}
	return "length mismatch"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestGenerateLeavesProcessEnvUntouched(t *testing.T) {
	// POST-REVIEW finding 6: the bash confined the exports to a subshell so
	// CLUSTER_NAME / K8S_VERSION never leak (they are read by the kubeone
	// driver; a leaked value renders the WRONG cluster on the next call in
	// the same process). The Go render never touches the process env — pin
	// exactly that.
	for _, v := range TemplateVars {
		if err := os.Unsetenv(v); err != nil {
			t.Fatal(err)
		}
	}
	d, _, _ := testDriver(t)
	out, err := d.Generate(hetznerFixture(t), "hetzner")
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the render really used the values.
	if !strings.Contains(out, "name: test-production") {
		t.Fatalf("the render produced nothing recognisable")
	}
	for _, v := range TemplateVars {
		if got, set := os.LookupEnv(v); set {
			t.Errorf("Generate leaked %s=%q into the process environment", v, got)
		}
	}
}

func TestGenerateFailsForMissingTemplateDir(t *testing.T) {
	d, _, stderr := testDriver(t)
	d.deps.Paths.Lok8s = filepath.Join(t.TempDir(), "nolok8s")
	if _, err := d.Generate(hetznerFixture(t), "hetzner"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "CAPI template directory not found") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestGenerateFailsForUnsupportedProvider(t *testing.T) {
	for _, provider := range []string{"gcp", "aws"} {
		d, _, stderr := testDriver(t)
		if _, err := d.Generate(hetznerFixture(t), provider); err == nil {
			t.Fatalf("%s: expected error", provider)
		}
		if !strings.Contains(stderr.String(), "not supported yet") {
			t.Fatalf("%s: stderr = %q", provider, stderr.String())
		}
	}
}

func TestGenerateRequiresSSHKeyName(t *testing.T) {
	d, _, stderr := testDriver(t)
	spec := writeSpec(t, d, "test.dev", `kind: Capi
metadata: {name: nokey}
spec:
  kubernetes: {version: v1.31.10}
  cluster: {domain: nokey.dev}
  provider:
    name: hetzner
    config: {region: fsn1}
`)
	if _, err := d.Generate(spec, "hetzner"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "spec.provider.config.sshKeyName is required for the hetzner CAPI provider") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestGenerateFailsOnBadPoolNameWithoutPartialStream(t *testing.T) {
	// The dispatch runs the driver with errexit-off semantics, so an
	// unguarded render once flowed on as a control plane with no workers.
	d, _, _ := testDriver(t)
	raw, err := os.ReadFile(hetznerFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), "  workers:\n    general:", "  workers:\n    \"-nope\":", 1)
	if bad == string(raw) {
		t.Fatal("fixture edit did not apply")
	}
	spec := writeSpec(t, d, "test.dev", bad)
	out, err := d.Generate(spec, "hetzner")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(out, "kind: Cluster") {
		t.Fatal("a partial manifest stream escaped the failed render")
	}
}

// ── Placement groups ──────────────────────────────────────

func TestGeneratePlacementGroups(t *testing.T) {
	d, _, _ := testDriver(t)
	raw, err := os.ReadFile(hetznerFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	pg := strings.Replace(string(raw), "    config:\n", "    config:\n      placementGroups: true\n", 1)
	if pg == string(raw) {
		t.Fatal("fixture edit did not apply")
	}
	spec := writeSpec(t, d, "test.dev", pg)
	out, err := d.Generate(spec, "hetzner")
	if err != nil {
		t.Fatal(err)
	}
	docs := yamlDocs(t, out)
	var hetznerCluster, cpTmpl, workerTmpl map[string]any
	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		name := docName(doc)
		switch {
		case kind == "HetznerCluster":
			hetznerCluster = doc
		case kind == "HCloudMachineTemplate" && strings.HasSuffix(name, "-control-plane"):
			cpTmpl = doc
		case kind == "HCloudMachineTemplate":
			workerTmpl = doc
		}
	}
	pgList, _ := dig(hetznerCluster, "spec", "hcloudPlacementGroups").([]any)
	if len(pgList) != 2 {
		t.Fatalf("hcloudPlacementGroups = %v", pgList)
	}
	for i, want := range []map[string]string{
		{"name": "control-plane", "type": "spread"},
		{"name": "workers", "type": "spread"},
	} {
		got, _ := pgList[i].(map[string]any)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("placement group %d: %s = %v, want %s", i, k, got[k], v)
			}
		}
	}
	if got := dig(cpTmpl, "spec", "template", "spec", "placementGroupName"); got != "control-plane" {
		t.Errorf("control-plane template placementGroupName = %v", got)
	}
	if got := dig(workerTmpl, "spec", "template", "spec", "placementGroupName"); got != "workers" {
		t.Errorf("worker template placementGroupName = %v", got)
	}

	// The documents the yq expression does NOT touch stay byte-identical to
	// the plain golden (the bash pipe left them untouched too).
	golden, err := os.ReadFile(filepath.Join("testdata", "generate_hetzner.golden"))
	if err != nil {
		t.Fatal(err)
	}
	plainDocs := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n---\n")
	pgDocs := strings.Split(strings.TrimSuffix(out, "\n"), "\n---\n")
	if len(plainDocs) != 7 || len(pgDocs) != 7 {
		t.Fatalf("doc counts: plain=%d pg=%d, want 7", len(plainDocs), len(pgDocs))
	}
	for _, i := range []int{0, 2, 4, 5} { // Cluster, KCP, MachineDeployment, KubeadmConfigTemplate
		if plainDocs[i] != pgDocs[i] {
			t.Errorf("untouched doc %d was reformatted by the placement-group injection", i)
		}
	}
}

func yamlDocs(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		t.Fatal("no YAML documents parsed")
	}
	return docs
}

func docName(doc map[string]any) string {
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, key := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[key]
	}
	return cur
}

// ── Template-variable drift gates (both ways) ─────────────

// renderedTemplates extracts the template files Generate actually feeds to
// the envsubst, read out of the driver SOURCE rather than restated here
// (restating them would be a third copy of the same fact — the bats gate
// derived them the same way from the bash source).
func renderedTemplates(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("generate.go")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(repoLok8s(t), "drivers", "capi", "cluster")
	re := regexp.MustCompile(`filepath\.Join\((core|prov), "([A-Za-z0-9._-]+\.yaml)"\)`)
	seen := map[string]bool{}
	var tpls []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		dir := filepath.Join(root, "core")
		if m[1] == "prov" {
			dir = filepath.Join(root, "providers", "hetzner")
		}
		p := filepath.Join(dir, m[2])
		if !seen[p] {
			seen[p] = true
			tpls = append(tpls, p)
		}
	}
	sort.Strings(tpls)
	return tpls
}

var placeholderRe = regexp.MustCompile(`\$\{[A-Z_][A-Z0-9_]*\}`)

// templatePlaceholders is every ${VAR} placeholder in the rendered
// templates. The cloud-init's own variables are written UNBRACED ($ARCH,
// $RUNC, $CONTAINERD, $KUBERNETES_VERSION) exactly so the whitelist can
// pass them through, which is what makes the braced form a reliable marker
// for "a lok8s template variable".
func templatePlaceholders(t *testing.T, tpls []string) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, tpl := range tpls {
		raw, err := os.ReadFile(tpl)
		if err != nil {
			t.Fatalf("extracted a path that is not a template: %s (%v)", tpl, err)
		}
		for _, m := range placeholderRe.FindAllString(string(raw), -1) {
			name := strings.Trim(m, "${}")
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestRenderedTemplatesAreDiscoverable(t *testing.T) {
	// ANTI-VACUITY for the two gates below: both compare a list against a
	// set derived from these files, and both pass trivially if the set is
	// empty.
	tpls := renderedTemplates(t)
	if len(tpls) < 7 {
		t.Fatalf("found only %d rendered template(s) in the driver source — the extraction is stale: %v", len(tpls), tpls)
	}
	phs := templatePlaceholders(t, tpls)
	if len(phs) < 10 {
		t.Fatalf("only %d placeholder(s) across %d templates — the placeholder sweep is broken", len(phs), len(tpls))
	}
}

func TestEveryPlaceholderIsInTemplateVars(t *testing.T) {
	// THE gate the list's own comment promises. A placeholder that is not
	// on the whitelist is not substituted, so its literal ${NAME} is
	// applied to the management cluster — the exact failure the list exists
	// to prevent.
	listed := map[string]bool{}
	for _, v := range TemplateVars {
		listed[v] = true
	}
	var untracked []string
	for _, ph := range templatePlaceholders(t, renderedTemplates(t)) {
		if !listed[ph] {
			untracked = append(untracked, ph)
		}
	}
	if len(untracked) > 0 {
		t.Fatalf("these placeholders are rendered but not in TemplateVars (the literal text would be applied): %v", untracked)
	}
}

func TestEveryTemplateVarIsUsed(t *testing.T) {
	// The other direction. A name left behind after a template drops it is
	// carried for nothing and reads as a live contract to the next author.
	used := map[string]bool{}
	for _, ph := range templatePlaceholders(t, renderedTemplates(t)) {
		used[ph] = true
	}
	var dead []string
	for _, v := range TemplateVars {
		if !used[v] {
			dead = append(dead, v)
		}
	}
	if len(dead) > 0 {
		t.Fatalf("these TemplateVars entries appear in no rendered template: %v", dead)
	}
}

// ── EnsureCredentialsSecret ───────────────────────────────

func TestEnsureCredentialsHetznerSecret(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "test-token")
	t.Setenv("HROBOT_USER", "")
	t.Setenv("HROBOT_PASSWORD", "")
	d, runner, _ := testDriver(t)
	if err := d.EnsureCredentialsSecret(context.Background(), hetznerFixture(t), "hetzner", "/tmp/kubeconfig.yaml"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want create|apply pair", len(runner.calls))
	}
	create := argvLine(runner.calls[0])
	for _, want := range []string{
		"kubectl create secret generic test-production-credentials",
		"--namespace capi-system",
		"--kubeconfig /tmp/kubeconfig.yaml",
		"--from-literal=hcloud-token=test-token",
		"--from-literal=robot-user=",
		"--from-literal=robot-password=",
		"--dry-run=client -o yaml",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("create argv missing %q:\n%s", want, create)
		}
	}
	if got := argvLine(runner.calls[1]); got != "kubectl apply --kubeconfig /tmp/kubeconfig.yaml -f -" {
		t.Errorf("apply argv = %q", got)
	}
}

func TestEnsureCredentialsUnknownProvider(t *testing.T) {
	d, runner, stderr := testDriver(t)
	if err := d.EnsureCredentialsSecret(context.Background(), hetznerFixture(t), "gcp", "/tmp/kubeconfig.yaml"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "unknown provider 'gcp' for credential check") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatal("no kubectl call may happen for an unknown provider")
	}
}

func TestEnsureCredentialsAWSSecret(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_REGION", "eu-central-1")
	d, runner, _ := testDriver(t)
	if err := d.EnsureCredentialsSecret(context.Background(), hetznerFixture(t), "aws", "/tmp/kubeconfig.yaml"); err != nil {
		t.Fatal(err)
	}
	create := argvLine(runner.calls[0])
	for _, want := range []string{
		"--from-literal=access-key-id=AKIAIOSFODNN7EXAMPLE",
		"--from-literal=region=eu-central-1",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("create argv missing %q", want)
		}
	}
}

func TestEnsureCredentialsAWSMissingKey(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_REGION", "")
	d, _, stderr := testDriver(t)
	if err := d.EnsureCredentialsSecret(context.Background(), hetznerFixture(t), "aws", "/tmp/kubeconfig.yaml"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr.String(), "AWS_ACCESS_KEY_ID") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// ── WaitReady ─────────────────────────────────────────────

func TestWaitReadyProvisioned(t *testing.T) {
	d, runner, _ := testDriver(t)
	runner.handler = func(c execx.Cmd, stdin string) error {
		if c.Stdout != nil {
			c.Stdout.Write([]byte("Provisioned"))
		}
		return nil
	}
	if err := d.WaitReady(context.Background(), "/tmp/kubeconfig.yaml", "test-cluster", "", 5); err != nil {
		t.Fatal(err)
	}
	want := "kubectl get cluster test-cluster --namespace default --kubeconfig /tmp/kubeconfig.yaml -o jsonpath={.status.phase}"
	if got := argvLine(runner.calls[0]); got != want {
		t.Errorf("argv = %q\nwant  %q", got, want)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	d, runner, stderr := testDriver(t)
	runner.handler = func(c execx.Cmd, stdin string) error {
		if c.Stdout != nil {
			c.Stdout.Write([]byte("Pending"))
		}
		return nil
	}
	if err := d.WaitReady(context.Background(), "/tmp/kubeconfig.yaml", "test-cluster", "", 1); err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(stderr.String(), "Timed out waiting for cluster test-cluster (1s)") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

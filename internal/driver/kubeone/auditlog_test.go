package kubeone

// auditlog_test.go pins the spec.auditLog merge of kubeone::generate_config
// (drivers/kubeone/config, kubeone::_inject_audit_log): the
// features.staticAuditLog fields, the absolute policy path, the siblings
// that must survive, the limits that stay unset, and the refusals. The
// bats twin is tests/unit/kubeone_audit_log_test.bats.

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

const testAuditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
    resources:
      - group: ""
        resources: ["secrets"]
  - level: None
`

// auditSpec writes the cluster spec with the given spec.auditLog block
// (indented under spec) and the policy file next to it. It returns the
// cluster file and the policy file's absolute path.
func auditSpec(t *testing.T, cy, auditLog string) string {
	t.Helper()
	testutil.WriteFile(t, cy, testSpecYAML+auditLog)
	policy := filepath.Join(filepath.Dir(cy), "audit-policy.yaml")
	testutil.WriteFile(t, policy, testAuditPolicy)
	return policy
}

func staticAuditLog(t *testing.T, outDir string) map[string]any {
	t.Helper()
	doc := parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))
	sal, ok := dig(t, doc, "features", "staticAuditLog").(map[string]any)
	if !ok {
		t.Fatalf("features.staticAuditLog missing: %v", doc["features"])
	}
	return sal
}

func TestGenerateConfigInjectsAuditLog(t *testing.T) {
	d, cy, outDir := genDriver(t)
	policy := auditSpec(t, cy, `  oidc:
    issuer: https://id.kubehz.dev
    clientID: kubectl
  auditLog:
    policy: audit-policy.yaml
    maxAge: 30
    maxBackup: 10
    maxSize: 100
`)
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	sal := staticAuditLog(t, outDir)
	if sal["enable"] != true {
		t.Errorf("staticAuditLog.enable = %v, want true", sal["enable"])
	}
	cfg := sal["config"].(map[string]any)
	if got := cfg["policyFilePath"]; got != policy {
		t.Errorf("policyFilePath = %v, want %s", got, policy)
	}
	if p, _ := cfg["policyFilePath"].(string); !filepath.IsAbs(p) {
		t.Errorf("policyFilePath %q is not absolute", p)
	}
	want := map[string]any{"logMaxAge": 30, "logMaxBackup": 10, "logMaxSize": 100}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("config.%s = %#v, want the integer %v", k, cfg[k], v)
		}
	}
	// KubeOne's default log path applies: the driver never sets logPath.
	if _, set := cfg["logPath"]; set {
		t.Errorf("logPath set: %v", cfg["logPath"])
	}
	// The sibling features survive the merge.
	doc := parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))
	if got := dig(t, doc, "features", "encryptionProviders", "enable"); got != true {
		t.Errorf("encryptionProviders lost in merge: %v", got)
	}
	if got := dig(t, doc, "features", "openidConnect", "enable"); got != true {
		t.Errorf("openidConnect lost in merge: %v", got)
	}
}

func TestGenerateConfigAuditLogOmitsUnsetLimits(t *testing.T) {
	d, cy, outDir := genDriver(t)
	// maxAge without a value reads as null: unset, like an absent field.
	policy := auditSpec(t, cy, "  auditLog:\n    policy: audit-policy.yaml\n    maxAge:\n    maxBackup: 7\n")
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	cfg := staticAuditLog(t, outDir)["config"].(map[string]any)
	if len(cfg) != 2 || cfg["policyFilePath"] != policy || cfg["logMaxBackup"] != 7 {
		t.Errorf("config = %v, want only policyFilePath and logMaxBackup (KubeOne defaults the rest)", cfg)
	}
}

func TestGenerateConfigAuditLogResolvesPathAgainstClusterDir(t *testing.T) {
	d, cy, outDir := genDriver(t)
	// A policy shared by several domains, one level above the cluster dir.
	shared := filepath.Join(filepath.Dir(filepath.Dir(cy)), "shared", "audit-policy.yaml")
	testutil.WriteFile(t, shared, testAuditPolicy)
	testutil.WriteFile(t, cy, testSpecYAML+"  auditLog:\n    policy: ./sub/../../shared/audit-policy.yaml\n")
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	cfg := staticAuditLog(t, outDir)["config"].(map[string]any)
	if cfg["policyFilePath"] != shared {
		t.Errorf("policyFilePath = %v, want the cleaned path %s", cfg["policyFilePath"], shared)
	}
}

func TestGenerateConfigAuditLogKeepsTemplateConfig(t *testing.T) {
	// A project's ejected template may already carry the feature, e.g. a
	// logPath. The merge sets enable + policyFilePath and keeps the rest.
	d, cy, outDir := genDriver(t)
	anchor := "  encryptionProviders:\n    enable: true\n"
	core := realCoreTemplate(t)
	if !strings.Contains(core, anchor) {
		t.Fatalf("core template lost its encryptionProviders block:\n%s", core)
	}
	core = strings.Replace(core, anchor, anchor+"  staticAuditLog:\n    enable: false\n    config:\n      logPath: /var/log/audit/kube.log\n", 1)
	testutil.WriteFile(t, filepath.Join(d.deps.Paths.Lok8s, "drivers", "kubeone", "cluster", "core", "kubeone.yaml"), core)
	policy := auditSpec(t, cy, "  auditLog:\n    policy: audit-policy.yaml\n")
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	sal := staticAuditLog(t, outDir)
	cfg := sal["config"].(map[string]any)
	if sal["enable"] != true || cfg["policyFilePath"] != policy || cfg["logPath"] != "/var/log/audit/kube.log" {
		t.Errorf("staticAuditLog = %v, want enable true, the policy and the template's logPath", sal)
	}
}

func TestGenerateConfigAuditLogAbsentLeavesManifestUntouched(t *testing.T) {
	d, cy, outDir := genDriver(t)
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	features := parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))["features"].(map[string]any)
	if _, present := features["staticAuditLog"]; present {
		t.Fatal("no spec.auditLog ⇒ NO staticAuditLog")
	}
	// An empty value reads as null: the same as an absent block.
	testutil.WriteFile(t, cy, testSpecYAML+"  auditLog:\n")
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	features = parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))["features"].(map[string]any)
	if _, present := features["staticAuditLog"]; present {
		t.Fatal("spec.auditLog: null ⇒ NO staticAuditLog")
	}
}

// assertNoManifest: a refused render leaves no kubeone.yaml and no temp
// file behind (the render writes a temp file and renames it only on
// success).
func assertNoManifest(t *testing.T, outDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(outDir, "kubeone.yaml")); !os.IsNotExist(err) {
		t.Errorf("a refused render left %s/kubeone.yaml (stat err %v)", outDir, err)
	}
	if left, _ := filepath.Glob(filepath.Join(outDir, ".kubeone.yaml.*")); len(left) > 0 {
		t.Errorf("a refused render left temp files: %v", left)
	}
}

func TestGenerateConfigAuditLogRefusesBadSpec(t *testing.T) {
	cases := []struct {
		name     string
		auditLog string
		wantErr  string
	}{
		{"not a mapping", "  auditLog: true\n",
			"spec.auditLog in CY is not a mapping. Set spec.auditLog.policy to the path of an audit Policy file."},
		{"unknown field", "  auditLog:\n    policy: audit-policy.yaml\n    maxBackups: 3\n",
			"spec.auditLog.maxBackups in CY is not a known field. Use policy, maxAge, maxBackup or maxSize."},
		{"policy missing", "  auditLog:\n    maxAge: 3\n",
			"spec.auditLog.policy in CY is not set. Set it to the path of an audit Policy file, relative to the cluster file."},
		{"policy empty", "  auditLog:\n    policy: \"\"\n",
			"spec.auditLog.policy in CY is not set."},
		{"policy not text", "  auditLog:\n    policy: [a.yaml]\n",
			"spec.auditLog.policy in CY is not a file path (found !!seq). Set it to the path of an audit Policy file, relative to the cluster file."},
		{"limit zero", "  auditLog:\n    policy: audit-policy.yaml\n    maxAge: 0\n",
			"spec.auditLog.maxAge in CY must be a whole number from 1 to 999999999 (found !!int '0'). Fix the value, or remove the field to use the KubeOne default."},
		{"limit negative", "  auditLog:\n    policy: audit-policy.yaml\n    maxSize: -5\n",
			"spec.auditLog.maxSize in CY must be a whole number from 1 to 999999999 (found !!int '-5')."},
		{"limit as text", "  auditLog:\n    policy: audit-policy.yaml\n    maxBackup: \"10\"\n",
			"spec.auditLog.maxBackup in CY must be a whole number from 1 to 999999999 (found !!str '10')."},
		{"limit fraction", "  auditLog:\n    policy: audit-policy.yaml\n    maxSize: 1.5\n",
			"(found !!float '1.5')"},
		{"limit mapping", "  auditLog:\n    policy: audit-policy.yaml\n    maxAge: {days: 3}\n",
			"(found !!map)"},
		{"limit too large", "  auditLog:\n    policy: audit-policy.yaml\n    maxAge: 1000000000\n",
			"(found !!int '1000000000')"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cy, outDir := genDriver(t)
			errBuf := d.deps.Stderr.(interface{ String() string })
			auditSpec(t, cy, tc.auditLog)
			if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err == nil {
				t.Fatal("expected failure")
			}
			want := strings.ReplaceAll(tc.wantErr, "CY", cy)
			if !strings.Contains(errBuf.String(), want) {
				t.Errorf("stderr = %q\nwant  %q", errBuf.String(), want)
			}
			assertNoManifest(t, outDir)
		})
	}
}

func TestGenerateConfigAuditLogRefusesBadPolicyFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		dir     bool
		missing bool
		wantErr string
	}{
		{name: "missing", missing: true,
			wantErr: "audit log: policy file not found: POLICY (spec.auditLog.policy: audit-policy.yaml). Create the file, or fix spec.auditLog.policy in CY."},
		{name: "directory", dir: true, wantErr: "audit log: policy file not found: POLICY"},
		{name: "not a Policy", content: "apiVersion: audit.k8s.io/v1\nkind: Event\nrules: [{level: None}]\n",
			wantErr: "audit log: POLICY is not an audit Policy. The file must have apiVersion audit.k8s.io/v1 and kind Policy. Fix the file, or set spec.auditLog.policy to another file."},
		{name: "no rules", content: "apiVersion: audit.k8s.io/v1\nkind: Policy\n",
			wantErr: "audit log: POLICY has no rules. The apiserver does not start with a policy that has no rules. Add at least one rule to the rules list."},
		{name: "not YAML", content: "apiVersion: [\n",
			wantErr: "audit log: POLICY is not valid YAML. Fix the file, or set spec.auditLog.policy to another file."},
		{name: "directive", content: "%YAML 1.2\n---\n" + testAuditPolicy,
			wantErr: "audit log: POLICY has a YAML directive (a line that starts with %). lok8s reads a policy file only as plain YAML. Remove the directive."},
		{name: "duplicate key", content: "kind: Policy\n" + testAuditPolicy,
			wantErr: "audit log: POLICY has a duplicate key. lok8s reads a policy file only as plain YAML. Keep each key once."},
		{name: "alias", content: "apiVersion: audit.k8s.io/v1\nkind: Policy\nrules:\n  - &r {level: None}\n  - *r\n",
			wantErr: "audit log: POLICY has an alias (*name). lok8s reads a policy file only as plain YAML. Write the value out in full."},
		{name: "merge key", content: "<<: {apiVersion: audit.k8s.io/v1, kind: Policy}\nrules: [{level: None}]\n",
			wantErr: "audit log: POLICY has a merge key (<<). lok8s reads a policy file only as plain YAML. Write the keys out in full."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cy, outDir := genDriver(t)
			errBuf := d.deps.Stderr.(interface{ String() string })
			policy := auditSpec(t, cy, "  auditLog:\n    policy: audit-policy.yaml\n")
			switch {
			case tc.missing:
				_ = os.Remove(policy)
			case tc.dir:
				_ = os.Remove(policy)
				if err := os.Mkdir(policy, 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				testutil.WriteFile(t, policy, tc.content)
			}
			if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err == nil {
				t.Fatal("expected failure")
			}
			want := strings.ReplaceAll(strings.ReplaceAll(tc.wantErr, "POLICY", policy), "CY", cy)
			if !strings.Contains(errBuf.String(), want) {
				t.Errorf("stderr = %q\nwant  %q", errBuf.String(), want)
			}
			assertNoManifest(t, outDir)
		})
	}
}

func TestGenerateConfigAuditLogUnreadablePolicy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without read permission")
	}
	d, cy, outDir := genDriver(t)
	errBuf := d.deps.Stderr.(interface{ String() string })
	policy := auditSpec(t, cy, "  auditLog:\n    policy: audit-policy.yaml\n")
	if err := os.Chmod(policy, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(policy, 0o600) })
	err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir)
	if err == nil {
		t.Fatal("expected failure")
	}
	want := "audit log: cannot read the policy file " + policy + ". Make it readable for the user that runs lo."
	if !strings.Contains(errBuf.String(), want) {
		t.Errorf("stderr = %q\nwant  %q", errBuf.String(), want)
	}
	// The cause stays on the error chain.
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error %v does not wrap the permission error", err)
	}
}

func TestGenerateConfigAuditLogRefusalKeepsTheOldManifest(t *testing.T) {
	// A refused render must not replace (or truncate) the manifest of the
	// last good render.
	d, cy, outDir := genDriver(t)
	auditSpec(t, cy, "  auditLog:\n    policy: audit-policy.yaml\n")
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(outDir, "kubeone.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, cy, testSpecYAML+"  auditLog:\n    policy: audit-policy.yaml\n    maxAge: 0\n")
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err == nil {
		t.Fatal("expected failure")
	}
	now, err := os.ReadFile(filepath.Join(outDir, "kubeone.yaml"))
	if err != nil || string(now) != string(good) {
		t.Errorf("the last good manifest changed after a refused render (err %v)", err)
	}
	if left, _ := filepath.Glob(filepath.Join(outDir, ".kubeone.yaml.*")); len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestProvisionChecksAuditLogBeforeTheInfrastructure(t *testing.T) {
	clearVarEnv(t)
	prov := &fakeProvider{}
	d, _, errBuf, p := testDriver(t, prov)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, testSpecYAML+"  auditLog:\n    policy: audit-policy.yaml\n    maxBackups: 3\n")
	if err := d.Provision(t.Context(), "test.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "spec.auditLog.maxBackups") {
		t.Errorf("stderr = %q", errBuf.String())
	}
	for _, l := range prov.log {
		if strings.HasPrefix(l, "provision:") {
			t.Fatalf("the provider provisioned before the spec.auditLog check: %v", prov.log)
		}
	}
}

// auditPolicyFixtures lists tests/fixtures/audit-policy/<verdict>--<case>.yaml.
// The same files drive the bats twin and hack/parity-orchestrate.sh.
func auditPolicyFixtures(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(t), "tests", "fixtures", "audit-policy", "*--*.yaml"))
	if err != nil || len(files) < 20 {
		t.Fatalf("audit policy fixtures missing (%d found, err %v)", len(files), err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f] = strings.SplitN(filepath.Base(f), "--", 2)[0]
	}
	return out
}

func TestAuditPolicyVerdictFixtures(t *testing.T) {
	seen := map[string]bool{}
	for path, want := range auditPolicyFixtures(t) {
		got, _ := auditPolicyVerdict(path)
		if got != want {
			t.Errorf("%s: verdict %q, want %q", filepath.Base(path), got, want)
		}
		seen[want] = true
	}
	// Anti-vacuity: every verdict a file can get has a fixture.
	for _, v := range []string{policyOK, policyDirective, policyInvalid, policyDuplicate, policyAlias, policyMerge, policyNotPolicy, policyNoRules} {
		if !seen[v] {
			t.Errorf("no fixture for the verdict %q", v)
		}
	}
}

// TestAuditPolicyVerdictMatchesYq runs the bash twin's own verdict
// function on every fixture and compares it with the Go verdict. It needs
// bash and the pinned yq (.bin/yq); it skips without them, and fails
// without them under CI=true.
func TestAuditPolicyVerdictMatchesYq(t *testing.T) {
	root := testutil.RepoRoot(t)
	yq := filepath.Join(root, ".bin", "yq")
	if _, err := os.Stat(yq); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("the pinned yq is missing under CI: %v", err)
		}
		t.Skip("no .bin/yq (b install)")
	}
	config := filepath.Join(root, ".lok8s", "drivers", "kubeone", "config")
	for path := range auditPolicyFixtures(t) {
		goVerdict, _ := auditPolicyVerdict(path)
		// Source the twin with the argsh pieces stubbed: only the verdict
		// function runs.
		script := `import() { :; }; source "$1"; kubeone::_audit_policy_verdict "$2"`
		cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", config, path)
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, ".bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: the bash verdict failed: %v", filepath.Base(path), err)
		}
		if bashVerdict := strings.TrimSpace(string(out)); bashVerdict != goVerdict {
			t.Errorf("%s: Go %q, bash %q", filepath.Base(path), goVerdict, bashVerdict)
		}
	}
}

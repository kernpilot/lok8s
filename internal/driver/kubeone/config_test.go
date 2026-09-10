package kubeone

// config_test.go pins kubeone::generate_config against
// drivers/kubeone/config: the whitelisted core-template render, the
// dynamicWorkers block, the OIDC injection and the registry-auth secretRef
// resolution. The spec defaults and the https boundary rule of
// kubeone::extract_vars live in vars_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func writeSpec(t *testing.T, d *Driver, content string) string {
	t.Helper()
	path := filepath.Join(d.deps.Paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, path, content)
	return path
}

// ── GenerateConfig ────────────────────────────────────────

func genDriver(t *testing.T) (*Driver, string, string) {
	t.Helper()
	clearVarEnv(t)
	d, _, _, p := testDriver(t, nil)
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "drivers", "kubeone", "cluster", "core", "kubeone.yaml"), realCoreTemplate(t))
	outDir := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	return d, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), outDir
}

func parseManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("manifest does not parse: %v\n%s", err, raw)
	}
	return doc
}

func dig(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: not a map at %q", keys, k)
		}
		cur = mm[k]
	}
	return cur
}

func TestGenerateConfigRendersCoreTemplate(t *testing.T) {
	d, cy, outDir := genDriver(t)
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	doc := parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))
	if doc["name"] != "test-prod" {
		t.Errorf("name = %v", doc["name"])
	}
	if got := dig(t, doc, "versions", "kubernetes"); got != "v1.35.5" {
		t.Errorf("versions.kubernetes = %v", got)
	}
	if _, ok := dig(t, doc, "cloudProvider", "hetzner").(map[string]any); !ok {
		// `hetzner: {}` decodes to an empty map
		t.Errorf("cloudProvider.hetzner missing: %v", doc["cloudProvider"])
	}
	if got := dig(t, doc, "clusterNetwork", "kubeProxy", "skipInstallation"); got != false {
		t.Errorf("kubeProxy.skipInstallation = %v", got)
	}
	if got := dig(t, doc, "clusterNetwork", "podSubnet"); got != "10.244.0.0/16" {
		t.Errorf("podSubnet = %v", got)
	}
}

func TestGenerateConfigDynamicWorkersHetzner(t *testing.T) {
	d, cy, outDir := genDriver(t)
	testutil.WriteFile(t, cy, testSpecYAML+`  datacenter: nbg1
  workers:
    pool-a:
      replicas: 3
      type: cx42
    pool-b:
      type: cx52
      image: debian-12
`)
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(outDir, "kubeone.yaml"))
	var doc struct {
		DynamicWorkers []struct {
			Name         string `yaml:"name"`
			Replicas     int    `yaml:"replicas"`
			ProviderSpec struct {
				CloudProviderSpec struct {
					ServerType string   `yaml:"serverType"`
					Location   string   `yaml:"location"`
					Image      string   `yaml:"image"`
					Networks   []string `yaml:"networks"`
				} `yaml:"cloudProviderSpec"`
			} `yaml:"providerSpec"`
		} `yaml:"dynamicWorkers"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse: %v\n%s", err, raw)
	}
	if len(doc.DynamicWorkers) != 2 {
		t.Fatalf("pools = %d, want 2\n%s", len(doc.DynamicWorkers), raw)
	}
	a := doc.DynamicWorkers[0]
	if a.Name != "pool-a" || a.Replicas != 3 {
		t.Errorf("pool-a = %+v", a)
	}
	cps := a.ProviderSpec.CloudProviderSpec
	if cps.ServerType != "cx42" || cps.Location != "nbg1" || cps.Image != "ubuntu-22.04" {
		t.Errorf("pool-a cps = %+v", cps)
	}
	if len(cps.Networks) != 1 || cps.Networks[0] != "test-prod" {
		t.Errorf("pool-a networks = %v", cps.Networks)
	}
	b := doc.DynamicWorkers[1]
	if b.Replicas != 1 || b.ProviderSpec.CloudProviderSpec.Image != "debian-12" {
		t.Errorf("pool-b = %+v", b)
	}
}

func TestGenerateConfigRejectsBadPools(t *testing.T) {
	cases := []struct {
		name    string
		workers string
		wantErr string
	}{
		{
			"invalid pool name",
			"  workers:\n    bad pool:\n      type: cx42\n",
			"Invalid worker pool name: bad pool (must be alphanumeric with hyphens)",
		},
		{
			"non-numeric replicas",
			"  workers:\n    pool-a:\n      replicas: many\n      type: cx42\n",
			"Invalid replicas for pool pool-a: many (must be numeric)",
		},
		{
			"missing type",
			"  workers:\n    pool-a:\n      replicas: 1\n",
			"Invalid server/instance type for pool pool-a:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cy, outDir := genDriver(t)
			errBuf := d.deps.Stderr.(interface{ String() string })
			testutil.WriteFile(t, cy, testSpecYAML+tc.workers)
			if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err == nil {
				t.Fatal("expected failure")
			}
			if !strings.Contains(errBuf.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want %q", errBuf.String(), tc.wantErr)
			}
		})
	}
}

func TestGenerateConfigUnsupportedWorkerProvider(t *testing.T) {
	d, cy, outDir := genDriver(t)
	testutil.WriteFile(t, cy, testSpecYAML+"  workers:\n    pool-a:\n      type: t3.large\n")
	if err := d.GenerateConfig(t.Context(), cy, "gcp", outDir); err == nil {
		t.Fatal("expected failure")
	}
}

func TestGenerateConfigInjectsOIDC(t *testing.T) {
	d, cy, outDir := genDriver(t)
	testutil.WriteFile(t, cy, testSpecYAML+`  oidc:
    issuer: https://id.kubehz.dev
    clientID: kubectl
`)
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	doc := parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))
	if got := dig(t, doc, "features", "openidConnect", "enable"); got != true {
		t.Fatalf("openidConnect.enable = %v", got)
	}
	cfg := dig(t, doc, "features", "openidConnect", "config").(map[string]any)
	want := map[string]any{
		"issuerUrl":      "https://id.kubehz.dev",
		"clientId":       "kubectl",
		"usernameClaim":  "sub",
		"usernamePrefix": "oidc:",
		"groupsClaim":    "groups",
		"groupsPrefix":   "oidc:",
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("config.%s = %v, want %v", k, cfg[k], v)
		}
	}
	// The pre-existing features block survives the merge.
	if got := dig(t, doc, "features", "encryptionProviders", "enable"); got != true {
		t.Errorf("encryptionProviders lost in merge: %v", got)
	}
}

func TestGenerateConfigOIDCAbsentLeavesManifestUntouched(t *testing.T) {
	d, cy, outDir := genDriver(t)
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	doc := parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))
	features := doc["features"].(map[string]any)
	if _, present := features["openidConnect"]; present {
		t.Fatal("no spec.oidc ⇒ NO openidConnect wiring")
	}
}

// ── Registry auth ─────────────────────────────────────────

const registriesSpec = testSpecYAML + `  registries:
    registry-1.docker.io:
      auth:
        secretRef:
          name: dockerio
`

func TestRegistryAuthNeedsDomainName(t *testing.T) {
	d, cy, outDir := genDriver(t)
	errBuf := d.deps.Stderr.(interface{ String() string })
	testutil.WriteFile(t, cy, registriesSpec)
	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "registry auth: DOMAIN_NAME not exported; cannot resolve registry secrets") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestRegistryAuthInjectsResolvedCreds(t *testing.T) {
	d, cy, outDir := genDriver(t)
	t.Setenv("DOMAIN_NAME", "test.lok8s.dev")
	testutil.WriteFile(t, cy, registriesSpec)
	secd := filepath.Join(d.deps.Paths.Clusters, "test.lok8s.dev", "secrets")
	testutil.WriteFile(t, filepath.Join(secd, "Secret.dockerio.provisioning.username"), "user1\n")
	testutil.WriteFile(t, filepath.Join(secd, "Secret.dockerio.provisioning.password"), "pass1\n")

	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err != nil {
		t.Fatal(err)
	}
	doc := parseManifest(t, filepath.Join(outDir, "kubeone.yaml"))
	auth := dig(t, doc, "containerRuntime", "containerd", "registries", "registry-1.docker.io", "auth").(map[string]any)
	if auth["username"] != "user1" || auth["password"] != "pass1" {
		t.Fatalf("auth = %v", auth)
	}
}

func TestRegistryAuthAllUnconfiguredIsError(t *testing.T) {
	d, cy, outDir := genDriver(t)
	errBuf := d.deps.Stderr.(interface{ String() string })
	t.Setenv("DOMAIN_NAME", "test.lok8s.dev")
	testutil.WriteFile(t, cy, registriesSpec) // no secret files → nothing configurable

	if err := d.GenerateConfig(t.Context(), cy, "hetzner", outDir); err == nil {
		t.Fatal("declared-but-unconfigurable registries are a breaking misconfig, not a silent anonymous fallback")
	}
	if !strings.Contains(errBuf.String(), "registry auth: secret provisioning/dockerio lacks username/password — 'registry-1.docker.io' left anonymous") {
		t.Errorf("missing per-host warn: %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "1 registry(ies) declared in spec.registries but none could be configured") {
		t.Errorf("missing final error: %q", errBuf.String())
	}
}

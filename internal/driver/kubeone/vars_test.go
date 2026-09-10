package kubeone

// vars_test.go covers the variable extraction in vars.go.

import (
	"os"
	"strings"
	"testing"
)

func TestExtractVarsDefaults(t *testing.T) {
	clearVarEnv(t)
	d, _, _, _ := testDriver(t, nil)
	cy := writeSpec(t, d, testSpecYAML)

	if err := d.ExtractVars(t.Context(), cy); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CLUSTER_NAME":                   "test-prod",
		"K8S_VERSION":                    "v1.35.5",
		"POD_SUBNET":                     "10.244.0.0/16",
		"SERVICE_SUBNET":                 "10.96.0.0/12",
		"CP_REPLICAS":                    "1",
		"CNI_PLUGIN":                     "canal",
		"CSI_PLUGIN":                     "external", // Ceph-first default (BREAKING vs old specs)
		"KUBE_PROXY_SKIP":                "false",
		"CLOUD_PROVIDER":                 "hetzner",
		"SSH_USER":                       "root",
		"SSH_PORT":                       "22",
		"ADDONS_ENABLED":                 "false",
		"ADDONS_PATH":                    "./addons",
		"LOK8S_SPEC_OIDC_USERNAMECLAIM":  "sub",
		"LOK8S_SPEC_OIDC_USERNAMEPREFIX": "oidc:",
		"LOK8S_SPEC_OIDC_GROUPSCLAIM":    "groups",
		"LOK8S_SPEC_OIDC_GROUPSPREFIX":   "oidc:",
		"LOK8S_SPEC_OIDC_ISSUER":         "",
		"LOK8S_SPEC_OIDC_CLIENTID":       "",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestExtractVarsKubeProxyEnum(t *testing.T) {
	clearVarEnv(t)
	d, _, errBuf, _ := testDriver(t, nil)

	cy := writeSpec(t, d, testSpecYAML+"  network:\n    kubeProxy: disabled\n")
	if err := d.ExtractVars(t.Context(), cy); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KUBE_PROXY_SKIP") != "true" {
		t.Fatalf("disabled → KUBE_PROXY_SKIP = %q, want true", os.Getenv("KUBE_PROXY_SKIP"))
	}

	// A YAML bool `kubeProxy: false` is NOT recognized as disable — yq's
	// `// "enabled"` treats false as falsy and yields the default.
	cy = writeSpec(t, d, testSpecYAML+"  network:\n    kubeProxy: false\n")
	if err := d.ExtractVars(t.Context(), cy); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KUBE_PROXY_SKIP") != "false" {
		t.Fatalf("bool false must read as the default 'enabled'")
	}

	// The string enum rejects anything else, verbatim message.
	cy = writeSpec(t, d, testSpecYAML+"  network:\n    kubeProxy: nope\n")
	if err := d.ExtractVars(t.Context(), cy); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "extract_vars: invalid spec.network.kubeProxy 'nope' (expected 'enabled' or 'disabled')") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestExtractVarsSSHFromProviderOutput(t *testing.T) {
	clearVarEnv(t)
	prov := &fakeProvider{output: []byte(`{"access":[{"user":"admin","port":2222,"privateKey":"~/.ssh/k","publicKey":"~/.ssh/k.pub"}]}`)}
	d, _, _, _ := testDriver(t, prov)
	cy := writeSpec(t, d, testSpecYAML)

	if err := d.ExtractVars(t.Context(), cy); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SSH_USER") != "admin" || os.Getenv("SSH_PORT") != "2222" {
		t.Fatalf("SSH from provider output: user=%q port=%q", os.Getenv("SSH_USER"), os.Getenv("SSH_PORT"))
	}
	if os.Getenv("SSH_PRIVATE_KEY") != "~/.ssh/k" {
		t.Fatalf("SSH_PRIVATE_KEY = %q", os.Getenv("SSH_PRIVATE_KEY"))
	}
}

func TestOIDCHTTPSBoundary(t *testing.T) {
	clearVarEnv(t)
	d, _, errBuf, _ := testDriver(t, nil)
	cy := writeSpec(t, d, testSpecYAML+"  oidc:\n    issuer: http://id.example.com\n    clientID: kubectl\n")
	if err := d.ExtractVars(t.Context(), cy); err == nil {
		t.Fatal("expected failure — plain-http issuer must not pass the boundary")
	}
	if !strings.Contains(errBuf.String(), "spec.oidc.issuer must be an https:// URL, got 'http://id.example.com'") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

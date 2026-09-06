package lo

// registrytls_test.go — port of tests/unit/registry_tls_test.bats: the
// spec.registries.tls knob (default true), the .registries.json tls/port
// fields + query helpers, registry-config http-block rendering, the
// Secret-plugin-driven cert mint (SAN list + extraction + remint skip), and
// the untrusted-CA nudge. The Secret plugin is a fake exec through the
// runner seam.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
)

func writeTLSSpec(t *testing.T, clustersDir, tls string) string {
	t.Helper()
	cy := filepath.Join(clustersDir, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-tls
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.50.0/24"
  registries:
    tls: `+tls+`
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
  runtime: kind
  bootstrap: []
`)
	return cy
}

func TestTLSDefaultsTrueWhenAbsent(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-shared.lok8s.yaml")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOK8S_REGISTRY_TLS") != "true" || os.Getenv("LOK8S_REGISTRY_PORT") != "443" {
		t.Fatalf("tls default wrong: TLS=%s PORT=%s",
			os.Getenv("LOK8S_REGISTRY_TLS"), os.Getenv("LOK8S_REGISTRY_PORT"))
	}
}

func TestTLSExplicitTrueAndFalse(t *testing.T) {
	_, _, errBuf, p := testDriver(t)

	cy := writeTLSSpec(t, p.Clusters, "true")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOK8S_REGISTRY_TLS") != "true" || os.Getenv("LOK8S_REGISTRY_PORT") != "443" {
		t.Fatal("tls: true must set port 443")
	}
	rf, err := regFile()
	if err != nil || !rf.TLS || rf.Port != 443 {
		t.Fatalf("json tls/port wrong: %+v", rf)
	}
	if rf.url("10.125.50.101") != "https://10.125.50.101" {
		t.Fatal("registry url must be https in TLS mode")
	}

	cy = writeTLSSpec(t, p.Clusters, "false")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOK8S_REGISTRY_TLS") != "false" || os.Getenv("LOK8S_REGISTRY_PORT") != "80" {
		t.Fatal("tls: false must keep port 80")
	}
	rf, err = regFile()
	if err != nil || rf.TLS || rf.Port != 80 {
		t.Fatalf("json tls/port wrong: %+v", rf)
	}
	if rf.url("10.125.50.101") != "http://10.125.50.101" {
		t.Fatal("registry url must be http in plain mode")
	}
}

func TestTLSNonBooleanRejected(t *testing.T) {
	_, _, _, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, `"maybe"`)
	var errBuf bytes.Buffer
	if err := readNetworkConfig(cy, &errBuf); err == nil {
		t.Fatal("non-boolean tls accepted")
	}
	if !strings.Contains(errBuf.String(), "spec.registries.tls must be true or false") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

// ── http-block rendering ─────────────────────────────────

func TestRenderRegistryConfigTLSSwapsHTTPBlock(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	buildYAML := filepath.Join(tmp, "build.yaml")
	writeFile(t, buildYAML, realRegistryTemplate(t, "build.yaml"))

	out, err := renderRegistryConfig(buildYAML, "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"addr: :443",
		"certificate: /etc/registry/certs/tls.crt",
		"key: /etc/registry/certs/tls.key",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// The original :80 listener must be gone.
	if strings.Contains(out, "addr: :80") {
		t.Fatalf(":80 listener survived the swap:\n%s", out)
	}
}

func TestRenderRegistryConfigPlainKeeps80(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	buildYAML := filepath.Join(tmp, "build.yaml")
	writeFile(t, buildYAML, realRegistryTemplate(t, "build.yaml"))

	out, err := renderRegistryConfig(buildYAML, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "addr: :80") {
		t.Fatalf(":80 missing:\n%s", out)
	}
	if strings.Contains(out, "tls:") || strings.Contains(out, "addr: :443") {
		t.Fatalf("plain mode grew a TLS block:\n%s", out)
	}
}

func TestRenderRegistryConfigMirrorKeepsRemoteURLUnderTLS(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mirrorYAML := filepath.Join(tmp, "mirror.yaml")
	writeFile(t, mirrorYAML, realRegistryTemplate(t, "mirror.yaml"))

	out, err := renderRegistryConfig(mirrorYAML, "https://registry-1.docker.io", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "remoteurl: https://registry-1.docker.io") ||
		!strings.Contains(out, "addr: :443") {
		t.Fatalf("mirror TLS render wrong:\n%s", out)
	}
}

// ── cert mint via the Secret plugin ──────────────────────

// stubSecretPlugin creates the plugin binary path on disk (executable — the
// mint stats it) and wires the fake runner to answer its exec: capture the
// manifest from stdin, emit a k8s Secret with base64 FAKECRT/FAKEKEY. This
// is the LO_RENDER=exec pipeline; the default in-process mint (the
// generator imported as a package) is covered by
// TestRegistriesTLSCertMintsInProcess.
func stubSecretPlugin(t *testing.T, runner *fakeRunner, base string) (pluginBin string, gotManifest *string) {
	t.Helper()
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	pluginHome := filepath.Join(base, ".kustomize")
	pluginBin = filepath.Join(pluginHome, "secrets.lok8s.dev", "v1", "secret", "Secret")
	writeFile(t, pluginBin, "#!/bin/sh\nexit 1\n") // never actually executed
	os.Chmod(pluginBin, 0o755)
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", pluginHome)
	t.Setenv("PATH_SECRETS", filepath.Join(base, ".secrets-store"))
	os.MkdirAll(filepath.Join(base, ".secrets-store"), 0o755)

	manifest := new(string)
	runner.handler = func(c execx.Cmd) error {
		if c.Name != pluginBin {
			return nil
		}
		var buf bytes.Buffer
		if c.Stdin != nil {
			buf.ReadFrom(c.Stdin)
		}
		*manifest = buf.String()
		// base64(FAKECRT) = RkFLRUNSVA==, base64(FAKEKEY) = RkFLRUtFWQ==
		writeOut(c, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: registries-tls\n  namespace: lok8s-system\ntype: kubernetes.io/tls\ndata:\n  tls.crt: RkFLRUNSVA==\n  tls.key: RkFLRUtFWQ==\n")
		return nil
	}
	return pluginBin, manifest
}

func TestRegistriesTLSCertBuildsSANsAndDrivesThePlugin(t *testing.T) {
	d, runner, errBuf, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, "true")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	_, manifest := stubSecretPlugin(t, runner, p.Base)

	if err := d.registriesTLSCert(context.Background(), errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}

	// Cert material extracted (base64-decoded) from the plugin's Secret.
	if got := readFileT(t, filepath.Join(p.Base, ".secrets", "tls", "registries", "tls.crt")); got != "FAKECRT" {
		t.Fatalf("tls.crt = %q", got)
	}
	if got := readFileT(t, filepath.Join(p.Base, ".secrets", "tls", "registries", "tls.key")); got != "FAKEKEY" {
		t.Fatalf("tls.key = %q", got)
	}

	// SANs handed to the plugin as cert.hosts: framework hostnames, mirror
	// domain, IPs.
	for _, want := range []string{"lok8s.local", "lok8s.cache", "docker.io", "10.125.50.101", "10.125.50.102"} {
		if !strings.Contains(*manifest, want) {
			t.Errorf("SAN %q missing from plugin manifest:\n%s", want, *manifest)
		}
	}
}

func TestRegistriesTLSCertRemintSkippedWhenSANsUnchanged(t *testing.T) {
	d, runner, errBuf, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, "true")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	_, manifest := stubSecretPlugin(t, runner, p.Base)

	if err := d.registriesTLSCert(context.Background(), errBuf); err != nil {
		t.Fatal(err)
	}
	*manifest = "" // detector: did the plugin run again?

	if err := d.registriesTLSCert(context.Background(), errBuf); err != nil {
		t.Fatal(err)
	}
	if *manifest != "" {
		t.Fatal("plugin re-invoked although the SAN set was unchanged")
	}
}

func TestRegistriesTLSCertNoopWhenTLSDisabled(t *testing.T) {
	d, _, errBuf, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, "false")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	if err := d.registriesTLSCert(context.Background(), errBuf); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(p.Base, ".secrets", "tls", "registries", "tls.crt")) {
		t.Fatal("plain mode minted a cert")
	}
}

func TestRegistriesTLSCertFailsFastWhenPluginMissing(t *testing.T) {
	d, _, errBuf, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, "true")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	t.Setenv(render.ModeEnv, string(render.ModeExec)) // only the exec pipeline needs the binary
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", filepath.Join(p.Base, ".kustomize-empty"))
	t.Setenv("PATH_SECRETS", filepath.Join(p.Base, ".secrets-store"))

	var vErr bytes.Buffer
	if err := d.registriesTLSCert(context.Background(), &vErr); err == nil {
		t.Fatal("missing plugin reconciled as success")
	}
	if !strings.Contains(vErr.String(), "Secret plugin is not built") {
		t.Fatalf("wrong error:\n%s", vErr.String())
	}
}

// TestRegistriesTLSCertMintsInProcess drives the DEFAULT pipeline: no
// plugin binary, no KUSTOMIZE_PLUGIN_HOME, no runner call — the imported
// secrets.lok8s.dev generator mints the leaf against a throwaway CAROOT
// (created on demand, like the dev CA) into a throwaway PATH_SECRETS store.
// The extracted tls.crt/tls.key must be a real pair: a leaf signed by that
// CA whose SANs are exactly the registry hostnames + IPs the exec path
// handed to the plugin, with the key matching the cert.
func TestRegistriesTLSCertMintsInProcess(t *testing.T) {
	d, runner, errBuf, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, "true")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	t.Setenv(render.ModeEnv, "")
	t.Setenv("CAROOT", filepath.Join(p.Base, "caroot"))
	t.Setenv("PATH_SECRETS", filepath.Join(p.Base, ".secrets-store"))
	os.MkdirAll(filepath.Join(p.Base, ".secrets-store"), 0o755)
	runner.handler = func(c execx.Cmd) error {
		t.Fatalf("in-process mint must not exec anything, ran %s %v", c.Name, c.Args)
		return nil
	}

	if err := d.registriesTLSCert(context.Background(), errBuf); err != nil {
		t.Fatalf("registriesTLSCert: %v\n%s", err, errBuf.String())
	}

	tlsDir := filepath.Join(p.Base, ".secrets", "tls", "registries")
	crtPEM, err := os.ReadFile(filepath.Join(tlsDir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(tlsDir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		t.Fatalf("tls.crt/tls.key are not a matching pair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Fatal("minted a CA, not a leaf")
	}
	// The same SAN set the exec path put into cert.hosts (see
	// TestRegistriesTLSCertBuildsSANsAndDrivesThePlugin): hostnames as DNS
	// SANs, addresses as IP SANs.
	for _, want := range []string{"lok8s.local", "lok8s.cache", "docker.io"} {
		found := false
		for _, dns := range leaf.DNSNames {
			found = found || dns == want
		}
		if !found {
			t.Errorf("DNS SAN %q missing (have %v)", want, leaf.DNSNames)
		}
	}
	for _, want := range []string{"10.125.50.101", "10.125.50.102"} {
		found := false
		for _, ip := range leaf.IPAddresses {
			found = found || ip.String() == want
		}
		if !found {
			t.Errorf("IP SAN %q missing (have %v)", want, leaf.IPAddresses)
		}
	}
	// Signed by the throwaway CA the mint created at CAROOT.
	caPEM, err := os.ReadFile(filepath.Join(p.Base, "caroot", "rootCA.pem"))
	if err != nil {
		t.Fatalf("CAROOT CA not created: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("rootCA.pem unparsable")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "lok8s.local",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("leaf does not verify against the CAROOT CA: %v", err)
	}
	// The generator's cache is the source of truth: the store now holds
	// the leaf under the Secret's name, and the .sans key makes the next
	// call a no-op (idempotence shared with the exec path).
	if !fileExists(filepath.Join(p.Base, ".secrets-store", "Secret.registries-tls.lok8s-system.tls.crt")) {
		t.Fatal("leaf not cached in PATH_SECRETS")
	}
	if got := readFileT(t, filepath.Join(tlsDir, ".sans")); !strings.Contains(got, "lok8s.local") {
		t.Fatalf(".sans = %q", got)
	}
	if err := d.registriesTLSCert(context.Background(), errBuf); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(tlsDir, "tls.crt")); !bytes.Equal(again, crtPEM) {
		t.Fatal("unchanged SAN set re-minted the cert")
	}
}

// ── untrusted-CA nudge ───────────────────────────────────

func TestRegistriesTLSNudgeWarnsWhenCAUntrusted(t *testing.T) {
	d, runner, errBuf, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, "true")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAROOT", t.TempDir()) // no rootCA.pem → untrusted
	// The nudge probes for openssl via Look — plant a fake in the project
	// .bin so the probe finds it; the verify itself goes through the fake
	// runner (never invoked here since the CA file is absent).
	writeFile(t, filepath.Join(p.Bin, "openssl"), "#!/bin/sh\nexit 1\n")
	os.Chmod(filepath.Join(p.Bin, "openssl"), 0o755)
	_ = runner

	var vErr bytes.Buffer
	d.registriesTLSNudge(context.Background(), &vErr)
	if !strings.Contains(vErr.String(), "lo trust") {
		t.Fatalf("nudge missing:\n%s", vErr.String())
	}
}

func TestRegistriesTLSNudgeSilentWhenTLSDisabled(t *testing.T) {
	d, _, errBuf, p := testDriver(t)
	cy := writeTLSSpec(t, p.Clusters, "false")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	var vErr bytes.Buffer
	d.registriesTLSNudge(context.Background(), &vErr)
	if vErr.Len() != 0 {
		t.Fatalf("plain mode nudged:\n%s", vErr.String())
	}
}

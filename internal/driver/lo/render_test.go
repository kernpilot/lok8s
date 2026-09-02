package lo

// render_test.go — port of tests/unit/render_certs_d_test.bats + the
// byte-exact kind-config goldens (generated ONCE from the bash
// lo::render_kind_config — see testdata/kindconfig_*.golden; the harness
// used PATH_CLUSTERS=/test/clusters, DOMAIN_NAME=test.lok8s.dev, cluster
// test-shared, kindest v1.31.4) and the OIDC auth-config goldens.

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/oidc"
)

// goldenDriver builds a driver whose Clusters path matches the golden
// harness (/test/clusters) — the certs.d hostPath is embedded in the
// rendered config.
func goldenDriver(t *testing.T) *Driver {
	t.Helper()
	d, _, _, _ := testDriver(t)
	d.deps.Paths = &config.Paths{
		Base:     "/test",
		Bin:      "/test/.bin",
		Lok8s:    "/test/.lok8s",
		Clusters: "/test/clusters",
	}
	t.Setenv("DOMAIN_NAME", "test.lok8s.dev")
	t.Setenv("LOK8S_MAX_CONCURRENT_DOWNLOADS", "3")
	return d
}

func setNodeEnv(t *testing.T, cp, workers, hostPorts, mounts string) {
	t.Setenv("LOK8S_CP_COUNT", cp)
	t.Setenv("LOK8S_WORKER_COUNT", workers)
	t.Setenv("LOK8S_HOST_PORTS", hostPorts)
	t.Setenv("LOK8S_EXTRA_MOUNTS_COUNT", mounts)
}

func assertGolden(t *testing.T, got, goldenName string) {
	t.Helper()
	want := readFileT(t, filepath.Join("testdata", goldenName))
	if got != want {
		t.Errorf("render diverges from the bash golden %s.\n--- bash\n%s\n--- go\n%s", goldenName, want, got)
	}
}

func TestRenderKindConfigDefaultMatchesBashByteForByte(t *testing.T) {
	d := goldenDriver(t)
	setNodeEnv(t, "1", "0", "false", "0")
	// The no-OIDC guarantee: without spec.oidc the render is byte-identical
	// to the pre-OIDC output — pinned by the golden.
	got := d.renderKindConfig("test-shared", "v1.31.4", "lok8s", "")
	assertGolden(t, got, "kindconfig_default.golden")
}

func TestRenderKindConfigHostPortsMatchesBash(t *testing.T) {
	d := goldenDriver(t)
	setNodeEnv(t, "1", "0", "true", "0")
	assertGolden(t, d.renderKindConfig("test-shared", "v1.31.4", "lok8s", ""), "kindconfig_hostports.golden")
}

func TestRenderKindConfigMultiNodeMatchesBash(t *testing.T) {
	d := goldenDriver(t)
	setNodeEnv(t, "3", "2", "false", "0")
	assertGolden(t, d.renderKindConfig("test-shared", "v1.31.4", "lok8s", ""), "kindconfig_multinode.golden")
}

func TestRenderKindConfigExtraMountsMatchesBash(t *testing.T) {
	d := goldenDriver(t)
	setNodeEnv(t, "1", "0", "false", "2")
	spec := filepath.Join(t.TempDir(), "mounts.yaml")
	writeFile(t, spec, `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-shared
spec:
  nodes:
    extraMounts:
      - hostPath: /src/app
        containerPath: /app
        readOnly: true
      - hostPath: /data
        containerPath: /data
`)
	assertGolden(t, d.renderKindConfig("test-shared", "v1.31.4", "lok8s", spec), "kindconfig_extramounts.golden")
}

func TestRenderKindConfigOIDCMatchesBash(t *testing.T) {
	d := goldenDriver(t)
	setNodeEnv(t, "1", "0", "true", "0")
	t.Setenv(oidc.EnvIssuer, "https://id.kubehz.dev")
	t.Setenv(oidc.EnvClientID, "kubectl-client")
	assertGolden(t, d.renderKindConfig("test-shared", "v1.31.4", "lok8s", ""), "kindconfig_oidc.golden")
}

// ── OIDC auth-config render ──────────────────────────────

func TestRenderAuthConfigMatchesBashGoldens(t *testing.T) {
	t.Setenv(oidc.EnvIssuer, "https://id.kubehz.dev")
	t.Setenv(oidc.EnvClientID, "kubectl-client")
	t.Setenv(oidc.EnvUsernameClaim, "sub")
	t.Setenv(oidc.EnvUsernamePrefix, "oidc:")
	t.Setenv(oidc.EnvGroupsClaim, "groups")
	t.Setenv(oidc.EnvGroupsPrefix, "oidc:")
	t.Setenv(oidc.EnvCABundle, "")

	var errBuf bytes.Buffer
	got, err := renderAuthConfig(&errBuf)
	if err != nil {
		t.Fatal(err)
	}
	// The bash harness captured `oidc::render_auth_config > file` — file
	// carries the final newline the echo emitted.
	assertGolden(t, got+"\n", "authconfig_noca.golden")

	t.Setenv(oidc.EnvCABundle, "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----")
	got, err = renderAuthConfig(&errBuf)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, got+"\n", "authconfig_ca.golden")
}

func TestRenderAuthConfigRejectsPlainHTTPIssuer(t *testing.T) {
	t.Setenv(oidc.EnvIssuer, "http://id.kubehz.dev")
	t.Setenv(oidc.EnvClientID, "kubectl-client")
	var errBuf bytes.Buffer
	if _, err := renderAuthConfig(&errBuf); err == nil {
		t.Fatal("plain-http issuer accepted")
	}
	if !bytes.Contains(errBuf.Bytes(), []byte("spec.oidc.issuer must be an https:// URL, got 'http://id.kubehz.dev'")) {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

// ── certs.d (render_certs_d_test.bats) ───────────────────

// certsDDriver builds a driver + a plain-mode registry JSON with no entries
// (isolating the dir-refresh behavior, like the bats stubs).
func certsDDriver(t *testing.T) (*Driver, string) {
	t.Helper()
	d, _, _, p := testDriver(t)
	t.Setenv("DOMAIN_NAME", "test.dev")
	jsonPath := filepath.Join(t.TempDir(), ".registries.json")
	writeFile(t, jsonPath, `{"shared":false,"tls":false,"port":80,"network":{"name":"lok8s-registries","cidr":"10.125.200.0/24"},"project_network":"lok8s","registries":[]}`)
	t.Setenv("LOK8S_REGISTRY_JSON", jsonPath)
	return d, filepath.Join(p.Clusters, "test.dev", ".containerd", "certs.d")
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

func TestWriteCertsDCreatesDir(t *testing.T) {
	d, certsD := certsDDriver(t)
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !dirExists(certsD) {
		t.Fatal("certs.d not created")
	}
}

func TestWriteCertsDPreservesDirInodeAcrossRuns(t *testing.T) {
	// A kind node bind-mounts clusters/<domain>/.containerd/certs.d;
	// removing the dir gives it a new inode while the running node's mount
	// still points at the deleted one → the node sees an EMPTY certs.d →
	// containerd falls back to HTTPS:443 → ImagePullBackOff on every
	// re-`lo up`.
	d, certsD := certsDDriver(t)
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	before := inode(t, certsD)
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	if after := inode(t, certsD); after != before {
		t.Fatalf("certs.d inode changed across runs (%d → %d) — bind-mount broken", before, after)
	}
}

func TestWriteCertsDClearsStaleHostEntries(t *testing.T) {
	d, certsD := certsDDriver(t)
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(certsD, "stale.registry", "hosts.toml"), "old")
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(certsD, "stale.registry")); err == nil {
		t.Fatal("stale host entry survived the refresh")
	}
}

func TestWriteCertsDSelfProtectingGitignore(t *testing.T) {
	d, certsD := certsDDriver(t)
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(filepath.Dir(certsD), ".gitignore")
	content := readFileT(t, gi)
	hasStar, hasSentinel := false, false
	for _, line := range bytes.Split([]byte(content), []byte("\n")) {
		if string(line) == "*" {
			hasStar = true
		}
		if string(line) == "!.gitignore" {
			hasSentinel = true
		}
	}
	if !hasStar || !hasSentinel {
		t.Fatalf(".gitignore must ignore everything (*) but keep itself (!.gitignore):\n%s", content)
	}

	// Survives the in-place refresh (it lives in the .containerd parent).
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !fileExists(gi) {
		t.Fatal(".gitignore did not survive the refresh")
	}
}

func TestWriteCertsDDoesNotClobberConformingGitignore(t *testing.T) {
	// The sentinel guard rewrites only a missing or pre-fix (*-only) file;
	// a conforming one is left untouched — proven by a user marker
	// surviving a re-run.
	d, certsD := certsDDriver(t)
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(filepath.Dir(certsD), ".gitignore")
	writeFile(t, gi, "*\n!.gitignore\n# user-added marker\n")
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(readFileT(t, gi)), []byte("# user-added marker")) {
		t.Fatal("a conforming .gitignore was clobbered")
	}
}

// ── certs.d hosts.toml content (registry_tls_test.bats slice) ──

func tlsCertsDDriver(t *testing.T, tls bool) (*Driver, string) {
	t.Helper()
	d, _, errBuf, p := testDriver(t)
	t.Setenv("DOMAIN_NAME", "test.lok8s.dev")
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	tlsVal := "true"
	if !tls {
		tlsVal = "false"
	}
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
    tls: `+tlsVal+`
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
`)
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatalf("readNetworkConfig: %v\n%s", err, errBuf.String())
	}
	return d, filepath.Join(p.Clusters, "test.lok8s.dev", ".containerd", "certs.d")
}

func TestWriteCertsDTLSModeEmitsHTTPSAndCA(t *testing.T) {
	d, certsD := tlsCertsDDriver(t, true)
	// write_certs_d resolves the dev CA from CAROOT directly (binary-free).
	caroot := t.TempDir()
	writeFile(t, filepath.Join(caroot, "rootCA.pem"), "FAKE-CA\n")
	t.Setenv("CAROOT", caroot)

	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}

	// rootCA copied into the certs.d tree.
	if got := readFileT(t, filepath.Join(certsD, ".ca", "rootCA.pem")); got != "FAKE-CA\n" {
		t.Fatalf("rootCA content = %q", got)
	}

	// build registry hostname entry — EXACT hosts.toml bytes: https + ca,
	// push capability, no skip_verify, no port in the server URL.
	want := `# Auto-generated by lok8s — registry mirror for build
server = "https://10.125.50.101"

[host."https://10.125.50.101"]
  capabilities = ["pull", "resolve", "push"]
  ca = "/etc/containerd/certs.d/.ca/rootCA.pem"
`
	if got := readFileT(t, filepath.Join(certsD, "lok8s.local", "hosts.toml")); got != want {
		t.Fatalf("hosts.toml bytes diverge:\n--- want\n%s\n--- got\n%s", want, got)
	}

	// direct IP entry also https + ca.
	ipEntry := readFileT(t, filepath.Join(certsD, "10.125.50.101", "hosts.toml"))
	if !bytes.Contains([]byte(ipEntry), []byte(`server = "https://10.125.50.101"`)) ||
		!bytes.Contains([]byte(ipEntry), []byte("ca =")) {
		t.Fatalf("direct IP entry wrong:\n%s", ipEntry)
	}
	// Mirror entry lands under its upstream domain with pull/resolve only.
	mirror := readFileT(t, filepath.Join(certsD, "docker.io", "hosts.toml"))
	if !bytes.Contains([]byte(mirror), []byte(`capabilities = ["pull", "resolve"]`)) {
		t.Fatalf("mirror capabilities wrong:\n%s", mirror)
	}
}

func TestWriteCertsDPlainModeKeepsSkipVerify(t *testing.T) {
	d, certsD := tlsCertsDDriver(t, false)
	if err := d.writeCertsD(os.Stderr); err != nil {
		t.Fatal(err)
	}
	if dirExists(filepath.Join(certsD, ".ca")) {
		t.Fatal("plain mode must not create .ca")
	}
	want := `# Auto-generated by lok8s — registry mirror for build
server = "http://10.125.50.101"

[host."http://10.125.50.101"]
  capabilities = ["pull", "resolve", "push"]
  skip_verify = true
`
	if got := readFileT(t, filepath.Join(certsD, "lok8s.local", "hosts.toml")); got != want {
		t.Fatalf("plain hosts.toml bytes diverge:\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// ── OIDC auth-config write (inode rule) ──────────────────

func TestWriteOIDCAuthConfigTruncateInPlaceKeepsInode(t *testing.T) {
	d, _, _, p := testDriver(t)
	t.Setenv(oidc.EnvIssuer, "https://id.kubehz.dev")
	t.Setenv(oidc.EnvClientID, "kubectl-client")

	var errBuf bytes.Buffer
	if err := d.writeOIDCAuthConfig("test.dev", &errBuf); err != nil {
		t.Fatal(err)
	}
	authConfig := filepath.Join(p.Clusters, "test.dev", ".oidc", "auth-config.yaml")
	before := inode(t, authConfig)

	if err := d.writeOIDCAuthConfig("test.dev", &errBuf); err != nil {
		t.Fatal(err)
	}
	if after := inode(t, authConfig); after != before {
		t.Fatalf("auth-config inode changed (%d → %d) — a live node mount would see the deleted file", before, after)
	}

	// Disabled OIDC clears the CONTENT in place (keep the inode for any
	// live mount), never removes the file.
	t.Setenv(oidc.EnvIssuer, "")
	t.Setenv(oidc.EnvClientID, "")
	if err := d.writeOIDCAuthConfig("test.dev", &errBuf); err != nil {
		t.Fatal(err)
	}
	if after := inode(t, authConfig); after != before {
		t.Fatal("disable path replaced the file inode")
	}
	if got := readFileT(t, authConfig); got != "" {
		t.Fatalf("disable path left stale wiring:\n%s", got)
	}
}

package lo

// registries_test.go — the port of tests/unit/registry_lifecycle_test.bats:
// the idempotent registry container lifecycle (desired-state reconciliation
// via the config-hash label, durable config files under the state dir, the
// loud squatted-IP failure, portable ${REMOTE_URL} rendering) and the
// reserved dynamic range for the shared network — including the recreate of
// a legacy network that predates the reservation. The TLS half ports
// tests/unit/registry_tls_test.bats: registry-config http-block rendering,
// the Secret-plugin-driven cert mint (SAN list, extraction, remint skip)
// and the untrusted-CA nudge. The Secret plugin is a fake exec through the
// runner seam. The network reconcile lives in network_test.go, the `lo
// registry` verbs in registrycli_test.go.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crypto/tls"
	"crypto/x509"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func runRegistries(t *testing.T, d *Driver, cy string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := d.registries(t.Context(), &out, &errOut, "test.lok8s.dev", cy)
	return out.String(), errOut.String(), err
}

// ── Rendering: no envsubst dependency ────────────────────

func TestRenderSubstitutesRemoteURLWithoutEnvsubst(t *testing.T) {
	_, _, _, _, p, _ := lifecycleDriver(t)
	// The Go render is native by construction (no exec at all — the fake
	// runner would record one); assert the substitution result.
	out, err := renderRegistryConfig(
		filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "registry", "mirror.yaml"),
		"https://registry-1.docker.io", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "remoteurl: https://registry-1.docker.io") {
		t.Fatalf("remoteurl not substituted:\n%s", out)
	}
	if strings.Contains(out, "${REMOTE_URL}") {
		t.Fatalf("unexpanded placeholder left:\n%s", out)
	}
}

// ── IP holder lookup ─────────────────────────────────────

func TestRegistryIPHolderExactPrefixMatch(t *testing.T) {
	d, _, fd, _, _, _ := lifecycleDriver(t)
	fd.addMember("lok8s-registries", "10.125.200.20/24", "other-node")

	// .2 must NOT match inside .20 — IPv4Address carries the /prefix, so
	// the match is "<ip>/" as a prefix.
	if got := d.registryIPHolder(t.Context(), "lok8s-registries", "10.125.200.2"); got != "" {
		t.Fatalf("holder(.2) = %q, want empty — .2 matched inside .20", got)
	}
	if got := d.registryIPHolder(t.Context(), "lok8s-registries", "10.125.200.20"); got != "other-node" {
		t.Fatalf("holder(.20) = %q, want other-node", got)
	}
}

// ── Reconcile matrix ─────────────────────────────────────

func TestRegistriesFreshStartCreatesEverything(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	out, errText, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatalf("registries: %v\nstderr: %s", err, errText)
	}
	for _, want := range []string{
		"registry/lok8s-registry-build created",
		"registry/lok8s-registry-cache created",
		"registry/lok8s-registry-io-docker created",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Durable config, not a mktemp: the daemon re-binds it on every restart.
	cfg := filepath.Join(registryStateDir(), "lok8s-registry-io-docker.yaml")
	if !strings.Contains(readFileT(t, cfg), "remoteurl: https://registry-1.docker.io") {
		t.Fatal("mirror config missing substituted remoteurl")
	}
	// Mirror landed on its static shared-network IP.
	if !fd.hasMemberIP("lok8s-registries", "10.125.200.2/24", "lok8s-registry-io-docker") {
		t.Fatal("mirror not at its static shared-network IP")
	}
}

func TestRegistriesSecondRunIsPureNoop(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	if _, _, err := runRegistries(t, d, cy); err != nil {
		t.Fatal(err)
	}
	fd.log = nil

	out, _, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"registry/lok8s-registry-build unchanged",
		"registry/lok8s-registry-cache unchanged",
		"registry/lok8s-registry-io-docker unchanged",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, l := range fd.log {
		if strings.HasPrefix(l, "docker run") || strings.HasPrefix(l, "docker rm") {
			t.Fatalf("docker churn on a no-op run: %s", l)
		}
	}
}

func TestRegistriesConfigDriftRecreates(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	if _, _, err := runRegistries(t, d, cy); err != nil {
		t.Fatal(err)
	}
	// Simulate drift: the stored hash no longer matches the desired state.
	fd.setContainer("lok8s-registry-cache", "running", "stale-hash")

	out, _, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "registry/lok8s-registry-cache configured") {
		t.Fatalf("drifted running container not 'configured':\n%s", out)
	}
	if !strings.Contains(out, "registry/lok8s-registry-build unchanged") {
		t.Fatalf("undrifted container churned:\n%s", out)
	}
}

func TestRegistriesMovingStateDirRecreates(t *testing.T) {
	d, _, _, _, p, cy := lifecycleDriver(t)
	if _, _, err := runRegistries(t, d, cy); err != nil {
		t.Fatal(err)
	}
	// The config path feeds the hash: containers still bound to the
	// abandoned path must be recreated or the vanished-mount failure
	// returns.
	t.Setenv("LO_REGISTRY_STATE_DIR", filepath.Join(p.Base, "registry-state-moved"))

	out, _, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "registry/lok8s-registry-build configured") {
		t.Fatalf("state-dir move did not recreate:\n%s", out)
	}
	if !fsutil.FileExists(filepath.Join(registryStateDir(), "lok8s-registry-build.yaml")) {
		t.Fatal("config not written to the new state dir")
	}
}

func TestRegistriesDeadContainerRestarted(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	if _, _, err := runRegistries(t, d, cy); err != nil {
		t.Fatal(err)
	}
	// keep the hash, kill the container
	_, hash, _ := fd.containerStatus("lok8s-registry-io-docker")
	fd.setContainer("lok8s-registry-io-docker", "exited", hash)

	out, _, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "registry/lok8s-registry-io-docker restarted") {
		t.Fatalf("dead container not 'restarted':\n%s", out)
	}
}

func TestRegistriesSquattedIPFailsLoudlyHolderNamed(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	// A container holds the mirror's .2. Eviction is GONE by design — on a
	// range-reserved network this cannot happen, so when it does (legacy
	// network, concurrent tooling) the fix is recreating the network, not
	// silently detaching someone else's container.
	fd.setContainer("test-node", "running", "")
	fd.addMember("lok8s-registries", "10.125.200.2/24", "test-node")

	out, errText, err := runRegistries(t, d, cy)
	if err == nil {
		t.Fatalf("squatted IP reconciled as success:\n%s", out)
	}
	if !strings.Contains(errText, "held by 'test-node'") {
		t.Fatalf("holder not named:\n%s", errText)
	}
	if !strings.Contains(errText, "lo registry clean --shared") {
		t.Fatalf("shared-net remediation missing:\n%s", errText)
	}
	// The squatter was NOT touched — no eviction, no shuffle.
	if !fd.hasMemberIP("lok8s-registries", "10.125.200.2/24", "test-node") {
		t.Fatal("the squatter was evicted")
	}
	for _, l := range fd.log {
		if l == "docker network disconnect -f lok8s-registries test-node" {
			t.Fatal("the squatter was detached")
		}
	}
}

func TestRegistriesOneFailureDoesNotAbortTheRest(t *testing.T) {
	d, _, _, _, p, cy := lifecycleDriver(t)
	os.Remove(filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "registry", "build.yaml"))

	out, errText, err := runRegistries(t, d, cy)
	if err == nil {
		t.Fatal("missing config reported as success")
	}
	if !strings.Contains(errText, "error: registry/lok8s-registry-build") {
		t.Fatalf("build failure not surfaced:\n%s", errText)
	}
	if !strings.Contains(out, "registry/lok8s-registry-cache created") ||
		!strings.Contains(out, "registry/lok8s-registry-io-docker created") {
		t.Fatalf("later registries were aborted:\n%s", out)
	}
}

func TestRegistriesTransientlyHeldAddressRetries(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	// The endpoint of a just-removed container can lag its release: the
	// first `docker run` fails address-in-use with NO visible holder (the
	// entry is already gone by the re-check) — the retry must succeed, not
	// fail loudly.
	lagged := false
	fd.wrap = func(c execx.Cmd) (bool, error) {
		if len(c.Args) > 1 && c.Args[0] == "run" &&
			strings.Contains(strings.Join(c.Args, " "), "lok8s-registry-io-docker") && !lagged {
			lagged = true
			writeErr(c, "docker: failed to set up container networking: Address already in use\n")
			return true, os.ErrPermission // any non-nil error
		}
		return false, nil
	}

	out, errText, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatalf("bounded retry did not recover: %v\nstderr: %s", err, errText)
	}
	if !strings.Contains(out, "registry/lok8s-registry-io-docker created") {
		t.Fatalf("mirror not created on retry:\n%s", out)
	}
	if !fd.hasMemberIP("lok8s-registries", "10.125.200.2/24", "lok8s-registry-io-docker") {
		t.Fatal("mirror not at its static IP after retry")
	}
}

func TestRegistriesProjectNetworkSquatGetsNodeRebootRemediation(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	// Same persistent-holder failure, but on the PROJECT network
	// (build/cache live there): the shared-net "clean --shared" hint cannot
	// fix it — the error must point at the squatting node instead.
	fd.setContainer("test-node", "running", "")
	fd.addMember("lok8s", "10.125.50.101/24", "test-node")

	_, errText, err := runRegistries(t, d, cy)
	if err == nil {
		t.Fatal("project-net squat reconciled as success")
	}
	if !strings.Contains(errText, "held by 'test-node'") {
		t.Fatalf("holder not named:\n%s", errText)
	}
	if !strings.Contains(errText, "docker network disconnect -f lok8s test-node") {
		t.Fatalf("node-reboot remediation missing:\n%s", errText)
	}
	if strings.Contains(errText, "clean --shared && lo up") {
		t.Fatalf("the error recommends 'lo registry clean --shared' for a PROJECT-network squat — that command touches a different network and cannot fix this:\n%s", errText)
	}
}

func TestRegistriesLosingStartRaceIsUnchanged(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	if _, _, err := runRegistries(t, d, cy); err != nil {
		t.Fatal(err)
	}
	fd.setContainer("lok8s-registry-io-docker", "running", "stale-hash")

	// Concurrent-lo emulation: our rm is a no-op (the other run recreated
	// the container immediately) and our run loses the name to it.
	fd.wrap = func(c execx.Cmd) (bool, error) {
		if len(c.Args) >= 2 && c.Args[0] == "rm" && c.Args[1] == "-f" {
			return true, nil // no-op rm
		}
		if len(c.Args) >= 1 && c.Args[0] == "run" {
			writeErr(c, "docker: Error response from daemon: Conflict. The container name is already in use\n")
			return true, os.ErrPermission
		}
		return false, nil
	}

	out, errText, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatalf("losing the race must be success: %v\n%s", err, errText)
	}
	if !strings.Contains(out, "registry/lok8s-registry-io-docker unchanged") {
		t.Fatalf("race loss not reported unchanged:\n%s", out)
	}
}

// ── lok8s-registries ConfigMap ───────────────────────────

func TestRegistryConfigmapManifestBytes(t *testing.T) {
	// Byte-pins the bash jq -r data block: each entry's lines, ONE blank
	// line between entries (jq -r's extra newline per result), trailing run
	// stripped by the $() capture — and the KNOWN .port "5000" DEFECT.
	d, runner, _, _, p, cy := lifecycleDriver(t)
	testutil.WriteFile(t, filepath.Join(p.Base, ".kubeconfig", "test-lifecycle.yaml"), "apiVersion: v1\n")

	var manifest string
	base := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kubectl" {
			var buf bytes.Buffer
			if c.Stdin != nil {
				buf.ReadFrom(c.Stdin)
			}
			manifest = buf.String()
			return nil
		}
		return base(c)
	}

	var out, errOut bytes.Buffer
	if err := d.registryConfigmap(t.Context(), &out, &errOut, "test.lok8s.dev", cy); err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: v1
kind: ConfigMap
metadata:
  name: lok8s-registries
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: lok8s
data:
  build.ip: "10.125.50.101"
  build.port: "5000"

  cache.ip: "10.125.50.102"
  cache.port: "5000"

  io-docker.ip: "10.125.200.2"
  io-docker.port: "5000"
  io-docker.url: "https://registry-1.docker.io"
  io-docker.shared: "true"
`
	if manifest != want {
		t.Fatalf("lok8s-registries manifest diverges from the bash heredoc:\n--- want\n%s\n--- got\n%s", want, manifest)
	}
}

// ── Cleanup ──────────────────────────────────────────────

func TestCleanupKeepsSharedMirrors(t *testing.T) {
	d, _, fd, _, _, cy := lifecycleDriver(t)
	if _, _, err := runRegistries(t, d, cy); err != nil {
		t.Fatal(err)
	}

	d.cleanupRegistries(t.Context(), "test-lifecycle")

	if _, _, ok := fd.containerStatus("lok8s-registry-build"); ok {
		t.Fatal("project registry container survived cleanup")
	}
	if fsutil.FileExists(filepath.Join(registryStateDir(), "lok8s-registry-build.yaml")) {
		t.Fatal("project registry config survived cleanup")
	}
	if fsutil.FileExists(filepath.Join(registryStateDir(), "lok8s-registry-cache.yaml")) {
		t.Fatal("cache registry config survived cleanup")
	}
	// Shared mirrors persist across project lifecycles.
	if _, _, ok := fd.containerStatus("lok8s-registry-io-docker"); !ok {
		t.Fatal("shared mirror was removed by project cleanup")
	}
	if !fsutil.FileExists(filepath.Join(registryStateDir(), "lok8s-registry-io-docker.yaml")) {
		t.Fatal("shared mirror config was removed by project cleanup")
	}
}

func TestRenderRegistryConfigTLSSwapsHTTPBlock(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	buildYAML := filepath.Join(tmp, "build.yaml")
	testutil.WriteFile(t, buildYAML, realRegistryTemplate(t, "build.yaml"))

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
	testutil.WriteFile(t, buildYAML, realRegistryTemplate(t, "build.yaml"))

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
	testutil.WriteFile(t, mirrorYAML, realRegistryTemplate(t, "mirror.yaml"))

	out, err := renderRegistryConfig(mirrorYAML, "https://registry-1.docker.io", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "remoteurl: https://registry-1.docker.io") ||
		!strings.Contains(out, "addr: :443") {
		t.Fatalf("mirror TLS render wrong:\n%s", out)
	}
}

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
	testutil.WriteFile(t, pluginBin, "#!/bin/sh\nexit 1\n") // never actually executed
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

	if err := d.registriesTLSCert(t.Context(), errBuf); err != nil {
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

	if err := d.registriesTLSCert(t.Context(), errBuf); err != nil {
		t.Fatal(err)
	}
	*manifest = "" // detector: did the plugin run again?

	if err := d.registriesTLSCert(t.Context(), errBuf); err != nil {
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
	if err := d.registriesTLSCert(t.Context(), errBuf); err != nil {
		t.Fatal(err)
	}
	if fsutil.FileExists(filepath.Join(p.Base, ".secrets", "tls", "registries", "tls.crt")) {
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
	if err := d.registriesTLSCert(t.Context(), &vErr); err == nil {
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

	if err := d.registriesTLSCert(t.Context(), errBuf); err != nil {
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
	if !fsutil.FileExists(filepath.Join(p.Base, ".secrets-store", "Secret.registries-tls.lok8s-system.tls.crt")) {
		t.Fatal("leaf not cached in PATH_SECRETS")
	}
	if got := readFileT(t, filepath.Join(tlsDir, ".sans")); !strings.Contains(got, "lok8s.local") {
		t.Fatalf(".sans = %q", got)
	}
	if err := d.registriesTLSCert(t.Context(), errBuf); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(tlsDir, "tls.crt")); !bytes.Equal(again, crtPEM) {
		t.Fatal("unchanged SAN set re-minted the cert")
	}
}

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
	testutil.WriteFile(t, filepath.Join(p.Bin, "openssl"), "#!/bin/sh\nexit 1\n")
	os.Chmod(filepath.Join(p.Bin, "openssl"), 0o755)
	_ = runner

	var vErr bytes.Buffer
	d.registriesTLSNudge(t.Context(), &vErr)
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
	d.registriesTLSNudge(t.Context(), &vErr)
	if vErr.Len() != 0 {
		t.Fatalf("plain mode nudged:\n%s", vErr.String())
	}
}

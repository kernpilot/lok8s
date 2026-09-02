package lo

// registries_test.go — the port of tests/unit/registry_lifecycle_test.bats:
// the idempotent registry container lifecycle (desired-state reconciliation
// via the config-hash label, durable config files under the state dir, the
// loud squatted-IP failure, portable ${REMOTE_URL} rendering) and the
// reserved dynamic range for the shared network — including the recreate of
// a legacy network that predates the reservation.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func runRegistries(t *testing.T, d *Driver, cy string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := d.registries(context.Background(), &out, &errOut, "test.lok8s.dev", cy)
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

// ── Shared-network dynamic range ─────────────────────────

func TestRegistryDynamicRange(t *testing.T) {
	cases := []struct {
		cidr, want string
		ok         bool
	}{
		{"10.125.200.0/24", "10.125.200.128/25", true}, // /24 → upper /25
		{"10.60.0.0/16", "10.60.128.0/17", true},       // /16 → upper /17
		{"10.0.0.0/31", "", false},                     // /31 has no room to split
	}
	for _, tc := range cases {
		got, ok := registryDynamicRange(tc.cidr)
		if ok != tc.ok || got != tc.want {
			t.Errorf("registryDynamicRange(%s) = %q,%v want %q,%v", tc.cidr, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNetworkDynamicRange(t *testing.T) {
	// .192+ — ABOVE the registries (.101-.110) and the default MetalLB pool
	// (.125-.150): a dynamically-attached node can collide with neither.
	got, ok := networkDynamicRange("10.125.125.0/24")
	if !ok || got != "10.125.125.192/26" {
		t.Fatalf("networkDynamicRange(/24) = %q,%v want 10.125.125.192/26", got, ok)
	}
}

func TestNetworkFreshCreateReservesNodeRange(t *testing.T) {
	d, _, fd, errBuf, _, _ := lifecycleDriver(t)
	os.Remove(fd.networkPath("lok8s"))
	os.Remove(fd.networkPath("lok8s") + ".meta")

	if err := d.network(context.Background(), errBuf); err != nil {
		t.Fatalf("network: %v\n%s", err, errBuf.String())
	}
	joined := strings.Join(fd.log, "\n")
	if strings.Contains(joined, "--ip-range 10.125.50.0/24") {
		t.Fatal("the FULL subnet was passed as --ip-range")
	}
	if !strings.Contains(joined, "--ip-range 10.125.50.192/26") {
		t.Fatalf("the project network was created WITHOUT its reserved node range — a rebooting node can squat build/cache (.101/.102) or a MetalLB pool address again.\nlog:\n%s", joined)
	}
}

func TestRegistryNetworkFreshCreateReservesDynamicRange(t *testing.T) {
	d, _, fd, errBuf, _, _ := lifecycleDriver(t)
	os.Remove(fd.networkPath("lok8s-registries"))
	os.Remove(fd.networkPath("lok8s-registries") + ".meta")

	if err := d.registryNetwork(context.Background(), errBuf); err != nil {
		t.Fatalf("registryNetwork: %v", err)
	}
	if _, ipRange := fd.networkMeta("lok8s-registries"); ipRange != "10.125.200.128/25" {
		t.Fatalf("network created WITHOUT the reserved range (got %q) — dynamic attachers can squat the mirrors' static IPs again", ipRange)
	}
}

func TestRegistryNetworkWrongRangeRecreated(t *testing.T) {
	d, _, fd, errBuf, _, _ := lifecycleDriver(t)
	// A range that differs from the derived one (older tooling, a hand-made
	// network) can still let dynamic allocation overlap the statics — mere
	// non-emptiness must not pass for "reserved".
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "10.125.200.64/26")

	if err := d.registryNetwork(context.Background(), errBuf); err != nil {
		t.Fatalf("registryNetwork: %v", err)
	}
	if _, ipRange := fd.networkMeta("lok8s-registries"); ipRange != "10.125.200.128/25" {
		t.Fatalf("a mismatched --ip-range (10.125.200.64/26) was accepted as reserved (now %q)", ipRange)
	}
}

func TestRegistryNetworkReservedIsUntouched(t *testing.T) {
	d, _, fd, errBuf, _, _ := lifecycleDriver(t)
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "10.125.200.128/25")
	fd.log = nil

	if err := d.registryNetwork(context.Background(), errBuf); err != nil {
		t.Fatalf("registryNetwork: %v", err)
	}
	for _, l := range fd.log {
		if strings.HasPrefix(l, "docker network rm") || strings.HasPrefix(l, "docker network create") {
			t.Fatalf("a correctly-configured network was churned: %s", l)
		}
	}
}

func TestRegistryNetworkLegacyRecreatedMirrorsRemoved(t *testing.T) {
	d, _, fd, errBuf, _, _ := lifecycleDriver(t)
	// Pre-reservation network: subnet only, a mirror + a kind node attached.
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "")
	fd.setContainer("lok8s-registry-io-docker", "running", "some-hash")
	fd.setContainer("test-node", "running", "")
	fd.addMember("lok8s-registries", "10.125.200.2/24", "lok8s-registry-io-docker")
	fd.addMember("lok8s-registries", "10.125.200.7/24", "test-node")

	if err := d.registryNetwork(context.Background(), errBuf); err != nil {
		t.Fatalf("registryNetwork: %v\nstderr: %s", err, errBuf.String())
	}

	// Recreated with the range.
	if _, ipRange := fd.networkMeta("lok8s-registries"); ipRange != "10.125.200.128/25" {
		t.Fatalf("legacy network not recreated with range (got %q)", ipRange)
	}
	// The mirror container was REMOVED — a running mirror with a matching
	// config-hash would otherwise reconcile "unchanged" while detached from
	// the new network, silently breaking every pull.
	if _, _, ok := fd.containerStatus("lok8s-registry-io-docker"); ok {
		t.Fatal("the mirror survived the recreate — the reconcile will report it unchanged and never re-attach it")
	}
	// The kind node is detached but NOT removed — it re-attaches via
	// connectNodesToRegistryNetwork on its cluster's next lo up.
	if _, _, ok := fd.containerStatus("test-node"); !ok {
		t.Fatal("the kind node container was removed, not just detached")
	}
	for _, l := range fd.log {
		if l == "docker rm -f test-node" {
			t.Fatal("docker rm -f was invoked on the kind node")
		}
	}
}

func TestRegistryNetworkLegacyRecreateThenReconcileRestoresMirror(t *testing.T) {
	d, _, fd, errBuf, _, cy := lifecycleDriver(t)
	// End-to-end: the recreate followed by the normal reconcile must land
	// the mirror back on .2 on the NEW network — the property the whole
	// recreate design leans on.
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "")
	fd.setContainer("lok8s-registry-io-docker", "running", "some-hash")
	fd.addMember("lok8s-registries", "10.125.200.2/24", "lok8s-registry-io-docker")

	if err := d.registryNetwork(context.Background(), errBuf); err != nil {
		t.Fatalf("registryNetwork: %v", err)
	}
	out, errText, err := runRegistries(t, d, cy)
	if err != nil {
		t.Fatalf("registries: %v\nstderr: %s", err, errText)
	}
	if !strings.Contains(out, "registry/lok8s-registry-io-docker created") {
		t.Fatalf("mirror not recreated:\n%s", out)
	}
	if !fd.hasMemberIP("lok8s-registries", "10.125.200.2/24", "lok8s-registry-io-docker") {
		t.Fatal("mirror did not land back on its static .2 on the new network")
	}
}

// ── IP holder lookup ─────────────────────────────────────

func TestRegistryIPHolderExactPrefixMatch(t *testing.T) {
	d, _, fd, _, _, _ := lifecycleDriver(t)
	fd.addMember("lok8s-registries", "10.125.200.20/24", "other-node")

	// .2 must NOT match inside .20 — IPv4Address carries the /prefix, so
	// the match is "<ip>/" as a prefix.
	if got := d.registryIPHolder(context.Background(), "lok8s-registries", "10.125.200.2"); got != "" {
		t.Fatalf("holder(.2) = %q, want empty — .2 matched inside .20", got)
	}
	if got := d.registryIPHolder(context.Background(), "lok8s-registries", "10.125.200.20"); got != "other-node" {
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
	if !fileExists(filepath.Join(registryStateDir(), "lok8s-registry-build.yaml")) {
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

func TestRegistryNetworkLegacyRecreateSurvivesLaggingEndpointRelease(t *testing.T) {
	d, _, fd, errBuf, _, _ := lifecycleDriver(t)
	// First `docker network rm` fails (a just-removed mirror's endpoint
	// lags its release), the retry succeeds. Without the bounded retry the
	// recreate dies here transiently.
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "")
	fd.setContainer("lok8s-registry-io-docker", "running", "some-hash")
	fd.addMember("lok8s-registries", "10.125.200.2/24", "lok8s-registry-io-docker")

	lagged := false
	fd.wrap = func(c execx.Cmd) (bool, error) {
		if len(c.Args) >= 3 && c.Args[0] == "network" && c.Args[1] == "rm" &&
			c.Args[2] == "lok8s-registries" && !lagged {
			lagged = true
			writeErr(c, "Error response from daemon: error while removing network: network lok8s-registries has active endpoints\n")
			return true, os.ErrPermission
		}
		return false, nil
	}

	if err := d.registryNetwork(context.Background(), errBuf); err != nil {
		t.Fatalf("recreate did not survive the lagging release: %v", err)
	}
	if _, ipRange := fd.networkMeta("lok8s-registries"); ipRange != "10.125.200.128/25" {
		t.Fatalf("network not recreated with range (got %q)", ipRange)
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
	writeFile(t, filepath.Join(p.Base, ".kubeconfig", "test-lifecycle.yaml"), "apiVersion: v1\n")

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
	if err := d.registryConfigmap(context.Background(), &out, &errOut, "test.lok8s.dev", cy); err != nil {
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

	d.cleanupRegistries(context.Background(), "test-lifecycle")

	if _, _, ok := fd.containerStatus("lok8s-registry-build"); ok {
		t.Fatal("project registry container survived cleanup")
	}
	if fileExists(filepath.Join(registryStateDir(), "lok8s-registry-build.yaml")) {
		t.Fatal("project registry config survived cleanup")
	}
	if fileExists(filepath.Join(registryStateDir(), "lok8s-registry-cache.yaml")) {
		t.Fatal("cache registry config survived cleanup")
	}
	// Shared mirrors persist across project lifecycles.
	if _, _, ok := fd.containerStatus("lok8s-registry-io-docker"); !ok {
		t.Fatal("shared mirror was removed by project cleanup")
	}
	if !fileExists(filepath.Join(registryStateDir(), "lok8s-registry-io-docker.yaml")) {
		t.Fatal("shared mirror config was removed by project cleanup")
	}
}

func TestRegistryCleanSharedDetachesHolders(t *testing.T) {
	d, _, fd, _, _, _ := lifecycleDriver(t)
	// A foreign holder is attached — exactly the state the
	// squatted-registry error sends the operator here to fix. Keeping the
	// network would send the next lo up straight back into the same error.
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "")
	fd.addMember("lok8s-registries", "10.125.200.2/24", "foreign-node")

	var errBuf bytes.Buffer
	if err := d.RegistryClean(context.Background(), "test.lok8s.dev", true, &errBuf); err != nil {
		t.Fatalf("RegistryClean: %v\n%s", err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "detaching 'foreign-node'") {
		t.Fatalf("detach not announced:\n%s", errBuf.String())
	}
	if _, err := os.Stat(fd.networkPath("lok8s-registries")); err == nil {
		t.Fatal("the network survived clean --shared — the recommended remediation loops back into the same squatted-IP failure forever")
	}
}

package lo

// network_test.go covers the project docker network reconcile in
// network.go: the reserved node range, the dynamic range, the legacy
// recreate paths and the wrong-range recreate. Docker is the file-backed
// fake.

import (
	"os"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestRegistryDynamicRange(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

	if err := d.network(t.Context(), errBuf); err != nil {
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

	if err := d.registryNetwork(t.Context(), errBuf); err != nil {
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

	if err := d.registryNetwork(t.Context(), errBuf); err != nil {
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

	if err := d.registryNetwork(t.Context(), errBuf); err != nil {
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

	if err := d.registryNetwork(t.Context(), errBuf); err != nil {
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

	if err := d.registryNetwork(t.Context(), errBuf); err != nil {
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

	if err := d.registryNetwork(t.Context(), errBuf); err != nil {
		t.Fatalf("recreate did not survive the lagging release: %v", err)
	}
	if _, ipRange := fd.networkMeta("lok8s-registries"); ipRange != "10.125.200.128/25" {
		t.Fatalf("network not recreated with range (got %q)", ipRange)
	}
}

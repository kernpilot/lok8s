package lo

// config_test.go — port of tests/unit/network_config_test.bats +
// tests/unit/shared_registry_test.bats + the .registries.json byte-compat
// goldens (generated ONCE from the bash registry::config_generate — see
// testdata/; the JSON is an external contract read by Tilt, libs/image and
// the lo main header).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixturePath(t *testing.T, name string) string {
	return filepath.Join(repoRoot(t), "tests", "fixtures", name)
}

// loadFixture copies a shipped fixture into the temp clusters tree (the
// registries JSON lands next to the spec, so the fixture must not be read
// in place).
func loadFixture(t *testing.T, clustersDir, fixture string) string {
	t.Helper()
	cy := filepath.Join(clustersDir, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, readFileT(t, fixturePath(t, fixture)))
	return cy
}

// ── lo::read_network_config ──────────────────────────────

func TestReadNetworkConfigRequiresExplicitCIDR(t *testing.T) {
	_, _, _, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, "spec:\n  network:\n    name: lok8s\n")

	var errBuf bytes.Buffer
	if err := readNetworkConfig(cy, &errBuf); err == nil {
		t.Fatal("missing cidr accepted")
	}
	if !strings.Contains(errBuf.String(), "spec.network.cidr is required") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

func TestReadNetworkConfigRequiresExplicitNameNamingTheFile(t *testing.T) {
	_, _, _, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, "spec:\n  network:\n    cidr: \"10.125.50.0/24\"\n")

	var errBuf bytes.Buffer
	if err := readNetworkConfig(cy, &errBuf); err == nil {
		t.Fatal("missing name accepted")
	}
	if !strings.Contains(errBuf.String(), "spec.network.name is missing") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
	// The message must point the investigation at the right file.
	if !strings.Contains(errBuf.String(), "test.lok8s.dev/cluster.lok8s.yaml") {
		t.Fatalf("error does not name the file:\n%s", errBuf.String())
	}
}

func TestReadNetworkConfigMissingSpecIsItsOwnError(t *testing.T) {
	_, _, _, p := testDriver(t)
	var errBuf bytes.Buffer
	err := readNetworkConfig(filepath.Join(p.Clusters, "nope", "cluster.lok8s.yaml"), &errBuf)
	if err == nil {
		t.Fatal("missing spec accepted")
	}
	if !strings.Contains(errBuf.String(), "cluster spec not found") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

func TestReadNetworkConfigDerivesBaseIPSlot50(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-network.lok8s.yaml")

	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatalf("readNetworkConfig: %v\n%s", err, errBuf.String())
	}
	checks := map[string]string{
		"KIND_EXPERIMENTAL_DOCKER_NETWORK": "lok8s-test",
		"LOK8S_NETWORK_CIDR":               "10.125.50.0/24",
		"LOK8S_NETWORK_BASE_IP":            "10.125.50.0",
		// Private registries always live on the project subnet at .101/.102.
		"LOK8S_REGISTRY_IP_BUILD": "10.125.50.101",
		"LOK8S_REGISTRY_IP_CACHE": "10.125.50.102",
		// Non-shared mode: pull-throughs follow on the project subnet at
		// .103+ (mirror names are uppercased with - → _).
		"LOK8S_REGISTRY_IP_IO_DOCKER": "10.125.50.103",
		"LOK8S_REGISTRY_IP_IO_QUAY":   "10.125.50.104",
		"LOK8S_REGISTRY_IP_IO_K8S":    "10.125.50.105",
		"LOK8S_REGISTRY_IP_IO_GHCR":   "10.125.50.106",
		// Back-compat alias mirrors the CIDR.
		"LOK8S_NETWORK_SUBNET": "10.125.50.0/24",
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// ── Schema validation: reserved names + missing url ──────

func TestMirrorsRejectReservedBuildAndCache(t *testing.T) {
	for _, reserved := range []string{"build", "cache"} {
		_, _, _, p := testDriver(t)
		cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
		writeFile(t, cy, `spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.125.0/24"
  registries:
    mirrors:
      - name: `+reserved+`
      - name: io-docker
        url: https://registry-1.docker.io
`)
		var errBuf bytes.Buffer
		if err := readNetworkConfig(cy, &errBuf); err == nil {
			t.Fatalf("reserved mirror name %q accepted", reserved)
		}
		if !strings.Contains(errBuf.String(), "'"+reserved+"' is reserved") {
			t.Fatalf("wrong error for %q:\n%s", reserved, errBuf.String())
		}
	}
}

func TestMirrorMissingURLErrors(t *testing.T) {
	_, _, _, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, `spec:
  network:
    name: lok8s
    cidr: "10.125.125.0/24"
  registries:
    mirrors:
      - name: io-docker
`)
	var errBuf bytes.Buffer
	if err := readNetworkConfig(cy, &errBuf); err == nil {
		t.Fatal("mirror without url accepted")
	}
	if !strings.Contains(errBuf.String(), "url is required") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

// ── *.lok8s.dev defaults ─────────────────────────────────

func slotSpec(t *testing.T, p string, domain, name string) string {
	cy := filepath.Join(p, domain, "cluster.lok8s.yaml")
	writeFile(t, cy, `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: `+name+`
spec:
  cluster:
    domain: `+domain+`
`)
	return cy
}

func TestSlotFromDomain(t *testing.T) {
	_, _, _, p := testDriver(t)
	cases := []struct{ domain, name, want string }{
		{"lok8s.dev", "local", "125"},
		{"126.lok8s.dev", "e2e-noop", "126"},
		{"prod.example.com", "prod", ""},
		{"200.lok8s.dev", "invalid", ""}, // reserved slot 200
	}
	for _, tc := range cases {
		cy := slotSpec(t, p.Clusters, tc.domain, tc.name)
		if got := slotFromDomain(cy); got != tc.want {
			t.Errorf("slotFromDomain(%s) = %q, want %q", tc.domain, got, tc.want)
		}
	}
}

func TestReadNetworkConfigMinimalSlotSpecGetsFullDefaults(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := slotSpec(t, p.Clusters, "126.lok8s.dev", "e2e-noop")

	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatalf("readNetworkConfig: %v\n%s", err, errBuf.String())
	}
	checks := map[string]string{
		// Network derived from slot + metadata.name.
		"KIND_EXPERIMENTAL_DOCKER_NETWORK": "e2e-noop",
		"LOK8S_NETWORK_CIDR":               "10.125.126.0/24",
		"LOK8S_NETWORK_BASE_IP":            "10.125.126.0",
		// Registries default to NOT shared (flipped 2026-08-17: shared mode
		// dual-homes every kind node, which is what makes node-IP drift
		// possible — see heal.go). The standard mirror set still ships by
		// default, but on the PROJECT subnet at .103+.
		"LOK8S_REGISTRY_SHARED":       "false",
		"LOK8S_REGISTRY_IP_BUILD":     "10.125.126.101",
		"LOK8S_REGISTRY_IP_CACHE":     "10.125.126.102",
		"LOK8S_REGISTRY_IP_IO_DOCKER": "10.125.126.103",
		"LOK8S_REGISTRY_IP_IO_QUAY":   "10.125.126.104",
		"LOK8S_REGISTRY_IP_IO_K8S":    "10.125.126.105",
		"LOK8S_REGISTRY_IP_IO_GHCR":   "10.125.126.106",
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestReadNodeConfigHostPortDefaults(t *testing.T) {
	_, _, _, p := testDriver(t)

	// Bare lok8s.dev defaults hostPorts to true.
	cy := slotSpec(t, p.Clusters, "lok8s.dev", "local")
	if err := readNodeConfig(cy, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOK8S_CP_COUNT") != "1" || os.Getenv("LOK8S_WORKER_COUNT") != "0" {
		t.Fatalf("node count defaults wrong: cp=%s workers=%s",
			os.Getenv("LOK8S_CP_COUNT"), os.Getenv("LOK8S_WORKER_COUNT"))
	}
	if os.Getenv("LOK8S_HOST_PORTS") != "true" {
		t.Fatal("bare lok8s.dev must default hostPorts true")
	}

	// Numbered slot defaults hostPorts to false.
	cy = slotSpec(t, p.Clusters, "126.lok8s.dev", "e2e-noop")
	if err := readNodeConfig(cy, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOK8S_HOST_PORTS") != "false" {
		t.Fatal("numbered slot must default hostPorts false")
	}
}

func TestReadNodeConfigRejectsBadMaxConcurrentDownloads(t *testing.T) {
	_, _, _, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, cy, "spec:\n  nodes:\n    maxConcurrentDownloads: nope\n")

	var errBuf bytes.Buffer
	if err := readNodeConfig(cy, &errBuf); err == nil {
		t.Fatal("bad maxConcurrentDownloads accepted")
	}
	if !strings.Contains(errBuf.String(), "error: spec.nodes.maxConcurrentDownloads must be a positive integer, got 'nope'") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

func TestReadLBConfigSlotDefaultAndNonSlotEmpty(t *testing.T) {
	_, _, _, p := testDriver(t)

	cy := slotSpec(t, p.Clusters, "126.lok8s.dev", "e2e-noop")
	readLBConfig(cy)
	if got := os.Getenv("LOK8S_LB_POOL"); got != "10.125.126.125-10.125.126.150" {
		t.Fatalf("slot pool = %q", got)
	}

	cy = slotSpec(t, p.Clusters, "prod.example.com", "prod")
	readLBConfig(cy)
	if got := os.Getenv("LOK8S_LB_POOL"); got != "" {
		t.Fatalf("non-lok8s.dev pool = %q, want empty", got)
	}
}

func TestNonSlotDomainWithExplicitNetworkGetsDefaultRegistries(t *testing.T) {
	// Domain-independent defaults (per-project registries, io-* mirror set)
	// apply to ANY domain — including non-lok8s.dev — as long as
	// spec.network.{name,cidr} is explicit. Only slot-derived fields are
	// *.lok8s.dev-only.
	_, _, errBuf, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "prod.example.com", "cluster.lok8s.yaml")
	writeFile(t, cy, `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: prod
spec:
  cluster:
    domain: prod.example.com
  network:
    name: prod
    cidr: "192.168.1.0/24"
`)
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatalf("readNetworkConfig: %v\n%s", err, errBuf.String())
	}
	checks := map[string]string{
		"LOK8S_REGISTRY_SHARED":       "false",
		"LOK8S_REGISTRY_IP_BUILD":     "192.168.1.101",
		"LOK8S_REGISTRY_IP_CACHE":     "192.168.1.102",
		"LOK8S_REGISTRY_IP_IO_DOCKER": "192.168.1.103",
		"LOK8S_REGISTRY_IP_IO_GHCR":   "192.168.1.106",
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestBareSpecWithoutNetworkErrors(t *testing.T) {
	// lo-cluster.lok8s.yaml has no spec.network and its domain
	// (test.lok8s.dev) is NOT a slot-parseable *.lok8s.dev (test != digits).
	// Non-slot domains must supply network.name + network.cidr explicitly.
	_, _, _, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster.lok8s.yaml")
	var errBuf bytes.Buffer
	if err := readNetworkConfig(cy, &errBuf); err == nil {
		t.Fatal("bare legacy spec accepted")
	}
	if !strings.Contains(errBuf.String(), "spec.network") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

// ── Shared registry mode (shared_registry_test.bats) ─────

func TestSharedTrueRegistryLayout(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-shared.lok8s.yaml")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatalf("readNetworkConfig: %v\n%s", err, errBuf.String())
	}
	checks := map[string]string{
		"LOK8S_REGISTRY_SHARED":       "true",
		"LOK8S_REGISTRY_NETWORK":      "lok8s-registries",
		"LOK8S_REGISTRY_NETWORK_CIDR": "10.125.200.0/24",
		// Pull-through mirrors get sequential IPs from the shared registry
		// network, starting at .2 (docker bridge gateway takes .1).
		"LOK8S_REGISTRY_IP_IO_DOCKER": "10.125.200.2",
		"LOK8S_REGISTRY_IP_IO_QUAY":   "10.125.200.3",
		"LOK8S_REGISTRY_IP_IO_K8S":    "10.125.200.4",
		"LOK8S_REGISTRY_IP_IO_GHCR":   "10.125.200.5",
		// Framework-private registries always live on the project /24 at
		// fixed offsets, even in shared mode.
		"LOK8S_REGISTRY_IP_BUILD": "10.125.125.101",
		"LOK8S_REGISTRY_IP_CACHE": "10.125.125.102",
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestSharedFalseAllIPsOnProjectSubnet(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-no-shared.lok8s.yaml")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"LOK8S_REGISTRY_SHARED":       "false",
		"LOK8S_REGISTRY_IP_BUILD":     "10.125.125.101",
		"LOK8S_REGISTRY_IP_CACHE":     "10.125.125.102",
		"LOK8S_REGISTRY_IP_IO_DOCKER": "10.125.125.103",
		"LOK8S_REGISTRY_IP_IO_QUAY":   "10.125.125.104",
		"LOK8S_REGISTRY_IP_IO_K8S":    "10.125.125.105",
		"LOK8S_REGISTRY_IP_IO_GHCR":   "10.125.125.106",
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// ── .registries.json byte-compat goldens ─────────────────

func TestRegistriesJSONMatchesBashGolden(t *testing.T) {
	cases := []struct{ fixture, golden string }{
		{"lo-cluster-shared.lok8s.yaml", "registries_shared.golden.json"},
		{"lo-cluster-no-shared.lok8s.yaml", "registries_noshared.golden.json"},
		{"lo-cluster-network.lok8s.yaml", "registries_network.golden.json"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			_, _, errBuf, p := testDriver(t)
			cy := loadFixture(t, p.Clusters, tc.fixture)
			if err := readNetworkConfig(cy, errBuf); err != nil {
				t.Fatalf("readNetworkConfig: %v\n%s", err, errBuf.String())
			}
			got := readFileT(t, filepath.Join(filepath.Dir(cy), ".registries.json"))
			want := readFileT(t, filepath.Join("testdata", tc.golden))
			if got != want {
				t.Errorf(".registries.json diverges from the bash jq output.\n--- bash\n%s\n--- go\n%s", want, got)
			}
		})
	}
}

// ── IP validation (lo::validate_ips) ─────────────────────

func TestValidateIPsSharedLayoutPasses(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-shared.lok8s.yaml")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	var vErr bytes.Buffer
	if err := validateIPs("10.125.125.0/24", "", &vErr); err != nil {
		t.Fatalf("valid shared layout rejected:\n%s", vErr.String())
	}
	// MetalLB pool inside the project /24 passes too.
	if err := validateIPs("10.125.125.0/24", "10.125.125.125-10.125.125.150", &vErr); err != nil {
		t.Fatalf("valid pool rejected:\n%s", vErr.String())
	}
}

func mutateRegistryIP(t *testing.T, name, newIP string) {
	t.Helper()
	path := os.Getenv("LOK8S_REGISTRY_JSON")
	rf, err := loadRegistryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rf.Registries {
		if rf.Registries[i].Name == name {
			rf.Registries[i].IP = newIP
		}
	}
	raw, err := json.Marshal(rf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateIPsSharedMirrorOutsideRegistrySubnetFails(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-shared.lok8s.yaml")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	// The registry state lives in the JSON — mutate the FILE (validate must
	// observe the mutation, per-call read like the bash jq).
	mutateRegistryIP(t, "io-docker", "10.0.0.99")

	var vErr bytes.Buffer
	if err := validateIPs("10.125.125.0/24", "", &vErr); err == nil {
		t.Fatal("mirror outside the shared subnet accepted")
	}
	if !strings.Contains(vErr.String(), "outside subnet") {
		t.Fatalf("wrong error:\n%s", vErr.String())
	}
}

func TestValidateIPsBuildOutsideProjectSubnetFails(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-shared.lok8s.yaml")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	mutateRegistryIP(t, "build", "10.0.0.99")

	var vErr bytes.Buffer
	if err := validateIPs("10.125.125.0/24", "", &vErr); err == nil {
		t.Fatal("build IP outside the project subnet accepted")
	}
	if !strings.Contains(vErr.String(), "outside subnet") {
		t.Fatalf("wrong error:\n%s", vErr.String())
	}
}

func TestValidateIPsCountsAllErrorsAndPrintsAborting(t *testing.T) {
	_, _, errBuf, p := testDriver(t)
	cy := loadFixture(t, p.Clusters, "lo-cluster-shared.lok8s.yaml")
	if err := readNetworkConfig(cy, errBuf); err != nil {
		t.Fatal(err)
	}
	mutateRegistryIP(t, "io-docker", "10.0.0.99")
	mutateRegistryIP(t, "build", "10.0.0.98")

	var vErr bytes.Buffer
	if err := validateIPs("10.125.125.0/24", "", &vErr); err == nil {
		t.Fatal("broken layout accepted")
	}
	// COUNTS ALL errors, then one summary line — a validator that stops at
	// the first error hides the rest of a broken layout.
	if !strings.Contains(vErr.String(), "error: 2 IP validation error(s). Aborting.") {
		t.Fatalf("count-all summary missing:\n%s", vErr.String())
	}
}

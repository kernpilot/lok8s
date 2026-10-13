package lo

// dnssearch_test.go — networking.dnsSearch in the kind config and the
// spec.network.dnsSearch reader. The reason for the key is at
// renderKindConfig. The goldens in testdata/ pin the full render for both
// implementations: tests/unit/lo_dns_search_test.bats compares the bash
// render with the same files.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/oidc"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// assertDNSSearch parses a rendered kind config and requires
// networking.dnsSearch to be a LIST equal to want (an empty list for nil).
// A missing key or a null keeps the host's search domains: kind passes
// KIND_DNS_SEARCH only for a set list.
func assertDNSSearch(t *testing.T, config string, want []string) {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(config), &root); err != nil {
		t.Fatalf("the kind config does not parse: %v\n%s", err, config)
	}
	n := yqsem.Lookup(&root, "networking", "dnsSearch")
	if n == nil || n.Kind != yaml.SequenceNode {
		t.Fatalf("networking.dnsSearch is not a list (the nodes keep the host's search domains):\n%s", config)
	}
	got := []string{}
	for _, item := range n.Content {
		got = append(got, item.Value)
	}
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("networking.dnsSearch = %q, want %q:\n%s", got, want, config)
	}
}

func TestRenderKindConfigDropsTheHostSearchDomains(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		cp, workers, hostPorts string
		oidc                   bool
	}{
		{name: "default", cp: "1", workers: "0", hostPorts: "false"},
		{name: "multinode", cp: "3", workers: "2", hostPorts: "false"},
		{name: "host ports and oidc", cp: "1", workers: "0", hostPorts: "true", oidc: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := goldenDriver(t)
			setNodeEnv(t, tc.cp, tc.workers, tc.hostPorts, "0")
			if tc.oidc {
				t.Setenv(oidc.EnvIssuer, "https://id.kubehz.dev")
				t.Setenv(oidc.EnvClientID, "kubectl-client")
			}
			assertDNSSearch(t, d.renderKindConfig("test-shared", "v1.31.4", "lok8s", ""), nil)
		})
	}
}

// The opt-out: spec.network.dnsSearch replaces the empty default. The
// golden is shared with the bats twin.
func TestRenderKindConfigDNSSearchMatchesBash(t *testing.T) {
	d := goldenDriver(t)
	setNodeEnv(t, "1", "0", "false", "0")
	t.Setenv("LOK8S_NETWORK_DNS_SEARCH", "corp.example.com example.ts.net")
	got := d.renderKindConfig("test-shared", "v1.31.4", "lok8s", "")
	assertGolden(t, got, "kindconfig_dnssearch.golden")
	assertDNSSearch(t, got, []string{"corp.example.com", "example.ts.net"})
}

func TestRenderDNSSearch(t *testing.T) {
	for in, want := range map[string]string{
		"":                                `[]`,
		"  ":                              `[]`,
		"corp.example.com":                `["corp.example.com"]`,
		"corp.example.com example.ts.net": `["corp.example.com", "example.ts.net"]`,
	} {
		if got := renderDNSSearch(in); got != want {
			t.Errorf("renderDNSSearch(%q) = %s, want %s", in, got, want)
		}
	}
}

// readDNSSearch writes a spec with the given spec body and runs the
// reader. It returns the exported list, the stderr and the error.
func readDNSSearch(t *testing.T, specBody string) (string, string, error) {
	t.Helper()
	cy := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, "kind: Lo\nmetadata:\n  name: dns\nspec:\n"+specBody)
	t.Setenv("LOK8S_NETWORK_DNS_SEARCH", "stale.example")
	var errBuf bytes.Buffer
	err := readDNSSearchConfig(cy, &errBuf)
	return os.Getenv("LOK8S_NETWORK_DNS_SEARCH"), errBuf.String(), err
}

func TestReadDNSSearchConfigAcceptsDNSNames(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	name253 := strings.Repeat(label63+".", 3) + strings.Repeat("b", 61) // 3*64 + 61 = 253
	for _, tc := range []struct{ name, body, want string }{
		{"no spec.network", "  cluster:\n    domain: lok8s.dev\n", ""},
		{"no key", "  network:\n    name: n\n", ""},
		{"null", "  network:\n    dnsSearch: ~\n", ""},
		{"empty value", "  network:\n    dnsSearch:\n", ""},
		{"empty list", "  network:\n    dnsSearch: []\n", ""},
		{"two names", "  network:\n    dnsSearch:\n      - corp.example.com\n      - example.ts.net\n", "corp.example.com example.ts.net"},
		{"flow list, quoted", "  network:\n    dnsSearch: [\"fritz.box\", 'lan']\n", "fritz.box lan"},
		{"digits, hyphens, punycode", "  network:\n    dnsSearch: [1and1.example, a-b.example, xn--bcher-kva.example]\n", "1and1.example a-b.example xn--bcher-kva.example"},
		{"63-character label, 253 characters", "  network:\n    dnsSearch: [" + label63 + ".example, " + name253 + "]\n", label63 + ".example " + name253},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, stderr, err := readDNSSearch(t, tc.body)
			if err != nil || stderr != "" {
				t.Fatalf("read failed: %v\n%s", err, stderr)
			}
			if got != tc.want {
				t.Fatalf("LOK8S_NETWORK_DNS_SEARCH = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadDNSSearchConfigRejectsANonList(t *testing.T) {
	for _, tc := range []struct{ value, tag string }{
		{"corp.example.com", "!!str"},
		{"{a: b}", "!!map"},
		{"true", "!!bool"},
	} {
		got, stderr, err := readDNSSearch(t, "  network:\n    dnsSearch: "+tc.value+"\n")
		if err == nil {
			t.Fatalf("dnsSearch: %s accepted", tc.value)
		}
		want := "error: spec.network.dnsSearch must be a list of DNS names, got " + tc.tag +
			". Write a YAML list, for example [corp.example.com].\n"
		if stderr != want {
			t.Fatalf("dnsSearch: %s: stderr\n%q\nwant\n%q", tc.value, stderr, want)
		}
		if got != "" {
			t.Fatalf("dnsSearch: %s: a rejected read exported %q", tc.value, got)
		}
	}
}

// Every bad entry gets its own line (index, observed value, next action),
// and a good entry among them is not exported.
func TestReadDNSSearchConfigReportsEveryBadEntry(t *testing.T) {
	label64 := strings.Repeat("a", 64) + ".example"
	long := strings.Repeat(strings.Repeat("a", 63)+".", 4) + "b" // 4*64 + 1 = 257
	got, stderr, err := readDNSSearch(t, `  network:
    dnsSearch:
      - corp.example.com
      - Corp.Example.com
      - corp_example
      - ""
      - example.com.
      - -bad.example
      - bad-.example
      - `+label64+`
      - `+long+`
      - é.example
      - "corp.example.com other.example"
      - {name: x}
      - [a]
      - 123
      - ~
`)
	if err == nil {
		t.Fatal("bad entries accepted")
	}
	if got != "" {
		t.Fatalf("a rejected read exported %q", got)
	}
	var want strings.Builder
	for i, observed := range []string{
		"'Corp.Example.com'", "'corp_example'", "''", "'example.com.'",
		"'-bad.example'", "'bad-.example'", "'" + label64 + "'", "'" + long + "'",
		"'é.example'", "'corp.example.com other.example'",
		"!!map", "!!seq", "!!int", "!!null",
	} {
		fmt.Fprintf(&want, "error: spec.network.dnsSearch[%d] must be a lowercase DNS name, got %s. "+
			"Use only a-z, 0-9, hyphens and dots, for example corp.example.com.\n", i+1, observed)
	}
	if stderr != want.String() {
		t.Fatalf("stderr:\n%s\nwant:\n%s", stderr, want.String())
	}
}

// provisionKindConfig runs Provision on the lifecycle fixture with the
// given spec and returns the config file `kind create cluster` got ("" when
// kind create never ran), the runner, the stderr and the error.
func provisionKindConfig(t *testing.T, spec string) (string, *fakeRunner, string, error) {
	t.Helper()
	d, runner, _, errBuf, p := provisionFixture(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), spec)

	var config string
	base := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "create" && c.Args[1] == "cluster" {
			if i := slices.Index(c.Args, "--config"); i >= 0 && i+1 < len(c.Args) {
				raw, err := os.ReadFile(c.Args[i+1])
				if err != nil {
					t.Errorf("read the kind config: %v", err)
				}
				config = string(raw)
			}
		}
		return base(c)
	}
	err := d.Provision(t.Context(), "test.lok8s.dev")
	return config, runner, errBuf.String(), err
}

// specWithDNSSearch is the lifecycle spec with spec.network.dnsSearch set.
func specWithDNSSearch(t *testing.T, list string) string {
	t.Helper()
	const at = "    cidr: \"10.125.50.0/24\"\n"
	if !strings.Contains(specShared, at) {
		t.Fatalf("specShared has no %q line", at)
	}
	return strings.Replace(specShared, at, at+"    dnsSearch: "+list+"\n", 1)
}

func TestProvisionHandsKindAConfigWithoutTheHostSearchDomains(t *testing.T) {
	config, _, stderr, err := provisionKindConfig(t, specShared)
	if err != nil {
		t.Fatalf("Provision: %v\nstderr:\n%s", err, stderr)
	}
	if config == "" {
		t.Fatal("kind create cluster never got a config")
	}
	assertDNSSearch(t, config, nil)
}

// The opt-out end to end: spec.network.dnsSearch, the read pass, the
// render, the file kind create gets.
func TestProvisionHandsKindTheSpecDNSSearch(t *testing.T) {
	config, _, stderr, err := provisionKindConfig(t, specWithDNSSearch(t, "[corp.example.com, example.ts.net]"))
	if err != nil {
		t.Fatalf("Provision: %v\nstderr:\n%s", err, stderr)
	}
	if config == "" {
		t.Fatal("kind create cluster never got a config")
	}
	assertDNSSearch(t, config, []string{"corp.example.com", "example.ts.net"})
}

// A bad entry stops the provision in the read pass: no docker network, no
// registry and no kind cluster for a rejected spec.
func TestProvisionRejectsABadDNSSearchBeforeAnyToolRuns(t *testing.T) {
	config, runner, stderr, err := provisionKindConfig(t, specWithDNSSearch(t, "[corp_example]"))
	if err == nil {
		t.Fatal("Provision accepted a bad spec.network.dnsSearch entry")
	}
	if !strings.Contains(stderr, "error: spec.network.dnsSearch[0] must be a lowercase DNS name, got 'corp_example'.") {
		t.Fatalf("wrong error:\n%s", stderr)
	}
	if config != "" || len(runner.calls) != 0 {
		t.Fatalf("tools ran for a rejected spec: %q", runner.calls)
	}
}

package lo

// dnssearch_test.go — the kind config drops the host's DNS search domains.
// Docker copies the search line of the host's resolv.conf into every kind
// node, and kubelet appended it to the search list of every pod (lanisce/ops:
// `search <ns>.svc.cluster.local svc.cluster.local cluster.local fritz.box
// tail6fced5.ts.net`, ndots:5; the Codex CLI, a musl build, failed every
// request after 5 s). `networking.dnsSearch: []` makes kind start each node
// with KIND_DNS_SEARCH="", and the node entrypoint then removes the search
// lines on every boot. The goldens in testdata/ pin the full render for both
// implementations (tests/unit/lo_dns_search_test.bats compares the bash
// render with the same files).

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/oidc"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// assertEmptyDNSSearch parses a rendered kind config and requires
// networking.dnsSearch to be an EMPTY LIST: a missing key or a null keeps
// the host's search domains (kind passes KIND_DNS_SEARCH only for a set
// list).
func assertEmptyDNSSearch(t *testing.T, config string) {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(config), &root); err != nil {
		t.Fatalf("the kind config does not parse: %v\n%s", err, config)
	}
	n := yqsem.Lookup(&root, "networking", "dnsSearch")
	if n == nil || n.Kind != yaml.SequenceNode || len(n.Content) != 0 {
		t.Fatalf("networking.dnsSearch is not an empty list (the nodes keep the host's search domains):\n%s", config)
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
			assertEmptyDNSSearch(t, d.renderKindConfig("test-shared", "v1.31.4", "lok8s", ""))
		})
	}
}

func TestProvisionHandsKindAConfigWithoutTheHostSearchDomains(t *testing.T) {
	d, runner, _, errBuf, p := provisionFixture(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specShared)

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

	if err := d.Provision(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatalf("Provision: %v\nstderr:\n%s", err, errBuf.String())
	}
	if config == "" {
		t.Fatal("kind create cluster never got a config")
	}
	assertEmptyDNSSearch(t, config)
}

package lo

// dnssearch_crd_test.go — the Lo CRD (operator/crds/schema/lo.schema.yaml)
// admits exactly the spec.network.dnsSearch entries that CheckDNSSearch
// accepts. The CRD rule is pure OpenAPI (maxItems, anyOf of maxLength +
// pattern), so this test applies it with Go regexp, the engine the
// apiserver uses for `pattern`.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/testutil"
)

type crdStringRule struct {
	MaxLength int    `yaml:"maxLength"`
	Pattern   string `yaml:"pattern"`
}

func TestCRDDNSSearchRuleMatchesTheCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "operator", "crds", "schema", "lo.schema.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Properties struct {
				Network struct {
					Properties struct {
						DNSSearch struct {
							MaxItems int `yaml:"maxItems"`
							Items    struct {
								Type  string          `yaml:"type"`
								AnyOf []crdStringRule `yaml:"anyOf"`
							} `yaml:"items"`
						} `yaml:"dnsSearch"`
					} `yaml:"properties"`
				} `yaml:"network"`
			} `yaml:"properties"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	rule := doc.Spec.Properties.Network.Properties.DNSSearch
	if rule.MaxItems != MaxDNSSearch {
		t.Errorf("CRD maxItems = %d, the CLI limit is %d", rule.MaxItems, MaxDNSSearch)
	}
	if rule.Items.Type != "string" || len(rule.Items.AnyOf) == 0 {
		t.Fatalf("CRD items rule has an unexpected shape: %+v", rule.Items)
	}
	admits := func(s string) bool {
		for _, branch := range rule.Items.AnyOf {
			if len(s) <= branch.MaxLength && regexp.MustCompile(branch.Pattern).MatchString(s) {
				return true
			}
		}
		return false
	}

	label63 := strings.Repeat("a", 63)
	name253 := strings.Join([]string{label63, label63, label63, strings.Repeat("b", 61)}, ".")
	for _, s := range []string{
		"corp.example.com", "Corp.Example.COM.", "a", "1and1.example", "xn--bcher-kva.example",
		name253, name253 + ".", name253 + "b", name253 + "b.", name253 + "..",
		"", ".", "a..b", ".a", "a.example..", "-a.example", "a-.example", "corp_example",
		strings.Repeat("a", 64) + ".example", label63 + ".example", "a b", "é.example", "a.example\n",
	} {
		cli := dnsSearchEntryProblem(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}) == ""
		if crd := admits(s); crd != cli {
			t.Errorf("%q (%d characters): the CRD admits it = %v, the CLI accepts it = %v", s, len(s), crd, cli)
		}
	}
}

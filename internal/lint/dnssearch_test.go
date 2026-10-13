package lint

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// A list that the Lo driver rejects fails lint, with the driver's own
// lines (the indent of every lint finding) and one error per bad entry.
// The bash twin is lint::dns_search; hack/parity-configure.sh diffs both.
func TestLintRejectsABadDNSSearch(t *testing.T) {
	t.Parallel()
	l, base, out, errOut := newLinter(t)
	testutil.WriteFile(t, filepath.Join(base, "clusters", "d.dev", "cluster.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: d\nspec:\n  network:\n    dnsSearch: [corp.example.com, corp_example, \"\"]\n")

	if l.all("d.dev") {
		t.Fatalf("lint passed a bad spec.network.dnsSearch:\n%s%s", out.String(), errOut.String())
	}
	for _, want := range []string{
		"[error]   spec.network.dnsSearch[1] has a character that is not valid in a DNS name: 'corp_example'. Use only letters, digits, hyphens and dots.\n",
		"[error]   spec.network.dnsSearch[2] is empty. Write a DNS name, or remove the entry.\n",
		"[error] 2 validation error(s)\n",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr misses %q:\n%s", want, errOut.String())
		}
	}
}

// A valid list, a missing key and the spec of another driver add no error.
func TestLintAcceptsAValidDNSSearch(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"valid list":   "kind: Lo\nspec:\n  network:\n    dnsSearch: [Corp.Example.com., example.ts.net]\n",
		"no key":       "kind: Lo\nspec:\n  network:\n    name: n\n",
		"kubeone spec": "kind: KubeOne\nspec:\n  network:\n    dnsSearch: [corp_example]\n",
	} {
		t.Run(name, func(t *testing.T) {
			l, base, _, errOut := newLinter(t)
			spec := filepath.Join(base, "clusters", "d.dev", "cluster.lok8s.yaml")
			testutil.WriteFile(t, spec, body)
			if got := l.dnsSearch(spec); got != 0 || errOut.Len() != 0 {
				t.Fatalf("dnsSearch = %d errors:\n%s", got, errOut.String())
			}
		})
	}
}

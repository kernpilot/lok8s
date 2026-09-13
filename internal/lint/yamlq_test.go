package lint

// yamlq_test.go pins the compact JSON output to yq.

import (
	"testing"

	"github.com/kernpilot/lok8s/internal/bootstrapspec"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

func TestCompactJSONMatchesYq(t *testing.T) {
	t.Parallel()
	// Rendering contract spots that appear in error messages.
	docs := parseDocs([]byte(`- cilium
- ccm:
    wait: true
    dependsOn: [a, b]
- x: {n: 1.5, s: "q<&>"}
`))
	if len(docs) != 1 {
		t.Fatal("fixture parse failed")
	}
	items := yqsem.SeqItems(docs[0])
	for i, want := range []string{
		`"cilium"`,
		`{"ccm":{"wait":true,"dependsOn":["a","b"]}}`,
		`{"x":{"n":1.5,"s":"q<&>"}}`,
	} {
		if got := bootstrapspec.CompactJSON(items[i]); got != want {
			t.Errorf("CompactJSON[%d] = %s, want %s", i, got, want)
		}
	}
}

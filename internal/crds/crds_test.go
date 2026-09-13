package crds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// repoLayout is the lok8s checkout's layout; skips when the schema source
// is not part of the checkout.
func repoLayout(t *testing.T) Layout {
	root := testutil.RepoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "operator", "crds", "schema")); err != nil {
		t.Skip("repo schema source not available")
	}
	return NewLayout(&config.Paths{Base: root, Lok8s: filepath.Join(root, ".lok8s")})
}

// The committed CRDs are the parity fixture: they were rendered by the bash
// `yq eval` implementation, so a byte-identical Go render IS the gate.
func TestRenderMatchesCommittedCRDs(t *testing.T) {
	t.Parallel()
	l := repoLayout(t)
	schemas := l.Schemas()
	if len(schemas) < 5 {
		t.Fatalf("expected the repo's schema set, got %v", schemas)
	}
	rendered := testutil.Tree{Name: "lo crds render", Files: map[string]string{}}
	for _, schema := range schemas {
		kind, err := Kind(schema)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Render(schema)
		if err != nil {
			t.Fatalf("%s: %v", schema, err)
		}
		rendered.Files[kind+".yaml"] = string(got)
	}
	testutil.Drift{
		Want: rendered,
		Got:  testutil.ReadDir(t, "operator/crds", l.OutDir, func(rel string) bool { return !strings.HasSuffix(rel, ".yaml") || strings.Contains(rel, "/") }),
		Sync: "bin/lo crds",
	}.Check(t)
}

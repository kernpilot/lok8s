package crds

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
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
	l := repoLayout(t)
	schemas := l.Schemas()
	if len(schemas) < 5 {
		t.Fatalf("expected the repo's schema set, got %v", schemas)
	}
	for _, schema := range schemas {
		kind, err := Kind(schema)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Render(schema)
		if err != nil {
			t.Fatalf("%s: %v", schema, err)
		}
		want, err := os.ReadFile(filepath.Join(l.OutDir, kind+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: render differs from committed operator/crds/%s.yaml\n%s", kind, kind, firstDiff(string(want), string(got)))
		}
	}
}

func firstDiff(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + w + "\n  got:  " + g
		}
	}
	return "(identical?)"
}

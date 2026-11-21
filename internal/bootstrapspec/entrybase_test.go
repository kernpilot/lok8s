package bootstrapspec

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
)

// TestParseDirBaseOfEachKeyForm pins the dir name that the readers of an
// entry take (filepath.Base of the dir that Parse gives: the audit, the
// KubeOne skip, --only and the addons hint) for each key form. The bats twin
// (tests/unit/spec_entry_base_test.bats) holds the same rows and checks
// spec::entry_base against these values. A row with reject is a key that
// Parse refuses. Base ends in "base", as in the bats twin.
func TestParseDirBaseOfEachKeyForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key    string
		want   string
		reject bool
	}{
		{key: "..", reject: true},
		{key: "addons", want: "addons"},
		{key: "a/../b", want: "b"},
		{key: "./x", want: "x"},
		{key: "../x", want: "x"},
		{key: "/x", want: "x"},
		{key: "x/", want: "x"},
		{key: "x//y", want: "y"},
		{key: "/", want: "base"},
		{key: "///", want: "base"},
		{key: "", want: "addons"},
		{key: "a b/*?[x]", want: "*?[x]"},
		{key: "a\nb", want: "a\nb"},
		{key: "./a\nb", want: "a\nb"},
		{key: "x\n", want: "x\n"},
		{key: "-n", want: "-n"},
		{key: "-e", want: "-e"},
	} {
		t.Run(fmt.Sprintf("%q", tc.key), func(t *testing.T) {
			t.Parallel()
			base := filepath.Join(t.TempDir(), "base")
			paths := &config.Paths{Base: base, Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
			var reports []string
			p := &Parser{Paths: paths, Report: func(format string, a ...any) {
				reports = append(reports, fmt.Sprintf(format, a...))
			}}
			node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: tc.key}
			e, ok := p.Parse("test.lok8s.dev", Item{Raw: CompactJSON(node), Node: node})
			if tc.reject {
				if ok || len(reports) != 1 || !strings.Contains(reports[0], "failed to parse addon name") {
					t.Errorf("want the key refused: ok=%v reports=%q", ok, reports)
				}
				return
			}
			if !ok {
				t.Fatalf("Parse refused the key: %q", reports)
			}
			if got := filepath.Base(e.Dir); got != tc.want {
				t.Errorf("filepath.Base(%q) = %q, want %q", e.Dir, got, tc.want)
			}
		})
	}
}

package lint

// defaults.go — the `lo lint --notes` advisory (Go-only, WP9): a spec key
// whose value equals its documented default can be dropped. Printed only
// on --notes so every `check - lint` parity case stays byte-identical:
// the bash lint has no such line. Advisory: no finding, no exit code.
//
// Only keys with a default in docs/reference/specs.md ("Default
// resolution", the Lo driver): spec.nodes.controlPlane, spec.runtime and
// the mirror list, read from the driver's own constants (internal/driver/lo
// defaults.go) so the two cannot drift. A key that is derived from another
// (the slot network keys) has no fixed value to compare against and gets
// no note.

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// scalarDefaults are the documented scalar defaults of a Lo cluster spec:
// the key path under the document root and the value the driver uses
// when the key is absent.
var scalarDefaults = []struct {
	path []string
	def  string
}{
	{[]string{"spec", "nodes", "controlPlane"}, lodriver.DefaultControlPlane},
	{[]string{"spec", "runtime"}, lodriver.DefaultRuntime},
}

// notes prints one `[note]` line per key of a Lo cluster spec that equals
// its documented default. Other drivers document no defaults for these
// keys, so their specs get no note.
func (l *Linter) notes(specFile string) {
	if !l.Notes {
		return
	}
	root := firstDoc(specFile)
	if yqsem.Or(yqsem.Lookup(root, "kind"), "") != "Lo" {
		return
	}
	file := specFile
	if rel, err := filepath.Rel(l.Paths.Base, specFile); err == nil {
		file = rel
	}
	note := func(key, value string) {
		fmt.Fprintf(l.Out, "[note] %s: %s equals the default (%s); you can drop it\n", file, key, value)
	}
	for _, d := range scalarDefaults {
		n := yqsem.Lookup(root, d.path...)
		if n != nil && !yqsem.IsNull(n) && yqsem.Scalar(n) == d.def {
			note(strings.Join(d.path, "."), d.def)
		}
	}
	if mirrorsEqualDefault(yqsem.SeqItems(yqsem.Lookup(root, "spec", "registries", "mirrors"))) {
		names := make([]string, 0, len(lodriver.DefaultMirrors()))
		for _, m := range lodriver.DefaultMirrors() {
			names = append(names, m.Name)
		}
		note("spec.registries.mirrors", strings.Join(names, ", ")+" on the standard upstream URLs")
	}
}

// mirrorsEqualDefault reports whether the listed mirrors are exactly the
// default set: the same names and URLs, no extra entry and no extra key
// per entry (order does not matter). An empty list is absent, not equal.
func mirrorsEqualDefault(items []*yaml.Node) bool {
	defaults := lodriver.DefaultMirrors()
	if len(items) != len(defaults) {
		return false
	}
	want := map[string]string{}
	for _, m := range defaults {
		want[m.Name] = m.URL
	}
	for _, it := range items {
		keys, ok := yqsem.MapKeys(it)
		if !ok || len(keys) != 2 {
			return false
		}
		name := yqsem.Scalar(yqsem.Lookup(it, "name"))
		url, seen := want[name]
		if !seen || url != yqsem.Scalar(yqsem.Lookup(it, "url")) {
			return false
		}
		delete(want, name)
	}
	return len(want) == 0
}

package lint

// defaults.go — the `lo lint --notes` advisory (Go-only, WP9): a spec key
// whose value equals its documented default can be dropped. Printed only
// on --notes so every `check - lint` parity case stays byte-identical:
// the bash lint has no such line. Advisory: no finding, no exit code.
//
// Only keys with a default in docs/reference/specs.md ("Default
// resolution", the Lo driver): spec.nodes.controlPlane, spec.runtime and
// the mirror list. A key that is derived from another (the slot network
// keys) has no fixed value to compare against and gets no note.

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// scalarDefaults are the documented scalar defaults of a Lo cluster spec:
// the key path under the document root and the value the driver uses
// when the key is absent (internal/driver/lo: config.go, lo.go).
var scalarDefaults = []struct {
	path []string
	def  string
}{
	{[]string{"spec", "nodes", "controlPlane"}, "1"},
	{[]string{"spec", "runtime"}, "kind"},
}

// defaultMirrors is the mirror list the Lo driver uses when
// spec.registries.mirrors is absent (internal/driver/lo/configregistry.go).
var defaultMirrors = []struct{ name, url string }{
	{"io-docker", "https://registry-1.docker.io"},
	{"io-quay", "https://quay.io"},
	{"io-k8s", "https://registry.k8s.io"},
	{"io-ghcr", "https://ghcr.io"},
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
		note("spec.registries.mirrors", "io-docker, io-quay, io-k8s, io-ghcr on the standard upstream URLs")
	}
}

// mirrorsEqualDefault reports whether the listed mirrors are exactly the
// default set: the same names and URLs, no extra entry and no extra key
// per entry (order does not matter). An empty list is absent, not equal.
func mirrorsEqualDefault(items []*yaml.Node) bool {
	if len(items) != len(defaultMirrors) {
		return false
	}
	want := map[string]string{}
	for _, m := range defaultMirrors {
		want[m.name] = m.url
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

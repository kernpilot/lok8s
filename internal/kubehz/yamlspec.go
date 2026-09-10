package kubehz

// yamlspec.go — the cluster.lok8s.yaml reader (internal/yqsem's Doc: `//`
// fires on null AND false, a bare `yq -r '<path>'` prints the literal word
// "null" for a missing path, and a whole-file parse failure is a distinct
// state — Doc.Err — the callers turn into their own error (read_config's
// `|| return 1`)), plus the two sequence renderings the kubehz reads use.

import (
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// specDoc is one loaded cluster spec.
type specDoc struct{ yqsem.Doc }

// loadSpec parses a YAML file.
func loadSpec(path string) specDoc {
	return specDoc{yqsem.Load(path)}
}

// seqOrScalar mirrors the exclusions reader
// `(<path> // []) | (select(type == "!!seq") // [.]) | .[]` as `yq -r`
// lines: a sequence's scalar entries verbatim, a scalar coerced to a
// single entry, null/missing → nothing.
func (d specDoc) seqOrScalar(path ...string) []string {
	n := d.Lookup(path...)
	if !yqsem.Present(n) {
		return nil
	}
	if n.Kind == yaml.SequenceNode {
		return seqScalars(n)
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	return nil
}

// seqStrings mirrors `yq -r '<path>[]?'`: the scalar entries of a
// sequence, nothing for anything else (the `?` swallows the error).
func (d specDoc) seqStrings(path ...string) []string {
	return seqScalars(d.Lookup(path...))
}

// seqScalars renders a sequence's entries as `yq -r` lines: scalars
// verbatim, nulls as the word "null", nested shapes skipped.
func seqScalars(n *yaml.Node) []string {
	var out []string
	for _, e := range yqsem.SeqItems(n) {
		if yqsem.IsNull(e) {
			out = append(out, "null")
			continue
		}
		if e.Kind == yaml.ScalarNode {
			out = append(out, e.Value)
		}
	}
	return out
}

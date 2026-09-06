package kubehz

// yamlspec.go — yq-semantics readers over cluster.lok8s.yaml. The bash read
// every spec field with `yq -r '<path> // <default>'`; these helpers keep
// that contract (the capi driver's yamlspec idiom): `//` fires on null AND
// false, a bare `yq -r '<path>'` prints the literal word "null" for a
// missing path, and a whole-file parse failure is a distinct state the
// callers turn into their own error (read_config's `|| return 1`).

import (
	"os"

	"gopkg.in/yaml.v3"
)

// specDoc is one loaded cluster spec. err != nil = the file was missing or
// unparsable (the bash "yq failed" state).
type specDoc struct {
	root *yaml.Node
	err  error
}

// loadSpec parses a YAML file.
func loadSpec(path string) specDoc {
	raw, err := os.ReadFile(path)
	if err != nil {
		return specDoc{err: err}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return specDoc{err: err}
	}
	return specDoc{root: &root}
}

func yderef(n *yaml.Node) *yaml.Node {
	for n != nil && (n.Kind == yaml.DocumentNode || n.Kind == yaml.AliasNode) {
		if n.Kind == yaml.DocumentNode {
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
			continue
		}
		n = n.Alias
	}
	return n
}

// lookup walks a mapping path, nil when any hop is missing or not a map.
func (d specDoc) lookup(path ...string) *yaml.Node {
	if d.err != nil {
		return nil
	}
	n := yderef(d.root)
	for _, key := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				next = n.Content[i+1]
				break
			}
		}
		n = yderef(next)
	}
	return n
}

func isNull(n *yaml.Node) bool { return n == nil || n.Tag == "!!null" }

func isFalse(n *yaml.Node) bool {
	return n != nil && n.Tag == "!!bool" &&
		(n.Value == "false" || n.Value == "False" || n.Value == "FALSE")
}

// raw mirrors `yq -r '<path>'`: the scalar's string value, the literal
// word "null" when the path is missing or null, "" on an unreadable file
// or a non-scalar node.
func (d specDoc) raw(path ...string) string {
	if d.err != nil {
		return ""
	}
	n := d.lookup(path...)
	if isNull(n) {
		return "null"
	}
	if n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// or mirrors `yq -r '<path> // "<def>"'`: def fires when the path is
// missing, null, or FALSE (yq's alternative-operator falsiness); "" on an
// unreadable file.
func (d specDoc) or(def string, path ...string) string {
	if d.err != nil {
		return ""
	}
	n := d.lookup(path...)
	if isNull(n) || isFalse(n) || n.Kind != yaml.ScalarNode {
		return def
	}
	return n.Value
}

// orChain mirrors `yq -r 'A // B // "<def>"'` for scalar paths.
func (d specDoc) orChain(def string, paths ...[]string) string {
	if d.err != nil {
		return ""
	}
	for _, p := range paths {
		n := d.lookup(p...)
		if isNull(n) || isFalse(n) || n.Kind != yaml.ScalarNode {
			continue
		}
		return n.Value
	}
	return def
}

// seqOrScalar mirrors the exclusions reader
// `(<path> // []) | (select(type == "!!seq") // [.]) | .[]` as `yq -r`
// lines: a sequence's scalar entries verbatim, a scalar coerced to a
// single entry, null/missing → nothing.
func (d specDoc) seqOrScalar(path ...string) []string {
	n := d.lookup(path...)
	if isNull(n) || isFalse(n) {
		return nil
	}
	if n.Kind == yaml.SequenceNode {
		var out []string
		for _, e := range n.Content {
			e = yderef(e)
			if isNull(e) {
				out = append(out, "null")
				continue
			}
			if e.Kind == yaml.ScalarNode {
				out = append(out, e.Value)
			}
		}
		return out
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	return nil
}

// seqStrings mirrors `yq -r '<path>[]?'`: the scalar entries of a
// sequence, nothing for anything else (the `?` swallows the error).
func (d specDoc) seqStrings(path ...string) []string {
	n := d.lookup(path...)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, e := range n.Content {
		e = yderef(e)
		if isNull(e) {
			out = append(out, "null")
			continue
		}
		if e.Kind == yaml.ScalarNode {
			out = append(out, e.Value)
		}
	}
	return out
}

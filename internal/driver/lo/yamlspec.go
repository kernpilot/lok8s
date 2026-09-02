package lo

// yamlspec.go — yq-semantics YAML readers over cluster.lok8s.yaml. The bash
// read the spec with `yq -r '<path> // <default>'` per field; these helpers
// replicate that contract exactly, INCLUDING yq's `//` alternative firing on
// null AND false (not just missing) — e.g. `.spec.remote.tilt // "true"`
// yields "true" for an explicit `tilt: false`, and the port keeps that
// behavior rather than silently fixing it.

import (
	"os"

	"gopkg.in/yaml.v3"
)

// loadYAML parses a YAML file into its root node. A missing or unparsable
// file returns nil (callers that need the distinction stat the file first,
// like the bash did).
func loadYAML(path string) *yaml.Node {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return nil
	}
	return &root
}

// yderef unwraps document/alias nodes.
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

// ylookup walks a mapping path, nil when any hop is missing or not a map.
func ylookup(n *yaml.Node, path ...string) *yaml.Node {
	n = yderef(n)
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

func isNull(n *yaml.Node) bool {
	return n == nil || n.Tag == "!!null"
}

// yqRaw mirrors `yq -r '<path>'` for scalars: the scalar's string value,
// "null" when the path is missing or null (yq prints the literal word), and
// "" when the node is a non-scalar (the bash call sites only ever hit
// scalars here; empty routes them to their explicit-empty branches).
func yqRaw(root *yaml.Node, path ...string) string {
	n := ylookup(root, path...)
	if isNull(n) {
		return "null"
	}
	if n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// yqOr mirrors `yq -r '<path> // <def>'`: def fires when the path is
// missing, null, or FALSE (yq's alternative operator semantics); any other
// scalar value passes through.
func yqOr(root *yaml.Node, def string, path ...string) string {
	n := ylookup(root, path...)
	if isNull(n) {
		return def
	}
	if n.Kind != yaml.ScalarNode {
		return def
	}
	if n.Tag == "!!bool" && (n.Value == "false" || n.Value == "False" || n.Value == "FALSE") {
		return def
	}
	return n.Value
}

// yqPresent reports whether the path resolves to a non-null node (bash:
// `[[ -n $(yq -r '<path> // ""') ]]` over a map/list — the stringified
// YAML of a present node is non-empty).
func yqPresent(root *yaml.Node, path ...string) bool {
	return !isNull(ylookup(root, path...))
}

// yqSeq returns the sequence node's entries ([]*yaml.Node), nil for
// missing/non-sequence (yq `| length // 0` reads 0 there).
func yqSeq(root *yaml.Node, path ...string) []*yaml.Node {
	n := ylookup(root, path...)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		out[i] = yderef(c)
	}
	return out
}

func getenv(key string) string { return os.Getenv(key) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

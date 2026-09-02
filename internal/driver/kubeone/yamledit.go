package kubeone

// Small yaml.Node edit helpers standing in for the bash `yq -i` merges.
// Like yq -i, an edit re-serializes the whole manifest; unlike yq the
// output formatting is yaml.v3's (2-space indent) — the merged CONTENT is
// what the contract (and the tests) pin, kubeone parses either.

import (
	"os"

	"gopkg.in/yaml.v3"
)

// loadYAMLDoc parses a YAML file into its root mapping node (creating an
// empty mapping for an empty document).
func loadYAMLDoc(path string) (*yaml.Node, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
}

// saveYAMLDoc writes the mapping node back with 2-space indentation.
func saveYAMLDoc(path string, root *yaml.Node) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	enc := yaml.NewEncoder(f)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		_ = f.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ensureMapPath walks (creating missing mappings) to the mapping node at
// the given key path.
func ensureMapPath(root *yaml.Node, keys ...string) *yaml.Node {
	cur := root
	for _, key := range keys {
		next := mapValue(cur, key)
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(cur, key, next)
		}
		cur = next
	}
	return cur
}

// mapValue returns the value node for key in a mapping node, nil when
// absent.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setKey sets (or replaces) key in a mapping node.
func setKey(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

func strNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func boolNode(v bool) *yaml.Node {
	val := "false"
	if v {
		val = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: val}
}

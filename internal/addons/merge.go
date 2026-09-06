package addons

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// MergeValueFiles deep-merges YAML files left to right (later wins) with the
// yq idiom the bash pipeline used everywhere:
//
//	yq eval-all '. as $item ireduce ({}; . * $item)'
//
// Maps deep-merge; sequences and scalars REPLACE. Returns the merged YAML.
func MergeValueFiles(paths ...string) ([]byte, error) {
	acc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		acc = MergeNodes(acc, derefNode(&doc))
	}
	return marshalNode(acc)
}

// MergeYAML merges YAML documents given as strings, same semantics as
// MergeValueFiles (bootstrap valueFiles pre-merge feeds strings + files).
func MergeYAML(docs ...string) ([]byte, error) {
	acc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, d := range docs {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(d), &doc); err != nil {
			return nil, err
		}
		acc = MergeNodes(acc, derefNode(&doc))
	}
	return marshalNode(acc)
}

// MergeNodes is yq's `*` operator over two nodes: two mappings merge
// key-wise (right side deep-merged in, left order preserved, new keys
// appended); anything else — sequences, scalars, a nil/null right side over
// a map — takes the RIGHT side.
func MergeNodes(left, right *yaml.Node) *yaml.Node {
	left, right = derefNode(left), derefNode(right)
	if right == nil {
		return left
	}
	if left == nil || left.Kind != yaml.MappingNode || right.Kind != yaml.MappingNode {
		return right
	}
	merged := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	merged.Content = append(merged.Content, left.Content...)
	for i := 0; i+1 < len(right.Content); i += 2 {
		key, val := right.Content[i], right.Content[i+1]
		found := false
		for j := 0; j+1 < len(merged.Content); j += 2 {
			if merged.Content[j].Value == key.Value {
				merged.Content[j+1] = MergeNodes(merged.Content[j+1], val)
				found = true
				break
			}
		}
		if !found {
			merged.Content = append(merged.Content, key, val)
		}
	}
	return merged
}

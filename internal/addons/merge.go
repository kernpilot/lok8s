package addons

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// MergeValueFiles deep-merges YAML files left to right (later wins) with the
// yq idiom the bash pipeline used everywhere:
//
//	yq eval-all '. as $item ireduce ({}; . * $item)'
//
// Maps deep-merge; sequences and scalars REPLACE. Every document of every
// file takes part, in stream order (`eval-all` reads them all). Returns the
// merged YAML.
func MergeValueFiles(paths ...string) ([]byte, error) {
	acc := emptyMap()
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if acc, err = mergeStream(acc, raw); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return marshalNode(acc)
}

// MergeYAML merges YAML streams given as strings, same semantics as
// MergeValueFiles (bootstrap valueFiles pre-merge feeds strings + files).
func MergeYAML(docs ...string) ([]byte, error) {
	acc := emptyMap()
	for _, d := range docs {
		var err error
		if acc, err = mergeStream(acc, []byte(d)); err != nil {
			return nil, err
		}
	}
	return marshalNode(acc)
}

func emptyMap() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

// mergeStream folds every document of one YAML stream into acc, the way
// `yq eval-all` feeds each document to the ireduce. yaml.Unmarshal would
// read the first document only and drop the rest.
func mergeStream(acc *yaml.Node, raw []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return acc, nil
		}
		if err != nil {
			return nil, err
		}
		if acc, err = MergeDocument(acc, &doc); err != nil {
			return nil, err
		}
	}
}

// MergeDocument is one `. * $item` step of the ireduce, over whole
// documents. yq v4.53.3 (the pinned .bin/yq) at this level:
//
//   - a null document (`null`, `~`, an empty `---`) on either side is a
//     no-op: the other side comes through unchanged;
//   - a document that is not a map on either side is an error, printed by
//     yq as `cannot multiply !!map with !!seq` (the two tags in order);
//   - two maps deep-merge (MergeNodes).
//
// The nested rules differ (a nested null or sequence REPLACES); those live
// in MergeNodes. A missing document (the zero node an empty or comment-only
// stream decodes to) is skipped like a null: yq reads no document there.
//
// This is the ONE implementation of the idiom. The audit effective-values
// stack (internal/audit) folds its decoded documents through it as well, so
// the 17 pinned yq cases in merge_test.go cover both callers.
func MergeDocument(acc, doc *yaml.Node) (*yaml.Node, error) {
	acc, doc = yqsem.Deref(acc), yqsem.Deref(doc)
	if isNullDocument(doc) {
		return acc, nil
	}
	if isNullDocument(acc) {
		return doc, nil
	}
	if acc.Kind != yaml.MappingNode || doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("cannot multiply %s with %s", documentTag(acc), documentTag(doc))
	}
	return MergeNodes(acc, doc), nil
}

func isNullDocument(n *yaml.Node) bool {
	return n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

// documentTag is the resolved short tag yq names in its error line.
func documentTag(n *yaml.Node) string {
	if n.Kind == yaml.MappingNode {
		return "!!map"
	}
	if n.Kind == yaml.SequenceNode {
		return "!!seq"
	}
	return n.ShortTag()
}

// MergeNodes is yq's `*` operator over two NESTED nodes: two mappings merge
// key-wise (right side deep-merged in, left order preserved, new keys
// appended); anything else — sequences, scalars, a nil/null right side over
// a map — takes the RIGHT side. An absent right side (nil, or the zero
// node an empty or comment-only document decodes to) leaves the left side
// as it is. Whole documents go through mergeDocument, which adds the
// top-level rules (a null document is a no-op, a non-map document is an
// error).
func MergeNodes(left, right *yaml.Node) *yaml.Node {
	left, right = yqsem.Deref(left), yqsem.Deref(right)
	if right == nil || right.Kind == 0 {
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

package yqsem

// merge.go — YAML merge keys (`<<: *anchor`) the way the pinned yq reads a
// path under --yaml-fix-merge-anchor-to-spec (pkg/yqlib/operator_traverse_
// path.go, doTraverseMap and traverseMergeAnchor, yq v4.53.3). A plain path
// read in yq resolves aliases and merge keys one key at a time and never
// expands the rest of the document, so an alias bomb or an alias cycle off
// the path costs nothing.
//
// Lookup and MapGet do not resolve merge keys, but yq does, in every read.
// A reader that must never see a merged value as missing uses MapGetMerged
// and CheckMerges. The other readers keep their current behavior.

import (
	"errors"
	"io"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// ErrMergeValue is a merge key whose value is not a mapping or a list of
// mappings. yq skips such a value without an error (a list stops at the
// first item that is not a mapping, from its end), so a merged version
// could vanish; the readers refuse it instead.
var ErrMergeValue = errors.New("a merge key (<<) does not refer to a map or a list of maps")

// ErrMergeCycle is a merge chain that leads back to a mapping that the read
// is already in (`k: &k {<<: *k}`). yq recurses on it until it runs out of
// stack or memory.
var ErrMergeCycle = errors.New("a merge key (<<) refers back to its own map")

// LoadDocs parses every YAML document of a file, in file order. A file with
// no content has no document. A trailing `---` starts an empty document,
// the same as in yq (`yq ea '[.] | length'`). The error is the read error or
// the first parse error.
func LoadDocs(path string) ([]*yaml.Node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	var docs []*yaml.Node
	for {
		doc := new(yaml.Node)
		err := dec.Decode(doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
}

// isMergeKey reports a merge key: a plain `<<` (tag !!merge). A quoted
// "<<" is an ordinary string key, in yaml.v3 and in yq.
func isMergeKey(k *yaml.Node) bool {
	k = Deref(k)
	return k != nil && k.Kind == yaml.ScalarNode && k.Tag == "!!merge"
}

// MapGetMerged reads key from a mapping the way a yq path read does:
//
//   - the merge keys first, the last `<<` first; a mapping is read with
//     these same rules (its own merge keys too); a list is read from its
//     last item to its first, and stops at an item that is not a mapping;
//     any other value is skipped;
//   - then the keys of the mapping itself, in file order.
//
// Every match replaces the one before, so a key of the mapping wins (the
// last duplicate), and among the merged mappings the first `<<` and the
// first list item win: the YAML spec order. found reports whether the key
// exists at all (a null value is found). Like yq, the read visits every
// merge key on the way, also after a match; a merge chain that comes back
// to a mapping that the read is in is ErrMergeCycle.
func MapGetMerged(n *yaml.Node, key string) (v *yaml.Node, found bool, err error) {
	n = Deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	return mapGetMerged(n, key, map[*yaml.Node]bool{})
}

func mapGetMerged(n *yaml.Node, key string, path map[*yaml.Node]bool) (v *yaml.Node, found bool, err error) {
	if path[n] {
		return nil, false, ErrMergeCycle
	}
	path[n] = true
	defer delete(path, n)

	read := func(src *yaml.Node) error {
		sv, sf, err := mapGetMerged(src, key, path)
		if sf {
			v, found = sv, true
		}
		return err
	}
	for i := len(n.Content) - 2; i >= 0; i -= 2 {
		if !isMergeKey(n.Content[i]) {
			continue
		}
		switch src := Deref(n.Content[i+1]); {
		case src == nil:
		case src.Kind == yaml.MappingNode:
			if err := read(src); err != nil {
				return nil, false, err
			}
		case src.Kind == yaml.SequenceNode:
			for _, item := range slices.Backward(src.Content) {
				item = Deref(item)
				if item == nil || item.Kind != yaml.MappingNode {
					break
				}
				if err := read(item); err != nil {
					return nil, false, err
				}
			}
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := Deref(n.Content[i]); k != nil && !isMergeKey(k) && k.Value == key {
			v, found = Deref(n.Content[i+1]), true
		}
	}
	return v, found, nil
}

// CheckMerges returns ErrMergeValue for the first merge key under n (in the
// whole tree, aliases not followed) whose value is not a mapping or a list
// of mappings, after one alias step: what yq would skip without an error.
// It walks each node once and expands nothing. Cycles are MapGetMerged's
// part: a cycle that a read does not pass through costs nothing.
func CheckMerges(n *yaml.Node) error {
	isMap := func(v *yaml.Node) bool {
		v = Deref(v)
		return v != nil && v.Kind == yaml.MappingNode
	}
	var walk func(n *yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n == nil || n.Kind == yaml.AliasNode {
			return nil
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if !isMergeKey(n.Content[i]) {
					continue
				}
				switch v := Deref(n.Content[i+1]); {
				case isMap(v):
				case v != nil && v.Kind == yaml.SequenceNode:
					for _, item := range v.Content {
						if !isMap(item) {
							return ErrMergeValue
						}
					}
				default:
					return ErrMergeValue
				}
			}
		}
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(n)
}

package yqsem

// merge.go — YAML merge keys (`<<: *anchor`) as yq resolves them under
// --yaml-fix-merge-anchor-to-spec, in a path read and in explode(.).
//
// Lookup and MapGet do not resolve merge keys, but yq does, in every read.
// A reader that must never see a merged value as missing uses MapGetMerged
// and CheckMerges. The other readers keep their current behavior.

import (
	"errors"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// ErrMergeValue is the merge key that yq's explode(.) refuses: its value is
// not a mapping or a list of mappings ("can only use merge anchors with
// maps (!!map) or sequences (!!seq) of maps").
var ErrMergeValue = errors.New("a merge key (<<) does not refer to a map or a list of maps")

// ErrMergeCycle is a merge chain that leads back to its own mapping
// (`k: &k {<<: *k}`). yq overflows its stack on it.
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

// mergeSources returns the mappings that a merge value names, in
// precedence order: one mapping, or the mappings of a list. ok is false for
// any other value (ErrMergeValue).
func mergeSources(v *yaml.Node) (srcs []*yaml.Node, ok bool) {
	v = Deref(v)
	if v == nil {
		return nil, false
	}
	switch v.Kind {
	case yaml.MappingNode:
		return []*yaml.Node{v}, true
	case yaml.SequenceNode:
		for _, item := range v.Content {
			item = Deref(item)
			if item == nil || item.Kind != yaml.MappingNode {
				return nil, false
			}
			srcs = append(srcs, item)
		}
		return srcs, true
	}
	return nil, false
}

// MapGetMerged is MapGet with the merge keys of the mapping resolved:
//
//   - a key of the mapping itself wins, also when its value is null, and
//     the last duplicate wins (yq reads the last one);
//   - otherwise the merge keys supply the key, in file order; for a list
//     (`<<: [*a, *b]`) the first mapping that has the key wins. A merged
//     mapping resolves its own merge keys the same way.
//
// found reports whether the key exists at all: a null value is found. A
// merge value that is not a mapping, and a merge cycle, supply nothing here;
// CheckMerges reports them.
func MapGetMerged(n *yaml.Node, key string) (v *yaml.Node, found bool) {
	return mapGetMerged(Deref(n), key, map[*yaml.Node]bool{})
}

func mapGetMerged(n *yaml.Node, key string, seen map[*yaml.Node]bool) (*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.MappingNode || seen[n] {
		return nil, false
	}
	seen[n] = true
	var local *yaml.Node
	found := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := Deref(n.Content[i]); k != nil && !isMergeKey(k) && k.Value == key {
			local, found = Deref(n.Content[i+1]), true
		}
	}
	if found {
		return local, true
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if !isMergeKey(n.Content[i]) {
			continue
		}
		srcs, _ := mergeSources(n.Content[i+1])
		for _, src := range srcs {
			if v, ok := mapGetMerged(src, key, seen); ok {
				return v, true
			}
		}
	}
	return nil, false
}

// CheckMerges walks every node under n (aliases followed once) and returns
// ErrMergeValue or ErrMergeCycle for the first merge key that yq's
// explode(.) cannot resolve. explode(.) checks the whole document, not only
// the path that a reader walks, so this check does too.
func CheckMerges(n *yaml.Node) error {
	seen := map[*yaml.Node]bool{}
	state := map[*yaml.Node]int{} // the merge graph: 1 = on the path, 2 = done
	var follow func(m *yaml.Node) error
	follow = func(m *yaml.Node) error {
		switch state[m] {
		case 1:
			return ErrMergeCycle
		case 2:
			return nil
		}
		state[m] = 1
		for i := 0; i+1 < len(m.Content); i += 2 {
			if !isMergeKey(m.Content[i]) {
				continue
			}
			srcs, ok := mergeSources(m.Content[i+1])
			if !ok {
				return ErrMergeValue
			}
			for _, src := range srcs {
				if err := follow(src); err != nil {
					return err
				}
			}
		}
		state[m] = 2
		return nil
	}
	var walk func(n *yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n == nil || seen[n] {
			return nil
		}
		seen[n] = true
		if n.Kind == yaml.AliasNode {
			return walk(n.Alias)
		}
		if n.Kind == yaml.MappingNode {
			if err := follow(n); err != nil {
				return err
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

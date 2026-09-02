package audit

// YAML plumbing shared by the checks, built on yaml.Node (NOT decoded Go
// values) so scalar literals keep their source spelling — the same fidelity
// the bash gets from yq. `policyAuditMode: True` must stay "True" (≠ "true" →
// the unknown branch), `~` must stay "~", `1.0` must stay "1.0"; a decoded
// bool/float would silently canonicalize all three (verified against the
// pinned yq v4.53: it preserves scalar style even through a merge).

import (
	"errors"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// firstDocNode parses the FIRST document of path, nil on any read/parse
// error. Spec files are read first-document-only (they are single-document in
// practice; a multi-document spec would make the bash yq emit one line per
// document and fail its numeric/equality guards anyway).
func firstDocNode(path string) *yaml.Node {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return resolveNode(&doc)
}

// decodeDocs parses every document of path. partial=true keeps the documents
// decoded before a parse error (yq streams output per document, so a broken
// trailing document does not erase earlier ones); partial=false discards
// everything on any error (yq eval-all collects before emitting).
func decodeDocs(path string, partial bool) ([]*yaml.Node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	var docs []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if partial {
				return docs, nil
			}
			return nil, err
		}
		docs = append(docs, resolveNode(&n))
	}
	return docs, nil
}

// resolveNode unwraps document and alias nodes to the underlying value node.
func resolveNode(n *yaml.Node) *yaml.Node {
	for n != nil {
		switch {
		case n.Kind == yaml.DocumentNode && len(n.Content) > 0:
			n = n.Content[0]
		case n.Kind == yaml.AliasNode && n.Alias != nil:
			n = n.Alias
		default:
			return n
		}
	}
	return nil
}

// mapValue returns the value node for key in a mapping node (first match),
// nil when absent or when n is not a mapping.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := resolveNode(n.Content[i])
		if k != nil && k.Value == key {
			return resolveNode(n.Content[i+1])
		}
	}
	return nil
}

// hasKey reports whether a mapping node carries the key (yq: has("k")).
func hasKey(n *yaml.Node, key string) bool {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := resolveNode(n.Content[i])
		if k != nil && k.Value == key {
			return true
		}
	}
	return false
}

// lookupPath walks nested mapping keys; nil as soon as one is absent.
func lookupPath(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		n = mapValue(n, k)
		if n == nil {
			return nil
		}
	}
	return n
}

// lookupFile is lookupPath over a file's first document.
func lookupFile(path string, keys ...string) *yaml.Node {
	return lookupPath(firstDocNode(path), keys...)
}

// isNullNode reports a YAML null (absent nodes are handled by callers).
func isNullNode(n *yaml.Node) bool {
	n = resolveNode(n)
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// yqRenderNode mirrors `yq -r` output for a node: an absent node renders as
// "null" (yq's missing-key result); a scalar renders as its SOURCE literal
// (yq preserves scalar style: `~` stays "~", `True` stays "True"); a
// map/sequence renders as YAML with the trailing newline trimmed (bash `$()`
// strips it).
func yqRenderNode(n *yaml.Node) string {
	n = resolveNode(n)
	if n == nil {
		return "null"
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!null" && n.Value == "" {
			return "null"
		}
		return n.Value
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// altNode mirrors yq's `//` alternative operator applied to a node read:
// absent, null, or boolean false yields def; anything else its rendering
// (`//` treats ONLY null and false as empty — an empty string is kept).
func altNode(n *yaml.Node, def string) string {
	n = resolveNode(n)
	if n == nil || isNullNode(n) {
		return def
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!bool" && strings.EqualFold(n.Value, "false") {
		return def
	}
	return yqRenderNode(n)
}

// specLine returns the 1-based line of the value node at the key path, 0 when
// the key is absent or the file is unreadable (bash: audit::_spec_line, the
// yq `line` operator — fail-soft like every other read).
func specLine(path string, keys ...string) int {
	n := lookupFile(path, keys...)
	if n == nil {
		return 0
	}
	return n.Line
}

// mergeYAMLDocs is the yq deep-merge the effective-values stack uses
// (`yq eval-all '. as $item ireduce ({}; . * $item)'`): every document of
// every file merges left to right into a fresh mapping — maps deep-merge,
// everything else (lists, scalars, explicit nulls at a key) REPLACES. A
// document that is a whole-document null is a no-op (yq: `x * null` → x); a
// non-map document is an ERROR, exactly like yq's "cannot multiply !!map
// with !!str" — the caller turns that into an unknown finding, never a
// fall-through pass. extra (may be nil) merges last — the inline override.
func mergeYAMLDocs(files []string, extra *yaml.Node) (*yaml.Node, error) {
	acc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, f := range files {
		docs, err := decodeDocs(f, false)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			if err := mergeDoc(acc, d); err != nil {
				return nil, err
			}
		}
	}
	if extra != nil {
		if err := mergeDoc(acc, extra); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

func mergeDoc(acc *yaml.Node, doc *yaml.Node) error {
	doc = resolveNode(doc)
	if doc == nil || isNullNode(doc) {
		return nil
	}
	if doc.Kind != yaml.MappingNode {
		return errors.New("cannot multiply !!map with " + doc.Tag)
	}
	mergeMap(acc, doc)
	return nil
}

// mergeMap merges src into dst: keys new to dst append; keys present in both
// deep-merge when BOTH values are mappings, otherwise src's value replaces
// (including an explicit null — yq replaces at key level).
func mergeMap(dst, src *yaml.Node) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		key := resolveNode(src.Content[i])
		val := resolveNode(src.Content[i+1])
		if key == nil {
			continue
		}
		existing := -1
		for j := 0; j+1 < len(dst.Content); j += 2 {
			k := resolveNode(dst.Content[j])
			if k != nil && k.Value == key.Value {
				existing = j
				break
			}
		}
		if existing < 0 {
			dst.Content = append(dst.Content, key, val)
			continue
		}
		cur := resolveNode(dst.Content[existing+1])
		if cur != nil && cur.Kind == yaml.MappingNode && val != nil && val.Kind == yaml.MappingNode {
			mergeMap(cur, val)
			continue
		}
		dst.Content[existing+1] = val
	}
}

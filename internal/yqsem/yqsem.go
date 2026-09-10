// Package yqsem is the ONE yq-semantics reader over yaml.v3 nodes. The bash
// implementation reads every spec field through `yq -r '<path> // <def>'`,
// `yq -r '<path>'`, `| tostring`, `keys`, `.[]` … and the ports must yield the
// same bytes. Every package used to carry its own copy of the same walker;
// this leaf package holds the core (load, dereference, path lookup, scalar
// rendering, the `//` alternative) so the coercions live — and are pinned —
// in one place.
//
// The alternative operator comes in THREE flavours because the ports do not
// agree, and each call site is parity-pinned to the one it had:
//
//   - Or is yq's `//`: null AND boolean false (false/False/FALSE — the
//     pinned yq v4.53 treats every spelling as falsy) fall through to the
//     default.
//   - OrNull fires on null only; a false passes through as its text.
//   - OrLiteralFalse fires on null and the lowercase `false` bool only
//     (False/FALSE pass through). A port quirk that predates this package;
//     callers keep it so their output stays byte-identical. It differs from
//     Or only for the two capitalised spellings.
//
// Non-scalar nodes (a map where a string belongs) yield the default in all
// three — yq would render the whole subtree as YAML, which no caller
// survives; the sites that print such a rendering (audit) do it themselves.
package yqsem

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Deref unwraps document and alias nodes to the underlying value node. A
// document without content dereferences to nil.
func Deref(n *yaml.Node) *yaml.Node {
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

// MapGet returns the (dereferenced) value for key in a mapping node — first
// match, document order — or nil when the key is absent or n is not a
// mapping. Keys are dereferenced too, so an aliased key matches by its
// anchored text the way yq resolves it.
func MapGet(n *yaml.Node, key string) *yaml.Node {
	n = Deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := Deref(n.Content[i]); k != nil && k.Value == key {
			return Deref(n.Content[i+1])
		}
	}
	return nil
}

// Lookup walks a mapping path from n; nil as soon as one hop is missing or
// not a mapping (yq: `.a.b.c` on a missing key reads null).
func Lookup(n *yaml.Node, path ...string) *yaml.Node {
	n = Deref(n)
	for _, key := range path {
		n = MapGet(n, key)
		if n == nil {
			return nil
		}
	}
	return n
}

// HasKey reports whether the mapping node carries key (yq: `has("k")`).
func HasKey(n *yaml.Node, key string) bool {
	n = Deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := Deref(n.Content[i]); k != nil && k.Value == key {
			return true
		}
	}
	return false
}

// MapKeys returns a mapping's keys in document order (yq `keys` preserves
// it); ok=false when n is not a mapping (yq errors there).
func MapKeys(n *yaml.Node) ([]string, bool) {
	n = Deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, false
	}
	keys := make([]string, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		keys = append(keys, n.Content[i].Value)
	}
	return keys, true
}

// SeqItems returns a sequence's (dereferenced) elements, nil when n is not a
// sequence (yq `.[]` on anything else errors; `| length // 0` reads 0).
func SeqItems(n *yaml.Node) []*yaml.Node {
	n = Deref(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	items := make([]*yaml.Node, 0, len(n.Content))
	for _, c := range n.Content {
		items = append(items, Deref(c))
	}
	return items
}

// IsNull reports a missing node or a YAML null (`~`, `null`, an empty value).
func IsNull(n *yaml.Node) bool {
	n = Deref(n)
	return n == nil || n.Tag == "!!null"
}

// IsFalse reports a boolean false in any of yq's spellings
// (false/False/FALSE). A quoted "false" is a string and is NOT false.
func IsFalse(n *yaml.Node) bool {
	n = Deref(n)
	return n != nil && n.Tag == "!!bool" &&
		(n.Value == "false" || n.Value == "False" || n.Value == "FALSE")
}

// isLiteralFalse is the lowercase-only variant OrLiteralFalse is built on.
func isLiteralFalse(n *yaml.Node) bool {
	return n != nil && n.Tag == "!!bool" && n.Value == "false"
}

// Scalar returns a scalar's source text, "" for a missing or non-scalar
// node. A null scalar keeps its spelling (`~` stays "~").
func Scalar(n *yaml.Node) string {
	n = Deref(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// Raw mirrors `yq -r '<path>'` on a resolved node: the scalar's text, the
// literal word "null" for a missing or null node, "" for a non-scalar (yq
// would render the subtree; the callers route "" to their explicit-empty
// branches).
func Raw(n *yaml.Node) string {
	n = Deref(n)
	if IsNull(n) {
		return "null"
	}
	if n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// Or mirrors `yq -r '<path> // "<def>"'`: def fires on a missing, null, or
// boolean-false node (any spelling) and on a non-scalar; every other scalar
// passes through as its source text (a float `1.30` stays "1.30").
func Or(n *yaml.Node, def string) string {
	n = Deref(n)
	if IsNull(n) || IsFalse(n) || n.Kind != yaml.ScalarNode {
		return def
	}
	return n.Value
}

// OrNull is the null-only alternative: def fires on a missing, null, or
// non-scalar node; a boolean false passes through as "false".
func OrNull(n *yaml.Node, def string) string {
	n = Deref(n)
	if IsNull(n) || n.Kind != yaml.ScalarNode {
		return def
	}
	return n.Value
}

// OrLiteralFalse is Or restricted to the lowercase `false` bool: def fires on
// a missing, null, non-scalar, or `false` node; `False`/`FALSE` pass through.
// Kept for the call sites that shipped with this coercion (see the package
// comment); new code wants Or.
func OrLiteralFalse(n *yaml.Node, def string) string {
	n = Deref(n)
	if IsNull(n) || n.Kind != yaml.ScalarNode || isLiteralFalse(n) {
		return def
	}
	return n.Value
}

// ToString mirrors yq's `| tostring`: "null" for a missing/null node, the
// scalar's text otherwise ("null" again for a non-scalar — the only reliable
// missing-vs-false distinction the env reader relies on).
func ToString(n *yaml.Node) string {
	n = Deref(n)
	if n == nil || n.Tag == "!!null" || n.Kind != yaml.ScalarNode {
		return "null"
	}
	return n.Value
}

// Present reports whether the path resolves to a node that is neither null
// nor false — `yq -e '<path>'` succeeding.
func Present(n *yaml.Node) bool {
	return !IsNull(n) && !IsFalse(n)
}

// LoadNode parses a YAML file into its root node (the document wrapper —
// every reader dereferences). nil when the file is missing or unparsable;
// callers that need the distinction stat the file first, like the bash did.
func LoadNode(path string) *yaml.Node {
	return Load(path).Root
}

// Doc is one loaded YAML document together with its load state. The bash
// `$(yq … file)` collapses to the EMPTY string when yq fails (unreadable or
// unparsable file) — a distinct state from "the path is missing" (which
// reads "null") — and the Doc readers keep that split.
type Doc struct {
	Root *yaml.Node
	Err  error
}

// Load parses a YAML file. A missing or unparsable file loads as a Doc
// whose Err is set and whose readers all yield "".
func Load(path string) Doc {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Doc{Err: err}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return Doc{Err: err}
	}
	return Doc{Root: &root}
}

// OK reports whether the document loaded.
func (d Doc) OK() bool { return d.Err == nil }

// Lookup is Lookup from the document root; nil when the document did not
// load.
func (d Doc) Lookup(path ...string) *yaml.Node {
	if d.Err != nil {
		return nil
	}
	return Lookup(d.Root, path...)
}

// Raw is `yq -r '<path>'` over the document: "" when it did not load.
func (d Doc) Raw(path ...string) string {
	if d.Err != nil {
		return ""
	}
	return Raw(d.Lookup(path...))
}

// Or is `yq -r '<path> // "<def>"'` over the document: "" when it did not
// load (NOT the default — the whole yq call failed).
func (d Doc) Or(def string, path ...string) string {
	if d.Err != nil {
		return ""
	}
	return Or(d.Lookup(path...), def)
}

// OrChain is `yq -r 'A // B // "<def>"'` over scalar paths: the first path
// whose node is truthy wins; "" when the document did not load.
func (d Doc) OrChain(def string, paths ...[]string) string {
	if d.Err != nil {
		return ""
	}
	for _, p := range paths {
		n := d.Lookup(p...)
		if IsNull(n) || IsFalse(n) || n.Kind != yaml.ScalarNode {
			continue
		}
		return n.Value
	}
	return def
}

// Present is `yq -e '<path>'` succeeding over the document.
func (d Doc) Present(path ...string) bool {
	return Present(d.Lookup(path...))
}

package lint

// yq-shaped YAML access helpers. The bash implementation reads every file
// through `yq -r`, and its exact coercions are load-bearing for lint parity
// (`// ""` fires on missing/null but NOT on an explicit empty string; `keys`
// errors on a non-map, which under `mapfile < <(yq … 2>/dev/null)` reads as
// "stop emitting"). These helpers encode those semantics over yaml.v3 nodes.
//
// Single-document assumption: the spec files lint reads (cluster/deploy
// specs, services.yaml, lok8s.yaml, kustomization.yaml) are single-document;
// value reads use the first document like every other Go port does. The ONE
// check where multi-document input is routine — the per-manifest label check
// — iterates documents faithfully (see labelsQuery).

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// fileDocs parses every YAML document in path. nil on read or parse error
// (bash: yq fails, `2>/dev/null` sites read nothing).
func fileDocs(path string) []*yaml.Node {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseDocs(raw)
}

// parseDocs returns every document root (empty non-nil slice for an empty
// stream); nil ONLY on a parse error, so callers can tell "yq failed" from
// "yq emitted nothing".
func parseDocs(raw []byte) []*yaml.Node {
	docs := []*yaml.Node{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			return nil
		}
		docs = append(docs, deref(&doc))
	}
}

// firstDoc parses path and returns the first document's root, nil when the
// file is missing, unparseable, or empty.
func firstDoc(path string) *yaml.Node {
	docs := fileDocs(path)
	if len(docs) == 0 {
		return nil
	}
	return docs[0]
}

// deref unwraps document and alias nodes.
func deref(n *yaml.Node) *yaml.Node {
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

// nodeAt walks a mapping path, nil when any hop is missing or not a mapping.
func nodeAt(n *yaml.Node, path ...string) *yaml.Node {
	n = deref(n)
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
		n = deref(next)
	}
	return n
}

// mapKeys returns a mapping's keys in document order; ok=false when n is not
// a mapping (bash: `keys` errors and the capture stops).
func mapKeys(n *yaml.Node) ([]string, bool) {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, false
	}
	keys := make([]string, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		keys = append(keys, n.Content[i].Value)
	}
	return keys, true
}

func hasKey(n *yaml.Node, key string) bool {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}

// isNull reports a missing or !!null node.
func isNull(n *yaml.Node) bool {
	n = deref(n)
	return n == nil || n.Tag == "!!null"
}

// valueOr renders a scalar the way `yq -r '<expr> // "<def>"'` does: missing
// or null → def; scalar → its value. A non-scalar returns the placeholder —
// callers only test the result for emptiness/equality, never print it (yq
// would render the whole map/seq as YAML there; no lint message does).
func valueOr(n *yaml.Node, def string) string {
	n = deref(n)
	if isNull(n) {
		return def
	}
	if n.Kind != yaml.ScalarNode {
		return "<non-scalar>"
	}
	return n.Value
}

// scalarText renders a scalar the way `yq -r` prints it: the value for a
// string/number/bool, the literal "null" for a null node, "" for a missing or
// non-scalar node (yq would render a map/seq as YAML there; callers skip).
func scalarText(n *yaml.Node) string {
	n = deref(n)
	if n == nil || n.Tag == "!!null" {
		if n != nil {
			return "null"
		}
		return ""
	}
	if n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// normTag returns a node's tag as the yq JSON round-trip would report it
// (bash: `yq -o=json … | yq 'tag'`): the standard tags pass through, anything
// exotic (!!timestamp, !!binary, custom) serializes to a JSON string and
// re-reads as !!str. nil reads as !!null.
func normTag(n *yaml.Node) string {
	n = deref(n)
	if n == nil {
		return "!!null"
	}
	switch n.Tag {
	case "!!map", "!!seq", "!!str", "!!int", "!!float", "!!bool", "!!null":
		return n.Tag
	}
	return "!!str"
}

// seqItems returns a sequence's element nodes (nil when not a sequence).
func seqItems(n *yaml.Node) []*yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	items := make([]*yaml.Node, 0, len(n.Content))
	for _, c := range n.Content {
		items = append(items, deref(c))
	}
	return items
}

// sortedDirNames lists dir's subdirectory names in byte order (bash: a `*/`
// glob under LC_COLLATE=C). Symlinks to directories match, like the glob.
func sortedDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if name[0] == '.' {
			continue // globs skip dotfiles
		}
		info, err := os.Stat(dir + "/" + name)
		if err == nil && info.IsDir() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// sortedFileNames lists dir's entries (files, in byte order) whose names have
// the given suffix, skipping dotfiles like a glob would.
func sortedFileNames(dir, suffix string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if name[0] == '.' {
			continue
		}
		if suffix != "" && !hasSuffix(name, suffix) {
			continue
		}
		info, err := os.Stat(dir + "/" + name)
		if err == nil && info.Mode().IsRegular() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

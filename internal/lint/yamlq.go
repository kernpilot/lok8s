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

	"github.com/kernpilot/lok8s/internal/yqsem"
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
		docs = append(docs, yqsem.Deref(&doc))
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

// valueOr renders a scalar the way `yq -r '<expr> // "<def>"'` does: missing
// or null → def; scalar → its value. A non-scalar returns the placeholder —
// callers only test the result for emptiness/equality, never print it (yq
// would render the whole map/seq as YAML there; no lint message does).
func valueOr(n *yaml.Node, def string) string {
	if n = yqsem.Deref(n); n != nil && n.Kind != yaml.ScalarNode {
		return "<non-scalar>"
	}
	return yqsem.OrNull(n, def) // null only: an explicit false stays "false"
}

// scalarText renders a scalar the way `yq -r` prints it: the value for a
// string/number/bool, the literal "null" for a null node, "" for a missing or
// non-scalar node (yq would render a map/seq as YAML there; callers skip).
func scalarText(n *yaml.Node) string {
	n = yqsem.Deref(n)
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
	n = yqsem.Deref(n)
	if n == nil {
		return "!!null"
	}
	switch n.Tag {
	case "!!map", "!!seq", "!!str", "!!int", "!!float", "!!bool", "!!null":
		return n.Tag
	}
	return "!!str"
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

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

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/yqsem"
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
	return yqsem.Deref(&doc)
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
	defer func() { _ = f.Close() }()
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
		docs = append(docs, yqsem.Deref(&n))
	}
	return docs, nil
}

// lookupFile is yqsem.Lookup over a file's first document.
func lookupFile(path string, keys ...string) *yaml.Node {
	return yqsem.Lookup(firstDocNode(path), keys...)
}

// yqRenderNode mirrors `yq -r` output for a node: an absent node renders as
// "null" (yq's missing-key result); a scalar renders as its SOURCE literal
// (yq preserves scalar style: `~` stays "~", `True` stays "True"); a
// map/sequence renders as YAML with the trailing newline trimmed (bash `$()`
// strips it).
func yqRenderNode(n *yaml.Node) string {
	n = yqsem.Deref(n)
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
	if !yqsem.Present(n) { // yq flavour: null and false/False/FALSE
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
// every file folds left to right into a fresh mapping through
// addons.MergeDocument, the one implementation of the idiom (maps
// deep-merge, everything else replaces, a null document is a no-op, a
// non-map document is yq's "cannot multiply !!map with !!str" error). The
// caller turns that error into an unknown finding, never a fall-through
// pass. extra (may be nil) merges last: the inline override.
func mergeYAMLDocs(files []string, extra *yaml.Node) (*yaml.Node, error) {
	acc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, f := range files {
		docs, err := decodeDocs(f, false)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			if acc, err = addons.MergeDocument(acc, d); err != nil {
				return nil, err
			}
		}
	}
	if extra != nil {
		var err error
		if acc, err = addons.MergeDocument(acc, extra); err != nil {
			return nil, err
		}
	}
	return acc, nil
}

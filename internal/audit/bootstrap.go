package audit

// The spec.bootstrap reader the cilium check needs — the SAME resolution +
// parsing rules as .lok8s/libs/bootstrap (bootstrap::_resolve_entries /
// bootstrap::_parse_entry, shared through internal/bootstrapspec), scoped
// to the fields the audit consumes: the entry's resolved addon DIR and its
// inline helm-values override. Validation failures behave exactly like the
// bash caller (`… 2>/dev/null || continue`): the entry is SKIPPED silently
// (a nil Report) — which means a malformed cilium entry makes the cilium
// check report "not in spec.bootstrap" (pass), a preserved quirk.

import (
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/bootstrapspec"
)

// bootstrapEntry is one resolved spec.bootstrap entry.
type bootstrapEntry struct {
	dir           string     // resolved addon directory
	inline        *yaml.Node // inline helm values (values:/valueFiles:/legacy), nil when none
	inlineInclude bool       // bash: [[ -n inline && inline != "null" ]]
}

// resolveBootstrapEntries is bootstrapspec.Resolve over the spec file's
// first document (an unreadable file resolves like an absent spec).
func resolveBootstrapEntries(specFile, kind string) []bootstrapspec.Item {
	return bootstrapspec.Resolve(firstDocNode(specFile), kind)
}

// parseBootstrapEntry parses one entry for the audit (name/dir/inline).
// ok=false stands in for the bash `return 1` — the caller skips the entry.
// The full validation set runs even for fields the audit ignores
// (env/wait/dependsOn/name), because a violation in ANY of them skips the
// entry in bash too. valueFiles pre-merge (files in list order, inline
// `values:` on top) with the same deep-merge idiom the values stack uses.
func (a *Auditor) parseBootstrapEntry(domainName string, item bootstrapspec.Item) (e bootstrapEntry, ok bool) {
	var merged *yaml.Node
	parser := &bootstrapspec.Parser{
		Paths: a.paths(),
		MergeValueFiles: func(files []string, values *yaml.Node) error {
			var extra *yaml.Node
			if values != nil && inlineIncluded(values) {
				extra = values
			}
			m, err := mergeYAMLDocs(files, extra)
			if err != nil {
				return err
			}
			merged = m
			return nil
		},
	}
	spec, ok := parser.Parse(domainName, item)
	if !ok {
		return e, false
	}
	e.dir = spec.Dir
	switch {
	case spec.Legacy:
		e.inline = spec.Value
	case len(spec.ValueFiles) > 0:
		e.inline = merged
	case spec.Values != nil:
		e.inline = spec.Values
	}
	e.inlineInclude = e.inline != nil && inlineIncluded(e.inline)
	return e, true
}

// inlineIncluded mirrors the bash gate `[[ -n inline && inline != "null" ]]`
// on the RENDERED inline values: a null node renders "null" and an empty
// string renders "" — both excluded; everything else (including "{}" and "~")
// joins the values stack.
func inlineIncluded(n *yaml.Node) bool {
	r := yqRenderNode(n)
	return r != "" && r != "null"
}

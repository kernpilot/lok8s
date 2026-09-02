package audit

// The spec.bootstrap reader the cilium check needs — the SAME resolution +
// parsing rules as .lok8s/libs/bootstrap (bootstrap::_resolve_entries /
// bootstrap::_parse_entry), scoped to the fields the audit consumes: the
// entry's resolved addon DIR and its inline helm-values override. Validation
// failures behave exactly like the bash caller (`… 2>/dev/null || continue`):
// the entry is SKIPPED silently — which means a malformed cilium entry makes
// the cilium check report "not in spec.bootstrap" (pass), a preserved quirk.

import (
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	entryNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	shellVarRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// bootstrapEntry is one resolved spec.bootstrap entry.
type bootstrapEntry struct {
	dir           string     // resolved addon directory
	inline        *yaml.Node // inline helm values (values:/valueFiles:/legacy), nil when none
	inlineInclude bool       // bash: [[ -n inline && inline != "null" ]]
}

// resolveBootstrapEntries mirrors bootstrap::_resolve_entries: an explicit
// non-empty spec.bootstrap yields exactly those entries in order; an explicit
// empty `bootstrap: []` yields nothing (authoritative opt-out); an absent key
// falls back to the per-driver default — only `lo` (kind) ships without a CNI
// and defaults to [cilium]. Entries come back as raw yaml nodes (scalar or
// single-key map), matching the compact-JSON lines the bash emits.
func resolveBootstrapEntries(specFile, kind string) []*yaml.Node {
	doc := firstDocNode(specFile)
	bs := lookupPath(doc, "spec", "bootstrap")
	var entries []*yaml.Node
	if bs != nil {
		switch bs.Kind {
		case yaml.SequenceNode:
			for _, item := range bs.Content {
				entries = append(entries, resolveNode(item))
			}
		case yaml.MappingNode:
			// yq's `.spec.bootstrap[]?` iterates a mapping's VALUES.
			for i := 1; i < len(bs.Content); i += 2 {
				entries = append(entries, resolveNode(bs.Content[i]))
			}
		}
	}
	if len(entries) > 0 {
		return entries
	}
	// Distinguish a *defined* empty list (opt out) from an *absent* key
	// (per-driver default).
	if spec := lookupPath(doc, "spec"); spec != nil && hasKey(spec, "bootstrap") {
		return nil
	}
	if kind == "lo" {
		return []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "cilium"}}
	}
	return nil
}

// parseBootstrapEntry mirrors the bootstrap::_parse_entry subset the audit
// reads (name/dir/inline). ok=false stands in for the bash `return 1` — the
// audit's caller skips the entry. The full validation set runs even for
// fields the audit ignores (env/wait/dependsOn/name), because a violation in
// ANY of them skips the entry in bash too.
func (a *Auditor) parseBootstrapEntry(domainName string, entry *yaml.Node) (e bootstrapEntry, ok bool) {
	entry = resolveNode(entry)
	if entry == nil {
		return e, false
	}

	var raw string
	var val *yaml.Node
	if entry.Kind == yaml.MappingNode {
		// Map entry {"<name-or-path>": <value>} — must have EXACTLY one key.
		if len(entry.Content) != 2 {
			return e, false
		}
		raw = yqRenderNode(entry.Content[0])
		val = resolveNode(entry.Content[1])
		// The map VALUE must itself be a map or null/empty; a scalar or
		// sequence value is rejected up front.
		if val != nil && !isNullNode(val) && val.Kind != yaml.MappingNode {
			return e, false
		}
	} else {
		raw = yqRenderNode(entry)
	}

	// Resolve <name-or-path> → addon dir. Identical rules for a scalar entry
	// and a map key: absolute path (concatenated onto PATH_BASE, exactly like
	// the bash `${PATH_BASE}${_raw}`), ./|../ path relative to the cluster
	// dir, or a bare framework-addon name.
	switch {
	case strings.HasPrefix(raw, "/"):
		e.dir = a.Base + raw
	case strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../"):
		e.dir = a.Clusters + "/" + domainName + "/" + raw
	default:
		e.dir = a.Lok8s + "/addons/" + raw
	}

	// Scalar entry (or a map with an empty/null value): nothing more to parse.
	if val == nil || isNullNode(val) {
		return e, true
	}

	reserved := false
	for _, k := range []string{"values", "valueFiles", "env", "wait", "dependsOn", "name"} {
		if hasKey(val, k) {
			reserved = true
			break
		}
	}
	if !reserved {
		// LEGACY shim: the whole value map is the inline helm values.
		e.inline = val
		e.inlineInclude = inlineIncluded(val)
		return e, true
	}

	// NEW schema — validations in the bash order; any violation skips the
	// entry (errors are suppressed by the audit caller).
	if hasKey(val, "name") {
		n := mapValue(val, "name")
		if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!str" ||
			n.Value == "" || !entryNameRe.MatchString(n.Value) {
			return e, false
		}
	}
	if w := altNode(mapValue(val, "wait"), "false"); w != "true" && w != "false" {
		return e, false
	}
	if hasKey(val, "values") {
		// `values:` is helm-only: flag a non-chart target only when the dir
		// EXISTS but lacks chart.yaml (a missing dir is bootstrap::apply's
		// error to surface).
		if isDir(e.dir) && !isFile(filepath.Join(e.dir, "chart.yaml")) {
			return e, false
		}
		e.inline = mapValue(val, "values")
	}
	if hasKey(val, "valueFiles") {
		if isDir(e.dir) && !isFile(filepath.Join(e.dir, "chart.yaml")) {
			return e, false
		}
		vf := mapValue(val, "valueFiles")
		if vf == nil || vf.Kind != yaml.SequenceNode {
			return e, false
		}
		var files []string
		for _, item := range vf.Content {
			it := resolveNode(item)
			if it == nil || it.Kind != yaml.ScalarNode || it.Tag != "!!str" {
				return e, false
			}
			p := it.Value
			if p == "" {
				return e, false
			}
			if !strings.HasPrefix(p, "/") {
				p = a.Clusters + "/" + domainName + "/" + p
			}
			if !isFile(p) {
				return e, false
			}
			files = append(files, p)
		}
		if len(files) > 0 {
			// Pre-merge (files in list order, inline `values:` on top) with
			// the same deep-merge idiom the values stack uses.
			var extra *yaml.Node
			if e.inline != nil && inlineIncluded(e.inline) {
				extra = e.inline
			}
			merged, err := mergeYAMLDocs(files, extra)
			if err != nil {
				return e, false
			}
			e.inline = merged
		}
	}
	if hasKey(val, "env") {
		env := mapValue(val, "env")
		if env != nil && !isNullNode(env) {
			if env.Kind != yaml.MappingNode {
				return e, false
			}
			for i := 0; i+1 < len(env.Content); i += 2 {
				k := resolveNode(env.Content[i])
				v := resolveNode(env.Content[i+1])
				if v != nil && (v.Kind == yaml.MappingNode || v.Kind == yaml.SequenceNode) {
					return e, false
				}
				if k == nil || !shellVarRe.MatchString(k.Value) {
					return e, false
				}
			}
		}
	}
	if hasKey(val, "dependsOn") {
		deps := mapValue(val, "dependsOn")
		if deps == nil || deps.Kind != yaml.SequenceNode {
			return e, false
		}
		for _, item := range deps.Content {
			it := resolveNode(item)
			if it == nil || isNullNode(it) ||
				it.Kind == yaml.MappingNode || it.Kind == yaml.SequenceNode {
				return e, false
			}
		}
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

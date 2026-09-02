package bootstrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/ui"
	"gopkg.in/yaml.v3"
)

// Entry is one parsed spec.bootstrap entry (bash: the out-params of
// bootstrap::_parse_entry).
type Entry struct {
	// Raw is the compact-JSON entry as resolved (used verbatim in the
	// "addon not found" error, like bash's ${entry}).
	Raw string
	// Name is the entry identity: basename / map-key, or the explicit
	// `name:` override.
	Name string
	// Dir is the resolved addon directory (never changed by `name:`).
	Dir string
	// Inline is the merged inline helm values as YAML ("" when none;
	// "null" for an explicit `values: null`, matching yq -r).
	Inline string
	// EnvLines is the newline-separated KEY=value envsubst overrides
	// ("" when none) — the exact shape bash hands _apply_one.
	EnvLines string
	// Wait marks a global barrier gate (`wait: true`).
	Wait bool
	// Deps are the dependsOn entry names, in order.
	Deps []string
	// Explicit reports whether Name came from an explicit `name:` override
	// (a name collision on it is a hard error, not a tolerated clash).
	Explicit bool
}

var (
	entryNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	shellVarRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// parseError prints the bash error() line and returns it as the error.
func parseError(stderr io.Writer, format string, a ...any) error {
	ui.Errorf(stderr, format, a...)
	return fmt.Errorf(format, a...)
}

func nodeTag(n *yaml.Node) string {
	n = derefNode(n)
	if n == nil {
		return "!!null"
	}
	switch n.Kind {
	case yaml.MappingNode:
		return "!!map"
	case yaml.SequenceNode:
		return "!!seq"
	}
	if n.Tag == "" {
		return "!!str"
	}
	return n.Tag
}

// tagWord strips the "!!" prefix (bash: ${tag#!!}) for error messages.
func tagWord(n *yaml.Node) string { return strings.TrimPrefix(nodeTag(n), "!!") }

// scalarString is yq's `-r` print of a scalar node ("null" for null; the
// raw value otherwise). Non-scalars fall back to their YAML rendering
// (trimmed) — only reachable in error-message paths.
func scalarString(n *yaml.Node) string {
	n = derefNode(n)
	if n == nil || nodeTag(n) == "!!null" {
		return "null"
	}
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// yamlString renders a node as YAML (yq -r of a map — the inline-values
// channel), trimmed of the trailing newline. The entry arrived as compact
// JSON, whose flow/quoting styles would otherwise stick to the nodes —
// clear them so the output is the block YAML yq emitted.
func yamlString(n *yaml.Node) string {
	n = derefNode(n)
	if n == nil || nodeTag(n) == "!!null" {
		return "null"
	}
	clearStyles(n)
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return ""
	}
	enc.Close()
	return strings.TrimRight(buf.String(), "\n")
}

func clearStyles(n *yaml.Node) {
	if n == nil {
		return
	}
	n.Style = 0
	for _, c := range n.Content {
		clearStyles(c)
	}
}

// tostring is yq's `tostring` on a scalar element (bools/ints keep their
// literal text; null becomes "null").
func tostring(n *yaml.Node) string { return scalarString(n) }

// ParseEntry parses ONE spec.bootstrap entry — the compact JSON from
// ResolveEntries — into the fields the apply path needs (bash:
// bootstrap::_parse_entry; the full schema doc lives there). Returns an
// error (after printing it) on a malformed entry or on `values:` set
// against a non-chart target. Pure apart from that one filesystem touch
// (the chart.yaml check) and the valueFiles reads.
//
// Entry forms:
//
//	scalar  "cilium" | "./targets/x" | "/abs/x"   → name/dir only
//	map     {"<name-or-path>": <value>}
//
// The map VALUE is read in one of two schemas: NEW (carries a reserved key
// values | valueFiles | env | wait | dependsOn | name) or LEGACY (no
// reserved key → the WHOLE value map IS the inline helm values).
func ParseEntry(p *config.Paths, stderr io.Writer, domain, entry string) (*Entry, error) {
	e := &Entry{Raw: entry}

	var parsed yaml.Node
	var raw string
	var val *yaml.Node
	if strings.HasPrefix(entry, "{") {
		// Map entry {"<name-or-path>": <value>} — must have EXACTLY one
		// key. A multi-key map is a config mistake (and would silently mix
		// values per key).
		if err := yaml.Unmarshal([]byte(entry), &parsed); err != nil {
			return nil, parseError(stderr, "bootstrap: failed to parse addon name from %s", entry)
		}
		m := derefNode(&parsed)
		if m == nil || m.Kind != yaml.MappingNode {
			return nil, parseError(stderr, "bootstrap: failed to parse addon name from %s", entry)
		}
		nkeys := len(m.Content) / 2
		if nkeys != 1 {
			return nil, parseError(stderr, "bootstrap: entry must be a single-key map, got %d keys: %s", nkeys, entry)
		}
		raw = m.Content[0].Value
		val = derefNode(m.Content[1])
		// The map VALUE must itself be a map (the {values,env,wait} schema,
		// or the legacy whole-map-is-helm-values form) or null/empty. A
		// scalar or sequence value would let the reserved-key / legacy
		// logic below misbehave — reject it up front with a clear type.
		if t := nodeTag(val); t != "!!map" && t != "!!null" {
			return nil, parseError(stderr, "bootstrap: '%s' entry value must be a map of {values,env,wait} or chart values, got %s", raw, strings.TrimPrefix(t, "!!"))
		}
	} else {
		// Scalar entry — decode the JSON string to its raw value.
		if err := yaml.Unmarshal([]byte(entry), &parsed); err != nil {
			return nil, parseError(stderr, "bootstrap: failed to parse entry %s", entry)
		}
		raw = scalarString(derefNode(&parsed))
	}

	// Resolve <name-or-path> → addon name + dir. Identical rules for a
	// scalar entry and a map key: absolute path, ./|../ path relative to
	// the cluster dir, or a bare framework-addon name. (Plain string
	// concatenation, like bash — the un-normalized "/./" is part of the
	// observable contract.)
	switch {
	case strings.HasPrefix(raw, "/"):
		e.Dir = p.Base + raw
		e.Name = filepath.Base(raw)
	case strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../"):
		e.Dir = p.Clusters + "/" + domain + "/" + raw
		e.Name = filepath.Base(raw)
	default:
		e.Name = raw
		e.Dir = p.Lok8s + "/addons/" + raw
	}

	// Scalar entry (or a map with an empty/null value): nothing more to parse.
	if val == nil || nodeTag(val) == "!!null" {
		return e, nil
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
		e.Inline = yamlString(val)
		return e, nil
	}

	// NEW schema.
	if hasKey(val, "name") {
		nameNode := lookupMap(val, "name")
		// name: must be a STRING scalar — reject any other tag (!!bool,
		// !!int, !!float, !!null, !!map, !!seq). An unquoted YAML bool/int
		// coerces to "true"/"123" and would slip past the charset check —
		// almost certainly a mistake, so require quoting. A QUOTED
		// "true"/"123" is !!str and still passes.
		if nodeTag(nameNode) != "!!str" {
			return nil, parseError(stderr, "bootstrap: '%s' name: must be a non-empty scalar entry name (got %s)", raw, tagWord(nameNode))
		}
		oname := nameNode.Value
		if oname == "" {
			return nil, parseError(stderr, "bootstrap: '%s' name: must be a non-empty string", raw)
		}
		if !entryNameRe.MatchString(oname) {
			return nil, parseError(stderr, "bootstrap: '%s' name: '%s' is not a valid entry name (must match [A-Za-z0-9._-]+)", raw, oname)
		}
		e.Name = oname
		e.Explicit = true
	}

	// `wait:` must be a real boolean. A non-boolean scalar (yes/on/1, …)
	// would silently parse as a non-barrier — reject it.
	waitStr := "false"
	if wn := lookupMap(val, "wait"); wn != nil && nodeTag(wn) != "!!null" {
		waitStr = scalarString(wn)
	}
	switch waitStr {
	case "true":
		e.Wait = true
	case "false":
		e.Wait = false
	default:
		return nil, parseError(stderr, "bootstrap: '%s' has a non-boolean wait: '%s' (use true or false)", raw, waitStr)
	}

	if hasKey(val, "values") {
		// Only flag a non-chart target when the dir EXISTS but lacks
		// chart.yaml. If the dir is missing entirely, stay silent here and
		// let Engine.Apply surface the authoritative "addon not found".
		if dirExists(e.Dir) && !fileExists(filepath.Join(e.Dir, "chart.yaml")) {
			return nil, parseError(stderr, "bootstrap: '%s' sets 'values:' but is not a chart addon (no chart.yaml under %s); 'values:' is helm-only", raw, e.Dir)
		}
		e.Inline = yamlString(lookupMap(val, "values"))
	}

	if hasKey(val, "valueFiles") {
		// valueFiles: same helm-only rule as `values:` (a kustomize target
		// has no chart to feed); same missing-dir leniency.
		if dirExists(e.Dir) && !fileExists(filepath.Join(e.Dir, "chart.yaml")) {
			return nil, parseError(stderr, "bootstrap: '%s' sets 'valueFiles:' but is not a chart addon (no chart.yaml under %s); 'valueFiles:' is helm-only", raw, e.Dir)
		}
		vf := lookupMap(val, "valueFiles")
		// The container must be a SEQUENCE of file paths. A scalar or a map
		// is a config mistake — reject it up front.
		if nodeTag(vf) != "!!seq" {
			return nil, parseError(stderr, "bootstrap: '%s' valueFiles: must be a list of file paths (got %s)", raw, tagWord(vf))
		}
		// Every element must be a STRING scalar — an unquoted bool/int, a
		// null element (`[~]` / a bare `-`), or a nested map/list is not a
		// path (same strictness as the name: tag check).
		for _, el := range vf.Content {
			if t := nodeTag(el); t != "!!str" {
				return nil, parseError(stderr, "bootstrap: '%s' valueFiles: each element must be a file path string (got %s)", raw, strings.TrimPrefix(t, "!!"))
			}
		}
		// Resolve each path against the CLUSTER DIR (the directory holding
		// the cluster's lok8s yaml — the same base ./targets/... entries
		// resolve from); an absolute path passes through. A missing file is
		// a hard error: silently skipping it would render the addon with
		// half its values.
		var files []string
		for _, el := range vf.Content {
			v := derefNode(el).Value
			// An empty-string element ("") is !!str, so it survives the tag
			// check above — but it is not a path. Hard error (NOT a silent
			// skip): the fail-fast contract is "never render with half the
			// values".
			if v == "" {
				return nil, parseError(stderr, "bootstrap: '%s' valueFiles: empty element — each element must be a file path", raw)
			}
			if !strings.HasPrefix(v, "/") {
				v = p.Clusters + "/" + domain + "/" + v
			}
			if !fileExists(v) {
				return nil, parseError(stderr, "bootstrap: '%s' valueFiles: file not found: %s", raw, v)
			}
			files = append(files, v)
		}
		if len(files) > 0 {
			// Pre-merge (files in list order, inline `values:` on top) with
			// the SAME deep-merge idiom addons.Render stacks values with
			// (maps deep-merge, lists REPLACE); the result rides render's
			// existing inline-values arg.
			docs := make([]string, 0, len(files)+1)
			for _, f := range files {
				raw2, err := os.ReadFile(f)
				if err != nil {
					return nil, parseError(stderr, "bootstrap: '%s' valueFiles: failed to merge (%s)", raw, strings.Join(files, " "))
				}
				docs = append(docs, string(raw2))
			}
			if e.Inline != "" && e.Inline != "null" {
				docs = append(docs, e.Inline)
			}
			merged, err := addons.MergeYAML(docs...)
			if err != nil {
				return nil, parseError(stderr, "bootstrap: '%s' valueFiles: failed to merge (%s)", raw, strings.Join(files, " "))
			}
			e.Inline = strings.TrimRight(string(merged), "\n")
		}
	}

	if hasKey(val, "env") {
		envNode := lookupMap(val, "env")
		// The env CONTAINER must itself be a map of KEY: scalar. (A
		// null/empty `env:` is a harmless no-op.)
		if t := nodeTag(envNode); t != "!!map" && t != "!!null" {
			return nil, parseError(stderr, "bootstrap: '%s' env: must be a map of KEY: scalar (got %s)", raw, strings.TrimPrefix(t, "!!"))
		}
		if nodeTag(envNode) == "!!map" {
			// env: takes KEY: scalar only. A map/array value would
			// tostring-flatten to a bogus string (e.g. the ccm chart-value
			// `env:` map mistakenly placed at the reserved-key level
			// instead of under values:) — reject it so the mistake is
			// loud, not silent.
			var badKeys []string
			for i := 0; i+1 < len(envNode.Content); i += 2 {
				if t := nodeTag(envNode.Content[i+1]); t == "!!map" || t == "!!seq" {
					badKeys = append(badKeys, envNode.Content[i].Value)
				}
			}
			if len(badKeys) > 0 {
				return nil, parseError(stderr, "bootstrap: '%s' env: values must be scalars; non-scalar value for: %s (did you mean values:?)", raw, strings.Join(badKeys, ", "))
			}
			// Each env: key is exported VERBATIM around the render, so it
			// must be a valid POSIX shell variable name. Validate here
			// (fail fast at parse time, before any apply).
			var lines []string
			for i := 0; i+1 < len(envNode.Content); i += 2 {
				k := envNode.Content[i].Value
				if k == "" {
					continue
				}
				if !shellVarRe.MatchString(k) {
					return nil, parseError(stderr, "bootstrap: '%s' env: key '%s' is not a valid shell variable name (must match [A-Za-z_][A-Za-z0-9_]*)", raw, k)
				}
				lines = append(lines, k+"="+tostring(derefNode(envNode.Content[i+1])))
			}
			e.EnvLines = strings.Join(lines, "\n")
		}
	}

	if hasKey(val, "dependsOn") {
		// dependsOn: resolved to indices, edge-built, and cycle-checked in
		// Engine.Apply (which alone knows every entry's name). Validate
		// only the SHAPE here, fail-fast at parse time.
		depNode := lookupMap(val, "dependsOn")
		if nodeTag(depNode) != "!!seq" {
			return nil, parseError(stderr, "bootstrap: '%s' dependsOn: must be a list of entry names (got %s)", raw, tagWord(depNode))
		}
		// A null element (`dependsOn: [~]` / a bare `-` list item) is NOT a
		// name: it would coerce to the literal string "null" and then fail
		// downstream as a confusing "unknown entry 'null'". Reject it right
		// here with a clear message.
		for _, el := range depNode.Content {
			if nodeTag(el) == "!!null" {
				return nil, parseError(stderr, "bootstrap: '%s' dependsOn: null element — must be a list of entry names", raw)
			}
		}
		for _, el := range depNode.Content {
			if t := nodeTag(el); t == "!!map" || t == "!!seq" {
				return nil, parseError(stderr, "bootstrap: '%s' dependsOn: each element must be a scalar entry name (got a map/list element)", raw)
			}
		}
		for _, el := range depNode.Content {
			e.Deps = append(e.Deps, tostring(derefNode(el)))
		}
	}

	return e, nil
}

// InlineValues returns the cluster spec's merged inline values for ONE
// addon, by the SAME semantics the bootstrap applies (bash:
// bootstrap::inline_values — both entry shapes, `values:` + `valueFiles:`
// pre-merge, cluster-dir resolution). Returns "" when the spec has no entry
// for the addon or the entry carries no values. The kubeone driver's
// render_addons reads this so an addon it renders for `kubeone apply`
// carries the SAME values the bootstrap would overlay (issue #157: an
// inline-only value silently reverted at upgrade time). A parse error is a
// hard error, never a silent empty.
func InlineValues(p *config.Paths, stderr io.Writer, domain, clusterYAML, addon string) (string, error) {
	if !fileExists(clusterYAML) {
		return "", parseError(stderr, "inline_values: cluster yaml not found: %s", clusterYAML)
	}
	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		return "", parseError(stderr, "inline_values: cluster yaml not found: %s", clusterYAML)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return "", err
	}
	bootstrapNode := lookupMap(lookupMap(derefNode(&root), "spec"), "bootstrap")
	if bootstrapNode == nil || bootstrapNode.Kind != yaml.SequenceNode {
		return "", nil
	}
	for _, el := range bootstrapNode.Content {
		e, err := ParseEntry(p, stderr, domain, compactJSON(derefNode(el)))
		if err != nil {
			return "", err
		}
		// Match name AND the FRAMEWORK addon dir: a cluster-local target
		// that happens to share the basename (`./targets/cilium`) must not
		// shadow the framework entry's values — keep scanning past a
		// same-name target.
		if e.Name == addon && e.Dir == p.Lok8s+"/addons/"+addon {
			if e.Inline == "" || e.Inline == "null" {
				return "", nil
			}
			return e.Inline, nil
		}
	}
	return "", nil
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

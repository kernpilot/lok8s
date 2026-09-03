package lint

// Bootstrap validation (bash: lint::bootstrap + the shared
// bootstrap::_resolve_entries / bootstrap::_parse_entry from
// .lok8s/libs/bootstrap). Entry resolution + parsing is DELIBERATELY the same
// logic the apply path uses so `lo lint` and `lo bootstrap` never disagree on
// what an entry means. Re-implementing the parse ad hoc is what used to break
// the map form: a plain `yq '.spec.bootstrap[]?'` shatters
// `- ccm: {wait: true, dependsOn: [...]}` into separate YAML lines, each then
// mis-read as a bogus addon name. Entry forms handled by the parser:
//
//	"cilium"                        → .lok8s/addons/cilium/
//	"./targets/foo"                 → clusters/<domain>/targets/foo/
//	"/abs/path"                     → <repo-root>/abs/path/
//	{ccm: {values,env,wait,dependsOn,name}}  → map form (ccm addon)
//
// A malformed entry (multi-key map, non-map value, bad name:) is reported by
// parseEntry itself and counted here.
//
// Each entry is carried alongside its compact-JSON rendering (bash: `yq
// -o=json -I=0`) because the JSON string IS part of the error messages.

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

type bootstrapEntry struct {
	json string
	node *yaml.Node
}

// bootstrap validates each spec.bootstrap entry resolves to an existing addon
// directory (bash: lint::bootstrap). Returns the number of errors found.
func (l *Linter) bootstrap(domainDir, specFile, domainName string) int {
	// Only cluster specs carry spec.bootstrap
	if !isFile(domainDir + "/cluster.lok8s.yaml") {
		return 0
	}
	kind, err := domain.SpecDriver(specFile, "")
	if err != nil {
		return 0
	}

	errs := 0
	for _, entry := range l.resolveEntries(specFile, kind) {
		// parseEntry decodes scalar vs map form and validates the entry
		// shape, handing back the resolved addon name + dir.
		_, dir, ok := l.parseEntry(domainName, entry)
		if !ok {
			// parseEntry already emitted a specific "bootstrap: ..." error.
			errs++
			continue
		}
		if !isDir(dir) {
			// Report the ORIGINAL entry (matches the apply path's
			// addon-not-found error) — the parsed name alone hides which YAML
			// entry failed for the path/name: forms.
			ui.Errorf(l.ErrOut, "  spec.bootstrap entry not found: %s (resolved to %s)", entry.json, dir)
			errs++
		}
	}
	return errs
}

// resolveEntries resolves which bootstrap addon entries to apply (bash:
// bootstrap::_resolve_entries). Three cases:
//
//   - explicit non-empty spec.bootstrap → exactly those entries, in order
//   - explicit empty `bootstrap: []`    → nothing (authoritative opt-out)
//   - absent spec.bootstrap             → per-driver default
//
// The default is per-driver, NOT one-size-fits-all: only `lo` (kind) ships
// without a CNI and must have one bootstrapped. KubeOne deploys its own
// cilium during `kubeone apply`; Capi/Kkp clusters bring their CNI from the
// management cluster / addon set. Defaulting those to [cilium] caused a stray
// cilium apply on managed clusters.
func (l *Linter) resolveEntries(specFile, kind string) []bootstrapEntry {
	root := firstDoc(specFile)
	items := seqItems(nodeAt(root, "spec", "bootstrap"))
	if len(items) > 0 {
		entries := make([]bootstrapEntry, 0, len(items))
		for _, item := range items {
			entries = append(entries, bootstrapEntry{json: compactJSON(item), node: item})
		}
		return entries
	}
	// Empty result — distinguish a *defined* empty list (opt out) from an
	// *absent* key (fall back to the per-driver default).
	if hasKey(nodeAt(root, "spec"), "bootstrap") {
		return nil
	}
	if kind == "lo" {
		// The default is emitted as the BARE string "cilium" (bash: echo),
		// not a JSON-quoted one — the not-found message shows it unquoted.
		return []bootstrapEntry{{json: "cilium", node: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "cilium"}}}
	}
	return nil
}

var (
	entryNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// parseEntry parses ONE spec.bootstrap entry into the addon name + dir the
// lint check needs (bash: bootstrap::_parse_entry, minus the inline-values /
// env / dependsOn flattening the apply path consumes — every VALIDATION and
// its message is preserved). ok=false after printing an error on a malformed
// entry or on `values:` set against a non-chart target.
//
// The map VALUE is read in one of two schemas:
//
//	NEW    — it carries a reserved key (values | valueFiles | env | wait |
//	         dependsOn | name); each is validated exactly like the apply path.
//	LEGACY — no reserved key → the WHOLE value map IS the inline helm values
//	         (nothing to validate here).
func (l *Linter) parseEntry(domainName string, entry bootstrapEntry) (name, dir string, ok bool) {
	var raw string
	var val *yaml.Node
	if strings.HasPrefix(entry.json, "{") {
		// Map entry {"<name-or-path>": <value>} — must have EXACTLY one key.
		// A multi-key map is a config mistake (and iterating values would
		// silently mix them).
		n := deref(entry.node)
		nkeys := 0
		if n != nil && n.Kind == yaml.MappingNode {
			nkeys = len(n.Content) / 2
		}
		if nkeys != 1 {
			ui.Errorf(l.ErrOut, "bootstrap: entry must be a single-key map, got %d keys: %s", nkeys, entry.json)
			return "", "", false
		}
		raw = n.Content[0].Value
		val = deref(n.Content[1])
		// The map VALUE must itself be a map (the {values,env,wait} schema,
		// or the legacy whole-map-is-helm-values form) or null/empty. A
		// scalar or sequence value — e.g. `- addon: true` → {"addon":true},
		// or `- addon: []` → {"addon":[]} — would let the reserved-key /
		// legacy-values logic below misbehave. Reject it up front with a
		// clear type.
		valTag := normTag(val)
		if valTag != "!!map" && valTag != "!!null" {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' entry value must be a map of {values,env,wait} or chart values, got %s", raw, strings.TrimPrefix(valTag, "!!"))
			return "", "", false
		}
	} else {
		// Scalar entry — the raw value.
		raw = scalarText(entry.node)
	}

	// Resolve <name-or-path> → addon name + dir. Identical rules for a scalar
	// entry and a map key: absolute path, ./|../ path relative to the cluster
	// dir, or a bare framework-addon name. Paths are concatenated verbatim
	// (no cleaning) — the resolved dir appears in error messages byte-for-byte.
	switch {
	case strings.HasPrefix(raw, "/"):
		dir = l.Paths.Base + raw
		name = baseName(raw)
	case strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../"):
		dir = l.Paths.Clusters + "/" + domainName + "/" + raw
		name = baseName(raw)
	default:
		name = raw
		// Read-only: the project's copy when present, else the embedded one
		// from a temp dir — lint never ejects.
		dir, _, _ = assets.Peek(l.Paths, "addons/"+raw)
	}

	// Scalar entry (or a map with an empty/null value): nothing more to parse.
	if isNull(val) {
		return name, dir, true
	}

	hasReserved := hasKey(val, "values") || hasKey(val, "valueFiles") || hasKey(val, "env") ||
		hasKey(val, "wait") || hasKey(val, "dependsOn") || hasKey(val, "name")
	if !hasReserved {
		// LEGACY shim: the whole value map is the inline helm values.
		return name, dir, true
	}

	// name: OVERRIDE this entry's identity. Must be a STRING scalar — an
	// unquoted YAML bool/int (name: true, name: 123) coerces to "true"/"123"
	// and would slip past the charset check below — almost certainly a
	// mistake, so require quoting. A QUOTED "true"/"123" is !!str and passes.
	if hasKey(val, "name") {
		nameNode := nodeAt(val, "name")
		if normTag(nameNode) != "!!str" {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' name: must be a non-empty scalar entry name (got %s)", raw, strings.TrimPrefix(normTag(nameNode), "!!"))
			return "", "", false
		}
		oname := nameNode.Value
		if oname == "" {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' name: must be a non-empty string", raw)
			return "", "", false
		}
		if !entryNameRe.MatchString(oname) {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' name: '%s' is not a valid entry name (must match [A-Za-z0-9._-]+)", raw, oname)
			return "", "", false
		}
		name = oname
	}

	// `wait:` must be a real boolean. A non-boolean scalar (yes/on/1, …)
	// would silently parse as a non-barrier — reject it. Only "true"/"false"
	// (a real YAML/JSON bool) is accepted.
	wait := "false"
	if w := nodeAt(val, "wait"); !isNull(w) {
		wait = scalarText(w)
		if wait == "" {
			wait = compactJSON(w) // non-scalar wait: render like yq would try
		}
	}
	if wait != "true" && wait != "false" {
		ui.Errorf(l.ErrOut, "bootstrap: '%s' has a non-boolean wait: '%s' (use true or false)", raw, wait)
		return "", "", false
	}

	if hasKey(val, "values") {
		// Only flag a non-chart target when the dir EXISTS but lacks
		// chart.yaml. If the dir is missing entirely, stay silent here and
		// let the apply path surface the authoritative "addon not found" —
		// otherwise a typo'd addon name with `values:` misleads with "not a
		// chart addon" instead.
		if isDir(dir) && !isFile(dir+"/chart.yaml") {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' sets 'values:' but is not a chart addon (no chart.yaml under %s); 'values:' is helm-only", raw, dir)
			return "", "", false
		}
	}

	if hasKey(val, "valueFiles") {
		// valueFiles: same helm-only rule as `values:` (a kustomize target
		// has no chart to feed); same missing-dir leniency.
		if isDir(dir) && !isFile(dir+"/chart.yaml") {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' sets 'valueFiles:' but is not a chart addon (no chart.yaml under %s); 'valueFiles:' is helm-only", raw, dir)
			return "", "", false
		}
		vfNode := nodeAt(val, "valueFiles")
		// The container must be a SEQUENCE of file paths. A scalar
		// (`valueFiles: ./x.yaml`) or a map is a config mistake — reject it
		// up front (same fail-fast spirit as the env/dependsOn container
		// checks).
		if normTag(vfNode) != "!!seq" {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' valueFiles: must be a list of file paths (got %s)", raw, strings.TrimPrefix(normTag(vfNode), "!!"))
			return "", "", false
		}
		// Every element must be a STRING scalar — an unquoted bool/int, a
		// null element (`[~]` / a bare `-`), or a nested map/list is not a
		// path (same strictness as the name: tag check).
		for _, el := range seqItems(vfNode) {
			if tag := normTag(el); tag != "!!str" {
				ui.Errorf(l.ErrOut, "bootstrap: '%s' valueFiles: each element must be a file path string (got %s)", raw, strings.TrimPrefix(tag, "!!"))
				return "", "", false
			}
		}
		// Resolve each path against the CLUSTER DIR (the directory holding
		// the cluster's lok8s yaml — the same base ./targets/... entries
		// resolve from); an absolute path passes through. A missing file is a
		// hard error: silently skipping it would render the addon with half
		// its values.
		var vfFiles []string
		for _, el := range seqItems(vfNode) {
			vf := el.Value
			// An empty-string element ("") is !!str, so it survives the tag
			// check above — but it is not a path. Hard error (NOT a silent
			// skip): the fail-fast contract is "never render with half the
			// values".
			if vf == "" {
				ui.Errorf(l.ErrOut, "bootstrap: '%s' valueFiles: empty element — each element must be a file path", raw)
				return "", "", false
			}
			if !strings.HasPrefix(vf, "/") {
				vf = l.Paths.Clusters + "/" + domainName + "/" + vf
			}
			if !isFile(vf) {
				ui.Errorf(l.ErrOut, "bootstrap: '%s' valueFiles: file not found: %s", raw, vf)
				return "", "", false
			}
			vfFiles = append(vfFiles, vf)
		}
		// The apply path deep-merges the files here; lint only needs the
		// merge's FAILURE mode (an unparseable file), reported with the same
		// message. (yq would additionally leak its own parse error to stderr
		// — an unexercised cosmetic difference.)
		for _, vf := range vfFiles {
			raws, err := os.ReadFile(vf)
			if err != nil || parseDocs(raws) == nil {
				ui.Errorf(l.ErrOut, "bootstrap: '%s' valueFiles: failed to merge (%s)", raw, strings.Join(vfFiles, " "))
				return "", "", false
			}
		}
	}

	if hasKey(val, "env") {
		envNode := nodeAt(val, "env")
		// The env CONTAINER must itself be a map of KEY: scalar. (A
		// null/empty `env:` is a harmless no-op.)
		if tag := normTag(envNode); tag != "!!map" && tag != "!!null" {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' env: must be a map of KEY: scalar (got %s)", raw, strings.TrimPrefix(tag, "!!"))
			return "", "", false
		}
		// env: takes KEY: scalar only. A map/array value would
		// tostring-flatten to a bogus string (e.g. the ccm chart-value `env:`
		// map mistakenly placed at the reserved-key level instead of under
		// values:) — reject it so the mistake is loud, not silent.
		envKeys, _ := mapKeys(envNode)
		var badEnv []string
		for _, k := range envKeys {
			if tag := normTag(nodeAt(envNode, k)); tag == "!!map" || tag == "!!seq" {
				badEnv = append(badEnv, k)
			}
		}
		if len(badEnv) > 0 {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' env: values must be scalars; non-scalar value for: %s (did you mean values:?)", raw, strings.Join(badEnv, ", "))
			return "", "", false
		}
		// Each env: key is exported VERBATIM by the apply path, so it must be
		// a valid POSIX shell variable name. Validate here (fail fast at
		// parse time, before any apply) with a clear message.
		for _, k := range envKeys {
			if k == "" {
				continue
			}
			if !envKeyRe.MatchString(k) {
				ui.Errorf(l.ErrOut, "bootstrap: '%s' env: key '%s' is not a valid shell variable name (must match [A-Za-z_][A-Za-z0-9_]*)", raw, k)
				return "", "", false
			}
		}
	}

	if hasKey(val, "dependsOn") {
		depNode := nodeAt(val, "dependsOn")
		// dependsOn: resolved/edge-built/cycle-checked by the apply path;
		// validate only the SHAPE here: a sequence container, every element a
		// scalar name.
		if tag := normTag(depNode); tag != "!!seq" {
			ui.Errorf(l.ErrOut, "bootstrap: '%s' dependsOn: must be a list of entry names (got %s)", raw, strings.TrimPrefix(tag, "!!"))
			return "", "", false
		}
		// A null element (`dependsOn: [~]` / a bare `-` list item) is NOT a
		// name: it would coerce to the literal string "null" and fail
		// downstream as a confusing "unknown entry 'null'". Reject it right
		// here with a clear message — BEFORE the map/seq element check, like
		// the bash.
		for _, el := range seqItems(depNode) {
			if normTag(el) == "!!null" {
				ui.Errorf(l.ErrOut, "bootstrap: '%s' dependsOn: null element — must be a list of entry names", raw)
				return "", "", false
			}
		}
		for _, el := range seqItems(depNode) {
			if tag := normTag(el); tag == "!!map" || tag == "!!seq" {
				ui.Errorf(l.ErrOut, "bootstrap: '%s' dependsOn: each element must be a scalar entry name (got a map/list element)", raw)
				return "", "", false
			}
		}
	}

	return name, dir, true
}

// baseName mirrors `basename` (trailing slashes stripped, last component).
func baseName(path string) string {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// compactJSON renders a node the way `yq -o=json -I=0` does: strings
// JSON-escaped without HTML escaping, numbers/bools verbatim, objects/arrays
// compact and in document order. The rendering is part of the error-message
// contract (the entry JSON appears in "entry not found" / "single-key map"
// messages).
func compactJSON(n *yaml.Node) string {
	n = deref(n)
	if n == nil {
		return "null"
	}
	switch n.Kind {
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!null":
			return "null"
		case "!!bool", "!!int", "!!float":
			return n.Value
		default:
			return jsonString(n.Value)
		}
	case yaml.SequenceNode:
		var b strings.Builder
		b.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(compactJSON(c))
		}
		b.WriteByte(']')
		return b.String()
	case yaml.MappingNode:
		var b strings.Builder
		b.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(n.Content[i].Value))
			b.WriteByte(':')
			b.WriteString(compactJSON(n.Content[i+1]))
		}
		b.WriteByte('}')
		return b.String()
	}
	return "null"
}

// jsonString escapes a string as JSON WITHOUT HTML escaping (yq keeps <,>,&
// raw; encoding/json would entity-escape them).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

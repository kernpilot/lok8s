// Package bootstrapspec is the ONE reader of spec.bootstrap entries. The
// bootstrap engine, `lo lint` and `lo audit` all resolve and parse entries
// through it, so the three never disagree on what an entry means (bash:
// bootstrap::_resolve_entries and bootstrap::_parse_entry in
// .lok8s/libs/bootstrap, which lint and audit call as well).
//
// Entry forms:
//
//	scalar  "cilium" | "./targets/x" | "/abs/x"   → name/dir only
//	map     {"<name-or-path>": <value>}
//
// The map VALUE is read in one of two schemas: NEW (carries a reserved key
// values | valueFiles | env | wait | dependsOn | name) or LEGACY (no
// reserved key → the WHOLE value map IS the inline helm values).
//
// The package validates and resolves. What differs per caller stays with
// the caller: the error channel (Parser.Report: printed by the engine and
// the linter, silent in the audit) and the valueFiles merge
// (Parser.MergeValueFiles: the engine merges to a YAML string, the audit
// to a node, the linter only checks that each file parses). The
// validation order and every message are the bash ones.
package bootstrapspec

import (
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// Item is one resolved spec.bootstrap entry: the node, and its compact
// JSON rendering (bash: `yq -o=json -I=0`), which IS part of the error
// messages. The per-driver default is the BARE word cilium (bash: echo),
// so its Raw carries no quotes.
type Item struct {
	Raw  string
	Node *yaml.Node
}

// Resolve lists the entries to apply for a spec document (bash:
// bootstrap::_resolve_entries). Three cases:
//
//   - explicit non-empty spec.bootstrap → exactly those entries, in order
//     (`.spec.bootstrap[]?`: the items of a sequence, the VALUES of a map)
//   - explicit empty `bootstrap: []`    → nothing (authoritative opt-out)
//   - absent spec.bootstrap             → per-driver default
//
// The default is per-driver, NOT one-size-fits-all: only `lo` (kind) ships
// without a CNI and must have one bootstrapped. KubeOne deploys its own
// cilium during `kubeone apply`; Capi/Kkp clusters bring their CNI from the
// management cluster / addon set. Defaulting those to [cilium] caused a
// stray cilium apply on managed clusters.
func Resolve(root *yaml.Node, kind string) []Item {
	spec := yqsem.Lookup(root, "spec")
	bs := yqsem.Lookup(spec, "bootstrap")
	var items []Item
	if bs != nil {
		switch bs.Kind {
		case yaml.SequenceNode:
			for _, el := range bs.Content {
				n := yqsem.Deref(el)
				items = append(items, Item{Raw: CompactJSON(n), Node: n})
			}
		case yaml.MappingNode:
			for i := 1; i < len(bs.Content); i += 2 {
				n := yqsem.Deref(bs.Content[i])
				items = append(items, Item{Raw: CompactJSON(n), Node: n})
			}
		}
	}
	if len(items) > 0 {
		return items
	}
	// Distinguish a *defined* empty list (opt out) from an *absent* key
	// (per-driver default).
	if spec != nil && yqsem.HasKey(spec, "bootstrap") {
		return nil
	}
	if kind == "lo" {
		return []Item{{Raw: "cilium", Node: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "cilium"}}}
	}
	return nil
}

// Entry is one parsed entry (bash: the out-params of
// bootstrap::_parse_entry).
type Entry struct {
	// Raw is the compact-JSON entry as resolved (used verbatim in the
	// "addon not found" error, like bash's ${entry}).
	Raw string
	// Key is the <name-or-path> as written (bash: _raw).
	Key string
	// Name is the entry identity: basename / map-key, or the explicit
	// `name:` override.
	Name string
	// Dir is the resolved addon directory (never changed by `name:`).
	Dir string
	// Explicit reports whether Name came from an explicit `name:` override
	// (a name collision on it is a hard error, not a tolerated clash).
	Explicit bool
	// Builtin reports a bare framework-addon entry (Dir resolved through
	// internal/assets: the project's .lok8s/addons/<name> when present, else
	// the copy embedded in the binary) as opposed to a cluster-local target
	// or an absolute path.
	Builtin bool
	// Legacy is the whole-map-is-helm-values shim: Value is the inline
	// helm values.
	Legacy bool
	// Value is the map value (nil for a scalar entry or a null value).
	Value *yaml.Node
	// Values is the `values:` node, nil when the key is absent (a present
	// key with a null value is a null node).
	Values *yaml.Node
	// ValueFiles are the `valueFiles:` paths, resolved against the cluster
	// dir; every one exists.
	ValueFiles []string
	// Env are the `env:` KEY=tostring(value) pairs, in map order (an empty
	// key is dropped).
	Env []EnvVar
	// Wait marks a global barrier gate (`wait: true`).
	Wait bool
	// Deps are the dependsOn entry names, in order.
	Deps []string

	// domain is the cluster domain valueFiles resolve under.
	domain string
}

// EnvVar is one env: override.
type EnvVar struct {
	Key   string
	Value string
}

// Parser parses entries for one caller.
type Parser struct {
	Paths *config.Paths
	// Report receives each validation failure as a printf pair (the bash
	// error() text, without the [error] prefix). nil = silent.
	Report func(format string, a ...any)
	// MergeValueFiles runs at the point bash merged valueFiles (files in
	// list order, the inline `values:` on top). values is the `values:`
	// node, nil when absent. A non-nil error fails the entry with the bash
	// "failed to merge" message. nil = no merge.
	MergeValueFiles func(files []string, values *yaml.Node) error
}

var (
	entryNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	shellVarRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (p *Parser) report(format string, a ...any) {
	if p.Report != nil {
		p.Report(format, a...)
	}
}

// Parse parses ONE entry (bash: bootstrap::_parse_entry). ok=false after
// reporting the failure. Pure apart from the filesystem touches bash made
// (the chart.yaml check, the valueFiles stat, the addon peek).
func (p *Parser) Parse(domain string, item Item) (e *Entry, ok bool) {
	e = &Entry{Raw: item.Raw, domain: domain}
	n := yqsem.Deref(item.Node)
	if n != nil && n.Kind == yaml.MappingNode {
		// Map entry {"<name-or-path>": <value>} — must have EXACTLY one
		// key. A multi-key map is a config mistake (and would silently mix
		// values per key).
		if nkeys := len(n.Content) / 2; nkeys != 1 {
			p.report("bootstrap: entry must be a single-key map, got %d keys: %s", nkeys, item.Raw)
			return nil, false
		}
		e.Key = n.Content[0].Value
		e.Value = yqsem.Deref(n.Content[1])
		// The map VALUE must itself be a map (the {values,env,wait} schema,
		// or the legacy whole-map-is-helm-values form) or null/empty. A
		// scalar or sequence value would let the reserved-key / legacy
		// logic below misbehave — reject it up front with a clear type.
		if t := nodeTag(e.Value); t != "!!map" && t != "!!null" {
			p.report("bootstrap: '%s' entry value must be a map of {values,env,wait} or chart values, got %s", e.Key, strings.TrimPrefix(t, "!!"))
			return nil, false
		}
	} else {
		// Scalar entry: its raw value (bash: yq -r '.').
		e.Key = scalarString(n)
	}
	if !p.resolveDir(domain, e) {
		return nil, false
	}

	// Scalar entry (or a map with an empty/null value): nothing more to parse.
	if e.Value == nil || nodeTag(e.Value) == "!!null" {
		e.Value = nil
		return e, true
	}
	if !hasReservedKey(e.Value) {
		// LEGACY shim: the whole value map is the inline helm values.
		e.Legacy = true
		return e, true
	}

	// NEW schema, in the bash order.
	for _, step := range []func(*Entry) bool{p.parseName, p.parseWait, p.parseValues, p.parseValueFiles, p.parseEnv, p.parseDeps} {
		if !step(e) {
			return nil, false
		}
	}
	return e, true
}

// resolveDir maps <name-or-path> → addon name + dir. Identical rules for a
// scalar entry and a map key: absolute path, ./|../ path relative to the
// cluster dir, or a bare framework-addon name. (Plain string concatenation,
// like bash — the un-normalized "/./" is part of the observable contract.)
func (p *Parser) resolveDir(domain string, e *Entry) bool {
	switch {
	case strings.HasPrefix(e.Key, "/"):
		e.Dir = p.Paths.Base + e.Key
		e.Name = filepath.Base(e.Key)
	case strings.HasPrefix(e.Key, "./") || strings.HasPrefix(e.Key, "../"):
		e.Dir = p.Paths.Clusters + "/" + domain + "/" + e.Key
		e.Name = filepath.Base(e.Key)
	default:
		e.Name = e.Key
		e.Builtin = true
		// Peek never writes into the project: a parse is not a use. The
		// apply path re-resolves builtin entries with assets.Resolve, which
		// ejects on first use. A name the binary does not ship resolves to
		// its would-be local dir, so the "addon not found" report keeps the
		// bash wording and path.
		dir, _, err := assets.Peek(p.Paths, "addons/"+e.Key)
		if err != nil {
			p.report("bootstrap: failed to parse addon name from %s", e.Raw)
			return false
		}
		e.Dir = dir
	}
	return true
}

func hasReservedKey(val *yaml.Node) bool {
	for _, k := range []string{"values", "valueFiles", "env", "wait", "dependsOn", "name"} {
		if yqsem.HasKey(val, k) {
			return true
		}
	}
	return false
}

// parseName: `name:` overrides the entry identity. It must be a STRING
// scalar — reject any other tag (!!bool, !!int, !!float, !!null, !!map,
// !!seq). An unquoted YAML bool/int coerces to "true"/"123" and would slip
// past the charset check; almost certainly a mistake, so require quoting.
// A QUOTED "true"/"123" is !!str and still passes.
func (p *Parser) parseName(e *Entry) bool {
	if !yqsem.HasKey(e.Value, "name") {
		return true
	}
	nameNode := yqsem.MapGet(e.Value, "name")
	if nodeTag(nameNode) != "!!str" {
		p.report("bootstrap: '%s' name: must be a non-empty scalar entry name (got %s)", e.Key, tagWord(nameNode))
		return false
	}
	oname := nameNode.Value
	if oname == "" {
		p.report("bootstrap: '%s' name: must be a non-empty string", e.Key)
		return false
	}
	if !entryNameRe.MatchString(oname) {
		p.report("bootstrap: '%s' name: '%s' is not a valid entry name (must match [A-Za-z0-9._-]+)", e.Key, oname)
		return false
	}
	e.Name = oname
	e.Explicit = true
	return true
}

// parseWait: `wait:` must be a real boolean (bash: `yq -r '.wait //
// false'`, so a null or a false in any spelling reads as false). A
// non-boolean scalar (yes/on/1, …) would silently parse as a non-barrier;
// reject it.
func (p *Parser) parseWait(e *Entry) bool {
	waitStr := "false"
	if wn := yqsem.MapGet(e.Value, "wait"); yqsem.Present(wn) {
		waitStr = scalarString(wn)
	}
	switch waitStr {
	case "true":
		e.Wait = true
	case "false":
		e.Wait = false
	default:
		p.report("bootstrap: '%s' has a non-boolean wait: '%s' (use true or false)", e.Key, waitStr)
		return false
	}
	return true
}

// parseValues: `values:` is helm-only. Only flag a non-chart target when
// the dir EXISTS but lacks chart.yaml. If the dir is missing entirely, stay
// silent here and let the apply path surface the authoritative "addon not
// found".
func (p *Parser) parseValues(e *Entry) bool {
	if !yqsem.HasKey(e.Value, "values") {
		return true
	}
	if fsutil.DirExists(e.Dir) && !fsutil.FileExists(filepath.Join(e.Dir, "chart.yaml")) {
		p.report("bootstrap: '%s' sets 'values:' but is not a chart addon (no chart.yaml under %s); 'values:' is helm-only", e.Key, e.Dir)
		return false
	}
	e.Values = yqsem.MapGet(e.Value, "values")
	return true
}

// parseValueFiles: `valueFiles:` has the same helm-only rule as `values:`
// (a kustomize target has no chart to feed) and the same missing-dir
// leniency. The container must be a SEQUENCE of file path strings, every
// path resolved against the CLUSTER DIR (an absolute path passes
// through). A missing or empty element is a hard error: silently skipping
// it would render the addon with half its values. The files then merge
// through the caller's MergeValueFiles.
func (p *Parser) parseValueFiles(e *Entry) bool {
	if !yqsem.HasKey(e.Value, "valueFiles") {
		return true
	}
	if fsutil.DirExists(e.Dir) && !fsutil.FileExists(filepath.Join(e.Dir, "chart.yaml")) {
		p.report("bootstrap: '%s' sets 'valueFiles:' but is not a chart addon (no chart.yaml under %s); 'valueFiles:' is helm-only", e.Key, e.Dir)
		return false
	}
	vf := yqsem.MapGet(e.Value, "valueFiles")
	if nodeTag(vf) != "!!seq" {
		p.report("bootstrap: '%s' valueFiles: must be a list of file paths (got %s)", e.Key, tagWord(vf))
		return false
	}
	for _, el := range vf.Content {
		if t := nodeTag(el); t != "!!str" {
			p.report("bootstrap: '%s' valueFiles: each element must be a file path string (got %s)", e.Key, strings.TrimPrefix(t, "!!"))
			return false
		}
	}
	var files []string
	for _, el := range vf.Content {
		v := yqsem.Deref(el).Value
		if v == "" {
			p.report("bootstrap: '%s' valueFiles: empty element — each element must be a file path", e.Key)
			return false
		}
		if !strings.HasPrefix(v, "/") {
			v = p.Paths.Clusters + "/" + e.domain + "/" + v
		}
		if !fsutil.FileExists(v) {
			p.report("bootstrap: '%s' valueFiles: file not found: %s", e.Key, v)
			return false
		}
		files = append(files, v)
	}
	e.ValueFiles = files
	if len(files) > 0 && p.MergeValueFiles != nil {
		if err := p.MergeValueFiles(files, e.Values); err != nil {
			p.report("bootstrap: '%s' valueFiles: failed to merge (%s)", e.Key, strings.Join(files, " "))
			return false
		}
	}
	return true
}

// parseEnv: `env:` takes KEY: scalar only. The container must be a map (a
// null/empty `env:` is a harmless no-op); a map/array value would
// tostring-flatten to a bogus string (e.g. the ccm chart-value `env:` map
// mistakenly placed at the reserved-key level instead of under values:),
// so reject it loudly. Each key is exported VERBATIM around the render, so
// it must be a valid POSIX shell variable name.
func (p *Parser) parseEnv(e *Entry) bool {
	if !yqsem.HasKey(e.Value, "env") {
		return true
	}
	envNode := yqsem.MapGet(e.Value, "env")
	if t := nodeTag(envNode); t != "!!map" && t != "!!null" {
		p.report("bootstrap: '%s' env: must be a map of KEY: scalar (got %s)", e.Key, strings.TrimPrefix(t, "!!"))
		return false
	}
	if nodeTag(envNode) != "!!map" {
		return true
	}
	var badKeys []string
	for i := 0; i+1 < len(envNode.Content); i += 2 {
		if t := nodeTag(envNode.Content[i+1]); t == "!!map" || t == "!!seq" {
			badKeys = append(badKeys, envNode.Content[i].Value)
		}
	}
	if len(badKeys) > 0 {
		p.report("bootstrap: '%s' env: values must be scalars; non-scalar value for: %s (did you mean values:?)", e.Key, strings.Join(badKeys, ", "))
		return false
	}
	for i := 0; i+1 < len(envNode.Content); i += 2 {
		k := envNode.Content[i].Value
		if k == "" {
			continue
		}
		if !shellVarRe.MatchString(k) {
			p.report("bootstrap: '%s' env: key '%s' is not a valid shell variable name (must match [A-Za-z_][A-Za-z0-9_]*)", e.Key, k)
			return false
		}
		e.Env = append(e.Env, EnvVar{Key: k, Value: scalarString(envNode.Content[i+1])})
	}
	return true
}

// parseDeps: `dependsOn:` is resolved to indices, edge-built and
// cycle-checked by the engine (which alone knows every entry's name).
// Validate only the SHAPE here: a sequence, no null element (it would
// coerce to the literal "null" and fail downstream as a confusing "unknown
// entry 'null'"), every element a scalar.
func (p *Parser) parseDeps(e *Entry) bool {
	if !yqsem.HasKey(e.Value, "dependsOn") {
		return true
	}
	depNode := yqsem.MapGet(e.Value, "dependsOn")
	if nodeTag(depNode) != "!!seq" {
		p.report("bootstrap: '%s' dependsOn: must be a list of entry names (got %s)", e.Key, tagWord(depNode))
		return false
	}
	for _, el := range depNode.Content {
		if nodeTag(el) == "!!null" {
			p.report("bootstrap: '%s' dependsOn: null element — must be a list of entry names", e.Key)
			return false
		}
	}
	for _, el := range depNode.Content {
		if t := nodeTag(el); t == "!!map" || t == "!!seq" {
			p.report("bootstrap: '%s' dependsOn: each element must be a scalar entry name (got a map/list element)", e.Key)
			return false
		}
	}
	for _, el := range depNode.Content {
		e.Deps = append(e.Deps, scalarString(el))
	}
	return true
}

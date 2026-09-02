package kkp

// yamlspec.go — yq-semantics YAML readers over cluster.lok8s.yaml (the
// per-driver copy of the lo/capi helper idiom). Contracts replicated
// exactly:
//
//   - `yq -r '<path>'` prints the literal word "null" for a missing/null
//     path (driver::provision reads spec.kkp.apiUrl BARE, so a spec without
//     it exports KKP_API_URL="null" and the HTTPS validation rejects the
//     literal "null" — the port keeps that, not a cleaned-up empty string);
//   - `yq -r '<path> // "<def>"'` fires the default on null/false too;
//   - `$(yq … unreadable-file)` collapses to "" (the yq call failed).

import (
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
)

type specDoc struct {
	root *yaml.Node
	ok   bool
}

func loadSpec(path string) specDoc {
	raw, err := os.ReadFile(path)
	if err != nil {
		return specDoc{}
	}
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return specDoc{}
	}
	return specDoc{root: &root, ok: true}
}

func yderef(n *yaml.Node) *yaml.Node {
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

func (d specDoc) lookup(path ...string) *yaml.Node {
	if !d.ok {
		return nil
	}
	n := yderef(d.root)
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
		n = yderef(next)
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n == nil || n.Tag == "!!null"
}

func isFalse(n *yaml.Node) bool {
	return n != nil && n.Tag == "!!bool" &&
		(n.Value == "false" || n.Value == "False" || n.Value == "FALSE")
}

// raw mirrors `yq -r '<path>'`: "null" for missing/null, "" on an
// unreadable file or non-scalar.
func (d specDoc) raw(path ...string) string {
	if !d.ok {
		return ""
	}
	n := d.lookup(path...)
	if isNull(n) {
		return "null"
	}
	if n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// or mirrors `yq -r '<path> // "<def>"'` (default on null/false too).
func (d specDoc) or(def string, path ...string) string {
	if !d.ok {
		return ""
	}
	n := d.lookup(path...)
	if isNull(n) || isFalse(n) || n.Kind != yaml.ScalarNode {
		return def
	}
	return n.Value
}

// providerName ports the map-or-scalar read the kkp driver used everywhere:
//
//	(.spec.provider | select(type == "!!map") | .name) //
//	(.spec.provider | select(type == "!!str")) // "<def>"
//
// A map's .name wins when truthy; a bare string spec.provider is taken as
// the name (even an EMPTY string — jq/yq treat "" as truthy, so it stops
// the chain); anything else falls to the default.
func (d specDoc) providerName(def string) string {
	n := d.lookup("spec", "provider")
	if n != nil && n.Kind == yaml.MappingNode {
		name := yderef(func() *yaml.Node {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "name" {
					return n.Content[i+1]
				}
			}
			return nil
		}())
		if !isNull(name) && !isFalse(name) && name.Kind == yaml.ScalarNode {
			return name.Value
		}
		return def
	}
	if n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		return n.Value
	}
	return def
}

// poolNames mirrors spec::pool_names (document order, unvalidated).
func (d specDoc) poolNames() []string {
	n := d.lookup("spec", "workers")
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var names []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		names = append(names, n.Content[i].Value)
	}
	return names
}

// poolCount mirrors spec::pool_count.
func (d specDoc) poolCount() int { return len(d.poolNames()) }

// poolNameRe is the ONE pool-name rule (utils/spec.sh, issue #132).
var poolNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

func validatePoolName(pool string, stderr io.Writer) bool {
	if poolNameRe.MatchString(pool) {
		return true
	}
	ui.Errorf(stderr, "Invalid worker pool name: %s (must be alphanumeric with hyphens)", pool)
	return false
}

// poolField mirrors spec::pool_field: the default is applied HERE, not via
// `//`, so a legitimate `false` is preserved ("false" is non-empty and not
// "null"). Dotted fields (autoscaler.min) walk nested maps.
func (d specDoc) poolField(pool, field, def string) string {
	path := append([]string{"spec", "workers", pool}, strings.Split(field, ".")...)
	n := d.lookup(path...)
	if isNull(n) || n.Kind != yaml.ScalarNode {
		return def
	}
	if n.Value == "" {
		return def
	}
	return n.Value
}

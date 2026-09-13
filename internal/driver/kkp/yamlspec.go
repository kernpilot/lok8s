package kkp

// yamlspec.go — the cluster.lok8s.yaml reader (internal/yqsem's Doc; the
// contracts it keeps: `yq -r '<path>'` prints the literal word "null" for a
// missing path — driver::provision reads spec.kkp.apiUrl BARE, so a spec
// without it exports KKP_API_URL="null" and the HTTPS validation rejects the
// literal "null", not a cleaned-up empty string; `//` fires the default on
// null/false too; an unreadable file reads "" everywhere), plus the kkp
// provider-name coercion and the pool helpers.

import (
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

type specDoc struct{ yqsem.Doc }

func loadSpec(path string) specDoc {
	return specDoc{yqsem.Load(path)}
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
	n := d.Lookup("spec", "provider")
	if n != nil && n.Kind == yaml.MappingNode {
		return yqsem.Or(yqsem.MapGet(n, "name"), def)
	}
	if n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		return n.Value
	}
	return def
}

// poolNames mirrors spec::pool_names (document order, unvalidated).
func (d specDoc) poolNames() []string {
	names, _ := yqsem.MapKeys(d.Lookup("spec", "workers"))
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
	ui.ErrorTo(stderr, "Invalid worker pool name: %s (must be alphanumeric with hyphens)", pool)
	return false
}

// poolField mirrors spec::pool_field: the default is applied HERE, not via
// `//`, so a legitimate `false` is preserved ("false" is non-empty and not
// "null"). Dotted fields (autoscaler.min) walk nested maps.
func (d specDoc) poolField(pool, field, def string) string {
	path := append([]string{"spec", "workers", pool}, strings.Split(field, ".")...)
	if v := yqsem.OrNull(d.Lookup(path...), ""); v != "" {
		return v
	}
	return def
}

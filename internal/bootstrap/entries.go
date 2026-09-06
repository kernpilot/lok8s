// Package bootstrap is the Go port of the framework-level cluster
// infrastructure addon system (.lok8s/libs/bootstrap, all 1413 lines —
// bash wins on any divergence).
//
// It applies spec.bootstrap entries after the cluster is provisioned. Works
// with ALL drivers (Lo, KubeOne, Capi, Kkp) — not driver-specific.
//
// Entries form a DAG and apply CONCURRENTLY (capped) — ordering edges come
// from `dependsOn: [name, …]` and `wait: true`; semantics on Engine.Apply.
//
// Addon resolution:
//
//	"cilium"          → .lok8s/addons/cilium/
//	"./targets/foo"   → clusters/<domain>/targets/foo/
//	"/absolute/path"  → /absolute/path/
//
// Provider-aware values:
//
//	addons/cilium/values.yaml          — base (always loaded)
//	addons/cilium/values.lo.yaml       — driver-specific (if exists)
//	addons/cilium/values.hetzner.yaml  — provider-specific (if exists)
//
// Per-entry overrides (map form): the reserved keys values / valueFiles /
// env / wait / dependsOn / name, plus the legacy whole-map-is-helm-values
// shim — full schema at ParseEntry. Effective helm-values stack:
// base < driver < provider < valueFiles < values:.
package bootstrap

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PlatformOwned lists the addon DIRS that must never bootstrap onto a
// HOSTED cluster (the platform owns them there). Extend here, nowhere else
// (bash: BOOTSTRAP_PLATFORM_OWNED="cilium ccm").
var PlatformOwned = []string{"cilium", "ccm"}

// ResolveEntries resolves which bootstrap addon entries to apply, one
// compact-JSON string per element (bash: bootstrap::_resolve_entries — the
// exact `yq -o=json -I=0 '.spec.bootstrap[]?'` stream shape, so map entries
// stay one element and YAML comments are gone). Pure (no cluster access).
// Three cases:
//   - explicit non-empty spec.bootstrap → exactly those entries, in order
//   - explicit empty `bootstrap: []`    → nothing (authoritative opt-out)
//   - absent spec.bootstrap             → per-driver default
//
// The default is per-driver, NOT one-size-fits-all: only `lo` (kind) ships
// without a CNI and must have one bootstrapped. KubeOne deploys its own
// cilium during `kubeone apply`; Capi/Kkp clusters bring their CNI from the
// management cluster / addon set. Defaulting those to [cilium] caused a
// stray cilium apply on managed clusters. (The lo default is emitted as the
// bare word `cilium`, matching the bash `echo "cilium"`.)
func ResolveEntries(clusterYAML, kind string) ([]string, error) {
	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	spec := lookupMap(derefNode(&root), "spec")
	bootstrapNode := lookupMap(spec, "bootstrap")
	if bootstrapNode != nil && bootstrapNode.Kind == yaml.SequenceNode && len(bootstrapNode.Content) > 0 {
		entries := make([]string, 0, len(bootstrapNode.Content))
		for _, el := range bootstrapNode.Content {
			entries = append(entries, compactJSON(derefNode(el)))
		}
		return entries, nil
	}
	// Empty result — distinguish a *defined* empty list (opt out) from an
	// *absent* key (fall back to the per-driver default).
	if hasKey(spec, "bootstrap") {
		return nil, nil
	}
	if kind == "lo" {
		return []string{"cilium"}, nil
	}
	return nil, nil
}

func hasKey(n *yaml.Node, key string) bool {
	n = derefNode(n)
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

func lookupMap(n *yaml.Node, key string) *yaml.Node {
	n = derefNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return derefNode(n.Content[i+1])
		}
	}
	return nil
}

func derefNode(n *yaml.Node) *yaml.Node {
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

// compactJSON renders a YAML node as compact single-line JSON, preserving
// map key order — the `yq -o=json -I=0` shape the bash entry stream carries.
func compactJSON(n *yaml.Node) string {
	n = derefNode(n)
	if n == nil {
		return "null"
	}
	switch n.Kind {
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
	case yaml.SequenceNode:
		var b strings.Builder
		b.WriteByte('[')
		for i, el := range n.Content {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(compactJSON(el))
		}
		b.WriteByte(']')
		return b.String()
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!null":
			return "null"
		case "!!bool", "!!int", "!!float":
			return n.Value
		default:
			return jsonString(n.Value)
		}
	}
	return "null"
}

func jsonString(s string) string {
	q := strconv.Quote(s)
	return q
}

// nothingToApplyDebug is the shared debug line both empty-entry exits print
// (bash prints it twice, verbatim).
func nothingToApplyDebug(domain, kind string) string {
	return fmt.Sprintf("bootstrap: nothing to apply for %s (kind=%s)", domain, kind)
}

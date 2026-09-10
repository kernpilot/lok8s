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

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/bootstrapspec"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// PlatformOwned lists the addon DIRS that must never bootstrap onto a
// HOSTED cluster (the platform owns them there). Extend here, nowhere else
// (bash: BOOTSTRAP_PLATFORM_OWNED="cilium ccm").
var PlatformOwned = []string{"cilium", "ccm"}

// ResolveEntries resolves which bootstrap addon entries to apply, one
// compact-JSON string per element (bash: bootstrap::_resolve_entries — the
// exact `yq -o=json -I=0 '.spec.bootstrap[]?'` stream shape, so map entries
// stay one element and YAML comments are gone). Pure (no cluster access).
// The cases and the per-driver default are bootstrapspec.Resolve's; the lo
// default is the bare word `cilium`, matching the bash `echo "cilium"`.
func ResolveEntries(clusterYAML, kind string) ([]string, error) {
	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	items := bootstrapspec.Resolve(yqsem.Deref(&root), kind)
	if len(items) == 0 {
		return nil, nil
	}
	entries := make([]string, 0, len(items))
	for _, it := range items {
		entries = append(entries, it.Raw)
	}
	return entries, nil
}

// nothingToApplyDebug is the shared debug line both empty-entry exits print
// (bash prints it twice, verbatim).
func nothingToApplyDebug(domain, kind string) string {
	return fmt.Sprintf("bootstrap: nothing to apply for %s (kind=%s)", domain, kind)
}

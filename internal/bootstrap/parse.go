package bootstrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/bootstrapspec"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
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
	// Builtin reports a bare framework-addon entry (Dir resolved through
	// internal/assets: the project's .lok8s/addons/<name> when present, else
	// the copy embedded in the binary) as opposed to a cluster-local target
	// or an absolute path.
	Builtin bool
}

// parseError prints the bash error() line and returns the same text as an
// error marked handled (ui.Handled).
func parseError(stderr io.Writer, format string, a ...any) error {
	ui.Errorf(stderr, format, a...)
	return ui.Handled(fmt.Errorf(format, a...))
}

// yamlString renders a node as YAML (yq -r of a map — the inline-values
// channel), trimmed of the trailing newline. The entry arrived as compact
// JSON, whose flow/quoting styles would otherwise stick to the nodes —
// clear them so the output is the block YAML yq emitted. A nil or null
// node renders "null", like yq -r.
func yamlString(n *yaml.Node) string {
	n = yqsem.Deref(n)
	if n == nil || n.Tag == "!!null" {
		return "null"
	}
	clearStyles(n)
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return ""
	}
	_ = enc.Close()
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

// ParseEntry parses ONE spec.bootstrap entry — the compact JSON from
// ResolveEntries — into the fields the apply path needs (bash:
// bootstrap::_parse_entry; the shared reader is internal/bootstrapspec,
// the schema doc lives there). Returns an error (after printing it) on a
// malformed entry or on `values:` set against a non-chart target. The
// valueFiles pre-merge (files in list order, inline `values:` on top) uses
// the SAME deep-merge idiom addons.Render stacks values with (maps
// deep-merge, lists REPLACE); the result rides render's existing
// inline-values arg.
func ParseEntry(p *config.Paths, stderr io.Writer, domain, entry string) (*Entry, error) {
	var parsed yaml.Node
	if err := yaml.Unmarshal([]byte(entry), &parsed); err != nil {
		if strings.HasPrefix(entry, "{") {
			return nil, parseError(stderr, "bootstrap: failed to parse addon name from %s", entry)
		}
		return nil, parseError(stderr, "bootstrap: failed to parse entry %s", entry)
	}
	node := yqsem.Deref(&parsed)
	if strings.HasPrefix(entry, "{") && (node == nil || node.Kind != yaml.MappingNode) {
		return nil, parseError(stderr, "bootstrap: failed to parse addon name from %s", entry)
	}

	var reported error
	merged := ""
	parser := &bootstrapspec.Parser{
		Paths:  p,
		Report: func(format string, a ...any) { reported = parseError(stderr, format, a...) },
		MergeValueFiles: func(files []string, values *yaml.Node) error {
			docs := make([]string, 0, len(files)+1)
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				docs = append(docs, string(raw))
			}
			if values != nil {
				if inline := yamlString(values); inline != "" && inline != "null" {
					docs = append(docs, inline)
				}
			}
			out, err := addons.MergeYAML(docs...)
			if err != nil {
				return err
			}
			merged = strings.TrimRight(string(out), "\n")
			return nil
		},
	}
	spec, ok := parser.Parse(domain, bootstrapspec.Item{Raw: entry, Node: node})
	if !ok {
		return nil, reported
	}
	e := &Entry{
		Raw: spec.Raw, Name: spec.Name, Dir: spec.Dir,
		Wait: spec.Wait, Deps: spec.Deps, Explicit: spec.Explicit, Builtin: spec.Builtin,
	}
	switch {
	case spec.Legacy:
		e.Inline = yamlString(spec.Value)
	case len(spec.ValueFiles) > 0:
		e.Inline = merged
	case spec.Values != nil:
		e.Inline = yamlString(spec.Values)
	}
	if len(spec.Env) > 0 {
		lines := make([]string, 0, len(spec.Env))
		for _, kv := range spec.Env {
			lines = append(lines, kv.Key+"="+kv.Value)
		}
		e.EnvLines = strings.Join(lines, "\n")
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
	if !fsutil.FileExists(clusterYAML) {
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
	bootstrapNode := yqsem.MapGet(yqsem.MapGet(yqsem.Deref(&root), "spec"), "bootstrap")
	if bootstrapNode == nil || bootstrapNode.Kind != yaml.SequenceNode {
		return "", nil
	}
	for _, el := range bootstrapNode.Content {
		e, err := ParseEntry(p, stderr, domain, bootstrapspec.CompactJSON(el))
		if err != nil {
			return "", err
		}
		// Match name AND the FRAMEWORK addon dir: a cluster-local target
		// that happens to share the basename (`./targets/cilium`) must not
		// shadow the framework entry's values — keep scanning past a
		// same-name target.
		if e.Name == addon && e.Builtin && filepath.Base(e.Dir) == addon {
			if e.Inline == "" || e.Inline == "null" {
				return "", nil
			}
			return e.Inline, nil
		}
	}
	return "", nil
}

package capi

// pg.go — the placement-group injection (the tail of capi::generate). The
// bash piped the rendered stream through one `yq eval` expression:
//
//   (select(.kind == "HetznerCluster") | .spec.hcloudPlacementGroups) =
//       [{"name": "control-plane", "type": "spread"},
//        {"name": "workers", "type": "spread"}]
//   | (select(.kind == "HCloudMachineTemplate" and
//       (.metadata.name | test("-control-plane$"))) |
//       .spec.template.spec.placementGroupName) = "control-plane"
//   | (select(.kind == "HCloudMachineTemplate" and
//       (.metadata.name | test("-control-plane$") | not)) |
//       .spec.template.spec.placementGroupName) = "workers"
//
// Here the same edit is a yaml.Node surgery per document. Like the kubeone
// yamledit helpers, only the merged CONTENT is the contract: documents that
// match are re-serialized by yaml.v3 (2-space indent, comments preserved),
// documents that don't are passed through byte-identical — yq's exact
// output formatting (4-space sequence indent) is not reproduced.

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// injectPlacementGroups applies the opt-in spread placement groups to the
// rendered multi-document stream (input and output without a trailing
// newline, matching the caller's $()-stripped handling).
func injectPlacementGroups(rendered string) (string, error) {
	docs := strings.Split(rendered, "\n---\n")
	for i, doc := range docs {
		var root yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
			return "", fmt.Errorf("capi: placement-group injection: %w", err)
		}
		m := yderef(&root)
		if m == nil || m.Kind != yaml.MappingNode {
			continue
		}
		kind := mapScalar(m, "kind")
		name := mapScalar(mapValue(m, "metadata"), "name")

		changed := false
		switch kind {
		case "HetznerCluster":
			spec := ensureMapPath(m, "spec")
			setKey(spec, "hcloudPlacementGroups", pgList())
			changed = true
		case "HCloudMachineTemplate":
			group := "workers"
			if strings.HasSuffix(name, "-control-plane") {
				group = "control-plane"
			}
			tmplSpec := ensureMapPath(m, "spec", "template", "spec")
			setKey(tmplSpec, "placementGroupName", strNode(group))
			changed = true
		}
		if !changed {
			continue
		}

		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(m); err != nil {
			return "", fmt.Errorf("capi: placement-group injection: %w", err)
		}
		if err := enc.Close(); err != nil {
			return "", fmt.Errorf("capi: placement-group injection: %w", err)
		}
		docs[i] = strings.TrimRight(buf.String(), "\n")
	}
	return strings.Join(docs, "\n---\n"), nil
}

func pgList() *yaml.Node {
	entry := func(name string) *yaml.Node {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			strNode("name"), strNode(name),
			strNode("type"), strNode("spread"),
		}}
	}
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{
		entry("control-plane"), entry("workers"),
	}}
}

// ── yaml.Node edit helpers (the kubeone yamledit idiom) ────

func mapValue(m *yaml.Node, key string) *yaml.Node {
	m = yderef(m)
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapScalar(m *yaml.Node, key string) string {
	v := yderef(mapValue(m, key))
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

func ensureMapPath(root *yaml.Node, keys ...string) *yaml.Node {
	cur := root
	for _, key := range keys {
		next := yderef(mapValue(cur, key))
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(cur, key, next)
		}
		cur = next
	}
	return cur
}

func setKey(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

func strNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

package cli

import (
	"os"

	"gopkg.in/yaml.v3"
)

// yqScalar reads one scalar out of a YAML file with `yq -r '.a.b // "alt"'`
// semantics: a missing/unreadable file, an absent key, a null OR a boolean
// false all yield alt (jq's `//` treats false like null — a quirk the
// run header inherits: `.spec.registries.tls // true` reads "true" for
// `tls: false`); any other scalar yields its ORIGINAL text (yq prints a
// float `1.30` as written, not re-formatted).
func yqScalar(file, alt string, keys ...string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return alt
	}
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil {
		return alt
	}
	n := yqNode(&root, keys...)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return alt
	}
	if n.Tag == "!!bool" && n.Value == "false" {
		return alt
	}
	return n.Value
}

// yqNode walks a mapping path (documents/aliases dereferenced).
func yqNode(n *yaml.Node, keys ...string) *yaml.Node {
	n = yqDeref(n)
	for _, k := range keys {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
				break
			}
		}
		n = yqDeref(next)
	}
	return n
}

func yqDeref(n *yaml.Node) *yaml.Node {
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

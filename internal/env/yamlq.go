package env

// yamlq.go — ordered yaml.Node access with yq's read semantics, replacing
// the per-field `yq -r '<path> // <default>'` / `| tostring` subprocesses of
// the bash implementation:
//   - `//` is "alternative on null OR false": both fall through to the
//     default (which is why enabled/build go through ToString instead);
//   - `tostring` renders missing keys as "null" and booleans as
//     "true"/"false" — the only reliable missing-vs-false distinction.
// Mapping order is preserved (services iterate in document order, exactly
// like `yq '.services | keys'`).

import (
	"os"

	"gopkg.in/yaml.v3"
)

func deref(n *yaml.Node) *yaml.Node {
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

func mapGet(n *yaml.Node, key string) *yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return deref(n.Content[i+1])
		}
	}
	return nil
}

// Path walks nested mapping keys, nil when absent.
func Path(n *yaml.Node, keys ...string) *yaml.Node {
	cur := deref(n)
	for _, key := range keys {
		cur = mapGet(cur, key)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// scalarOrNode is yq's `<node> // "<def>"` over a resolved node: missing,
// null and false all yield the default. Non-scalar shapes (a map where a
// string belongs) also fall back — degenerate inputs yq would render as a
// multi-line block that no caller survives anyway.
func scalarOrNode(n *yaml.Node, def string) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return def
	}
	if n.Tag == "!!bool" && n.Value == "false" {
		return def
	}
	return n.Value
}

// ScalarOr is scalarOrNode over a path from the document root.
func ScalarOr(root *yaml.Node, def string, keys ...string) string {
	return scalarOrNode(Path(root, keys...), def)
}

// toStringNode is yq's `| tostring`: "null" for a missing/null node, the
// scalar's rendering otherwise.
func toStringNode(n *yaml.Node) string {
	if n == nil || n.Tag == "!!null" {
		return "null"
	}
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return "null"
}

// ToString is toStringNode over a path from the document root.
func ToString(root *yaml.Node, keys ...string) string {
	return toStringNode(Path(root, keys...))
}

// BareEnvsubst is GNU gettext envsubst with no SHELL-FORMAT (the bash
// pipeline's bare `envsubst`): every `${NAME}` and identifier-boundary bare
// `$NAME` reference is replaced with the variable's value — undefined vars
// become the empty string — while anything that is not a plain reference
// (`${X:-y}`, `${arr[0]}`, `$$`) passes through untouched. Single pass;
// substituted values are not rescanned.
func BareEnvsubst(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); {
		ch := data[i]
		if ch != '$' || i+1 >= len(data) {
			out = append(out, ch)
			i++
			continue
		}
		if data[i+1] == '{' {
			j := i + 2
			for j < len(data) && isIdentByte(data[j]) {
				j++
			}
			if j > i+2 && j < len(data) && data[j] == '}' {
				out = append(out, os.Getenv(string(data[i+2:j]))...)
				i = j + 1
				continue
			}
			out = append(out, ch)
			i++
			continue
		}
		if isIdentStartByte(data[i+1]) {
			j := i + 1
			for j < len(data) && isIdentByte(data[j]) {
				j++
			}
			out = append(out, os.Getenv(string(data[i+1:j]))...)
			i = j
			continue
		}
		out = append(out, ch)
		i++
	}
	return out
}

func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool {
	return isIdentStartByte(c) || (c >= '0' && c <= '9')
}

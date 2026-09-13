package bootstrapspec

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// nodeTag is the tag yq reports for a node: !!map / !!seq by kind, !!null
// for an absent node, the resolved tag of a scalar (!!str when yaml.v3
// left it empty).
func nodeTag(n *yaml.Node) string {
	n = yqsem.Deref(n)
	if n == nil {
		return "!!null"
	}
	switch n.Kind {
	case yaml.MappingNode:
		return "!!map"
	case yaml.SequenceNode:
		return "!!seq"
	}
	if n.Tag == "" {
		return "!!str"
	}
	return n.Tag
}

// tagWord strips the "!!" prefix (bash: ${tag#!!}) for error messages.
func tagWord(n *yaml.Node) string { return strings.TrimPrefix(nodeTag(n), "!!") }

// scalarString is yq's `-r` print of a node ("null" for null; the raw
// value of a scalar; yq's `tostring` on a list element). A non-scalar
// renders as YAML, trimmed (bash: `$(yq -r …)` of a map), which only
// error-message paths reach.
func scalarString(n *yaml.Node) string {
	n = yqsem.Deref(n)
	if n == nil || nodeTag(n) == "!!null" {
		return "null"
	}
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// CompactJSON renders a node the way `yq -o=json -I=0` does: strings
// JSON-escaped without HTML escaping, numbers/bools verbatim, objects and
// arrays compact and in document order. The rendering is part of the
// error-message contract (the entry JSON appears in the "entry not found"
// and "single-key map" messages).
func CompactJSON(n *yaml.Node) string {
	n = yqsem.Deref(n)
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
			b.WriteString(CompactJSON(c))
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
			b.WriteString(CompactJSON(n.Content[i+1]))
		}
		b.WriteByte('}')
		return b.String()
	}
	return "null"
}

// jsonString escapes a string as JSON WITHOUT HTML escaping (yq keeps <,>,&
// raw; encoding/json would entity-escape them). Control characters go out
// as \u00xx, valid JSON; strconv.Quote would write \x01, which no JSON
// reader accepts.
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

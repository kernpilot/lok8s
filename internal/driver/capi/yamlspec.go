package capi

// yamlspec.go — yq-semantics YAML readers over cluster.lok8s.yaml, plus the
// whitelist envsubst the generator renders with. The bash read the spec with
// `yq -r '<path> // <default>'` per field; these helpers replicate that
// contract exactly, INCLUDING:
//
//   - yq's `//` alternative firing on null AND false (not just missing);
//   - a bare `yq -r '<path>'` printing the literal word "null" for a
//     missing path;
//   - `$(yq … missing-file)` collapsing to the EMPTY string when the file
//     itself cannot be read (yq fails, the command substitution captures
//     nothing) — driver::provision depends on that: a missing spec makes
//     mgmt_domain "" and routes to the kubehz::read_config guard, never to
//     a "null" management domain.

import (
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
)

// specDoc is one loaded cluster spec. ok=false = the file was missing or
// unparsable (the bash "yq failed" state: every read yields "").
type specDoc struct {
	root *yaml.Node
	ok   bool
}

// loadSpec parses a YAML file; a missing or unparsable file loads as the
// not-ok document.
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

// yderef unwraps document/alias nodes.
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

// lookup walks a mapping path, nil when any hop is missing or not a map.
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

// raw mirrors `yq -r '<path>'`: the scalar's string value, the literal word
// "null" when the path is missing or null, "" on an unreadable file (the
// whole yq call failed) or a non-scalar node.
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

// or mirrors `yq -r '<path> // "<def>"'`: def fires when the path is
// missing, null, or FALSE (yq's alternative-operator falsiness); "" on an
// unreadable file.
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

// present mirrors `yq -e '<path>'` succeeding: the node exists and is
// neither null nor false (yq -e fails on falsy results).
func (d specDoc) present(path ...string) bool {
	n := d.lookup(path...)
	return !isNull(n) && !isFalse(n)
}

// poolNames mirrors spec::pool_names: the keys of spec.workers in DOCUMENT
// ORDER (mikefarah yq preserves map order), unvalidated — validation is the
// caller's next step, per name.
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

// poolNameRe is the ONE pool-name rule (utils/spec.sh, issue #132): the name
// is interpolated into rendered YAML, so it is constrained to what a
// Kubernetes object name can hold anyway.
var poolNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

// validatePoolName mirrors spec::validate_pool_name.
func validatePoolName(pool string, stderr io.Writer) bool {
	if poolNameRe.MatchString(pool) {
		return true
	}
	ui.Errorf(stderr, "Invalid worker pool name: %s (must be alphanumeric with hyphens)", pool)
	return false
}

// poolField mirrors spec::pool_field: read one field of one pool through the
// bracket form, defaulting in the CALLER's layer (not `//`) so a legitimate
// `false` is preserved — `yq -r` prints it as "false", which is non-empty
// and not "null", so the default does NOT fire.
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

// envsubstMap is template::envsubst with the values held in a LOCAL map
// instead of the process environment. The bash confines its exports to a
// subshell so CLUSTER_NAME / K8S_VERSION never leak into the caller
// (POST-REVIEW finding 6 — a leaked value renders the WRONG cluster on the
// kubeone driver's next read); the Go render never touches the process env
// at all, which is the same containment enforced structurally. Semantics
// are the GNU SHELL-FORMAT contract: replace exactly the literal `${NAME}`
// and identifier-boundary bare `$NAME` tokens of the listed vars, pass
// every other byte through untouched (the cloud-init's own $ARCH, $RUNC,
// $KUBERNETES_VERSION … stay literal), single pass.
func envsubstMap(data []byte, vars map[string]string) []byte {
	var out strings.Builder
	out.Grow(len(data))
	for i := 0; i < len(data); {
		c := data[i]
		if c != '$' || i+1 >= len(data) {
			out.WriteByte(c)
			i++
			continue
		}
		if data[i+1] == '{' {
			// ${NAME} — braced form. Only a plain identifier immediately
			// closed by `}` is a candidate; `${NAME:-x}`, `${arr[0]}` and
			// friends pass through verbatim.
			j := i + 2
			for j < len(data) && isIdentByte(data[j]) {
				j++
			}
			if j > i+2 && j < len(data) && data[j] == '}' {
				if v, listed := vars[string(data[i+2:j])]; listed {
					out.WriteString(v)
					i = j + 1
					continue
				}
			}
			out.WriteByte(c)
			i++
			continue
		}
		if isIdentStartByte(data[i+1]) {
			// $NAME — bare form, maximal identifier (identifier boundary:
			// $FOO never fires inside $FOOBAR).
			j := i + 1
			for j < len(data) && isIdentByte(data[j]) {
				j++
			}
			if v, listed := vars[string(data[i+1:j])]; listed {
				out.WriteString(v)
				i = j
				continue
			}
			out.WriteByte(c)
			i++
			continue
		}
		out.WriteByte(c)
		i++
	}
	return []byte(out.String())
}

func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool {
	return isIdentStartByte(c) || (c >= '0' && c <= '9')
}

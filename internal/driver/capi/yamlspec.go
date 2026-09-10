package capi

// yamlspec.go — the cluster.lok8s.yaml reader (internal/yqsem's Doc, which
// keeps yq's `//` firing on null AND false, the literal "null" for a bare
// missing path, and the "" every read yields when the file itself cannot be
// read — driver::provision depends on that: a missing spec makes mgmt_domain
// "" and routes to the kubehz::read_config guard, never to a "null"
// management domain), plus the pool helpers and the whitelist envsubst the
// generator renders with.

import (
	"io"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// specDoc is one loaded cluster spec.
type specDoc struct{ yqsem.Doc }

// loadSpec parses a YAML file; a missing or unparsable file loads as the
// not-ok document (every read yields "").
func loadSpec(path string) specDoc {
	return specDoc{yqsem.Load(path)}
}

// poolNames mirrors spec::pool_names: the keys of spec.workers in DOCUMENT
// ORDER (mikefarah yq preserves map order), unvalidated — validation is the
// caller's next step, per name.
func (d specDoc) poolNames() []string {
	names, _ := yqsem.MapKeys(d.Lookup("spec", "workers"))
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
	if v := yqsem.OrNull(d.Lookup(path...), ""); v != "" {
		return v
	}
	return def
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

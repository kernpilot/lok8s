package build

// The native envsubst stage. Everywhere lok8s renders manifests it restricts
// substitution to an explicit variable list, so arbitrary `$…` in user
// content survives untouched. The semantics are GNU gettext envsubst's
// SHELL-FORMAT contract (the reference the bash template::envsubst shim also
// implements): replace exactly the literal `${NAME}` and identifier-boundary
// bare `$NAME` tokens of the listed vars (listed-but-unset → empty string,
// like GNU) and guarantee every other byte of the stream — including
// `${arr[0]}`/`${x:?}` shell in embedded ConfigMap scripts — passes through
// untouched. Substituted values are not rescanned (single pass, GNU
// behavior).

import (
	"os"
	"strings"
)

// EnvsubstWhitelist returns the names of every env var matching
// ^LOK8S_(SPEC|USER)_ — rebuilt per call so exports made just before the
// render (LOK8S_USER_API_*, lo::export_spec_envs) are automatically picked
// up. Bash: template::envsubst_whitelist.
func EnvsubstWhitelist() []string {
	var names []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name := kv[:eq]
		if strings.HasPrefix(name, "LOK8S_SPEC_") || strings.HasPrefix(name, "LOK8S_USER_") {
			names = append(names, name)
		}
	}
	return names
}

// Envsubst substitutes the listed vars in data (values from the process
// env). An empty list replaces NOTHING (GNU semantics). Bash:
// template::envsubst.
func Envsubst(data []byte, names []string) []byte {
	lookup := os.Getenv
	return envsubst(data, names, lookup)
}

// EnvsubstWith is Envsubst with an explicit lookup instead of the process
// env. The bootstrap addon render needs it: per-entry `env:` overrides are
// scoped to ONE entry (bash exports them in the entry's subshell only), and
// concurrent DAG entries must not race on os.Setenv — so the overrides ride
// a lookup closure instead of the environment.
func EnvsubstWith(data []byte, names []string, lookup func(string) string) []byte {
	return envsubst(data, names, lookup)
}

// envsubst is the testable core: single-pass GNU SHELL-FORMAT substitution
// restricted to names, with values from lookup (listed-but-unset → "").
func envsubst(data []byte, names []string, lookup func(string) string) []byte {
	if len(names) == 0 {
		return data
	}
	listed := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			listed[n] = true
		}
	}

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
				name := string(data[i+2 : j])
				if listed[name] {
					out.WriteString(lookup(name))
					i = j + 1
					continue
				}
			}
			out.WriteByte(c)
			i++
			continue
		}
		if isIdentStartByte(data[i+1]) {
			// $NAME — bare form. The token is the MAXIMAL identifier, so
			// $FOO never fires inside $FOOBAR (identifier boundary).
			j := i + 1
			for j < len(data) && isIdentByte(data[j]) {
				j++
			}
			name := string(data[i+1 : j])
			if listed[name] {
				out.WriteString(lookup(name))
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

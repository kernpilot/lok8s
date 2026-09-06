package operator

import (
	"os"
	"regexp"

	"github.com/kernpilot/lok8s/internal/build"
)

// envsubstAll is the bash hook's bare `envsubst < template` — GNU envsubst
// with NO SHELL-FORMAT: every `$NAME` / `${NAME}` in the stream is
// replaced from the environment, unset → "" (so the cloud-init's own $ARCH,
// $RUNC, $CONTAINERD … are blanked — that is what the bash hook shipped;
// the capi DRIVER's whitelist render is a different, deliberate contract).
// exported is the hook's own `export`ed variable set, consulted before the
// process environment, exactly as the exports shadowed it.
func envsubstAll(data []byte, exported map[string]string) []byte {
	lookup := func(name string) string {
		if v, ok := exported[name]; ok {
			return v
		}
		return os.Getenv(name)
	}
	return build.EnvsubstWith(data, referencedNames(data), lookup)
}

var envRefRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// referencedNames lists every identifier the stream references after a `$`
// — handing the whitelist engine "everything" is what turns it into the
// unrestricted default mode.
func referencedNames(data []byte) []string {
	seen := map[string]bool{}
	var names []string
	for _, m := range envRefRe.FindAllSubmatch(data, -1) {
		name := string(m[1])
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

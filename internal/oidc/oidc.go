// Package oidc reads spec.oidc from a cluster spec into the LOK8S_SPEC_OIDC_*
// environment — the Go port of .lok8s/utils/oidc.sh (oidc::enabled /
// oidc::load_spec). The env-var names are the contract: the drivers' spec
// readers (lo::export_spec_envs / kubeone::extract_vars, still bash) export
// the SAME names during provision/bootstrap, and commands that run OUTSIDE a
// driver context (`lo kubeconfig --oidc` in a fresh shell) load them from the
// spec here.
//
// spec.oidc schema (defaults applied on load, mirroring the spec readers):
//
//	spec:
//	  oidc:                          # OPTIONAL — absent ⇒ no apiserver OIDC wiring
//	    issuer: https://id.kubehz.dev   # REQUIRED; the IdP issuer URL (https)
//	    clientID: <kubectl-client-id>   # REQUIRED; the OIDC client/audience kubectl uses
//	    usernameClaim: sub              # default "sub"
//	    usernamePrefix: "oidc:"         # default "oidc:" ("-" ⇒ no prefix, k8s semantics)
//	    groupsClaim: groups             # default "groups"
//	    groupsPrefix: "oidc:"           # default "oidc:"
//	    caBundle: |                     # OPTIONAL inline PEM (dev mkcert IdPs)
package oidc

import (
	"errors"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrPrinted marks an error whose message was already printed in the bash
// implementation's own format ([error] … on stderr).
var ErrPrinted = errors.New("oidc: error already printed")

// Env var names — exact spelling shared with the bash drivers' spec readers.
const (
	EnvIssuer         = "LOK8S_SPEC_OIDC_ISSUER"
	EnvClientID       = "LOK8S_SPEC_OIDC_CLIENTID"
	EnvUsernameClaim  = "LOK8S_SPEC_OIDC_USERNAMECLAIM"
	EnvUsernamePrefix = "LOK8S_SPEC_OIDC_USERNAMEPREFIX"
	EnvGroupsClaim    = "LOK8S_SPEC_OIDC_GROUPSCLAIM"
	EnvGroupsPrefix   = "LOK8S_SPEC_OIDC_GROUPSPREFIX"
	EnvCABundle       = "LOK8S_SPEC_OIDC_CABUNDLE"
)

// Enabled reports whether both the issuer and the clientID are set (bash:
// oidc::enabled). Both are required for a usable jwt authenticator (issuer
// URL + the audience kubectl presents), so either alone is treated as "not
// configured".
func Enabled() bool {
	return os.Getenv(EnvIssuer) != "" && os.Getenv(EnvClientID) != ""
}

// LoadSpec exports the LOK8S_SPEC_OIDC_* vars from a cluster spec file, with
// the same defaults the drivers apply (bash: oidc::load_spec). Lets commands
// that run OUTSIDE a driver context read spec.oidc without a
// provision/bootstrap having exported the vars first. Error strings verbatim.
func LoadSpec(clusterYAML string, errOut io.Writer) error {
	raw, err := os.ReadFile(clusterYAML)
	if err != nil {
		ui.Errorf(errOut, "oidc: cluster spec not found: %s", clusterYAML)
		return ErrPrinted
	}

	// Fail loud on a malformed spec here, instead of surfacing a raw parse
	// error from whichever read below hits it first. An empty/comment-only
	// document is VALID yaml (null) — the reads below resolve it to their
	// defaults; only a real parse error must fail.
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		ui.Errorf(errOut, "oidc: could not parse cluster spec: %s", clusterYAML)
		return ErrPrinted
	}
	oidcNode := lookup(&root, "spec", "oidc")

	// yq `// "default"` semantics: the default fires on a MISSING or null key,
	// never on an explicit empty string (an explicit `usernamePrefix: ""` is a
	// deliberate "no prefix" and must survive the load).
	os.Setenv(EnvIssuer, scalarOr(lookup(oidcNode, "issuer"), ""))
	os.Setenv(EnvClientID, scalarOr(lookup(oidcNode, "clientID"), ""))
	os.Setenv(EnvUsernameClaim, scalarOr(lookup(oidcNode, "usernameClaim"), "sub"))
	os.Setenv(EnvUsernamePrefix, scalarOr(lookup(oidcNode, "usernamePrefix"), "oidc:"))
	os.Setenv(EnvGroupsClaim, scalarOr(lookup(oidcNode, "groupsClaim"), "groups"))
	os.Setenv(EnvGroupsPrefix, scalarOr(lookup(oidcNode, "groupsPrefix"), "oidc:"))
	os.Setenv(EnvCABundle, scalarOr(lookup(oidcNode, "caBundle"), ""))

	// Boundary rule enforced ONCE for every consumer (both drivers + the OIDC
	// kubeconfig): a configured issuer must be https — previously only
	// render_auth_config checked, so the kubeone manifest path could inject a
	// plain-http issuer silently.
	if issuer := os.Getenv(EnvIssuer); issuer != "" && !strings.HasPrefix(issuer, "https://") {
		ui.Errorf(errOut, "spec.oidc.issuer must be an https:// URL, got '%s'", issuer)
		return ErrPrinted
	}
	return nil
}

// lookup walks a mapping path from a node (dereferencing document/alias
// wrappers), nil when any hop is missing or not a mapping.
func lookup(n *yaml.Node, path ...string) *yaml.Node {
	n = deref(n)
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
		n = deref(next)
	}
	return n
}

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

// scalarOr returns the scalar value of n, or fallback when n is missing,
// null, or not a scalar (a non-scalar spec.oidc field is nonsense; treating
// it as unset routes it to the "no usable spec.oidc" error).
func scalarOr(n *yaml.Node, fallback string) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return fallback
	}
	return n.Value
}

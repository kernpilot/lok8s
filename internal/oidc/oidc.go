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
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// ErrHandled marks an error whose message was already printed in the bash
// implementation's own format ([error] … on stderr).
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

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
		ui.ErrorTo(errOut, "oidc: cluster spec not found: %s", clusterYAML)
		return ErrHandled
	}

	// Fail loud on a malformed spec here, instead of surfacing a raw parse
	// error from whichever read below hits it first. An empty/comment-only
	// document is VALID yaml (null) — the reads below resolve it to their
	// defaults; only a real parse error must fail.
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		ui.ErrorTo(errOut, "oidc: could not parse cluster spec: %s", clusterYAML)
		return ErrHandled
	}
	oidcNode := yqsem.Lookup(&root, "spec", "oidc")

	// yq `// "default"` semantics: the default fires on a MISSING or null key,
	// never on an explicit empty string (an explicit `usernamePrefix: ""` is a
	// deliberate "no prefix" and must survive the load).
	os.Setenv(EnvIssuer, yqsem.OrNull(yqsem.Lookup(oidcNode, "issuer"), ""))
	os.Setenv(EnvClientID, yqsem.OrNull(yqsem.Lookup(oidcNode, "clientID"), ""))
	os.Setenv(EnvUsernameClaim, yqsem.OrNull(yqsem.Lookup(oidcNode, "usernameClaim"), "sub"))
	os.Setenv(EnvUsernamePrefix, yqsem.OrNull(yqsem.Lookup(oidcNode, "usernamePrefix"), "oidc:"))
	os.Setenv(EnvGroupsClaim, yqsem.OrNull(yqsem.Lookup(oidcNode, "groupsClaim"), "groups"))
	os.Setenv(EnvGroupsPrefix, yqsem.OrNull(yqsem.Lookup(oidcNode, "groupsPrefix"), "oidc:"))
	os.Setenv(EnvCABundle, yqsem.OrNull(yqsem.Lookup(oidcNode, "caBundle"), ""))

	// Boundary rule enforced ONCE for every consumer (both drivers + the OIDC
	// kubeconfig): a configured issuer must be https — previously only
	// render_auth_config checked, so the kubeone manifest path could inject a
	// plain-http issuer silently.
	if issuer := os.Getenv(EnvIssuer); issuer != "" && !strings.HasPrefix(issuer, "https://") {
		ui.ErrorTo(errOut, "spec.oidc.issuer must be an https:// URL, got '%s'", issuer)
		return ErrHandled
	}
	return nil
}

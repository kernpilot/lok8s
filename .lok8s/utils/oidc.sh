# shellcheck shell=bash
# oidc.sh — render an apiserver StructuredAuthenticationConfiguration from
# spec.oidc (the LOK8S_SPEC_OIDC_* env vars exported by the drivers' spec
# readers — lo::export_spec_envs / kubeone::extract_vars).
#
# This drives the MODERN structured authentication config (the apiserver's
# --authentication-config file), NOT the legacy --oidc-* flags. Both drivers
# target Kubernetes v1.35.x.
#
# spec.oidc schema (all defaults applied by the spec readers via yq `// "..."`):
#   spec:
#     oidc:                          # OPTIONAL — absent ⇒ no apiserver OIDC wiring
#       issuer: https://id.kubehz.dev   # REQUIRED; the IdP issuer URL (https)
#       clientID: <kubectl-client-id>   # REQUIRED; the OIDC client/audience kubectl uses
#       usernameClaim: sub              # default "sub"
#       usernamePrefix: "oidc:"         # default "oidc:" ("-" ⇒ no prefix, k8s semantics)
#       groupsClaim: groups             # default "groups"
#       groupsPrefix: "oidc:"           # default "oidc:"
#       caBundle: |                     # OPTIONAL inline PEM (dev mkcert IdPs);
#         -----BEGIN CERTIFICATE-----   # absent ⇒ rely on system trust
#
# apiVersion: `apiserver.config.k8s.io/v1` is the STABLE AuthenticationConfiguration
# version on the v1.35 target (graduated v1alpha1 1.29 → v1beta1 1.30 → v1). This
# was VERIFIED empirically against the kube-apiserver v1.35.5 binary: it loads a
# config with this apiVersion all the way through startup, and rejects a bogus
# version with `no kind "AuthenticationConfiguration" is registered for version
# "...v1bogus"` — so v1 is the registered/correct version here.
#   https://kubernetes.io/docs/reference/config-api/apiserver-config.v1/
#   https://kubernetes.io/docs/reference/access-authn-authz/authentication/

OIDC_AUTH_CONFIG_APIVERSION="apiserver.config.k8s.io/v1"

# oidc::enabled — true (return 0) when both the issuer and the clientID are set.
# Both are required for a usable jwt authenticator (issuer URL + the audience
# kubectl presents), so either alone is treated as "not configured".
oidc::enabled() {
  [[ -n "${LOK8S_SPEC_OIDC_ISSUER:-}" && -n "${LOK8S_SPEC_OIDC_CLIENTID:-}" ]]
}

# oidc::render_auth_config — emit a valid AuthenticationConfiguration YAML to
# stdout from the LOK8S_SPEC_OIDC_* vars. Returns non-zero (and emits nothing)
# when no issuer is configured, so callers can guard cheaply.
#
# Shape (apiVersion verified above):
#   apiVersion: apiserver.config.k8s.io/v1
#   kind: AuthenticationConfiguration
#   jwt:
#     - issuer:
#         url: <issuer>
#         audiences: [<clientID>]
#         certificateAuthority: |        # only when caBundle is set
#           <PEM>
#       claimMappings:
#         username: { claim: <usernameClaim>, prefix: <usernamePrefix> }
#         groups:   { claim: <groupsClaim>,   prefix: <groupsPrefix> }
#
# audienceMatchPolicy is intentionally OMITTED: per the apiserver config schema
# it is only required when `audiences` has MORE THAN ONE entry. We always emit a
# single audience (the clientID), for which the field must be left unset
# (setting MatchAny with a single audience is rejected by the apiserver).
oidc::render_auth_config() {
  # set -e safety: every capture below is from an env var (no external command),
  # but guard the predicate explicitly so an unset issuer is a clean failure.
  local issuer="${LOK8S_SPEC_OIDC_ISSUER:-}"
  [[ -n "${issuer}" ]] || return 1

  local client_id="${LOK8S_SPEC_OIDC_CLIENTID:-}"
  [[ -n "${client_id}" ]] || { error "spec.oidc.clientID is required when spec.oidc is set"; return 1; }

  local username_claim="${LOK8S_SPEC_OIDC_USERNAMECLAIM:-sub}"
  local username_prefix="${LOK8S_SPEC_OIDC_USERNAMEPREFIX:-oidc:}"
  local groups_claim="${LOK8S_SPEC_OIDC_GROUPSCLAIM:-groups}"
  local groups_prefix="${LOK8S_SPEC_OIDC_GROUPSPREFIX:-oidc:}"
  local ca_bundle="${LOK8S_SPEC_OIDC_CABUNDLE:-}"

  # Defensive validation at the system boundary: the issuer is an external,
  # operator-supplied URL that lands in a config file the apiserver trusts.
  # Require https (OIDC discovery + token verification must not ride plain HTTP).
  if [[ "${issuer}" != https://* ]]; then
    error "spec.oidc.issuer must be an https:// URL, got '${issuer}'"
    return 1
  fi

  echo "# Rendered by lok8s from spec.oidc — apiserver StructuredAuthenticationConfiguration."
  echo "# apiserver.config.k8s.io/v1 — verified accepted by kube-apiserver v1.35.5."
  echo "apiVersion: ${OIDC_AUTH_CONFIG_APIVERSION}"
  echo "kind: AuthenticationConfiguration"
  echo "jwt:"
  echo "  - issuer:"
  echo "      url: \"${issuer}\""
  echo "      audiences:"
  echo "        - \"${client_id}\""
  # certificateAuthority holds the PEM bundle inline (not a path). Only emit it
  # when a caBundle was supplied; otherwise the apiserver uses system trust.
  if [[ -n "${ca_bundle}" ]]; then
    echo "      certificateAuthority: |"
    # Indent every line of the PEM under the block scalar. printf keeps the
    # final newline handling predictable; the while-read is set -e safe.
    local _line
    while IFS= read -r _line || [[ -n "${_line}" ]]; do
      echo "        ${_line}"
    done <<< "${ca_bundle}"
  fi
  echo "    claimMappings:"
  echo "      username:"
  echo "        claim: \"${username_claim}\""
  # prefix is REQUIRED by the schema when claim is set (may be empty string).
  # A literal "-" means "no prefix" in k8s OIDC semantics — preserve it as-is so
  # the apiserver applies its documented behavior (it is not rewritten to "").
  echo "        prefix: \"${username_prefix}\""
  echo "      groups:"
  echo "        claim: \"${groups_claim}\""
  echo "        prefix: \"${groups_prefix}\""
}

#!/usr/bin/env bats
# sso_gate_merge_test.bats — the `mergeType` gate.
#
# `mergeType: StrategicMerge` on the sso-gate SecurityPolicy is a SECURITY
# field, not a style choice: without it a route-level policy REPLACES the
# Gateway's one wholesale, deleting whatever guarded the routes it selects.
# Nothing else in CI defends it. addons_build_test.bats only proves the addon
# `kustomize build`s, and kustomize has no CRD schema — a typo (`mergetype:`),
# a wrong value (`StrategicMergePatch`), or the field moved onto a Gateway
# target all build perfectly cleanly and ship the hole.
#
# So assert on the RENDERED object: field present, spelled exactly, valued
# exactly, and attached to an xRoute target (the only shape Envoy Gateway
# accepts it on). The chart pin that provides the field is checked too — the
# manifest and the CRD version that understands it must move together.

setup() {
  load "../test_helper"
  command -v kustomize &>/dev/null || skip "kustomize required"
  command -v yq &>/dev/null || skip "yq required"
  _SSO_DIR="${_PROJECT_ROOT}/.lok8s/addons/sso-gate"
}

# _render → the sso-gate SecurityPolicy as rendered by kustomize.
_render() {
  kustomize build "${_SSO_DIR}" | yq eval -N 'select(.kind == "SecurityPolicy")' -
}

@test "sso-gate renders exactly one SecurityPolicy" {
  local names
  names="$(_render | yq eval -N '.metadata.name' -)"
  [ "${names}" = "sso-gate" ] \
    || fail "expected exactly one SecurityPolicy named 'sso-gate', got: ${names:-<none>}"
}

@test "the rendered sso-gate SecurityPolicy carries mergeType: StrategicMerge" {
  local got
  # Exact key AND exact value. `yq` returns null for a mistyped key, so a
  # `mergetype:`/`Merge-Type:` typo fails here rather than shipping silently.
  got="$(_render | yq eval -N '.spec.mergeType' -)"
  [ "${got}" = "StrategicMerge" ] \
    || fail "spec.mergeType is '${got}', expected exactly 'StrategicMerge' (the CRD accepts StrategicMerge and JSONMerge; 'Replace' and any typo are rejected)"
}

@test "mergeType sits on an xRoute target, the only shape Envoy Gateway allows" {
  local kinds
  # Envoy Gateway rejects mergeType on Gateway / Gateway-listener / ListenerSet
  # targets. Collect every target kind the policy declares, however it declares
  # it (targetRef, targetRefs, targetSelectors).
  kinds="$(_render | yq eval -N '
    [.spec.targetRef] + (.spec.targetRefs // []) + (.spec.targetSelectors // [])
    | map(select(tag == "!!map") | .kind // "-") | join(",")
  ' -)"
  [ -n "${kinds}" ] || fail "policy declares no target at all"
  local k
  for k in ${kinds//,/ }; do
    case "${k}" in
      HTTPRoute|GRPCRoute|TCPRoute) ;;
      *) fail "mergeType is set but the policy targets '${k}' — Envoy Gateway accepts mergeType only on HTTPRoute/GRPCRoute/TCPRoute (targets: ${kinds})" ;;
    esac
  done
}

@test "the envoy-gateway chart pin is at or above the v1.8.0 mergeType floor" {
  local pin major minor
  pin="$(yq eval -N '.version' "${_PROJECT_ROOT}/.lok8s/addons/envoy-gateway/chart.yaml")"
  [[ "${pin}" =~ ^v([0-9]+)\.([0-9]+)\. ]] \
    || fail "cannot parse the envoy-gateway chart pin '${pin}'"
  major="${BASH_REMATCH[1]}"; minor="${BASH_REMATCH[2]}"
  # mergeType arrived in Envoy Gateway v1.8.0. Below that the CRD has no such
  # field and a server-side apply of the sso-gate policy is REJECTED outright,
  # so the pin and the manifest must never drift apart.
  (( major > 1 || (major == 1 && minor >= 8) )) \
    || fail "envoy-gateway pinned at ${pin}, but .lok8s/addons/sso-gate/securitypolicy.yaml uses spec.mergeType, which needs v1.8.0+"
}

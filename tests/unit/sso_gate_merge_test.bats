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
#
# Assert on the SELECTOR as hard as on the field. A gate that only reads
# `.kind` fails OPEN: mistype the `group`, move the policy to another
# namespace, or retarget it at a kind the labeled routes are not, and the
# selector matches NOTHING — every labeled route stays fully public while
# every check here still passes. That is worse than the replace bug this
# addon exists to prevent, because the replace bug at least leaves a visible
# `Overridden` condition. So each of group, kind, match labels and namespace
# is pinned, and each one is mutation-proven to fail on its own.
#
# Reading only the fields this file happens to know about is NOT enough, and
# that lesson cost three review rounds: each round closed one way to narrow the
# selector to nothing and left a sibling field wide open. A `targetSelectors`
# entry has FIVE properties and THREE of them narrow the match — `matchLabels`,
# `matchExpressions` and `namespaces`. So the selector is read as a WHOLE
# OBJECT: `_targets` reports every field the target actually SETS, and the test
# whitelists that key set. A field outside the whitelist fails the test even if
# nobody here has heard of it — including one a future Envoy Gateway CRD adds.
# That turns the next new narrowing field from an invisible gap into a red test.

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

# _targets → one `group|kind|labels|fields` line per target the policy
# declares, however it declares it (targetRef, targetRefs, targetSelectors).
# `-` stands in for an absent field; `labels` is the matchLabels map flattened
# to key-sorted `k=v` pairs so a single string comparison covers the whole map
# (an EXTRA label narrows the selector just as fatally as a wrong one).
#
# `fields` is the sorted set of every key the target OBJECT sets, read off the
# object itself rather than off a list written here. It is what makes the
# selector assertion complete instead of merely long: the test does not need to
# know what a field means to notice it is there.
_targets() {
  _render | yq eval -N '
    [.spec.targetRef] + (.spec.targetRefs // []) + (.spec.targetSelectors // [])
    | .[] | select(tag == "!!map")
    | (.group // "-") + "|" + (.kind // "-") + "|"
      + ((.matchLabels // {}) | to_entries | sort_by(.key)
         | map(.key + "=" + .value) | join(","))
      + "|" + (keys | sort | join(","))
  ' -
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

@test "every sso-gate target selects the labeled HTTPRoutes, group and labels included" {
  local line group kind labels fields n=0
  while IFS='|' read -r group kind labels fields; do
    [[ -n "${group}${kind}${labels}${fields}" ]] || continue
    n=$(( n + 1 ))
    line="group=${group} kind=${kind} labels=${labels:-<none>} fields=${fields:-<none>}"

    # The Gateway API group, exactly. `gateway.networking.io`,
    # `networking.k8s.io` and an empty group all `kustomize build` cleanly, so
    # a wrong group ships out of this repo unnoticed without this gate. A live
    # apiserver does reject it — Envoy Gateway v1.9.0 answers a server-side
    # apply with `spec.targetSelectors[0]: Invalid value: group must be
    # gateway.networking.k8s.io` (verified 2026-08-22) — but that failure lands
    # on the USER at apply time, on a cluster, in whatever state the rollout is
    # in by then. Catching it here moves it to CI, where it is a red test
    # instead of a broken deploy.
    [ "${group}" = "gateway.networking.k8s.io" ] \
      || fail "target group is '${group}', expected 'gateway.networking.k8s.io' — the addon would ship a policy the cluster refuses to admit, and on any build that does admit it the selector matches no route at all, so labeling a route would leave it fully public (${line})"

    # Envoy Gateway also accepts mergeType on a TCPRoute target, but OIDC is a
    # redirect-and-cookie HTTP flow: on a TCP stream this policy enforces
    # nothing while still looking configured. Pin the kinds sso-gate can
    # meaningfully protect — this is the addon's own scope, NOT a claim about
    # what the CRD permits.
    case "${kind}" in
      HTTPRoute|GRPCRoute) ;;
      *) fail "target kind is '${kind}' — sso-gate serves an OIDC browser flow, so it may only target HTTPRoute/GRPCRoute; on any other kind the label buys no protection (${line})" ;;
    esac

    # The documented label, exactly and only. A typo'd key, a `"True"` value,
    # or an extra pair ANDed in narrows the selector to zero routes.
    [ "${labels}" = "sso.lok8s.dev/protect=true" ] \
      || fail "target match labels are '${labels:-<none>}', expected exactly 'sso.lok8s.dev/protect=true' — the label documented in docs/guide/addons.md and the one the selector matches must be the same string (${line})"

    # And now the whole object, not just the fields named above. Everything a
    # target sets is ANDed, so every unchecked field is a way to narrow the
    # match to zero routes while the three assertions above stay green:
    #
    #   matchExpressions: [{key: never.example.com/x, operator: Exists}]
    #   namespaces: {from: Selector, selector: {matchLabels: {nope: nope}}}
    #
    # Both are legal — Envoy Gateway v1.9.0 admits that object on a server-side
    # apply (verified 2026-08-22) — both select nothing, and neither is visible
    # to a field-by-field reading. Same fail-open class as a wrong `group`, one
    # field over. Whitelisting the key set closes the whole class at once and
    # keeps closing it: a `targetSelectors` property that does not exist yet
    # still trips this the day someone adds it. When that happens, do not just
    # append the name — decide whether the new field can narrow the match to
    # nothing, and only then widen the list.
    [ "${fields}" = "group,kind,matchLabels" ] \
      || fail "target sets fields '${fields:-<none>}', expected exactly 'group,kind,matchLabels' — an unexpected field is ANDed into the selector and can narrow it to zero routes, leaving every labeled route fully public; if the extra field is deliberate, prove it cannot empty the match and then add it to this whitelist (${line})"
  done < <(_targets)

  # Vacuity guard: no targets at all would pass every loop body above.
  (( n >= 1 )) || fail "policy declares no target at all — it selects nothing and enforces nothing"
}

@test "sso-gate ships in the namespace its same-namespace selector needs" {
  local ns
  ns="$(_render | yq eval -N '.metadata.namespace' -)"
  # `targetSelectors` is same-namespace unless it opts into
  # `namespaces` + a ReferenceGrant, which this addon does not. It ships in
  # `default` because that is where a stock lok8s gateway target puts its
  # routes, and both docs/guide/addons.md and this addon's kustomization.yaml
  # header state it. Moving the base elsewhere silently unprotects every route
  # that relied on the shipped default; other namespaces are served by LAYERING
  # a copy with a kustomize `namespace:` transform, which leaves this base alone.
  [ "${ns}" = "default" ] \
    || fail "the base sso-gate policy renders in namespace '${ns}', expected 'default' — this policy's targetSelectors carries no 'namespaces' opt-in, so it selects only routes beside it and a moved base selects none of the routes its docs say it protects. Serve another namespace by layering a copy, not by moving the base"
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

#!/usr/bin/env bats
# addons_build_test.bats — every RAW (non-chart) framework addon must
# `kustomize build` cleanly. Chart addons (chart.yaml/khelm) need network +
# the khelm plugin, so they are exercised elsewhere; the raw ones are pure
# manifests and have NO other build gate — before this test a broken raw
# addon (bad kustomization, unparsable manifest) shipped silently and only
# failed at bootstrap time on a user's cluster (found by the sso-gate
# review: nothing in CI ever built the addon manifests).

setup() {
  load "../test_helper"
  command -v kustomize &>/dev/null || skip "kustomize required for addon build tests"
}

@test "every raw framework addon kustomize-builds cleanly" {
  local dir name failed=()
  for dir in "${_PROJECT_ROOT}"/.lok8s/addons/*/; do
    name="$(basename "${dir}")"
    # chart addons render via khelm (network); only raw manifests build here.
    # A khelm addon is NOT always `chart.yaml` (cert-manager-webhook-hetzner
    # embeds its ChartRenderer in another file) — skip on any reference.
    [[ -f "${dir}chart.yaml" ]] && continue
    grep -rlq "kind: ChartRenderer" "${dir}" && continue
    kustomize build "${dir}" >/dev/null 2>&1 || failed+=("${name}")
  done
  [[ ${#failed[@]} -eq 0 ]] || fail "raw addon(s) failed kustomize build: ${failed[*]}"
}

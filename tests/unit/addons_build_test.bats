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
  local dir name built=0 failed=()
  for dir in "${_PROJECT_ROOT}"/.lok8s/addons/*/; do
    name="$(basename "${dir}")"
    # chart addons render via khelm (network); only raw manifests build here.
    # A khelm addon is NOT always `chart.yaml` (cert-manager-webhook-hetzner
    # embeds its ChartRenderer in another file) — skip on a REAL kind line
    # (anchored: a comment mentioning ChartRenderer must not lose coverage).
    [[ -f "${dir}chart.yaml" ]] && continue
    grep -rlqE '^kind: ChartRenderer' "${dir}" && continue
    # Keep the build's stderr: on failure it goes into the fail message so
    # the CI log names the actual kustomize error, not just the addon.
    if ! out=$(kustomize build "${dir}" 2>&1 >/dev/null); then
      failed+=("${name}: ${out}")
    fi
    built=$((built + 1))
  done
  [[ ${#failed[@]} -eq 0 ]] || fail "raw addon(s) failed kustomize build: ${failed[*]}"
  # Vacuity guard: the raw set must never silently shrink to zero (that would
  # green this test while validating nothing).
  (( built >= 1 )) || fail "no raw addon was built — skip heuristic ate them all"
}

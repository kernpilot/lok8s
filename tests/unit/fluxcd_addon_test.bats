#!/usr/bin/env bats
# fluxcd_addon_test.bats: the memory limit of the Flux controllers in the
# fluxcd addon. With a limit of 256Mi, the source controller got OOMKilled
# at the artifact build of each new revision of a Git repository of about
# 60 MB. Go: TestFluxAddonControllersHaveTheMemoryLimit in internal/assets.

setup() {
  load "../test_helper"
}

# _flux_memory_limit <controller>: resources.limits.memory of <controller>
# in the values of the addon. awk, not yq: the file is block YAML, and the
# test does not need the pinned toolchain.
_flux_memory_limit() {
  awk -v c="${1}:" '
    /^[^ #]/ { top = $1; lim = 0; next }
    top != c { next }
    /^    limits:/ { lim = 1; next }
    /^    [^ ]/ { lim = 0; next }
    lim && /^      memory:/ { print $2; exit }
  ' "${_PROJECT_ROOT}/.lok8s/addons/fluxcd/values.yaml"
}

@test "the fluxcd addon gives the source, kustomize and helm controllers a 1Gi memory limit" {
  local c
  for c in sourceController kustomizeController helmController; do
    run _flux_memory_limit "${c}"
    assert_success
    assert_output "1Gi"
  done
}

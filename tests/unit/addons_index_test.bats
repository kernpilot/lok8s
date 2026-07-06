#!/usr/bin/env bats
# addons_index_test.bats — unit tests for docs/.vitepress/build-addons-index.sh
# (the machine-readable addon index published at /addons-index.json).
#
# Pins the publication contract: valid JSON, {generatedAt, addons[]}, sorted +
# byte-for-byte deterministic under SOURCE_DATE_EPOCH, exactly one entry per
# .lok8s/addons/ dir, and metadata-only entry keys.

setup() {
  load "../test_helper"
  command -v yq &>/dev/null || skip "yq required for addons-index tests"
  command -v jq &>/dev/null || skip "jq required for addons-index tests"
  INDEX_SH="${_PROJECT_ROOT}/docs/.vitepress/build-addons-index.sh"
}

@test "index script emits valid JSON with generatedAt + a non-empty addons array" {
  run bash "${INDEX_SH}"
  assert_success
  run jq -e '.generatedAt and (.addons | length > 0)' <<<"${output}"
  assert_success
}

@test "index is byte-for-byte deterministic under SOURCE_DATE_EPOCH" {
  export SOURCE_DATE_EPOCH=1700000000
  local one two
  one=$(bash "${INDEX_SH}")
  two=$(bash "${INDEX_SH}")
  assert_equal "${one}" "${two}"
  assert_equal "$(jq -r '.generatedAt' <<<"${one}")" "2023-11-14T22:13:20Z"
}

@test "index addons are sorted by name" {
  run bash "${INDEX_SH}"
  assert_success
  run jq -e '.addons == (.addons | sort_by(.name))' <<<"${output}"
  assert_success
}

@test "index has exactly one entry per .lok8s/addons/ dir (parity, fails on drift)" {
  local expected=0 d
  for d in "${_PROJECT_ROOT}/.lok8s/addons/"*/; do
    [[ -d "${d}" ]] && expected=$((expected + 1))
  done
  run bash "${INDEX_SH}"
  assert_success
  assert_equal "$(jq -r '.addons | length' <<<"${output}")" "${expected}"
}

@test "index entries carry metadata-only keys and real pinned versions" {
  local cilium_v
  cilium_v=$(yq -r '.version' "${_PROJECT_ROOT}/.lok8s/addons/cilium/chart.yaml")
  local out
  out=$(bash "${INDEX_SH}")
  # only the contract keys — never values/env/manifests
  run jq -e 'all(.addons[]; (keys - ["name","chartVersion","appVersion","category"]) == [])' <<<"${out}"
  assert_success
  # spot-check a khelm-pinned addon against its own chart.yaml
  assert_equal "$(jq -r '.addons[] | select(.name == "cilium") | .chartVersion' <<<"${out}")" "${cilium_v}"
  assert_equal "$(jq -r '.addons[] | select(.name == "cilium") | .category' <<<"${out}")" "networking"
}

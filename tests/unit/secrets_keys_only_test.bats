#!/usr/bin/env bats
# secrets_keys_only_test.bats — the Secret plugin's keys-only setting
# (LOK8S_INTERNAL_SECRETS_KEYS_ONLY) is internal to lo. A value the caller
# exported never reaches a child of lo: the entry pins it to 0 at the start
# (Go: cmd/lo/main.go). Only `lo build --keys-only` turns it on, for its own
# render. The render pins of build::artifacts and addons::render, and
# build::keys_only itself, are tested in build_test.bats and
# bootstrap_test.bats.

setup() {
  load "../test_helper"
  setup_tmpdir
}

teardown() {
  teardown_tmpdir
}

# The real `lo` against a throwaway project dir, with stub tools first on
# PATH. Each stub logs the keys-only value it inherited. The caller's
# environment carries the setting as 1 (and the store-free switch as 1).
_run_lo_stubbed() {
  local proj="${BATS_TEST_TMPDIR}/proj" stub="${BATS_TEST_TMPDIR}/stub" tool
  mkdir -p "${proj}/clusters/d.dev" "${stub}"
  printf 'kind: Lo\nmetadata:\n  name: d\n' > "${proj}/clusters/d.dev/cluster.lok8s.yaml"
  printf 'resources:\n  - cm.yaml\n' > "${proj}/clusters/d.dev/kustomization.yaml"
  for tool in kubectl kind tilt docker kustomize; do
    cat > "${stub}/${tool}" <<'EOF'
#!/bin/sh
printf '%s keys-only=%s disable=%s\n' "$(basename "$0")" "${LOK8S_INTERNAL_SECRETS_KEYS_ONLY-<unset>}" "${LOK8S_SECRETS_DISABLE-<unset>}" >> "${STUB_LOG}"
case "$(basename "$0") ${1:-}" in
  "kustomize build") printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stub\n' ;;
  *) echo "v1.2.3" ;;
esac
EOF
    chmod +x "${stub}/${tool}"
  done
  export STUB_LOG="${BATS_TEST_TMPDIR}/stub.log"
  : > "${STUB_LOG}"
  run env -C "${proj}" \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    PATH="${stub}:${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1 \
    LOK8S_SECRETS_DISABLE=1 \
    "${_PROJECT_ROOT}/.lok8s/lo" "${@}"
}

# `lo version` starts the toolchain with no render overlay of its own: what
# the stubs see is what the entry left in the environment.
@test "the lo entry pins the keys-only setting to 0 for every child" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  _run_lo_stubbed version </dev/null
  assert_success
  run cat "${STUB_LOG}"
  [ "${#lines[@]}" -eq 5 ] || { echo "want 5 stub lines, got: ${output}"; return 1; }
  assert_line "kubectl keys-only=0 disable=1"
  assert_line "kind keys-only=0 disable=1"
  assert_line "kustomize keys-only=0 disable=1"
  assert_line "tilt keys-only=0 disable=1"
  assert_line "docker keys-only=0 disable=1"
}

# With the setting inherited as 1, `lo build` and `lo build --no-secrets`
# hand kustomize 0; only `lo build --keys-only` hands it 1 (with the
# store-free switch on), prints the render and writes no artifacts.yaml.
@test "lo build gives the keys-only setting 1 only for --keys-only" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local args want
  for args in "build --domain d.dev|kustomize keys-only=0 disable=0" \
              "build --domain d.dev --no-secrets|kustomize keys-only=0 disable=1" \
              "build --domain d.dev --keys-only|kustomize keys-only=1 disable=1"; do
    want="${args#*|}"
    rm -f "${BATS_TEST_TMPDIR}/proj/clusters/d.dev/artifacts.yaml"
    # shellcheck disable=SC2086 # the argv is the words of the case
    _run_lo_stubbed ${args%|*} </dev/null
    assert_success
    run cat "${STUB_LOG}"
    assert_output "${want}"
  done
  # The last case: --keys-only printed the render, wrote no file.
  _run_lo_stubbed build --domain d.dev --keys-only </dev/null
  assert_success
  assert_output --partial "kind: ConfigMap"
  assert_output --partial "lo build: d.dev rendered 1 document(s) -> stdout (keys only)"
  [ ! -e "${BATS_TEST_TMPDIR}/proj/clusters/d.dev/artifacts.yaml" ]
}

@test "lo build --keys-only refuses --split and --single" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local flag
  for flag in --split --single; do
    _run_lo_stubbed build --domain d.dev --keys-only "${flag}" </dev/null
    assert_failure
    assert_output --partial "--keys-only and --split/--single are mutually exclusive (--keys-only writes no file)"
    [ ! -e "${BATS_TEST_TMPDIR}/proj/clusters/d.dev/artifacts.yaml" ]
  done
}

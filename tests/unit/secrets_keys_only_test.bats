#!/usr/bin/env bats
# secrets_keys_only_test.bats — the Secret plugin's keys-only setting
# (LOK8S_INTERNAL_SECRETS_KEYS_ONLY) is internal to lo. A value the caller
# exported never reaches a child of lo: the entry pins it to 0 at the start
# (Go: cmd/lo/main.go). Only `lo build --keys-only` turns it on, for its own
# render. The render pins of build::artifacts and addons::render, and
# build::keys_only itself, are tested in build_test.bats and
# bootstrap_test.bats. The last test is the bash version check of the
# tracing rules in utils/keysonly.sh.

setup() {
  load "../test_helper"
  setup_tmpdir
}

teardown() {
  teardown_tmpdir
}

# _run_stubbed <entry> <args...>: run an entry of the bash tree against a
# throwaway project dir, with stub tools first on PATH. Each stub logs the
# keys-only value it inherited. The caller's environment carries the
# setting as 1 (and the store-free switch as 1).
_run_stubbed() {
  local entry="${1}"
  shift
  local proj="${BATS_TEST_TMPDIR}/proj" stub="${BATS_TEST_TMPDIR}/stub" tool
  # .kustomize: the plugin home with a Secret plugin, which --keys-only
  # requires. A test that has put its own plugin there keeps it; else the
  # plugin has the keys-only run.
  local plugin="${proj}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
  mkdir -p "${proj}/clusters/d.dev" "${plugin%/*}" "${stub}"
  if [[ ! -e "${plugin}" ]]; then
    printf '#!/bin/sh\n[ "$1" = --capabilities ] && { printf '"'"'dry-run\\nkeys-only\\n'"'"'; exit 0; }\nexit 1\n' > "${plugin}"
    chmod +x "${plugin}"
  fi
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
  run env -C "${proj}" -u KUSTOMIZE_PLUGIN_HOME \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    PATH="${stub}:${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1 \
    LOK8S_SECRETS_DISABLE=1 \
    "${entry}" "${@}"
}

# _run_lo_stubbed <args...>: the lo entry, run as _run_stubbed runs it.
_run_lo_stubbed() {
  _run_stubbed "${_PROJECT_ROOT}/.lok8s/lo" "${@}"
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

# The k8s lib is also an entry of its own (`.lok8s/libs/k8s infrastructure`),
# without the lo entry and its pin. Its two renders hand kustomize the
# keys-only setting 0 when the caller exported 1.
@test "the k8s entry's renders pin the keys-only setting to 0" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local proj="${BATS_TEST_TMPDIR}/proj" cmd
  mkdir -p "${proj}/clusters/d.dev/targets" "${proj}/clusters/d.dev/secrets" "${proj}/.k8s/overlays/d.dev"
  echo '*' > "${proj}/clusters/d.dev/secrets/.gitignore"
  for cmd in infrastructure platform; do
    _run_stubbed "${_PROJECT_ROOT}/.lok8s/libs/k8s" "${cmd}" --domain d.dev </dev/null
    assert_success
    run cat "${STUB_LOG}"
    assert_output "kustomize keys-only=0 disable=1"
  done
  [ -s "${proj}/clusters/d.dev/artifacts/infrastructure.yaml" ]
  [ -s "${proj}/.k8s/overlays/d.dev/artifacts/platform.yaml" ]
}

# Through the lo entry: `lo build --keys-only` refuses an installed Secret
# plugin without the keys-only run (one that prints only dry-run for
# --capabilities), before kustomize runs. The plugin gets the probe only.
@test "lo build --keys-only refuses an older Secret plugin" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local plugin="${BATS_TEST_TMPDIR}/proj/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
  mkdir -p "${plugin%/*}"
  export PLUGIN_LOG="${BATS_TEST_TMPDIR}/plugin.log"
  printf '#!/bin/sh\necho "plugin $*" >> "${PLUGIN_LOG}"\necho dry-run\n' > "${plugin}"
  chmod +x "${plugin}"
  _run_lo_stubbed build --domain d.dev --keys-only </dev/null
  assert_failure
  assert_output --partial "the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the keys-only run"
  refute_output --partial "kind: ConfigMap"
  run cat "${STUB_LOG}"
  refute_output --partial "kustomize"
  run cat "${PLUGIN_LOG}"
  assert_output "plugin --capabilities"
}

# The tracing rules of utils/keysonly.sh need `local -` (bash 4.4) when
# xtrace, functrace or errtrace is on. lo states bash 4.3 as its minimum:
# bash 4.3 with one of these options on stops at the start of lo, with a
# message, and not at the point that holds a Secret value. Without them,
# and with bash 4.4 or later, lo goes on.
@test "keysonly::check_trace_support stops bash 4.3 under a trace, at the start of lo" {
  error() { printf '%s\n' "$*" >&2; }
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/keysonly.sh"
  local flags version
  for flags in himxB hiTB hiEB ehuxTE; do
    run keysonly::check_trace_support 4 3 "${flags}"
    assert_failure 1
    assert_output "lo: bash 4.3 cannot keep Secret values out of a trace: run lo without set -x, set -T and set -E, or use bash 4.4 or later"
  done
  run keysonly::check_trace_support 3 2 x
  assert_failure 1
  assert_output --partial "lo: bash 3.2 cannot keep Secret values out of a trace"
  run keysonly::check_trace_support 4 3 ehuBH
  assert_success
  assert_output ""
  for version in "4 4" "4 10" "5 0" "5 3" "6 0"; do
    # shellcheck disable=SC2086 # the major and the minor version
    run keysonly::check_trace_support ${version} ehuxTE
    assert_success
    assert_output ""
  done
  # The lo entry calls it with the version and the options of its bash,
  # before main runs.
  local call main
  call="$(grep -nF 'keysonly::check_trace_support "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}" "$-" || exit 1' "${_PROJECT_ROOT}/.lok8s/lo" | cut -d: -f1)"
  main="$(grep -n '^main() {' "${_PROJECT_ROOT}/.lok8s/lo" | cut -d: -f1)"
  [ -n "${call}" ] && [ -n "${main}" ] && [ "${call}" -lt "${main}" ]
  [[ "$(sed -n "${call}p" "${_PROJECT_ROOT}/.lok8s/lo")" == keysonly::* ]]
}

#!/usr/bin/env bats
# parity_probe_test.bats: the probe of hack/lib/parity.sh (parity::init
# runs parity::prove_switch before any case). Both implementations of
# `lo version` run each tool that they find, docker included. So the probe
# has its own .bin with a stub for each host tool, first on its PATH, and
# the harness stops (exit 2) when the PATH finds a host tool past its stub
# or when a stub refused a call.

setup() {
  load "../test_helper"
  setup_tmpdir
  # The host tools: a failing stub of each, first on PATH (the net that
  # stands for /usr/bin/docker and the rest). The probe must find its own
  # stubs, never these: teardown fails when one of them ran.
  stub_tools docker kubectl kind tilt helm hcloud ssh curl
  LIB="${_PROJECT_ROOT}/hack/lib/parity.sh"
  export FAKE_LO_LOG="${BATS_TEST_TMPDIR}/lo.log"
  # A stand-in lo for the probe's `lo version`. It writes to FAKE_LO_LOG
  # where each host tool resolves on the PATH that it got, and the answer
  # of `docker --version`. FAKE_LO_CALL="<impl> <argv...>" makes one more
  # tool call in the run of that implementation. It prints a bash row when
  # the project file selects bash, as the frozen tree does.
  cat >"${BATS_TEST_TMPDIR}/lo" <<'SH'
#!/usr/bin/env bash
set -u
impl="$(sed -n 's/^    default: //p' lok8s.yaml)"
for tool in docker kubectl kind tilt helm hcloud ssh curl; do
  printf '%s %s %s\n' "${impl}" "${tool}" "$(type -P "${tool}")" >>"${FAKE_LO_LOG}"
done
printf '%s docker--version %s\n' "${impl}" "$(docker --version)" >>"${FAKE_LO_LOG}"
read -r -a call <<<"${FAKE_LO_CALL:-}"
if [[ "${call[0]:-}" == "${impl}" ]]; then
  "${call[@]:1}" >/dev/null 2>&1 || true
fi
echo "lok8s       dev"
[[ "${impl}" != bash ]] || echo "bash        5.2.0"
SH
  chmod +x "${BATS_TEST_TMPDIR}/lo"
  # A stand-in toolchain for parity::probe_bin: yq stays a link, and the
  # toolchain's docker gives way to the probe's stub.
  ROOT_FAKE="${BATS_TEST_TMPDIR}/root"
  mkdir -p "${ROOT_FAKE}/.bin"
  printf '#!/usr/bin/env bash\necho yq\n' >"${ROOT_FAKE}/.bin/yq"
  printf '#!/usr/bin/env bash\necho toolchain docker\n' >"${ROOT_FAKE}/.bin/docker"
  chmod +x "${ROOT_FAKE}/.bin/yq" "${ROOT_FAKE}/.bin/docker"
  PROBE="${BATS_TEST_TMPDIR}/p"
}

teardown() {
  local rc=0
  assert_no_tool_ran || rc=$?
  teardown_tmpdir
  return "${rc}"
}

# run_init [VAR=value...]: parity::init with the stand-in lo, in a child
# bash (the library sets -euo pipefail). The variables go to its env.
run_init() {
  run env -u PARITY_KEEP "$@" bash -c 'source "$1" && parity::init "$2" && echo "probe passed"' \
    _ "${LIB}" "${BATS_TEST_TMPDIR}/lo"
}

# run_lib <script>: the library sourced in a child bash, ROOT set to the
# stand-in toolchain, then <script> with $1 = the probe dir and $2 = the
# refusal log.
run_lib() {
  run bash -c 'source "$1" && ROOT="$2" && shift 2 && mkdir -p "$1" && '"${1}" \
    _ "${LIB}" "${ROOT_FAKE}" "${PROBE}" "${LOG:-${BATS_TEST_TMPDIR}/refused.log}"
}

@test "probe_bin: each host tool is a stub file, the rest of the toolchain stays linked" {
  run_lib 'parity::probe_bin "$1" "$2"'
  [ "${status}" -eq 0 ]
  local tool
  for tool in docker kubectl kind tilt helm hcloud ssh curl; do
    [[ -f "${PROBE}/.bin/${tool}" && -x "${PROBE}/.bin/${tool}" && ! -L "${PROBE}/.bin/${tool}" ]] \
      || { echo "${tool}: not a stub file in the probe .bin"; return 1; }
  done
  [ "$(readlink "${PROBE}/.bin/yq")" = "${ROOT_FAKE}/.bin/yq" ]
  [ -f "${BATS_TEST_TMPDIR}/refused.log" ]
  [ ! -s "${BATS_TEST_TMPDIR}/refused.log" ]
}

@test "probe_stub: the version query gets a version line, every other call is logged and exits 1" {
  # A log path with a space: the stub must quote it.
  LOG="${BATS_TEST_TMPDIR}/a dir/refused.log"
  mkdir -p "${BATS_TEST_TMPDIR}/a dir"
  run_lib 'parity::probe_bin "$1" "$2"'
  [ "${status}" -eq 0 ]
  run "${PROBE}/.bin/docker" --version
  [ "${status}" -eq 0 ]
  [ "${output}" = "parity-stub v0.0.0" ]
  run "${PROBE}/.bin/kubectl" version --client
  [ "${status}" -eq 0 ]
  [ "${output}" = "parity-stub v0.0.0" ]
  run "${PROBE}/.bin/kubectl" version
  [ "${status}" -eq 1 ]
  [ "${output}" = "parity stub: the probe must not run kubectl version" ]
  run "${PROBE}/.bin/helm" version
  [ "${status}" -eq 1 ]
  run "${PROBE}/.bin/docker" ps
  [ "${status}" -eq 1 ]
  [ "$(cat "${LOG}")" = "$(printf 'kubectl version\nhelm version\ndocker ps')" ]
}

@test "prove_switch: the probe passes, and its lo finds each host tool as the probe stub" {
  run_init
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"probe passed"* ]]
  # Two runs (bash, then go), each with eight tool lines and one answer.
  [ "$(grep -c '^bash ' "${FAKE_LO_LOG}")" -eq 9 ]
  [ "$(grep -c '^go ' "${FAKE_LO_LOG}")" -eq 9 ]
  local impl tool found
  while read -r impl tool found; do
    if [[ "${tool}" == docker--version ]]; then
      [[ "${found}" == "parity-stub v0.0.0" ]] \
        || { echo "${impl}: docker --version answered '${found}', not the probe stub"; return 1; }
    else
      [[ "${found}" == */probe/.bin/"${tool}" ]] \
        || { echo "${impl}: ${tool} resolves to '${found}', not to the probe stub"; return 1; }
    fi
  done <"${FAKE_LO_LOG}"
}

@test "prove_switch: a host tool call in the bash run fails the harness with exit 2 and names the call" {
  run_init FAKE_LO_CALL="bash docker ps"
  [ "${status}" -eq 2 ]
  [[ "${output}" == *"error: the probe called a host tool that it must not run (a parity stub refused it):"* ]]
  grep -qx '  docker ps' <<<"${output}"
  [[ "${output}" != *"probe passed"* ]]
  # The harness stopped after the bash run: the go run did not start.
  [ "$(grep -c '^go ' "${FAKE_LO_LOG}")" -eq 0 ]
}

@test "prove_switch: a host tool call in the go run fails the harness with exit 2 too" {
  run_init FAKE_LO_CALL="go docker ps"
  [ "${status}" -eq 2 ]
  grep -qx '  docker ps' <<<"${output}"
  [[ "${output}" != *"probe passed"* ]]
  [ "$(grep -c '^go ' "${FAKE_LO_LOG}")" -eq 9 ]
}

@test "prove_switch: the stub answers only the version query, so kubectl version without --client fails" {
  run_init FAKE_LO_CALL="bash kubectl version"
  [ "${status}" -eq 2 ]
  grep -qx '  kubectl version' <<<"${output}"
  [[ "${output}" != *"probe passed"* ]]
}

@test "probe_selfcheck: docker found past the probe stub exits 2 and names both paths" {
  run_lib 'parity::probe_bin "$1" "$2" && rm "$1/.bin/docker" && parity::probe_selfcheck "$1" && echo "selfcheck passed"'
  [ "${status}" -eq 2 ]
  [[ "${output}" == *"error: the probe PATH resolves docker to ${BATS_TEST_TMPDIR}/stub-bin/docker, not to its stub ${PROBE}/.bin/docker"* ]]
  [[ "${output}" != *"selfcheck passed"* ]]
}

@test "prove_switch: a probe bin without the docker stub stops with exit 2 before lo runs" {
  # The probe_stub of the library, wrapped: it writes no docker stub.
  run env -u PARITY_KEEP bash -c 'source "$1" &&
    eval "orig_$(declare -f parity::probe_stub)" &&
    parity::probe_stub() { [[ "$2" == docker ]] || orig_parity::probe_stub "$@"; } &&
    parity::init "$2" && echo "probe passed"' _ "${LIB}" "${BATS_TEST_TMPDIR}/lo"
  [ "${status}" -eq 2 ]
  [[ "${output}" == *"error: the probe PATH resolves docker to ${BATS_TEST_TMPDIR}/stub-bin/docker, not to its stub "*"/probe/.bin/docker"* ]]
  [[ "${output}" != *"probe passed"* ]]
  [ ! -e "${FAKE_LO_LOG}" ]
}

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
  # tool call in the run of that implementation. FAKE_LO_BREAK_LOG=1 puts a
  # directory in place of the stub log first, so a stub cannot write its
  # line. It prints a bash row when the project file selects bash, as the
  # frozen tree does.
  cat >"${BATS_TEST_TMPDIR}/lo" <<'SH'
#!/usr/bin/env bash
set -u
impl="$(sed -n 's/^    default: //p' lok8s.yaml)"
for tool in docker kubectl kind tilt helm hcloud ssh curl; do
  printf '%s %s %s\n' "${impl}" "${tool}" "$(type -P "${tool}")" >>"${FAKE_LO_LOG}"
done
printf '%s docker--version %s\n' "${impl}" "$(docker --version)" >>"${FAKE_LO_LOG}"
if [[ -n "${FAKE_LO_BREAK_LOG:-}" ]]; then
  rm -f ../probe-refused.log && mkdir -p ../probe-refused.log
fi
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
  # Under the WORK of run_case: parity::probe_case refuses a dir outside it.
  PROBE="${BATS_TEST_TMPDIR}/work/p"
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

# run_case <script>: the library in a child bash, with ROOT, WORK and the
# stub log set, and a project P whose .bin links the stand-in toolchain (as
# parity::new_project links the real one). Then <script>.
run_case() {
  run bash -c 'source "$1" && ROOT="$2" && WORK="$3" && PARITY_PROBE_LOG="$3/refused.log" &&
    mkdir -p "$3" "$4" && ln -s "$2/.bin" "$4/.bin" && P="$4" && '"${1}" \
    _ "${LIB}" "${ROOT_FAKE}" "${BATS_TEST_TMPDIR}/work" "${PROBE}"
}

# fixture_root <name> [tool...]: a stand-in checkout for parity::init,
# which finds ROOT two levels above PARITY_LIB_DIR: hack/lib, an empty
# .lok8s, and a .bin with yq and each <tool>. The test sets PARITY_LIB_DIR
# to <name>/hack/lib after it sources the library.
fixture_root() {
  local root="${BATS_TEST_TMPDIR}/${1}" tool
  shift
  mkdir -p "${root}/hack/lib" "${root}/.lok8s" "${root}/.bin"
  for tool in yq "$@"; do
    printf '#!/usr/bin/env bash\necho "toolchain %s"\n' "${tool}" >"${root}/.bin/${tool}"
    chmod +x "${root}/.bin/${tool}"
  done
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
  # kind get clusters (PARITY_PROBE_EMPTY): an empty list, exit 0, no log
  # line. Any other kind get is refused.
  run "${PROBE}/.bin/kind" get clusters
  [ "${status}" -eq 0 ]
  [ -z "${output}" ]
  run "${PROBE}/.bin/kind" get nodes
  [ "${status}" -eq 1 ]
  [ "$(cat "${LOG}")" = "$(printf 'kubectl version\nhelm version\ndocker ps\nkind get nodes')" ]
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
  [[ "${output}" == *"error: a parity stub refused a call (a host tool call, or a version query that the stub does not know):"* ]]
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
  run_lib 'parity::probe_bin "$1" "$2" && rm "$1/.bin/docker" && parity::probe_selfcheck "$1" "$2" && echo "selfcheck passed"'
  [ "${status}" -eq 2 ]
  [[ "${output}" == *"error: the probe PATH resolves docker to ${BATS_TEST_TMPDIR}/stub-bin/docker, not to its stub ${PROBE}/.bin/docker"* ]]
  [[ "${output}" != *"selfcheck passed"* ]]
}

@test "prove_switch: a probe bin without the docker stub stops with exit 2 before lo runs" {
  # The probe_stub of the library, wrapped: it writes no docker stub. Two
  # stand-in checkouts, so the result does not depend on the real .bin: in
  # with-docker the probe .bin keeps the link to the toolchain's docker, in
  # without-docker the PATH finds the host docker (here the net stub).
  fixture_root with-docker docker
  fixture_root without-docker
  local root want
  for root in with-docker without-docker; do
    run env -u PARITY_KEEP bash -c 'source "$1" && PARITY_LIB_DIR="$3/hack/lib" &&
      eval "orig_$(declare -f parity::probe_stub)" &&
      parity::probe_stub() { [[ "$2" == docker ]] || orig_parity::probe_stub "$@"; } &&
      parity::init "$2" && echo "probe passed"' _ "${LIB}" "${BATS_TEST_TMPDIR}/lo" "${BATS_TEST_TMPDIR}/${root}"
    [ "${status}" -eq 2 ] || { echo "${root}: rc ${status}: ${output}"; return 1; }
    # The part that is the same in both: the stub path that is not there.
    [[ "${output}" == *"error: the probe PATH resolves docker to "*", not to its stub "*"/probe/.bin/docker"* ]] \
      || { echo "${root}: ${output}"; return 1; }
    # Each checkout takes its own branch.
    want="${BATS_TEST_TMPDIR}/stub-bin/docker,"
    [[ "${root}" == without-docker ]] || want="/probe/.bin/docker -> ${BATS_TEST_TMPDIR}/with-docker/.bin/docker,"
    [[ "${output}" == *"${want}"* ]] || { echo "${root}: no '${want}' in: ${output}"; return 1; }
    [[ "${output}" != *"probe passed"* ]]
    [ ! -e "${FAKE_LO_LOG}" ]
  done
}

@test "prove_switch: a stub log that cannot be written stops with exit 2 before lo runs" {
  # probe_bin of the library, wrapped: the log becomes a link to /dev/null,
  # where each write succeeds and nothing stays.
  run env -u PARITY_KEEP bash -c 'source "$1" &&
    eval "orig_$(declare -f parity::probe_bin)" &&
    parity::probe_bin() { orig_parity::probe_bin "$@" && rm -f "$2" && ln -s /dev/null "$2"; } &&
    parity::init "$2" && echo "probe passed"' _ "${LIB}" "${BATS_TEST_TMPDIR}/lo"
  [ "${status}" -eq 2 ]
  [[ "${output}" == *"error: the probe cannot write its stub log "*"/probe-refused.log: a refused call would leave no line"* ]]
  [[ "${output}" != *"probe passed"* ]]
  [ ! -e "${FAKE_LO_LOG}" ]
}

@test "prove_switch: a refused call whose log line was lost still fails the harness with exit 2" {
  run_init FAKE_LO_BREAK_LOG=1 FAKE_LO_CALL="bash docker ps"
  [ "${status}" -eq 2 ]
  [[ "${output}" == *"error: the probe cannot write its stub log "*"/probe-refused.log: a refused call would leave no line"* ]]
  [[ "${output}" != *"probe passed"* ]]
  [ "$(grep -c '^go ' "${FAKE_LO_LOG}")" -eq 0 ]
}

@test "probe_case: the case runs with the probe stubs, then the project gets its own .bin and PATH back" {
  run_case 'count=0
    case_cmd() {
      echo "case: $(type -P docker) $(docker --version) $([[ -L "$P/.bin" ]] && echo link || echo dir)"
      count=$((count + 1))
    }
    parity::probe_case "$P" case_cmd
    echo "after: $(readlink "$P/.bin") $(type -P docker) count=${count}"'
  [ "${status}" -eq 0 ]
  grep -qx "case: ${PROBE}/.bin/docker parity-stub v0.0.0 dir" <<<"${output}"
  # The project link is back, PATH finds the host docker again (here the
  # net stub), and the case counted in this shell.
  grep -qx "after: ${ROOT_FAKE}/.bin ${BATS_TEST_TMPDIR}/stub-bin/docker count=1" <<<"${output}"
}

@test "probe_case: a refused call in the case fails the harness with exit 2 and names the call" {
  run_case 'case_cmd() { docker ps >/dev/null 2>&1 || true; echo "case ran"; }
    parity::probe_case "$P" case_cmd
    echo "after the case"'
  [ "${status}" -eq 2 ]
  grep -qx '  docker ps' <<<"${output}"
  [[ "${output}" == *"case ran"* ]]
  [[ "${output}" != *"after the case"* ]]
}

@test "probe_case: a probe bin without the docker stub stops with exit 2 before the case runs" {
  # The stand-in toolchain has a docker, so the probe .bin keeps the link
  # to it: the path is the stub's, but the file is not the stub.
  run_case 'eval "orig_$(declare -f parity::probe_stub)"
    parity::probe_stub() { [[ "$2" == docker ]] || orig_parity::probe_stub "$@"; }
    case_cmd() { echo "case ran"; }
    parity::probe_case "$P" case_cmd'
  [ "${status}" -eq 2 ]
  [[ "${output}" == *"error: the probe PATH resolves docker to ${PROBE}/.bin/docker -> ${ROOT_FAKE}/.bin/docker, not to its stub ${PROBE}/.bin/docker"* ]]
  [[ "${output}" != *"case ran"* ]]
}

@test "probe_case: a case that lists the kind clusters gets an empty list, and no stub refuses" {
  run_case 'case_cmd() { local out rc=0; out="$(kind get clusters)" || rc=$?; echo "case: rc=${rc} out=[${out}]"; }
    parity::probe_case "$P" case_cmd
    echo "after the case"'
  [ "${status}" -eq 0 ]
  grep -qx 'case: rc=0 out=\[\]' <<<"${output}"
  [[ "${output}" == *"after the case"* ]]
}

@test "probe_case: a dir that is not under the work dir stops with exit 2 before any change" {
  local work="${BATS_TEST_TMPDIR}/work" outside="${BATS_TEST_TMPDIR}/outside" dir
  for dir in "${outside}" "${work}/../outside" "${work}" "${BATS_TEST_TMPDIR}/absent"; do
    rm -rf "${work}" "${outside}"
    mkdir -p "${outside}/.bin"
    : >"${outside}/.bin/marker"
    export CASE_DIR="${dir}"
    run_case 'case_cmd() { echo "case ran"; }
      parity::probe_case "${CASE_DIR}" case_cmd
      echo "after the case"'
    [ "${status}" -eq 2 ] || { echo "${dir}: rc ${status}: ${output}"; return 1; }
    [[ "${output}" == *"error: parity::probe_case: ${dir} is not a directory under the work dir ${work}"* ]] \
      || { echo "${dir}: ${output}"; return 1; }
    [[ "${output}" != *"case ran"* ]]
    # The .bin outside is the one that was there: no probe stubs, no move.
    [ -f "${outside}/.bin/marker" ]
    [ ! -e "${outside}/.bin/docker" ]
    [ ! -e "${work}/probe-case.bin" ]
  done
}

@test "parity-leaves.sh: each lo drivers lo status case with a domain runs with the probe stubs" {
  # kind get clusters calls docker: the case must get the stub kind.
  local lines line
  lines="$(grep -E '^[^#]*run_pair .* drivers lo (status|s) [^[:space:]#]+[[:space:]]*(#.*)?$' "${_PROJECT_ROOT}/hack/parity-leaves.sh")"
  [ -n "${lines}" ]
  while IFS= read -r line; do
    [[ "${line}" == 'parity::probe_case "${AD}" run_pair '* ]] \
      || { echo "not with the probe stubs: ${line}"; return 1; }
  done <<<"${lines}"
}

@test "PARITY_PROBE_TOOLS: each host tool of the two version lists has a stub that answers its version argv" {
  # The tools that lo version runs from the project's own toolchain (the
  # links of parity::own_bin). They run for real and have no stub.
  local toolchain=" argsh yq kustomize "
  run bash -c 'source "$1" && printf "%s\n" "${PARITY_PROBE_TOOLS[@]}"' _ "${LIB}"
  [ "${status}" -eq 0 ]
  local probe="${output}" go_list bash_list bash_names
  # Go: the versionTools entries {"<tool>", []string{"<arg>", ...}, ...}.
  go_list="$(sed -n '/^var versionTools = /,/^}$/p' "${_PROJECT_ROOT}/internal/cli/cmd_version.go" \
    | sed -n 's/^[[:space:]]*{"\([A-Za-z0-9_-]*\)", \[\]string{\([^}]*\)}.*/\1=\2/p' | sed 's/", "/ /g; s/"//g')"
  # bash: the case lines of version::_of, <tool>) out="$(<tool> <argv> 2>/dev/null ...,
  # and the tool list of the for loop in main::version.
  bash_list="$(sed -n '/^version::_of() {/,/^}/p' "${_PROJECT_ROOT}/.lok8s/libs/version" \
    | sed -n 's/^[[:space:]]*\([A-Za-z0-9_-]*\))[[:space:]]*out="[$](\1 \(.*\) 2>\/dev\/null.*/\1=\2/p')"
  bash_names="$(sed -n 's/^[[:space:]]*for tool in \(.*\); do$/\1/p' "${_PROJECT_ROOT}/.lok8s/libs/version")"
  # Each read found the docker entry: the patterns still fit the sources.
  grep -qx 'docker=--version' <<<"${go_list}"
  grep -qx 'docker=--version' <<<"${bash_list}"
  [[ " ${bash_names} " == *" docker "* ]]
  local entry tool missing=""
  while IFS= read -r entry; do
    tool="${entry%%=*}"
    if [[ "${toolchain}" == *" ${tool} "* ]]; then continue; fi
    grep -qxF -- "${entry}" <<<"${probe}" || missing+=" '${entry}'"
  done <<<"${go_list}"$'\n'"${bash_list}"
  for tool in ${bash_names}; do
    if [[ "${toolchain}" == *" ${tool} "* ]]; then continue; fi
    grep -qE "^${tool}(=|\$)" <<<"${probe}" || missing+=" '${tool}'"
  done
  [ -z "${missing}" ] || { echo "not in PARITY_PROBE_TOOLS (hack/lib/parity.sh):${missing}"; return 1; }
}

#!/usr/bin/env bats
# kubeone_merge_tmpdir_test.bats — the merges into the rendered KubeOne
# manifest (drivers/kubeone/config, kubeone::_yq_edit) leave no copy of the
# manifest behind, and a trace of the registry merge shows no credential.
#
# `yq -i` writes through a temp file in TMPDIR and keeps it when its last
# write fails: a copy of the manifest, and after the registry merge that
# copy holds the registry credentials. Each failed-write case points TMPDIR
# at a fresh directory and requires it to be empty after the call, and the
# directory of the manifest to hold only the expected files. The control
# case pins the premise: under the same setup, a raw `yq -i` leaves its
# copy. The audit-log merge has the same case in kubeone_audit_log_test.bats.
#
# The credentials are fixtures. No case prints the content of a file or a
# trace: a leftover is shown by its name and mode only, and a credential
# check prints no value. Uses the real yq provided by `argsh test`.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
  # A read-only file in a read-only directory makes the write fail. Root
  # writes it anyway.
  [[ "$(id -u)" != 0 ]] || skip "root writes a read-only file"

  provider::detect() { echo "hetzner"; }
  export -f provider::detect
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"

  TMPD="${BATS_TEST_TMPDIR}/tmpdir"
  RO="${BATS_TEST_TMPDIR}/ro"
  RW="${BATS_TEST_TMPDIR}/rw"
  M="${RO}/m.yaml"
  mkdir -p "${TMPD}" "${RO}" "${RW}"
  printf 'features: {}\n' > "${M}"
  printf 'features: {}\n' > "${RW}/m.yaml"

  export LOK8S_SPEC_OIDC_ISSUER="https://id.example.com"
  export LOK8S_SPEC_OIDC_CLIENTID="kubectl"
  export LOK8S_SPEC_OIDC_USERNAMECLAIM="sub"
  export LOK8S_SPEC_OIDC_USERNAMEPREFIX="oidc:"
  export LOK8S_SPEC_OIDC_GROUPSCLAIM="groups"
  export LOK8S_SPEC_OIDC_GROUPSPREFIX="oidc:"
}

teardown() {
  # A failed case can stop before it gives the write access back.
  [[ ! -d "${RO:-}" ]] || chmod -R u+w "${RO}"
  teardown_tmpdir
}

# _read_only: the manifest and its directory can no longer be written.
_read_only() {
  chmod 400 "${M}"
  chmod 500 "${RO}"
}

# _writable: give the write access back (before the assertions, so that
# teardown can remove the directory).
_writable() {
  chmod 700 "${RO}"
  chmod 600 "${M}"
}

# _assert_tmpdir_empty: nothing is left in the fresh TMPDIR of the case.
_assert_tmpdir_empty() {
  local left
  left=$(find "${TMPD}" -mindepth 1 -printf '%P (mode %m)\n')
  [ -z "${left}" ] || { echo "left in TMPDIR: ${left}" >&2; return 1; }
}

# _assert_only <dir> <entry>...: the directory holds exactly these entries
# (paths relative to it). Only the names are shown.
_assert_only() {
  local dir="${1}" want got
  shift
  want=$(printf '%s\n' "$@" | sort)
  got=$(find "${dir}" -mindepth 1 -printf '%P\n' | sort)
  [ "${got}" = "${want}" ] || { echo "entries of ${dir}: ${got//$'\n'/ } (want: ${want//$'\n'/ })" >&2; return 1; }
}

# _fake_yq <dir> <script body>: a yq on PATH in front of the real one.
_fake_yq() {
  local dir="${1}" real; real=$(command -v yq)
  mkdir -p "${dir}"
  printf '#!/usr/bin/env bash\nREAL_YQ=%q\n%s\n' "${real}" "${2}" > "${dir}/yq"
  chmod +x "${dir}/yq"
}

# _registry_fixture <n>: a per-domain store with the fixture credentials
# fixture-user-<i> and fixture-password-<i> for the hosts
# registry<i>.example.com, and the cluster spec CY that names them.
_registry_fixture() {
  local n="${1}" i store="${PATH_CLUSTERS}/ko.cloud/secrets"
  export DOMAIN_NAME="ko.cloud"
  CY="${PATH_CLUSTERS}/ko.cloud/cluster.lok8s.yaml"
  mkdir -p "${store}"
  printf '%s\n' 'apiVersion: cluster.lok8s.dev/v1beta1' 'kind: KubeOne' 'metadata:' '  name: ko-test' \
    'spec:' '  registries:' > "${CY}"
  for (( i = 1; i <= n; i++ )); do
    printf 'fixture-user-%s' "${i}" > "${store}/Secret.fixture-pull-${i}.provisioning.username"
    printf 'fixture-password-%s' "${i}" > "${store}/Secret.fixture-pull-${i}.provisioning.password"
    printf '%s\n' "    registry${i}.example.com:" '      auth:' '        secretRef:' "          name: fixture-pull-${i}" >> "${CY}"
  done
}

# _assert_credentials_written <manifest> <n>: the merge wrote the fixture
# credentials of the n hosts. No value is printed.
_assert_credentials_written() {
  local m="${1}" n="${2}" i base
  for (( i = 1; i <= n; i++ )); do
    base=".containerRuntime.containerd.registries.\"registry${i}.example.com\".auth"
    [[ "$(yq -r "${base}.username" "${m}")" == "fixture-user-${i}" ]] \
      || { echo "the merge did not write the username of registry${i}" >&2; return 1; }
    [[ "$(yq -r "${base}.password" "${m}")" == "fixture-password-${i}" ]] \
      || { echo "the merge did not write the password of registry${i}" >&2; return 1; }
  done
}

# _assert_no_credential_in_output: neither fixture credential is in the
# output (stdout and stderr) of the run. The output is not printed.
_assert_no_credential_in_output() {
  [[ "${output}" != *fixture-user* ]] || { echo "a fixture username is in the output" >&2; return 1; }
  [[ "${output}" != *fixture-password* ]] || { echo "a fixture password is in the output" >&2; return 1; }
}

@test "control: under the same setup, a raw yq -i leaves its copy in TMPDIR" {
  # The premise of the failed-write cases: the yq on PATH keeps its temp
  # file in TMPDIR when the write of a read-only manifest in a read-only
  # directory fails. Without it, an empty TMPDIR would prove nothing.
  _read_only
  TMPDIR="${TMPD}" run yq -i '.features.x = 1' "${M}"
  _writable
  [ "${status}" -ne 0 ] || { echo "yq -i did not fail" >&2; return 1; }
  local -a left=()
  mapfile -t left < <(find "${TMPD}" -mindepth 1 -printf '%P\n')
  [ "${#left[@]}" -eq 1 ] || { echo "want 1 file in TMPDIR, got ${#left[@]}: ${left[*]}" >&2; return 1; }
  [[ "${left[0]}" =~ ^temp[0-9]+$ ]] || { echo "unexpected name in TMPDIR: ${left[0]}" >&2; return 1; }
}

@test "a failed write of features.openidConnect leaves TMPDIR as it was" {
  _read_only
  TMPDIR="${TMPD}" run kubeone::_inject_oidc "${M}"
  _writable
  assert_failure
  assert_output --partial "OIDC: failed to inject features.openidConnect"
  _assert_tmpdir_empty
  _assert_only "${RO}" m.yaml
}

@test "a failed write of the OIDC caFile leaves TMPDIR as it was" {
  export LOK8S_SPEC_OIDC_CABUNDLE="fixture-ca"
  # On a manifest that can be written, the merge sets caFile to the CA file
  # beside the manifest, and that file holds the bundle.
  TMPDIR="${TMPD}" run kubeone::_inject_oidc "${RW}/m.yaml"
  assert_success
  run yq -r '.features.openidConnect.config.caFile' "${RW}/m.yaml"
  assert_output "${RW}/.oidc/oidc-ca.pem"
  [ "$(cat "${RW}/.oidc/oidc-ca.pem")" = "fixture-ca" ] || { echo "the CA file does not hold the bundle" >&2; return 1; }
  _assert_only "${RW}" m.yaml .oidc .oidc/oidc-ca.pem
  _assert_tmpdir_empty

  # The features write succeeds. This yq makes the manifest and its
  # directory read-only just before the caFile write, so only that write
  # fails. The manifest is the last argument.
  _fake_yq "${BATS_TEST_TMPDIR}/fakeyq" 'for a in "$@"; do f="${a}"; done
if [[ "$*" == *caFile* ]]; then chmod 400 "${f}"; chmod 500 "${f%/*}"; fi
exec "${REAL_YQ}" "$@"'
  local saved_path="${PATH}"
  PATH="${BATS_TEST_TMPDIR}/fakeyq:${PATH}"
  TMPDIR="${TMPD}" run kubeone::_inject_oidc "${M}"
  PATH="${saved_path}"
  _writable
  assert_failure
  assert_output --partial "OIDC: failed to set caFile"
  refute_output --partial "OIDC: failed to inject features.openidConnect"
  _assert_tmpdir_empty
  _assert_only "${RO}" m.yaml .oidc .oidc/oidc-ca.pem
}

@test "a failed write of the registry credentials leaves TMPDIR as it was" {
  _registry_fixture 1
  # Anti-vacuity: on a manifest that can be written, the same merge writes
  # the fixture credentials.
  TMPDIR="${TMPD}" run kubeone::_inject_registry_auth "${RW}/m.yaml" "${CY}"
  [ "${status}" -eq 0 ] || { echo "the merge failed: status ${status}" >&2; return 1; }
  _assert_credentials_written "${RW}/m.yaml" 1
  _assert_only "${RW}" m.yaml
  _assert_tmpdir_empty

  _read_only
  TMPDIR="${TMPD}" run kubeone::_inject_registry_auth "${M}" "${CY}"
  _writable
  assert_failure
  assert_output --partial "registry auth: failed to inject 'registry1.example.com'"
  _assert_tmpdir_empty
  _assert_only "${RO}" m.yaml
}

# The trace cases run the merge in a child bash that sources the driver,
# then turns tracing on (utils/keysonly.sh: the two guard lines).

@test "a registry merge under set -x shows neither credential in its trace" {
  _registry_fixture 2
  run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
set -x
kubeone::_inject_registry_auth "$1" "$2"' bash "${RW}/m.yaml" "${CY}" "${_PROJECT_ROOT}/.lok8s"
  [ "${status}" -eq 0 ] || { echo "the merge failed: status ${status}" >&2; return 1; }
  # Anti-vacuity: the trace ran, and the merge wrote the values.
  [[ "${output}" == *kubeone::_inject_registry_auth* ]] || { echo "no trace of the call" >&2; return 1; }
  _assert_credentials_written "${RW}/m.yaml" 2
  _assert_no_credential_in_output
}

@test "a registry merge under set -T with a RETURN trap that turns tracing on shows neither credential" {
  # Two registries: the RETURN trap of a function that the merge calls for
  # the first one (debug, kubeone::_yq_edit) would turn tracing on again
  # before the merge reads the second one. The first guard line clears the
  # trap.
  _registry_fixture 2
  run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
set -x; set -T; trap "set -x" RETURN
kubeone::_inject_registry_auth "$1" "$2"' bash "${RW}/m.yaml" "${CY}" "${_PROJECT_ROOT}/.lok8s"
  [ "${status}" -eq 0 ] || { echo "the merge failed: status ${status}" >&2; return 1; }
  [[ "${output}" == *kubeone::_inject_registry_auth* ]] || { echo "no trace of the call" >&2; return 1; }
  _assert_credentials_written "${RW}/m.yaml" 2
  _assert_no_credential_in_output
}

@test "kubeone::_yq_edit under set -T with a DEBUG trap that turns tracing on shows no credential of the manifest" {
  # The manifest already holds the credentials, as after the registry
  # merge. A DEBUG trap runs before each command of the function: without
  # the first guard line, it would trace the captured manifest.
  printf '%s\n' 'containerRuntime:' '  containerd:' '    registries:' '      registry1.example.com:' \
    '        auth:' '          username: fixture-user-1' '          password: fixture-password-1' > "${RW}/m.yaml"
  run bash -c 'import() { :; }
source "$2/utils/verbose.sh"; source "$2/drivers/kubeone/config"
set -x; set -T; trap "set -x" DEBUG
kubeone::_yq_edit "$1" ".features.x = 1"' bash "${RW}/m.yaml" "${_PROJECT_ROOT}/.lok8s"
  [ "${status}" -eq 0 ] || { echo "the edit failed: status ${status}" >&2; return 1; }
  [[ "${output}" == *kubeone::_yq_edit* ]] || { echo "no trace of the call" >&2; return 1; }
  [[ "$(yq -r '.features.x' "${RW}/m.yaml")" == 1 ]] || { echo "the edit was not written" >&2; return 1; }
  _assert_credentials_written "${RW}/m.yaml" 1
  _assert_no_credential_in_output
}

@test "during the registry merge only yq gets RUSER and RPASS, not the rm of a signal trap" {
  # The render's signal trap (kubeone::_temp_traps) runs rm. A yq stub sends
  # TERM to the shell of the merge while it runs the registry write, so the
  # trap fires during the call. Both stubs log only whether RUSER and RPASS
  # are in their environment, never a value.
  _registry_fixture 1
  local stubs="${BATS_TEST_TMPDIR}/stubs" work="${RW}/.kubeone.yaml.work"
  export ENV_LOG="${BATS_TEST_TMPDIR}/env.log"
  REAL_YQ=$(command -v yq)
  REAL_RM=$(command -v rm)
  [[ -n "${REAL_YQ}" && -n "${REAL_RM}" ]] || { echo "no yq or rm" >&2; return 1; }
  export REAL_YQ REAL_RM
  cp "${RW}/m.yaml" "${work}"
  mkdir -p "${stubs}"
  cat > "${stubs}/rm" <<'SH'
#!/usr/bin/env bash
[[ -n "${REAL_RM:-}" ]] || exit 3
u=absent; p=absent
[[ -z "${RUSER+x}" ]] || u=present
[[ -z "${RPASS+x}" ]] || p=present
printf 'rm: RUSER %s, RPASS %s\n' "${u}" "${p}" >> "${ENV_LOG}"
exec "${REAL_RM}" "$@"
SH
  cat > "${stubs}/yq" <<'SH'
#!/usr/bin/env bash
[[ -n "${REAL_YQ:-}" ]] || exit 3
if [[ "$*" == *'strenv(RUSER)'* ]]; then
  u=absent; p=absent
  [[ -z "${RUSER+x}" ]] || u=present
  [[ -z "${RPASS+x}" ]] || p=present
  printf 'yq: RUSER %s, RPASS %s\n' "${u}" "${p}" >> "${ENV_LOG}"
  kill -TERM "${MERGE_PID}"
fi
exec "${REAL_YQ}" "$@"
SH
  chmod +x "${stubs}/rm" "${stubs}/yq"
  local saved_path="${PATH}"
  PATH="${stubs}:${PATH}"
  run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
export MERGE_PID=$$
kubeone::_temp_traps "$1"
kubeone::_inject_registry_auth "$1" "$2"
echo "the merge returned $?"' bash "${work}" "${CY}" "${_PROJECT_ROOT}/.lok8s"
  PATH="${saved_path}"
  # The trap ran (rm, then the TERM ends the shell) during the call.
  [ "${status}" -eq 143 ] || { echo "status ${status}, want 143 (TERM)" >&2; return 1; }
  refute_output --partial "the merge returned"
  [ ! -e "${work}" ] || { echo "the trap did not remove the temp manifest" >&2; return 1; }
  _assert_no_credential_in_output
  run cat "${ENV_LOG}"
  assert_output "yq: RUSER present, RPASS present
rm: RUSER absent, RPASS absent"
}

@test "kubeone::_yq_edit keeps a block scalar that ends in blank lines byte for byte" {
  # A keep (|+) block scalar keeps its final blank lines as part of its
  # value. A plain $(...) capture would drop the final newlines of the file.
  # The edit changes another key, so the case also shows the write. The
  # reference is the output of yq -i, the form that the helper replaces; it
  # is the input with only that key changed (yq does not normalise this
  # input), and the case checks that too.
  printf 'a: |+\n  line one\n\n\nb: 1\nc: |+\n  last\n\n\n' > "${RW}/m.yaml"
  printf 'a: |+\n  line one\n\n\nb: 2\nc: |+\n  last\n\n\n' > "${RW}/want.yaml"
  cp "${RW}/m.yaml" "${RW}/ref.yaml"
  TMPDIR="${TMPD}" yq -i '.b = 2' "${RW}/ref.yaml"
  cmp "${RW}/ref.yaml" "${RW}/want.yaml" || { echo "yq -i changed more than b" >&2; return 1; }
  TMPDIR="${TMPD}" run kubeone::_yq_edit "${RW}/m.yaml" '.b = 2'
  assert_success
  cmp "${RW}/m.yaml" "${RW}/ref.yaml"
  _assert_tmpdir_empty
}

# _yq_log_stub: a yq on PATH (YQ_STUBS) in front of the real one that logs
# each run to YQ_LOG, then runs the real yq.
_yq_log_stub() {
  YQ_STUBS="${BATS_TEST_TMPDIR}/yqlog"
  export YQ_LOG="${BATS_TEST_TMPDIR}/yq.log"
  REAL_YQ=$(command -v yq)
  [[ -n "${REAL_YQ}" ]] || { echo "no yq" >&2; return 1; }
  export REAL_YQ
  mkdir -p "${YQ_STUBS}"
  cat > "${YQ_STUBS}/yq" <<'SH'
#!/usr/bin/env bash
[[ -n "${REAL_YQ:-}" ]] || exit 3
echo "yq ran" >> "${YQ_LOG}"
exec "${REAL_YQ}" "$@"
SH
  chmod +x "${YQ_STUBS}/yq"
}

# _assert_name_refused <name>: kubeone::_yq_edit with <name> after the
# expression returns 1 with its message, runs no yq and leaves the file as
# it was.
_assert_name_refused() {
  local saved_path="${PATH}"
  : > "${YQ_LOG}"
  cp "${RW}/m.yaml" "${BATS_TEST_TMPDIR}/m.before"
  PATH="${YQ_STUBS}:${saved_path}"
  run kubeone::_yq_edit "${RW}/m.yaml" '.x = 1' "${1}"
  PATH="${saved_path}"
  [ "${status}" -eq 1 ] || { echo "name '${1}': status ${status}, want 1" >&2; return 1; }
  assert_output --partial "kubeone::_yq_edit: an argument after the expression is not a variable name"
  [ ! -s "${YQ_LOG}" ] || { echo "name '${1}': yq ran" >&2; return 1; }
  cmp "${RW}/m.yaml" "${BATS_TEST_TMPDIR}/m.before" || { echo "name '${1}': the file changed" >&2; return 1; }
}

@test "kubeone::_yq_edit refuses an argument that is not a variable name, and runs no yq" {
  _yq_log_stub
  _assert_name_refused 'A-B'
  _assert_name_refused '1X'
  _assert_name_refused ''
  # Anti-vacuity: a variable name of the caller, not exported, reaches yq.
  local FIXTURE_X=7
  : > "${YQ_LOG}"
  local saved_path="${PATH}"
  PATH="${YQ_STUBS}:${saved_path}"
  run kubeone::_yq_edit "${RW}/m.yaml" '.x = strenv(FIXTURE_X)' FIXTURE_X
  PATH="${saved_path}"
  assert_success
  [ -s "${YQ_LOG}" ] || { echo "the stub logged no yq run" >&2; return 1; }
  run yq -r '.x' "${RW}/m.yaml"
  assert_output "7"
}

@test "kubeone::_yq_edit refuses a name of its own (_ye_), and runs no yq" {
  # Its own variables start with _ye_: such a name would export the helper's
  # variable instead of the caller's.
  _yq_log_stub
  _assert_name_refused '_ye_file'
}

@test "kubeone::_yq_replace gives the caller's INT, TERM and HUP traps back, on success and on failure" {
  run bash -c 'import() { :; }
source "$2/utils/verbose.sh"; source "$2/drivers/kubeone/config"
trap "echo caller-int" INT; trap "echo caller-term" TERM; trap "echo caller-hup" HUP
before=$(trap -p INT TERM HUP)
[[ -n "${before}" ]] || echo "no caller traps"
kubeone::_yq_replace "$1" ".x = 1"; echo "success rc=$?"
if [[ "$(trap -p INT TERM HUP)" == "${before}" ]]; then echo "traps after success: same"; else echo "traps after success: changed"; fi
kubeone::_yq_replace "$1" ".x = ("; echo "failure rc=$?"
if [[ "$(trap -p INT TERM HUP)" == "${before}" ]]; then echo "traps after failure: same"; else echo "traps after failure: changed"; fi' \
    bash "${RW}/m.yaml" "${_PROJECT_ROOT}/.lok8s"
  assert_success
  refute_output --partial "no caller traps"
  assert_output --partial "success rc=0"
  assert_output --partial "traps after success: same"
  assert_output --partial "failure rc=1"
  assert_output --partial "traps after failure: same"
  # The success wrote the edit, the failure left the file, and no temp file
  # is left beside it.
  run yq -r '.x' "${RW}/m.yaml"
  assert_output "1"
  _assert_only "${RW}" m.yaml
}

@test "kubeone::_yq_edit refuses a name of the trace guard (_ko_) and a nameref, and runs no yq" {
  _yq_log_stub
  # The trace guard declares _ko_ locals: such a name would export the
  # guard's variable instead of the caller's.
  _assert_name_refused '_ko_rc'
  # A nameref passes the name pattern, but the export follows it to the
  # variable that it names, here one of the helper's own.
  local -n fixture_ref=_ye_file
  _assert_name_refused 'fixture_ref'
  # An unset nameref (no target yet) is refused as well; `[[ -R name ]]`
  # is false for it.
  local -n fixture_unset_ref
  _assert_name_refused 'fixture_unset_ref'
  # Anti-vacuity of the anchors: a plain variable whose name is the start
  # of a nameref's name is not a nameref. It passes and reaches yq.
  local FIXTURE_P=8
  local -n FIXTURE_P_ref=FIXTURE_P
  : > "${YQ_LOG}"
  local saved_path="${PATH}"
  PATH="${YQ_STUBS}:${saved_path}"
  run kubeone::_yq_edit "${RW}/m.yaml" '.x = strenv(FIXTURE_P)' FIXTURE_P
  PATH="${saved_path}"
  assert_success
  [ -s "${YQ_LOG}" ] || { echo "the stub logged no yq run" >&2; return 1; }
  run yq -r '.x' "${RW}/m.yaml"
  assert_output "8"
}

# _count_stubs <dir> <yq pattern>: stubs of cat, yq and rm in <dir>. Each
# logs to ENV_LOG how many lines of its environment hold a fixture
# credential and how many hold a placeholder (RUSER, RPASS, username,
# password or _ye_out set to "placeholder"). No value is logged. The cat
# stub logs only the reads of a .username or .password file. When its
# arguments hold <yq pattern>, the yq stub logs and sends TERM to MERGE_PID,
# so the trap of the shell runs rm during the call.
_count_stubs() {
  local dir="${1}"
  export ENV_LOG="${BATS_TEST_TMPDIR}/env.log" YQ_PATTERN="${2}"
  REAL_YQ=$(command -v yq)
  REAL_RM=$(command -v rm)
  REAL_CAT=$(command -v cat)
  [[ -n "${REAL_YQ}" && -n "${REAL_RM}" && -n "${REAL_CAT}" ]] || { echo "no yq, rm or cat" >&2; return 1; }
  export REAL_YQ REAL_RM REAL_CAT COUNT_SH="${dir}/count.sh"
  mkdir -p "${dir}"
  cat > "${dir}/count.sh" <<'SH'
_count() {
  local f p
  f=$(/usr/bin/env | /usr/bin/grep -cE 'fixture-(user|password)-')
  p=$(/usr/bin/env | /usr/bin/grep -cE '^(RUSER|RPASS|username|password|_ye_out)=placeholder$')
  printf '%s: fixture lines %s, placeholders %s\n' "${1}" "${f}" "${p}" >> "${ENV_LOG}"
}
SH
  cat > "${dir}/cat" <<'SH'
#!/usr/bin/env bash
[[ -n "${REAL_CAT:-}" ]] || exit 3
source "${COUNT_SH}"
case "${1:-}" in
  *.username) _count "cat username" ;;
  *.password) _count "cat password" ;;
esac
exec "${REAL_CAT}" "$@"
SH
  cat > "${dir}/yq" <<'SH'
#!/usr/bin/env bash
[[ -n "${REAL_YQ:-}" ]] || exit 3
if [[ "$*" == *"${YQ_PATTERN}"* ]]; then
  source "${COUNT_SH}"
  _count yq
  kill -TERM "${MERGE_PID}"
fi
exec "${REAL_YQ}" "$@"
SH
  cat > "${dir}/rm" <<'SH'
#!/usr/bin/env bash
[[ -n "${REAL_RM:-}" ]] || exit 3
source "${COUNT_SH}"
_count rm
exec "${REAL_RM}" "$@"
SH
  chmod +x "${dir}/cat" "${dir}/yq" "${dir}/rm"
}

@test "the registry merge gives no credential to the cat of the password or to the rm of a signal trap, also with exported names and set -a" {
  # A plain `local` keeps the export attribute of a variable of the caller,
  # and under set -a each assignment exports. The caller here exports
  # placeholders for RUSER, RPASS, username, password and _ye_out, then
  # turns set -a on. A TERM arrives at the registry write (see _count_stubs).
  _registry_fixture 1
  local stubs="${BATS_TEST_TMPDIR}/stubs" work="${RW}/.kubeone.yaml.work"
  _count_stubs "${stubs}" 'strenv(RUSER)'
  cp "${RW}/m.yaml" "${work}"
  local saved_path="${PATH}"
  PATH="${stubs}:${PATH}"
  run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
export RUSER=placeholder RPASS=placeholder username=placeholder password=placeholder _ye_out=placeholder
export MERGE_PID=$$
set -a
kubeone::_temp_traps "$1"
kubeone::_inject_registry_auth "$1" "$2"
echo "the merge returned $?"' bash "${work}" "${CY}" "${_PROJECT_ROOT}/.lok8s"
  PATH="${saved_path}"
  [ "${status}" -eq 143 ] || { echo "status ${status}, want 143 (TERM)" >&2; return 1; }
  refute_output --partial "the merge returned"
  [ ! -e "${work}" ] || { echo "the trap did not remove the temp manifest" >&2; return 1; }
  _assert_no_credential_in_output
  # yq has the two credentials (RUSER, RPASS); every other child sees only
  # the placeholders of the caller.
  run cat "${ENV_LOG}"
  assert_output "cat username: fixture lines 0, placeholders 5
cat password: fixture lines 0, placeholders 5
yq: fixture lines 2, placeholders 3
rm: fixture lines 0, placeholders 5"
}

@test "kubeone::_yq_edit gives the manifest that it holds to no child of a signal trap, also with _ye_out exported and set -a" {
  # The manifest holds the fixture credentials. The caller exports a
  # placeholder _ye_out and turns set -a on. The TERM from the yq stub runs
  # the trap after the capture of yq's output, while _ye_out holds the
  # edited manifest.
  local stubs="${BATS_TEST_TMPDIR}/stubs" work="${RW}/.kubeone.yaml.work"
  _count_stubs "${stubs}" '.x = 1'
  printf 'auth:\n  username: fixture-user-1\n  password: fixture-password-1\n' > "${work}"
  local saved_path="${PATH}"
  PATH="${stubs}:${PATH}"
  run bash -c 'import() { :; }
source "$2/utils/verbose.sh"; source "$2/drivers/kubeone/config"
export _ye_out=placeholder
export MERGE_PID=$$
set -a
kubeone::_temp_traps "$1"
kubeone::_yq_edit "$1" ".x = 1"
echo "the edit returned $?"' bash "${work}" "${_PROJECT_ROOT}/.lok8s"
  PATH="${saved_path}"
  [ "${status}" -eq 143 ] || { echo "status ${status}, want 143 (TERM)" >&2; return 1; }
  refute_output --partial "the edit returned"
  [ ! -e "${work}" ] || { echo "the trap did not remove the temp manifest" >&2; return 1; }
  _assert_no_credential_in_output
  run cat "${ENV_LOG}"
  assert_output "yq: fixture lines 0, placeholders 1
rm: fixture lines 0, placeholders 1"
}

@test "kubeone::_yq_edit returns non-zero when yq succeeds but the write back fails" {
  # This yq runs the real one, then makes the file read-only, so the write
  # of the result back into the file fails (setup skips root, which writes
  # a read-only file). It records the exit status of the real yq.
  export YQ_RC="${BATS_TEST_TMPDIR}/yq.rc"
  cp "${RW}/m.yaml" "${BATS_TEST_TMPDIR}/m.before"
  _fake_yq "${BATS_TEST_TMPDIR}/wbfail" '"${REAL_YQ}" "$@"; rc=$?
for F in "$@"; do :; done
chmod 400 "${F}"
printf "%s\n" "${rc}" > "${YQ_RC}"
exit "${rc}"'
  local saved_path="${PATH}" st
  PATH="${BATS_TEST_TMPDIR}/wbfail:${saved_path}"
  run kubeone::_yq_edit "${RW}/m.yaml" '.x = 1'
  PATH="${saved_path}"
  st="${status}"
  chmod 600 "${RW}/m.yaml"
  [ "${st}" -ne 0 ] || { echo "status 0 after a failed write" >&2; return 1; }
  run cat "${YQ_RC}"
  assert_output "0"
  cmp "${RW}/m.yaml" "${BATS_TEST_TMPDIR}/m.before" || { echo "the file changed" >&2; return 1; }
}

@test "kubeone::_yq_edit checks for a nameref without reading a value: no declare call names an argument" {
  # A child shell wraps the declare builtin in a function that logs its
  # arguments (flags and names only), then calls kubeone::_yq_edit with the
  # name of a variable that holds a fixture credential. The nameref check
  # lists the namerefs (declare -pn, no names); it does not run
  # `declare -p <name>`, which would print the value.
  export DECL_LOG="${BATS_TEST_TMPDIR}/declare.log"
  run bash -c 'import() { :; }
source "$2/utils/verbose.sh"; source "$2/drivers/kubeone/config"
declare() { printf "%s\n" "$*" >> "${DECL_LOG}"; builtin declare "$@"; }
f() { local +x FIXTURE_PW=fixture-password-1; kubeone::_yq_edit "$1" ".auth.password = strenv(FIXTURE_PW)" FIXTURE_PW; }
f "$1"
echo "the edit returned $?"' bash "${RW}/m.yaml" "${_PROJECT_ROOT}/.lok8s"
  assert_success
  assert_output "the edit returned 0"
  run yq -r '.auth.password == "fixture-password-1"' "${RW}/m.yaml"
  assert_output "true"
  # The check ran, through the wrapper, and no call named the argument.
  run cat "${DECL_LOG}"
  assert_output "-pn"
}

@test "the registry merge stops when the caller has a readonly nameref named username, and writes nothing through it" {
  # `local +x username` fails over a readonly nameref of the caller. An
  # assignment after that would write the username through the reference
  # into the exported variable that it names (TARGET), so the cat of the
  # password would get it. The stubs of _count_stubs log each cat of a
  # credential file and the yq of the registry write.
  _registry_fixture 1
  local stubs="${BATS_TEST_TMPDIR}/stubs" work="${RW}/.kubeone.yaml.work"
  _count_stubs "${stubs}" 'strenv(RUSER)'
  cp "${RW}/m.yaml" "${work}"
  local saved_path="${PATH}"
  PATH="${stubs}:${PATH}"
  run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
export TARGET=placeholder
declare -nr username=TARGET
kubeone::_inject_registry_auth "$1" "$2"
echo "the merge returned $?"
if [[ "${TARGET}" == placeholder ]]; then echo "TARGET kept"; else echo "TARGET changed"; fi' bash "${work}" "${CY}" "${_PROJECT_ROOT}/.lok8s"
  PATH="${saved_path}"
  assert_success
  assert_output --partial "kubeone::_inject_registry_auth: a local cannot be declared (a readonly name or a nameref of the caller)"
  assert_output --partial "the merge returned 1"
  assert_output --partial "TARGET kept"
  _assert_no_credential_in_output
  [ ! -s "${ENV_LOG}" ] || { echo "a cat of a credential or the yq of the write ran ($(wc -l < "${ENV_LOG}") lines logged)" >&2; return 1; }
  cmp "${work}" "${RW}/m.yaml" || { echo "the manifest changed" >&2; return 1; }
}

@test "kubeone::_yq_edit stops when the caller has a readonly nameref named _ye_out, and runs no yq" {
  # `local +x ... _ye_out` fails over a readonly nameref of the caller. The
  # capture of yq's output would then go through the reference into the
  # exported TARGET: the manifest, with its credentials.
  local stubs="${BATS_TEST_TMPDIR}/stubs" work="${RW}/.kubeone.yaml.work"
  _count_stubs "${stubs}" '.x = 1'
  printf 'auth:\n  username: fixture-user-1\n  password: fixture-password-1\n' > "${work}"
  cp "${work}" "${BATS_TEST_TMPDIR}/work.before"
  local saved_path="${PATH}"
  PATH="${stubs}:${PATH}"
  run bash -c 'import() { :; }
source "$2/utils/verbose.sh"; source "$2/drivers/kubeone/config"
export TARGET=placeholder
declare -nr _ye_out=TARGET
kubeone::_yq_edit "$1" ".x = 1"
echo "the edit returned $?"
if [[ "${TARGET}" == placeholder ]]; then echo "TARGET kept"; else echo "TARGET changed"; fi' bash "${work}" "${_PROJECT_ROOT}/.lok8s"
  PATH="${saved_path}"
  assert_success
  assert_output --partial "kubeone::_yq_edit: a local cannot be declared (a readonly name or a nameref of the caller)"
  assert_output --partial "the edit returned 1"
  assert_output --partial "TARGET kept"
  _assert_no_credential_in_output
  [ ! -s "${ENV_LOG}" ] || { echo "yq ran ($(wc -l < "${ENV_LOG}") lines logged)" >&2; return 1; }
  cmp "${work}" "${BATS_TEST_TMPDIR}/work.before" || { echo "the manifest changed" >&2; return 1; }
}

@test "after a registry merge that succeeds, the caller keeps set -a and its exported names with their values" {
  # No signal: the call returns. Both functions turn allexport off with
  # `local -`, so it is on again after the return, and their locals
  # (local +x) leave the caller's exported variables as they were.
  _registry_fixture 1
  local work="${RW}/.kubeone.yaml.work"
  cp "${RW}/m.yaml" "${work}"
  run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
export RUSER=placeholder RPASS=placeholder username=placeholder password=placeholder _ye_out=placeholder
set -a
kubeone::_inject_registry_auth "$1" "$2"
echo "the merge returned $?"
if [[ $- == *a* ]]; then echo "allexport: on"; else echo "allexport: off"; fi
for n in RUSER RPASS username password _ye_out; do
  if [[ "$(declare -p "${n}" 2>/dev/null)" == "declare -x ${n}="* ]]; then x=exported; else x="not exported"; fi
  if [[ "${!n-}" == placeholder ]]; then v=placeholder; else v="another value"; fi
  echo "${n}: ${x}, ${v}"
done' bash "${work}" "${CY}" "${_PROJECT_ROOT}/.lok8s"
  assert_success
  _assert_no_credential_in_output
  assert_output "the merge returned 0
allexport: on
RUSER: exported, placeholder
RPASS: exported, placeholder
username: exported, placeholder
password: exported, placeholder
_ye_out: exported, placeholder"
  _assert_credentials_written "${work}" 1
}

# _registry_password <value>: the fixture password of registry1.example.com
# (after _registry_fixture 1) is now <value>.
_registry_password() {
  printf '%s' "${1}" > "${PATH_CLUSTERS}/ko.cloud/secrets/Secret.fixture-pull-1.provisioning.password"
}

# _written <field> <value>: "true" when the registry merge wrote <value> as
# registry1.example.com's auth.<field>. No value is printed.
_written() {
  yq -r ".containerRuntime.containerd.registries.\"registry1.example.com\".auth.${1} == \"${2}\"" "${work}"
}

@test "the registry merge keeps the password out of an arithmetic error under localvar_inherit and a caller's declare -i" {
  # With localvar_inherit (BASHOPTS can turn it on), a local inherits the
  # attributes of the caller's variable of the same name: with -i, the
  # assignment of the password is arithmetic, and the error quotes it.
  # The fixture password holds a ':' so that the arithmetic fails.
  _registry_fixture 1
  _registry_password 'fixture-password-1:x'
  local work="${RW}/.kubeone.yaml.work"
  cp "${RW}/m.yaml" "${work}"
  run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
shopt -s localvar_inherit
declare -i password=0
kubeone::_inject_registry_auth "$1" "$2"
echo "the merge returned $?"' bash "${work}" "${CY}" "${_PROJECT_ROOT}/.lok8s"
  _assert_no_credential_in_output
  assert_success
  assert_output "the merge returned 0"
  run _written password 'fixture-password-1:x'
  assert_output "true"
  run _written username 'fixture-user-1'
  assert_output "true"
}

@test "kubeone::_yq_edit keeps the manifest out of an arithmetic error under localvar_inherit and a caller's declare -i _ye_out" {
  local work="${RW}/.kubeone.yaml.work"
  printf 'auth:\n  username: fixture-user-1\n  password: fixture-password-1\n' > "${work}"
  run bash -c 'import() { :; }
source "$2/utils/verbose.sh"; source "$2/drivers/kubeone/config"
shopt -s localvar_inherit
declare -i _ye_out=0
kubeone::_yq_edit "$1" ".x = 1"
echo "the edit returned $?"' bash "${work}" "${_PROJECT_ROOT}/.lok8s"
  _assert_no_credential_in_output
  [[ "${output}" != *auth* ]] || { echo "manifest text is in the output" >&2; return 1; }
  assert_success
  assert_output "the edit returned 0"
  run yq -r '.x == 1 and .auth.username == "fixture-user-1" and .auth.password == "fixture-password-1"' "${work}"
  assert_output "true"
}

@test "the registry merge writes the credentials unchanged under localvar_inherit and a caller's declare -u and -l" {
  # With localvar_inherit, a caller's -u or -l would change the case of
  # the username, the password or the captured manifest, without an error.
  # Two runs: the names that are -u in the first run are -l in the second.
  # Both credentials hold upper and lower case letters.
  _registry_fixture 1
  _registry_password 'fixture-Password-1'
  printf '%s' 'fixture-User-1' > "${PATH_CLUSTERS}/ko.cloud/secrets/Secret.fixture-pull-1.provisioning.username"
  local work="${RW}/.kubeone.yaml.work" upper lower
  for upper in 'username _ye_out' 'password'; do
    lower='password'
    [[ "${upper}" == 'password' ]] && lower='username _ye_out'
    cp "${RW}/m.yaml" "${work}"
    run bash -c 'import() { :; }
source "$3/utils/verbose.sh"; source "$3/drivers/kubeone/config"
shopt -s localvar_inherit
declare -u $4
declare -l $5
kubeone::_inject_registry_auth "$1" "$2"
echo "the merge returned $?"' bash "${work}" "${CY}" "${_PROJECT_ROOT}/.lok8s" "${upper}" "${lower}"
    [[ "${output}" != *[Ff][Ii][Xx][Tt][Uu][Rr][Ee]* ]] || { echo "a fixture credential is in the output" >&2; return 1; }
    assert_success
    assert_output "the merge returned 0"
    run _written username 'fixture-User-1'
    assert_output "true"
    run _written password 'fixture-Password-1'
    assert_output "true"
  done
}

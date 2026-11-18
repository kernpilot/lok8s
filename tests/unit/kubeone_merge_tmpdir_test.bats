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

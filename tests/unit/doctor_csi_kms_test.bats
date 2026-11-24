#!/usr/bin/env bats
# doctor_csi_kms_test.bats — the rook-ceph-csi-kms check of `lo doctor`
# (doctor::_kms_check).
#
# The cases are shared with the Go twin (internal/cli/cmd_doctor_csikms_test.go)
# under tests/fixtures/doctor-csi-kms/cases/<case>/:
#   cluster.lok8s.yaml   the domain's spec
#   no-kubeconfig        present: the domain has no kubeconfig
#   render               the answer of the render (kustomize build): `yaml
#                        <file in render/>`, or `fail`. Absent: the render
#                        must not run.
#   values               lines that the merged values of the render must hold
#                        (the values of the spec.bootstrap entry)
#   clusterroles         the answer of that labelled read: `json <file in
#   clusterrolebindings  json/>`, or `fail <rc> <stderr>`. Absent: the read
#   roles                must not run.
#   rolebindings
#   refs-clusterrolebindings
#   refs-rolebindings    the answer of that reference read (no selector), the
#   refs-clusterroles    same forms; refs-clusterroles is the read of the
#                        aggregating ClusterRoles
#   fake-render-only     present: the render file is not a real render of the
#                        addon (the Go twin's real render test skips the case)
#   want                 the exact output of the check; @BASE@ stands for the
#                        project directory (the kubeconfig in a command)
# Every failed read or render, and every name that the check must not print,
# carries the word CANARY: the check must never print it. kubectl and
# kustomize are stand-in scripts of this test, first on PATH (the check runs
# them as processes, not as shell functions). The render is the real
# addons::render of the addon of this tree up to the kustomize build: the
# values of the entry are merged, and the stand-in answers the build. No real
# kubectl or kustomize runs and no cluster is reached. Only this file knows two
# more read answers:
#   hang <s> <json file>   the read ends after <s> seconds (its sleep is a
#                          child of the stand-in)
#   early <json file>      the stand-in starts a child that ignores TERM (as a
#                          credential plugin can), prints the list and exits
#                          0 at once: the child stays in the process group of
#                          the read
# For these two the stand-in writes its pid to STUB_PID, its process group to
# STUB_PGID, the clock (in microseconds) when it starts to STUB_START, and the
# pid of its child to CHILD_PID.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }; export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/addons"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/libs/doctor"

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
  if ! command -v jq &>/dev/null; then skip "jq not available"; fi

  export FIX="${_PROJECT_ROOT}/tests/fixtures/doctor-csi-kms"
  export PATH_BASE="${BATS_TEST_TMPDIR}/project"
  export PATH_CLUSTERS="${PATH_BASE}/clusters"
  # As in a project: the tree under PATH_BASE, with the addon of this tree
  # (its chart.yaml and values.yaml are what the render stages).
  export PATH_LOK8S="${PATH_BASE}/.lok8s"
  mkdir -p "${PATH_LOK8S}/addons"
  cp -r "${_PROJECT_ROOT}/.lok8s/addons/rook-ceph-csi-kms" "${PATH_LOK8S}/addons/"
  unset LOK8S_CLUSTER_NAME KUSTOMIZE_PLUGIN_HOME
  export CALLS="${BATS_TEST_TMPDIR}/calls"
  export STUB_PID="${BATS_TEST_TMPDIR}/kubectl.pid" STUB_PGID="${BATS_TEST_TMPDIR}/kubectl.pgid"
  export STUB_START="${BATS_TEST_TMPDIR}/kubectl.start" CHILD_PID="${BATS_TEST_TMPDIR}/child.pid"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"

  STUB="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${STUB}"
  # The stand-in kubectl: records its argv, answers from the case directory:
  # a labelled read from the file of its resource, a reference read (no
  # selector) from refs-<resource>. It gets _stat_field of test_helper (a
  # script does not load that file).
  {
    echo '#!/usr/bin/env bash'
    declare -f _stat_field
    cat <<'STUB'
printf '%s\n' "$*" >> "${CALLS}"
# The check removes each raw list right after its read: no other .json file
# is next to the output file of this read (Linux: /proc).
if out="$(readlink "/proc/$$/fd/1" 2>/dev/null)" && [[ "${out}" == */lo-doctor-rbac.*/*.json ]]; then
  for f in "${out%/*}"/*.json; do
    [[ ! -e "${f}" || "${f}" == "${out}" ]] || echo "RAW-LEFT ${f##*/}" >> "${CALLS}"
  done
fi
res="${4:-}"
[[ " $* " == *" --selector "* ]] || res="refs-${res}"
if [[ ! -f "${CASE}/${res}" ]]; then
  echo "unexpected read ${res}"
  exit 0
fi
read -r kind a b < "${CASE}/${res}"
# _pids — the pid files: the clock, the stand-in and its process group.
_pids() {
  echo "${EPOCHREALTIME//[!0-9]/}" > "${STUB_START}"
  echo "$$" > "${STUB_PID}"
  _stat_field "$$" 3 > "${STUB_PGID}"
}
case "${kind}" in
  json) cat "${FIX}/json/${a}" ;;
  fail) printf '%s\n' "${b}" >&2; exit "${a}" ;;
  hang)
    _pids
    sleep "${a}" 3>&- &
    echo "$!" > "${CHILD_PID}"
    wait
    cat "${FIX}/json/${b}" ;;
  early)
    _pids
    bash -c 'trap "" TERM; echo "$$" > "${CHILD_PID}"; exec sleep 30' 3>&- &
    while [[ ! -s "${CHILD_PID}" ]]; do sleep 0.01; done
    cat "${FIX}/json/${a}" ;;
esac
STUB
  } > "${STUB}/kubectl"
  # The stand-in kustomize: the build of the bootstrap's render (its flags),
  # with the case's values lines in the merged values of the build directory.
  # A wrong call or a missing line goes into CALLS, so the calls of the case
  # do not match.
  cat > "${STUB}/kustomize" <<'STUB'
#!/usr/bin/env bash
if [[ "$#" != 4 || "${1}" != build || "${2}" != --enable-alpha-plugins || "${3}" != --enable-exec ]]; then
  printf 'kustomize BAD-ARGS %s\n' "$*" >> "${CALLS}"
  exit 1
fi
echo kustomize >> "${CALLS}"
if [[ ! -f "${CASE}/render" ]]; then
  echo "kustomize UNEXPECTED" >> "${CALLS}"
  exit 1
fi
_trim() { local s="${1#"${1%%[![:space:]]*}"}"; printf '%s' "${s%"${s##*[![:space:]]}"}"; }
if [[ -f "${CASE}/values" ]]; then
  declare -A have=()
  if [[ ! -f "${4}/values.merged.yaml" ]]; then
    echo "kustomize NO-MERGED-VALUES" >> "${CALLS}"
  else
    # Block style: the merge keeps the flow style of the inline values
    # (kms: [metadata]), and the case's lines are in block style.
    while IFS= read -r l; do have["x$(_trim "${l}")"]=1; done < <(yq '... style=""' "${4}/values.merged.yaml")
  fi
  while IFS= read -r w; do
    [[ -n "${have["x$(_trim "${w}")"]:-}" ]] || printf 'kustomize NO-VALUE %s\n' "${w}" >> "${CALLS}"
  done < "${CASE}/values"
fi
read -r kind file < "${CASE}/render"
case "${kind}" in
  yaml) cat "${FIX}/render/${file}" ;;
  fail) echo "Error: CANARY render" >&2; exit 1 ;;
  *) echo "kustomize BAD-ANSWER" >> "${CALLS}"; exit 1 ;;
esac
STUB
  chmod +x "${STUB}/kubectl" "${STUB}/kustomize"
  export PATH="${STUB}:${PATH}"
  [[ "$(command -v kubectl)" == "${STUB}/kubectl" ]] || fail "the stand-in kubectl is not first on PATH"
  [[ "$(command -v kustomize)" == "${STUB}/kustomize" ]] || fail "the stand-in kustomize is not first on PATH"
}

teardown() { teardown_tmpdir; }

NOT_CHECKED="  ! rook-ceph-csi-kms RBAC leftovers not checked: "
SELECTOR="lok8s.dev/name=rook-ceph-csi-kms"

# _case <name> — run the check for the case and compare the output, the
# render and the reads with what the case says.
_case() {
  _setup_case "${FIX}/cases/${1}"
  run doctor::_kms_check rbac.example
  _assert_case
}

# _setup_case <dir> — the domain of the case under PATH_BASE.
_setup_case() {
  export CASE="${1}"
  [[ -f "${CASE}/want" ]] || fail "no case ${CASE}"
  mkdir -p "${PATH_CLUSTERS}/rbac.example"
  cp "${CASE}/cluster.lok8s.yaml" "${PATH_CLUSTERS}/rbac.example/cluster.lok8s.yaml"
  if [[ ! -f "${CASE}/no-kubeconfig" ]]; then
    mkdir -p "${PATH_BASE}/.kubeconfig"
    printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/rbac-test.yaml"
  fi
}

# _assert_case — the output and the calls of the last run against the case,
# and no temp dir left.
_assert_case() {
  assert_success
  # The project directory has no single quote, so the case's quoting of the
  # kubeconfig path is the check's own.
  [[ "${PATH_BASE}" != *[\'@]* ]] || fail "the project directory ${PATH_BASE} has a quote or an @"
  local want
  want="$(cat "${CASE}/want")"
  assert_output "${want//@BASE@/"${PATH_BASE}"}"
  refute_output --partial CANARY
  refute_output --partial canary
  local want_calls="" res
  if [[ -f "${CASE}/render" ]]; then want_calls+="kustomize"$'\n'; fi
  for res in clusterroles clusterrolebindings roles rolebindings; do
    if [[ -f "${CASE}/${res}" ]]; then
      want_calls+="--kubeconfig ${PATH_BASE}/.kubeconfig/rbac-test.yaml get ${res} --all-namespaces --selector ${SELECTOR} -o json --request-timeout=10s"$'\n'
    fi
  done
  for res in clusterrolebindings rolebindings clusterroles; do
    if [[ -f "${CASE}/refs-${res}" ]]; then
      want_calls+="--kubeconfig ${PATH_BASE}/.kubeconfig/rbac-test.yaml get ${res} --all-namespaces -o json --request-timeout=10s"$'\n'
    fi
  done
  assert_equal "$(cat "${CALLS}" 2>/dev/null)" "${want_calls%$'\n'}"
  _assert_no_temp_dir
}

# The temp dir of the reads and the build dir of the render are gone.
_assert_no_temp_dir() {
  local left
  left="$(find "${TMPDIR}" -maxdepth 1 \( -name 'lo-doctor-rbac.*' -o -name 'lok8s-addon-*' \))"
  [[ -z "${left}" ]] || fail "temp dir left: ${left}"
}

# _path_without <tool...> — prints a PATH of one dir with a link to each
# program of PATH (the first one of a name wins, so the stand-ins too),
# except the named tools.
_path_without() {
  local dir="${BATS_TEST_TMPDIR}/path-without" d t
  rm -rf "${dir}"
  mkdir -p "${dir}"
  local -a dirs=()
  IFS=: read -ra dirs <<< "${PATH}"
  for d in "${dirs[@]}"; do
    [[ -d "${d}" ]] || continue
    ln -s "${d}"/* "${dir}/" 2>/dev/null || true
  done
  for t in "$@"; do rm -f "${dir}/${t}"; done
  for t in "$@"; do [[ ! -e "${dir}/${t}" ]] || fail "${t} is still in ${dir}"; done
  echo "${dir}"
}

# _inline_case <spec> <clusterroles answer or ""> <want> — a case dir of this
# file only.
_inline_case() {
  local dir="${BATS_TEST_TMPDIR}/case-inline"
  mkdir -p "${dir}"
  printf '%s\n' "${1}" > "${dir}/cluster.lok8s.yaml"
  if [[ -n "${2}" ]]; then echo "${2}" > "${dir}/clusterroles"; fi
  printf '%s\n' "${3}" > "${dir}/want"
  _setup_case "${dir}"
}

@test "kms: a candidate of an earlier option set of each kind warns with its delete command" { _case extra-objects; }
@test "kms: an equal set prints nothing" { _case equal; }
@test "kms: a ClusterRoleBinding of the option after a change of tenantNamespaces keeps its command while its role is in the render" { _case tenant-namespaces; }
@test "kms: without the addon but with rook-ceph each object of the addon is a candidate" { _case disabled-leftovers; }
@test "kms: without the addon but with rook-ceph and no object, nothing prints" { _case disabled-none; }
@test "kms: a leftover with an unusual name is counted, never printed" { _case disabled-hostile; }
@test "kms: a failed read of a leftover check is one warning" { _case disabled-read-fails; }
@test "kms: without either addon there is no read" { _case neither-addon; }
@test "kms: the addon as a path entry does not count" { _case path-entry-does-not-count; }
@test "kms: the addon and rook-ceph with a trailing slash count" { _case trailing-slash; }
@test "kms: the addon and rook-ceph with a trailing /. count" { _case trailing-dot; }
@test "kms: rook-ceph alone with a trailing slash counts" { _case disabled-trailing-slash; }
@test "kms: rook-ceph alone with a trailing /. counts" { _case disabled-trailing-dot; }
@test "kms: a forbidden read prints the reason that names the four kinds" { _case read-forbidden; }
@test "kms: a label that is not a string makes the list invalid" { _case read-malformed-label; }
@test "kms: a failed third read stops the reads, and no finding of an earlier read prints" { _case read-third-fails; }
@test "kms: a name or namespace that is not DNS-1123 is counted and never printed" { _case hostile-names; }
@test "kms: an object without the label of the addon is never a finding" { _case selector-ignored; }
@test "kms: a failed render is one warning, with no read" { _case render-fails; }
@test "kms: no kubeconfig prints the skip line when the addon is in spec.bootstrap" { _case no-kubeconfig; }
@test "kms: no kubeconfig prints nothing without the addon" { _case no-kubeconfig-disabled; }
@test "kms: a role that a binding outside the candidates names gets no delete command" { _case role-still-bound; }
@test "kms: a forbidden reference read gives the roles no delete command" { _case refs-forbidden; }
@test "kms: a reference read with a binding without its role is not a valid list" { _case refs-item-no-roleref; }
@test "kms: a ClusterRole with an aggregate-to label gets no delete command" { _case aggregated; }
@test "kms: with rook-ceph and path entries, a candidate makes the check say not checked" { _case path-entry-and-parent; }
@test "kms: with path entries and no candidate, nothing prints" { _case path-entry-no-candidate; }
@test "kms: an entry that does not parse stops the check, by its position" { _case entry-does-not-parse; }
@test "kms: a list with a continue token is not a valid list" { _case list-continue; }
@test "kms: an item without a name is not a valid list" { _case item-no-name; }
@test "kms: a Role without a namespace is not a valid list" { _case item-no-namespace; }
@test "kms: a render with a scalar document is malformed" { _case render-scalar-doc; }
@test "kms: a render with a Role without a name is malformed" { _case render-item-no-name; }
@test "kms: an env name of the entry changes the render through its envsubst" { _case render-env; }
@test "kms: a build output that the render's yq cannot read is a failed render, and its error does not print" { _case render-yaml-error; }
@test "kms: a ClusterRole that matchLabels or an empty selector selects gets no delete command" { _case aggregation-match-labels; }
@test "kms: a ClusterRole that an In expression selects gets no delete command" { _case aggregation-in; }
@test "kms: a ClusterRole that a NotIn expression selects gets no delete command" { _case aggregation-notin; }
@test "kms: a ClusterRole that an Exists expression selects gets no delete command" { _case aggregation-exists; }
@test "kms: a ClusterRole that a DoesNotExist expression selects gets no delete command" { _case aggregation-doesnotexist; }
@test "kms: selectors that do not select a ClusterRole, and its own selector, keep its delete command" { _case aggregation-no-match; }
@test "kms: an aggregation selector with an unknown operator gives no delete command" { _case aggregation-unknown-operator; }
@test "kms: an In expression without values gives no delete command" { _case aggregation-in-without-values; }
@test "kms: a NotIn expression without values gives no delete command" { _case aggregation-notin-without-values; }
@test "kms: an Exists expression with values gives no delete command" { _case aggregation-exists-with-values; }
@test "kms: a DoesNotExist expression with values gives no delete command" { _case aggregation-doesnotexist-with-values; }
@test "kms: a failed aggregation read gives no ClusterRole a delete command" { _case aggregation-read-fails; }
@test "kms: a candidate that aggregates another candidate keeps its command, and the other one gets none" { _case aggregation-by-a-candidate; }

# The bootstrap refuses an entry's env name that is lo's own (Go:
# addons.ProtectedEnvOverride, bash: bootstrap::_reserved_env_name): the
# check treats the entry as a failed render, with no build and no read.
@test "kms: an env name that the bootstrap refuses is a failed render" { _case render-reserved-env; }

# bootstrap::_resolve_entries does not return the status of its yq: the
# check reads the spec with yq first. Here the driver read passes and that
# yq fails.
@test "kms: a spec.bootstrap that yq cannot read stops the check" {
  _setup_case "${FIX}/cases/disabled-leftovers"
  printf 'kind: KubeOne\nspec: {bootstrap: [rook-ceph\n' > "${PATH_CLUSTERS}/rbac.example/cluster.lok8s.yaml"
  domain::spec_driver() { echo kubeone; }
  run doctor::_kms_check rbac.example
  assert_success
  assert_output "${NOT_CHECKED}spec.bootstrap: doctor cannot read the list of entries (see: lo bootstrap --dry-run)"
  [[ ! -e "${CALLS}" ]] || fail "a call ran: $(cat "${CALLS}")"
}

@test "kms: a resolver that fails stops the check" {
  _setup_case "${FIX}/cases/disabled-leftovers"
  bootstrap::_resolve_entries() { echo rook-ceph; return 1; }
  run doctor::_kms_check rbac.example
  assert_success
  assert_output "${NOT_CHECKED}spec.bootstrap: doctor cannot read the list of entries (see: lo bootstrap --dry-run)"
  [[ ! -e "${CALLS}" ]] || fail "a call ran: $(cat "${CALLS}")"
}

# The yq of doctor::_kms_ids sends its stderr to /dev/null: a stream that yq
# cannot read (here an alias of an anchor that is not there, which yq names in
# its error) gives the fixed reason only. The stand-in render gives that
# stream as the output of a render that passed.
@test "kms: a rendered stream that yq cannot read prints only the fixed reason" {
  _setup_case "${FIX}/cases/extra-objects"
  addons::render() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: *CANARY\n'; }
  run doctor::_kms_check rbac.example
  assert_success
  assert_output "${NOT_CHECKED}the render of the addon is malformed (a document that is not an object, an object without a kind, or an RBAC object without a name or, for a Role or RoleBinding, a namespace)"
  [[ ! -e "${CALLS}" ]] || fail "a call ran: $(cat "${CALLS}")"
}

# doctor::_kms_ids accepts a render only in the shape that both twins read
# (Go: TestDoctorKMSRenderShapes reads the same files).
@test "kms: the render shapes" {
  local f name got want n=0
  for f in "${FIX}"/render-shapes/*.yaml; do
    name="$(basename "${f}")"
    n=$(( n + 1 ))
    case "${name}" in
      ok-*)
        got="$(doctor::_kms_ids "[]" < "${f}")" || fail "${name}: refused"
        got="$(jq -r '[.[] | "\(.kind)/\(.namespace)/\(.name)"] | sort | join(" ")' <<< "${got}")"
        want="$(head -n 1 "${f}")"
        want="${want#"# ids: "}"
        [[ "${got}" == "${want}" ]] || fail "${name}: ids ${got}, want ${want}"
        ;;
      malformed-*)
        # Nothing on stdout or stderr: a parse error can quote the render.
        if got="$(doctor::_kms_ids "[]" < "${f}" 2>&1)"; then fail "${name}: accepted: ${got}"; fi
        [[ -z "${got}" ]] || fail "${name}: printed ${got}"
        ;;
      *) fail "${name}: neither ok- nor malformed-" ;;
    esac
  done
  (( n >= 10 )) || fail "only ${n} render shapes"
}

# doctor::_rbac_shape accepts a list only in the shape that both twins read
# (Go: TestDoctorKMSListShapes reads the same files).
@test "kms: the list shapes" {
  local f name shape n=0
  for f in "${FIX}"/list-shapes/*.json; do
    name="$(basename "${f}")"
    n=$(( n + 1 ))
    case "${name}" in
      labelled-cluster-*) shape=labelled-cluster ;;
      labelled-ns-*) shape=labelled-ns ;;
      ref-cluster-*) shape=ref-cluster ;;
      ref-ns-*) shape=ref-ns ;;
      agg-*) shape=agg ;;
      *) fail "${name}: no known shape" ;;
    esac
    case "${name}" in
      *-ok-*) doctor::_rbac_shape "${shape}" "${f}" || fail "${name}: refused" ;;
      *-invalid-*) if doctor::_rbac_shape "${shape}" "${f}"; then fail "${name}: accepted"; fi ;;
      *) fail "${name}: neither -ok- nor -invalid-" ;;
    esac
  done
  (( n >= 10 )) || fail "only ${n} list shapes"
}

# The stand-in kubectl sees the temp dir of the reads (Linux: /proc): there
# the raw list of an earlier read is gone (see the stand-in).
@test "kms: the stand-in kubectl sees the temp dir of the reads" {
  [[ -e /proc/self/fd/1 ]] || skip "no /proc"
  _setup_case "${FIX}/cases/disabled-leftovers"
  cat > "${STUB}/kubectl-probe" <<'PROBE'
#!/usr/bin/env bash
readlink "/proc/$$/fd/1" >> "${CALLS}.probe"
exec "${STUB_KUBECTL}" "$@"
PROBE
  chmod +x "${STUB}/kubectl-probe"
  mv "${STUB}/kubectl" "${STUB}/kubectl-real"
  mv "${STUB}/kubectl-probe" "${STUB}/kubectl"
  STUB_KUBECTL="${STUB}/kubectl-real" run doctor::_kms_check rbac.example
  assert_success
  [[ "$(head -n 1 "${CALLS}.probe")" == "${TMPDIR}"/lo-doctor-rbac.*/kms-clusterroles.json ]] \
    || fail "the read writes to $(head -n 1 "${CALLS}.probe")"
}

@test "kms: no kubectl says not checked when the addon is in spec.bootstrap" {
  _setup_case "${FIX}/cases/equal"
  local p; p="$(_path_without kubectl)"
  PATH="${p}" run doctor::_kms_check rbac.example
  assert_success
  assert_output "${NOT_CHECKED}kubectl not found"
  [[ ! -e "${CALLS}" ]] || fail "a call ran: $(cat "${CALLS}")"
}

@test "kms: no kubectl prints nothing without the addon" {
  _setup_case "${FIX}/cases/disabled-leftovers"
  local p; p="$(_path_without kubectl)"
  PATH="${p}" run doctor::_kms_check rbac.example
  assert_success
  assert_output ""
  [[ ! -e "${CALLS}" ]] || fail "a call ran: $(cat "${CALLS}")"
}

# Without yq or jq the check does nothing: the RBAC migration check before it
# says that it did not check.
@test "kms: no yq or no jq prints nothing and calls nothing" {
  local tool p
  for tool in yq jq; do
    _setup_case "${FIX}/cases/extra-objects"
    p="$(_path_without "${tool}")"
    PATH="${p}" run doctor::_kms_check rbac.example
    assert_success
    assert_output ""
    [[ ! -e "${CALLS}" ]] || fail "a call ran without ${tool}: $(cat "${CALLS}")"
  done
}

@test "kms: no active domain prints nothing" {
  run doctor::_kms_check ""
  assert_success
  assert_output ""
}

# The reads have the bound of the RBAC migration check: a read that does not
# end is stopped, and the check prints the fixed reason once. The stand-in
# answers a valid list after 5 s, and its sleep is its child. The stop ends
# both (TERM to the group), so the check returns in less than 3.5 s: the
# bound (1 s), the stop and the processes of the test. A stop that does
# nothing fails here: the check then waits for the stand-in (5 s), or the
# child still runs.
@test "kms: the bound stops a hung read" {
  doctor::_rbac_bound() { echo 1; }
  _inline_case "$(cat "${FIX}/cases/disabled-leftovers/cluster.lok8s.yaml")" "hang 5 cr-metadata.json" \
    "${NOT_CHECKED}kubectl did not answer within 30 s (the cluster API, or a credential plugin that waits for a login)"
  run doctor::_kms_check rbac.example
  _assert_case
  _assert_tree_gone 3500
}

# A read that ends first and leaves a child in its process group: the stop
# of the group ends the child too, with TERM and, as the child ignores TERM,
# with KILL after the grace (2 s). The reads give valid empty lists: no line.
@test "kms: a read that ends first stops the child that it leaves in its group" {
  _inline_case "$(cat "${FIX}/cases/disabled-leftovers/cluster.lok8s.yaml")" "early empty.json" ""
  local res
  for res in clusterrolebindings roles rolebindings; do echo "json empty.json" > "${CASE}/${res}"; done
  run doctor::_kms_check rbac.example
  _assert_case
  _assert_tree_gone 4500
}

# The command builder checks the namespace again (Go: rbacDeleteCommandIn):
# no command for a namespace that is not a DNS-1123 label of at most 63
# characters, also when a caller did not check it.
@test "kms: the delete command builder refuses a namespace that is not a DNS-1123 label" {
  local ns long
  long="$(printf 'a%.0s' {1..63})"
  for ns in "a b" "a;b" A a.b -a $'a\nb' 'a$(x)' "${long}a"; do
    run doctor::_rbac_delete /k role a "${ns}"
    assert_failure
    assert_output ""
  done
  run doctor::_rbac_delete /k role "a;b" rook-ceph
  assert_failure
  assert_output ""
  for ns in a rook-ceph "${long}"; do
    run doctor::_rbac_delete /k role a.b "${ns}"
    assert_success
    assert_output "kubectl --kubeconfig '/k' delete role a.b --namespace ${ns}"
  done
}

# The lines of the check come after the RBAC migration lines, in the domain
# section of the command.
@test "kms: the lines follow the RBAC migration lines" {
  _setup_case "${FIX}/cases/no-kubeconfig"
  DOMAIN_NAME=rbac.example run main::doctor
  assert_output --partial "  ℹ RBAC migration check skipped: spec.bootstrap has no system-upgrade-controller and no redis-operator
  ℹ rook-ceph-csi-kms RBAC check skipped: no kubeconfig at .kubeconfig/rbac-test.yaml"
}

# _alive <pid> — 0 when the process runs (a zombie does not count): /proc
# where it exists, else kill -0.
_alive() {
  if [[ -d /proc/self ]]; then
    local stat
    stat="$(cat "/proc/${1}/stat" 2>/dev/null)" || return 1
    [[ -n "${stat}" ]] || return 1
    stat="${stat##*) }"
    [[ "${stat:0:1}" != Z ]]
  else
    kill -0 "${1}" 2>/dev/null
  fi
}

# _group_alive <pgid> — 0 when a process of the process group runs (a zombie
# does not count): /proc where it exists, else kill -0 of the group.
_group_alive() {
  if [[ -d /proc/self ]]; then
    local f stat state pgrp
    for f in /proc/[0-9]*/stat; do
      { IFS= read -r stat < "${f}"; } 2>/dev/null || continue
      read -r state _ pgrp _ <<< "${stat##*) }"
      if [[ "${pgrp}" == "${1}" && "${state}" != Z ]]; then return 0; fi
    done
    return 1
  fi
  kill -0 -- "-${1}" 2>/dev/null
}

# _assert_tree_gone <max ms> — at once after the check returned: the
# stand-in and its child no longer run, no process of their process group
# runs, and the stand-in started less than <max ms> before. The stand-in
# does not lead its group (the leader of doctor::_rbac_run does).
_assert_tree_gone() {
  local now="${EPOCHREALTIME//[!0-9]/}" start took pid pgid
  start="$(cat "${STUB_START}")"
  [[ "${start}" =~ ^[0-9]+$ ]] || fail "no clock in ${STUB_START}"
  took=$(( (now - start) / 1000 ))
  for pid in "$(cat "${STUB_PID}")" "$(cat "${CHILD_PID}")"; do
    [[ "${pid}" =~ ^[0-9]+$ ]] || fail "a pid file has no pid"
    ! _alive "${pid}" || fail "process ${pid} still runs after the check"
  done
  pgid="$(cat "${STUB_PGID}")"
  [[ "${pgid}" =~ ^[0-9]+$ ]] || fail "no process group in ${STUB_PGID}"
  [[ "${pgid}" != "$(cat "${STUB_PID}")" ]] || fail "the stand-in leads its process group"
  ! _group_alive "${pgid}" || fail "a process of the group of the read still runs after the check"
  (( took < ${1} )) || fail "the check took ${took} ms, want less than ${1} ms"
}

# Every case directory has a @test above, and the Go twin reads the same
# directory: a new case runs in both suites or this fails.
@test "kms: every fixture case has a bats test" {
  local dir name
  for dir in "${FIX}"/cases/*/; do
    name="$(basename "${dir}")"
    grep -q "_case ${name};" "${BATS_TEST_FILENAME}" || fail "case ${name} has no @test"
  done
}

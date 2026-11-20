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
#   clusterroles         the answer of that read: `json <file in json/>`, or
#   clusterrolebindings  `fail <rc> <stderr>`. Absent: the read must not run.
#   roles
#   rolebindings
#   want                 the exact output of the check; @BASE@ stands for the
#                        project directory (the kubeconfig in a command)
# Every failed read or render, and every name that the check must not print,
# carries the word CANARY: the check must never print it. kubectl and
# kustomize are stand-in scripts of this test, first on PATH (the check runs
# them as processes, not as shell functions). The render is the real
# addons::render of the addon of this tree up to the kustomize build: the
# values of the entry are merged, and the stand-in answers the build. No real
# kubectl or kustomize runs and no cluster is reached. Only this file knows one
# more read answer:
#   hang <s> <json file>   the read ends after <s> seconds

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }; export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/addons"
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
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"

  STUB="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${STUB}"
  # The stand-in kubectl: records its argv, answers from the case directory.
  cat > "${STUB}/kubectl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${CALLS}"
res="${4:-}"
if [[ ! -f "${CASE}/${res}" ]]; then
  echo "unexpected read ${res}"
  exit 0
fi
read -r kind a b < "${CASE}/${res}"
case "${kind}" in
  json) cat "${FIX}/json/${a}" ;;
  fail) printf '%s\n' "${b}" >&2; exit "${a}" ;;
  hang) sleep "${a}" 3>&-; cat "${FIX}/json/${b}" ;;
esac
STUB
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

@test "kms: an object of an earlier option set of each kind warns with its delete command" { _case extra-objects; }
@test "kms: an equal set prints nothing" { _case equal; }
@test "kms: a ClusterRoleBinding of the option after a change of tenantNamespaces warns" { _case tenant-namespaces; }
@test "kms: without the addon but with rook-ceph each object of the addon is a leftover" { _case disabled-leftovers; }
@test "kms: without the addon but with rook-ceph and no object, nothing prints" { _case disabled-none; }
@test "kms: a leftover with an unusual name is counted, never printed" { _case disabled-hostile; }
@test "kms: a failed read of a leftover check is one warning" { _case disabled-read-fails; }
@test "kms: without either addon there is no read" { _case neither-addon; }
@test "kms: the addon as a path entry does not count" { _case path-entry-does-not-count; }
@test "kms: a forbidden read prints the reason that names the four kinds" { _case read-forbidden; }
@test "kms: a label that is not a string makes the list invalid" { _case read-malformed-label; }
@test "kms: a failed third read stops the reads, and no finding of an earlier read prints" { _case read-third-fails; }
@test "kms: a name or namespace that is not DNS-1123 is counted and never printed" { _case hostile-names; }
@test "kms: an object without the label of the addon is never a finding" { _case selector-ignored; }
@test "kms: a failed render is one warning, with no read" { _case render-fails; }
@test "kms: no kubeconfig prints the skip line when the addon is in spec.bootstrap" { _case no-kubeconfig; }
@test "kms: no kubeconfig prints nothing without the addon" { _case no-kubeconfig-disabled; }

# The bootstrap refuses an entry's env name that is lo's own (Go:
# addons.ProtectedEnvOverride, bash: bootstrap::_reserved_env_name): the
# check treats the entry as a failed render, with no build and no read.
@test "kms: an env name that the bootstrap refuses is a failed render" { _case render-reserved-env; }

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
# answers a valid list after 5 s: without the bound, this fails.
@test "kms: the bound stops a hung read" {
  doctor::_rbac_bound() { echo 1; }
  _inline_case "$(cat "${FIX}/cases/disabled-leftovers/cluster.lok8s.yaml")" "hang 5 cr-metadata.json" \
    "${NOT_CHECKED}kubectl did not answer within 30 s (the cluster API, or a credential plugin that waits for a login)"
  run doctor::_kms_check rbac.example
  _assert_case
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

# Every case directory has a @test above, and the Go twin reads the same
# directory: a new case runs in both suites or this fails.
@test "kms: every fixture case has a bats test" {
  local dir name
  for dir in "${FIX}"/cases/*/; do
    name="$(basename "${dir}")"
    grep -q "_case ${name};" "${BATS_TEST_FILENAME}" || fail "case ${name} has no @test"
  done
}

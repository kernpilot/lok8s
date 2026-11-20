#!/usr/bin/env bats
# doctor_test.bats — `lo doctor`'s provider/infrastructure section.
#
# doctor::_provider_section resolves the active domain's provider (reusing the
# provider contract helpers) and, when that provider implements the OPTIONAL
# provider::doctor hook, renders its TAB-separated report in the doctor lib's
# ✓/! style. Existing behaviour (env/tool checks) is untouched: the section is
# silent unless a provider-backed cluster spec is present.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_LOK8S="${BATS_TEST_TMPDIR}/.lok8s"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  import() { :; }
  export -f import

  # provider contract helpers (read_name/write_config/load), the domain
  # readers (domain::driver, for doctor::_kind_fails) + the doctor lib.
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/provider.sh"
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/doctor"

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
}

teardown() { teardown_tmpdir; }

# A cluster spec whose provider is <name>.
_spec_with_provider() {
  local domain="$1" name="$2"
  mkdir -p "${PATH_CLUSTERS}/${domain}"
  cat > "${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: ${domain}
spec:
  provider:
    name: ${name}
YAML
}

# A provider implementing the 5 required contract functions + optionally doctor.
_mock_provider() {
  local name="$1" with_doctor="${2:-1}"
  mkdir -p "${PATH_LOK8S}/providers/${name}"
  cat > "${PATH_LOK8S}/providers/${name}/main" <<'SCRIPT'
provider::validate() { return 0; }
provider::credential_data() { echo "k=v"; }
provider::provision() { return 0; }
provider::destroy() { return 0; }
provider::output() { echo '{}'; }
SCRIPT
  if [[ "${with_doctor}" == 1 ]]; then
    cat >> "${PATH_LOK8S}/providers/${name}/main" <<'SCRIPT'
provider::doctor() {
  printf 'ok\thcloud API reachable\n'
  printf 'warn\tRobot creds unset — set HROBOT_USER/HROBOT_PASSWORD\n'
  printf 'summary\t1 ok, 1 warn\n'
}
SCRIPT
  fi
}

@test "lo doctor renders the provider/infrastructure section from provider::doctor" {
  _mock_provider mock 1
  _spec_with_provider test.example mock

  run doctor::_provider_section test.example
  assert_success
  assert_output --partial "provider / infrastructure (mock)"
  # provider::doctor's ok/warn lines rendered in the lib's ✓/! style. Assert the
  # status GLYPH tied to its message (not just the message substring) so a
  # prefix-stripping / wrong-status regression — dropping the glyph or mapping
  # ok→! (or vice-versa) — fails instead of silently passing.
  assert_output --partial '  ✓ hcloud API reachable'
  assert_output --partial '  ! Robot creds unset'
  assert_output --partial "1 ok, 1 warn"
}

@test "lo doctor notes when a provider has no doctor hook" {
  _mock_provider nodoc 0
  _spec_with_provider test.example nodoc

  run doctor::_provider_section test.example
  assert_success
  assert_output --partial "provider / infrastructure (nodoc)"
  assert_output --partial "no doctor hook"
}

@test "lo doctor provider section is silent without a provider-backed spec" {
  mkdir -p "${PATH_CLUSTERS}/plain.example"
  cat > "${PATH_CLUSTERS}/plain.example/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: plain
spec:
  cluster:
    domain: plain.example
YAML

  run doctor::_provider_section plain.example
  assert_success
  refute_output --partial "provider / infrastructure"
}

@test "lo doctor provider section is silent with no domain / no spec" {
  run doctor::_provider_section ""
  assert_success
  assert_output ""

  run doctor::_provider_section missing.example
  assert_success
  assert_output ""
}

# The git worktree case (kubehz.in.net recreate, 2026-10-23): the project has
# no .bin/argsh. The entrypoint names the fix before it sources anything, for
# `lo doctor` as for every other command, instead of a raw bash error.
@test "lo doctor without the project's .bin/argsh names the fix" {
  local bin="${BATS_TEST_TMPDIR}/empty-bin"
  mkdir -p "${bin}"
  run env PATH_BIN="${bin}" bash "${_PROJECT_ROOT}/.lok8s/lo" doctor < /dev/null
  assert_failure
  assert_output "[error] argsh is missing at ${bin}/argsh: lo runs on it (fix: add github.com/arg-sh/argsh to the b.yaml in the same directory, then b install)"
  # A b.yaml that lists argsh needs only the install (the Go doctor's rule).
  printf 'binaries:\n  argsh: {}\n' > "${bin}/b.yaml"
  run env PATH_BIN="${bin}" bash "${_PROJECT_ROOT}/.lok8s/lo" doctor < /dev/null
  assert_failure
  assert_output "[error] argsh is missing at ${bin}/argsh: lo runs on it (fix: b install)"
}

# ── doctor::_kind: the minimum kind release ──────────────────────────────────
# The kind config that lo up renders sets networking.dnsSearch, which needs
# kind v0.18.0. Go twin: TestDoctorKindMinimumRelease
# (internal/cli/cmd_doctor_test.go), with the same lines.

_kind_purpose="local clusters (Lo driver)"
_kind_need="lo up needs kind v0.18.0 or later for networking.dnsSearch"

# _kind_stub <kind version output, printf %b> [<kind exit code>] — writes
# a stub kind into ${BATS_TEST_TMPDIR}/kind-bin. The stub answers only
# `kind version`; any other argv exits 98 (a version unknown line).
_kind_stub() {
  local bin="${BATS_TEST_TMPDIR}/kind-bin"
  mkdir -p "${bin}"
  printf '%b' "${1}" > "${bin}/kind.out"
  printf '#!/usr/bin/env bash\n[[ "$*" == version ]] || exit 98\ncat "%s"\nexit %s\n' \
    "${bin}/kind.out" "${2:-0}" > "${bin}/kind"
  chmod +x "${bin}/kind"
}

# _kind_run <kind version output, printf %b> [<kind exit code>] — runs
# doctor::_kind for the domain ${_kind_domain} (default: no domain) with
# the stub kind (_kind_stub) first on PATH.
_kind_run() {
  _kind_stub "$@"
  PATH="${BATS_TEST_TMPDIR}/kind-bin:${PATH}" run doctor::_kind "${_kind_domain-}" "${_kind_purpose}"
}

_kind_older() {
  assert_failure
  assert_output "  ✗ kind ${1} — ${_kind_purpose}; ${_kind_need} (fix: b update kind)"
}

_kind_pass() {
  assert_success
  assert_output "  ✓ kind — ${_kind_purpose}"
}

_kind_unknown() {
  assert_success
  assert_output "  ! kind — ${_kind_purpose}; version unknown, ${_kind_need}"
}

@test "doctor::_kind fails on a kind older than v0.18.0" {
  _kind_run 'kind v0.17.0 go1.19.4 linux/amd64\n'
  _kind_older v0.17.0
  # The minor and the patch compare as numbers, not as text.
  _kind_run 'kind v0.9.0 go1.15.2 linux/amd64\n'
  _kind_older v0.9.0
  _kind_run 'kind v0.17.9\n'
  _kind_older v0.17.9
  # A pre-release of an older release fails, and the line names only the
  # matched numbers.
  _kind_run 'kind v0.17.0-rc.1+abc go1.19 linux/amd64\n'
  _kind_older v0.17.0
  # A tab separates the fields too, and a leading space is skipped (the
  # split of the Go twin).
  _kind_run 'kind\tv0.17.0\tgo1.19.4 linux/amd64\n'
  _kind_older v0.17.0
  _kind_run ' kind v0.17.0 go1.19.4 linux/amd64\n'
  _kind_older v0.17.0
}

@test "doctor::_kind passes v0.18.0 and later, also a pre-release of a later release" {
  _kind_run 'kind v0.18.0 go1.20.2 linux/amd64\n'
  _kind_pass
  _kind_run 'kind v0.31.0-alpha.12+0123456789abcd go1.25.5 linux/amd64\n'
  _kind_pass
  _kind_run 'kind v1.0.0 go1.26.1 linux/amd64\n'
  _kind_pass
  # Only major.minor.patch count (the rule of the Go twin).
  _kind_run 'kind v0.18.0-alpha.1 go1.20 linux/amd64\n'
  _kind_pass
  # Leading zeros compare as decimal numbers, not as octal.
  _kind_run 'kind v0.018.0 go1.20 linux/amd64\n'
  _kind_pass
  # Leading spaces and tabs are skipped.
  _kind_run ' \t kind v0.32.0 go1.25.5 linux/amd64\n'
  _kind_pass
}

@test "doctor::_kind warns when it cannot read the version" {
  _kind_run 'kind vdev go1.25.5 linux/amd64\n'
  _kind_unknown
  _kind_run 'kind v0.18.0rc1 go1.20 linux/amd64\n'
  _kind_unknown
  _kind_run 'kind v0.1234567890.0 go1.20 linux/amd64\n'
  _kind_unknown
  _kind_run 'v0.18.0\n'
  _kind_unknown
  _kind_run 'kindest v0.32.0 go1.25.5 linux/amd64\n'
  _kind_unknown
  _kind_run ''
  _kind_unknown
  _kind_run '\nkind v0.32.0 go1.25.5 linux/amd64\n'
  _kind_unknown
  _kind_run 'kind v0.32.0\r\n'
  _kind_unknown
  # A failed `kind version` is a warning, also with a valid line.
  _kind_run 'kind v0.32.0 go1.25.5 linux/amd64\n' 1
  _kind_unknown
}

@test "doctor::_kind without kind is the warning of an optional tool" {
  local p="" d
  local -a dirs=()
  IFS=: read -r -a dirs <<< "${PATH}"
  for d in "${dirs[@]}"; do
    [[ -x "${d}/kind" ]] || p+="${p:+:}${d}"
  done
  PATH="${p}" run doctor::_kind "" "${_kind_purpose}"
  assert_success
  assert_output "  ! kind not found (optional) — ${_kind_purpose}"
}

# A kind file without the exec bit is not a kind that can run: the line is
# the warning of an absent kind, as in the Go twin (toolchain.LookPath).
# command -v alone prints such a file when no kind on PATH can run (the
# first check). Go twin: TestDoctorKindNeedsTheExecBit.
@test "doctor::_kind takes a kind without the exec bit as not found" {
  local bin="${BATS_TEST_TMPDIR}/noexec-bin" p="" d
  mkdir -p "${bin}"
  printf '#!/bin/sh\necho kind v0.32.0\n' > "${bin}/kind"
  chmod 0644 "${bin}/kind"
  local -a dirs=()
  IFS=: read -r -a dirs <<< "${PATH}"
  for d in "${dirs[@]}"; do
    [[ -x "${d}/kind" ]] || p+="${p:+:}${d}"
  done
  p="${bin}:${p}"
  [ "$(PATH="${p}" && command -v kind)" = "${bin}/kind" ]
  PATH="${p}" run doctor::_kind "" "${_kind_purpose}"
  assert_success
  assert_output "  ! kind not found (optional) — ${_kind_purpose}"
}

# Before a kind that can run, a kind without the exec bit is skipped: the
# first kind on PATH that can run is the one that runs.
@test "doctor::_kind runs the first kind on PATH that can run" {
  local bin="${BATS_TEST_TMPDIR}/noexec-bin"
  mkdir -p "${bin}"
  printf '#!/bin/sh\necho kind v0.17.0\n' > "${bin}/kind"
  chmod 0644 "${bin}/kind"
  _kind_stub 'kind v0.18.0 go1.20.2 linux/amd64\n'
  PATH="${bin}:${BATS_TEST_TMPDIR}/kind-bin:${PATH}" run doctor::_kind "" "${_kind_purpose}"
  _kind_pass
}

# _kind_domains — one domain of each kind that doctor::_kind_fails tells
# apart (Go twin: doctorKindDomains in internal/cli/cmd_doctor_test.go).
_kind_domains() {
  local d
  for d in lo.dev metal.dev app.dev nokind.dev badkind.dev; do
    mkdir -p "${PATH_CLUSTERS}/${d}"
  done
  printf 'kind: Lo\nmetadata:\n  name: lo\n' > "${PATH_CLUSTERS}/lo.dev/cluster.lok8s.yaml"
  printf 'kind: KubeOne\nmetadata:\n  name: metal\n' > "${PATH_CLUSTERS}/metal.dev/cluster.lok8s.yaml"
  printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: lo.dev\n' > "${PATH_CLUSTERS}/app.dev/deploy.lok8s.yaml"
  printf 'metadata:\n  name: nokind\n' > "${PATH_CLUSTERS}/nokind.dev/cluster.lok8s.yaml"
  printf 'kind: ../lo\nmetadata:\n  name: badkind\n' > "${PATH_CLUSTERS}/badkind.dev/cluster.lok8s.yaml"
}

# An older kind fails where lo up runs kind (a domain of the Lo driver),
# and where there is no active domain: no name, no spec, or a spec whose
# driver doctor cannot read. For a domain of another driver, a deploy
# domain included, it is a warning with the same text. The minimum kind
# passes for every domain. Go twin: TestDoctorToolsSectionOlderKindByDomain.
@test "doctor::_kind fails an older kind only where the domain needs it" {
  _kind_domains
  local d
  for d in lo.dev "" missing.dev nokind.dev badkind.dev; do
    _kind_domain="${d}" _kind_run 'kind v0.17.0 go1.19.4 linux/amd64\n'
    _kind_older v0.17.0
  done
  for d in metal.dev app.dev; do
    _kind_domain="${d}" _kind_run 'kind v0.17.0 go1.19.4 linux/amd64\n'
    assert_success
    assert_output "  ! kind v0.17.0 — ${_kind_purpose}; ${_kind_need} (fix: b update kind)"
  done
  for d in lo.dev "" missing.dev nokind.dev badkind.dev metal.dev app.dev; do
    _kind_domain="${d}" _kind_run 'kind v0.18.0 go1.20.2 linux/amd64\n'
    _kind_pass
  done
}

# Without yq, doctor::_kind_fails does not read the driver of a domain with
# a cluster.lok8s.yaml: an older kind is a warning there, and a second
# warning says why. A deploy domain needs no yq, and with no domain (no
# name, no spec) an older kind still fails. The Go twin reads the driver
# without yq, so it fails an older kind for lo.dev (see doctor::_kind_fails).
@test "doctor::_kind without yq warns for a cluster spec and says why" {
  _kind_domains
  _kind_stub 'kind v0.17.0 go1.19.4 linux/amd64\n'
  local tools="${BATS_TEST_TMPDIR}/no-yq-bin" t d
  mkdir -p "${tools}"
  for t in bash env cat; do
    ln -s "$(command -v "${t}")" "${tools}/${t}"
  done
  local p="${BATS_TEST_TMPDIR}/kind-bin:${tools}"
  [ -z "$(PATH="${p}" && command -v yq)" ]
  for d in lo.dev metal.dev nokind.dev badkind.dev; do
    PATH="${p}" run doctor::_kind "${d}" "${_kind_purpose}"
    assert_success
    assert_output "  ! kind v0.17.0 — ${_kind_purpose}; ${_kind_need} (fix: b update kind)
  ! kind: the driver of ${d} not checked: yq not found"
  done
  PATH="${p}" run doctor::_kind app.dev "${_kind_purpose}"
  assert_success
  assert_output "  ! kind v0.17.0 — ${_kind_purpose}; ${_kind_need} (fix: b update kind)"
  for d in "" missing.dev; do
    PATH="${p}" run doctor::_kind "${d}" "${_kind_purpose}"
    _kind_older v0.17.0
  done
}

# main::doctor gives the active domain (DOMAIN_NAME here, as the entrypoint
# exports it) to the kind line: an older kind is a failure for a domain of
# the Lo driver and a warning for a KubeOne domain. Every required tool
# but yq is a stub that exits 99, and mkcert too, so doctor reaches no
# cluster (the domain readers need the real yq, and setup skips without
# it). So the exit status comes from the kind line alone, and the minimum
# kind passes for the Lo domain. Go twin:
# TestDoctorKindLineReadsTheActiveDomain.
@test "lo doctor reads the active domain for the kind line" {
  _kind_domains
  _kind_stub 'kind v0.17.0 go1.19.4 linux/amd64\n'
  local bin="${BATS_TEST_TMPDIR}/kind-bin" tool
  for tool in argsh kustomize kubectl envsubst docker jq mkcert; do
    printf '#!/bin/sh\nexit 99\n' > "${bin}/${tool}"
    chmod +x "${bin}/${tool}"
  done
  :args() { :; }
  local line="kind v0.17.0 — ${_kind_purpose}; ${_kind_need} (fix: b update kind)"
  DOMAIN_NAME=lo.dev PATH="${bin}:${PATH}" run main::doctor < /dev/null
  assert_failure 1
  assert_line "  ✗ ${line}"
  refute_line --partial "MISSING (required)"
  DOMAIN_NAME=metal.dev PATH="${bin}:${PATH}" run main::doctor < /dev/null
  assert_success
  assert_line "  ! ${line}"
  _kind_stub 'kind v0.18.0 go1.20.2 linux/amd64\n'
  DOMAIN_NAME=lo.dev PATH="${bin}:${PATH}" run main::doctor < /dev/null
  assert_success
  assert_line "  ✓ kind — ${_kind_purpose}"
}

# ── doctor::_sso_check: the SecurityPolicy CRD for sso-gate (issue #141) ─────
# The cases are shared with the Go twin (internal/cli/cmd_doctor_ssogate_test.go)
# under tests/fixtures/doctor-sso-gate/cases/<case>/ (the files are listed in
# that test). kubectl is a stand-in script of this test, first on PATH: it
# records its argv and answers the CRD read from the case (`json <file>`,
# `none`, `blank` or `fail <rc> <stderr>`); any other kubectl command exits
# 99. Only this file knows one more answer:
#   hang <s> <json file>   the read ends after <s> seconds
# No real kubectl runs and no cluster is reached.

_sso_no_answer="kubectl did not answer within 30 s (the cluster API, or a credential plugin that waits for a login)"
_sso_read="get customresourcedefinitions.apiextensions.k8s.io securitypolicies.gateway.envoyproxy.io --ignore-not-found -o json --request-timeout=10s"

# _sso_setup — the bootstrap entry parser, a project under BATS_TEST_TMPDIR
# and the stand-in kubectl.
_sso_setup() {
  :usage() { :; }
  :args() { shift; }
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  if ! command -v jq &>/dev/null; then skip "jq not available"; fi
  export SSO_FIX="${_PROJECT_ROOT}/tests/fixtures/doctor-sso-gate"
  export PATH_BASE="${BATS_TEST_TMPDIR}/project"
  export PATH_CLUSTERS="${PATH_BASE}/clusters"
  export PATH_LOK8S="${PATH_BASE}/.lok8s"
  unset LOK8S_CLUSTER_NAME
  export CALLS="${BATS_TEST_TMPDIR}/kubectl.calls"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  SSO_STUB="${BATS_TEST_TMPDIR}/sso-stub"
  mkdir -p "${SSO_STUB}"
  cat > "${SSO_STUB}/kubectl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${CALLS}"
[[ "${3:-} ${4:-}" == "get customresourcedefinitions.apiextensions.k8s.io" ]] || exit 99
if [[ ! -f "${CASE}/crd" ]]; then
  echo "unexpected read"
  exit 0
fi
read -r kind a b < "${CASE}/crd"
case "${kind}" in
  json) cat "${SSO_FIX}/json/${a}" ;;
  none) ;;
  blank) printf ' \n\t\r\n' ;;
  fail) printf '%s\n' "${b}" >&2; exit "${a}" ;;
  hang) sleep "${a}" 3>&-; cat "${SSO_FIX}/json/${b}" ;;
esac
STUB
  chmod +x "${SSO_STUB}/kubectl"
  export PATH="${SSO_STUB}:${PATH}"
  [[ "$(command -v kubectl)" == "${SSO_STUB}/kubectl" ]] || fail "the stand-in kubectl is not first on PATH"
}

# _sso_case_dir <dir> — the domain sso.example of the case under PATH_BASE.
_sso_case_dir() {
  export CASE="${1}"
  [[ -f "${CASE}/want" ]] || fail "no case ${CASE}"
  SSO_KC_NAME=sso-test
  if [[ -f "${CASE}/kubeconfig-name" ]]; then SSO_KC_NAME="$(cat "${CASE}/kubeconfig-name")"; fi
  mkdir -p "${PATH_CLUSTERS}/sso.example"
  local f
  for f in cluster.lok8s.yaml deploy.lok8s.yaml; do
    if [[ -f "${CASE}/${f}" ]]; then cp "${CASE}/${f}" "${PATH_CLUSTERS}/sso.example/${f}"; fi
  done
  if [[ ! -f "${CASE}/no-kubeconfig" ]]; then
    mkdir -p "${PATH_BASE}/.kubeconfig"
    printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/${SSO_KC_NAME}.yaml"
  fi
}

# _sso_assert_case — the output and the read of the last run against the
# case, and no temp dir left.
_sso_assert_case() {
  assert_success
  assert_output "$(cat "${CASE}/want")"
  refute_output --partial CANARY
  local want_calls=""
  if [[ -f "${CASE}/crd" ]]; then
    want_calls="--kubeconfig ${PATH_BASE}/.kubeconfig/${SSO_KC_NAME}.yaml ${_sso_read}"
  fi
  assert_equal "$(cat "${CALLS}" 2>/dev/null)" "${want_calls}"
  local left
  left="$(find "${TMPDIR}" -maxdepth 1 -name 'lo-doctor-rbac.*')"
  [[ -z "${left}" ]] || fail "temp dir left: ${left}"
}

# _sso_case <name> — run the check for the shared case.
_sso_case() {
  _sso_setup
  _sso_case_dir "${SSO_FIX}/cases/${1}"
  run doctor::_sso_check sso.example
  _sso_assert_case
}

# _sso_path_without <tool> — prints a PATH of one new dir with a link to each
# program of PATH except <tool> (the first one of a name wins, so the
# stand-in kubectl too).
_sso_path_without() {
  local dir="${BATS_TEST_TMPDIR}/sso-path-without-${1}" d f
  mkdir "${dir}" || fail "${dir} exists"
  local -a dirs=() files=()
  IFS=: read -ra dirs <<< "${PATH}"
  for d in "${dirs[@]}"; do
    [[ -d "${d}" ]] || continue
    files=()
    for f in "${d}"/*; do
      [[ "${f##*/}" == "${1}" ]] || files+=("${f}")
    done
    (( ${#files[@]} == 0 )) || ln -s "${files[@]}" "${dir}/" 2>/dev/null || true
  done
  [[ ! -e "${dir}/${1}" ]] || fail "${1} is still in ${dir}"
  echo "${dir}"
}

@test "sso: the CRD that declares spec.mergeType passes" { _sso_case declared; }
@test "sso: the CRD without spec.mergeType fails, and the check returns 0" { _sso_case not-declared; }
@test "sso: v1alpha1 after another version is read by its name" { _sso_case v1alpha1-last; }
@test "sso: a first version with the field does not hide a v1alpha1 without it" { _sso_case first-version-declares; }
@test "sso: the addon as a map entry counts" { _sso_case map-entry; }
@test "sso: a v1alpha1 without a schema fails" { _sso_case no-schema; }
@test "sso: a null field fails" { _sso_case field-null; }
@test "sso: a key in another case is not the field" { _sso_case field-key-case; }
@test "sso: no CRD skips" { _sso_case no-crd; }
@test "sso: an empty List is no CRD" { _sso_case no-crd-empty-list; }
@test "sso: JSON white space only is no CRD" { _sso_case no-crd-white-space; }
@test "sso: a CRD without v1alpha1 is not checked" { _sso_case no-v1alpha1; }
@test "sso: mergeType only at a nested place is not the field" { _sso_case field-nested-only; }
@test "sso: a v1alpha1 that is not served is not checked" { _sso_case v1alpha1-not-served; }
@test "sso: not served wins over a v1alpha1 without the field" { _sso_case v1alpha1-not-served-without-field; }
@test "sso: a storage version without the field fails" { _sso_case stored-without-field; }
@test "sso: a storage version with the field passes" { _sso_case stored-with-field; }
@test "sso: no field in v1alpha1 and in the storage version gives the line of v1alpha1" { _sso_case absent-in-v1alpha1-and-storage; }
@test "sso: a forbidden read prints the reason of the CRD" { _sso_case read-forbidden; }
@test "sso: an unauthorized read prints the reason of the CRD" { _sso_case read-unauthorized; }
@test "sso: a refused connection prints the fixed reason" { _sso_case read-refused; }
@test "sso: another kubectl failure prints the reason of the CRD" { _sso_case read-other; }
@test "sso: an answer that is not JSON is invalid" { _sso_case read-not-json; }
@test "sso: an answer that is not an object is invalid" { _sso_case read-not-an-object; }
@test "sso: a List with an item is invalid" { _sso_case read-list-with-item; }
@test "sso: a second JSON document is invalid" { _sso_case read-two-documents; }
@test "sso: a leading byte-order mark is invalid" { _sso_case read-bom; }
@test "sso: a NaN is invalid" { _sso_case read-nan; }
@test "sso: a number out of the float64 range is invalid" { _sso_case read-out-of-range; }
@test "sso: two versions named v1alpha1 are invalid" { _sso_case read-two-v1alpha1; }
@test "sso: a version name that is not a string is invalid" { _sso_case read-name-not-a-string; }
@test "sso: a step of the path that is not an object is invalid" { _sso_case read-step-not-an-object; }
@test "sso: a field that is not an object is invalid" { _sso_case read-field-not-an-object; }
@test "sso: versions that are not a list are invalid" { _sso_case read-versions-not-a-list; }
@test "sso: a version that is not an object is invalid" { _sso_case read-version-not-an-object; }
@test "sso: a served flag that is not a boolean is invalid" { _sso_case read-served-not-a-boolean; }
@test "sso: a version without the storage flag is invalid" { _sso_case read-storage-absent; }
@test "sso: no storage version is invalid" { _sso_case read-no-storage-version; }
@test "sso: two storage versions are invalid" { _sso_case read-two-storage-versions; }
@test "sso: a field of the storage version that is not an object is invalid" { _sso_case read-stored-field-not-an-object; }
@test "sso: no sso-gate in spec.bootstrap prints nothing and reads nothing" { _sso_case addon-not-in-spec; }
@test "sso: the default spec.bootstrap has no sso-gate" { _sso_case default-bootstrap; }
@test "sso: sso-gate as a path entry does not count" { _sso_case addon-as-path-entry; }
@test "sso: no kubeconfig skips" { _sso_case no-kubeconfig; }
@test "sso: a kubeconfig path with a control character skips" { _sso_case kubeconfig-control-char; }
@test "sso: a deploy domain prints nothing" { _sso_case deploy-domain; }
@test "sso: no cluster spec prints nothing" { _sso_case no-cluster-spec; }
@test "sso: an unreadable cluster spec prints nothing" { _sso_case spec-cannot-be-read; }

# Each shared case has a test above, so the bats twin runs what the Go twin
# runs.
@test "sso: every shared case has a bats test" {
  local dir name missing=""
  for dir in "${_PROJECT_ROOT}"/tests/fixtures/doctor-sso-gate/cases/*/; do
    name="$(basename "${dir}")"
    grep -qE "\{ _sso_case ${name}; \}\$" "${BATS_TEST_FILENAME}" || missing+=" ${name}"
  done
  [[ -z "${missing}" ]] || fail "cases without a bats test:${missing}"
}

@test "sso: no active domain prints no line" {
  _sso_setup
  run doctor::_sso_check ""
  assert_success
  assert_output ""
  [[ ! -e "${CALLS}" ]] || fail "a read ran: $(cat "${CALLS}")"
}

@test "sso: no jq says not checked and reads nothing" {
  _sso_setup
  _sso_case_dir "${SSO_FIX}/cases/declared"
  local p; p="$(_sso_path_without jq)"
  PATH="${p}" run doctor::_sso_check sso.example
  assert_success
  assert_output "  ! sso-gate SecurityPolicy CRD not checked: jq not found"
  [[ ! -e "${CALLS}" ]] || fail "a read ran: $(cat "${CALLS}")"
}

@test "sso: no kubectl says not checked" {
  _sso_setup
  _sso_case_dir "${SSO_FIX}/cases/declared"
  local p; p="$(_sso_path_without kubectl)"
  PATH="${p}" run doctor::_sso_check sso.example
  assert_success
  assert_output "  ! sso-gate SecurityPolicy CRD not checked: kubectl not found"
  [[ ! -e "${CALLS}" ]] || fail "a read ran: $(cat "${CALLS}")"
}

# The bound stops a read that does not end (a credential plugin that waits
# for a login). The stand-in answers the declared CRD after 5 s: without the
# bound, the check passes and this fails.
@test "sso: the bound stops a hung read" {
  _sso_setup
  doctor::_rbac_bound() { echo 1; }
  local dir="${BATS_TEST_TMPDIR}/case-hang"
  mkdir -p "${dir}"
  cp "${SSO_FIX}/cases/declared/cluster.lok8s.yaml" "${dir}/"
  echo "hang 5 declared.json" > "${dir}/crd"
  printf '%s\n' "  ! sso-gate SecurityPolicy CRD not checked: ${_sso_no_answer}" > "${dir}/want"
  _sso_case_dir "${dir}"
  local start="${SECONDS}"
  run doctor::_sso_check sso.example
  _sso_assert_case
  (( SECONDS - start < 5 )) || fail "the check waited for the read: $(( SECONDS - start )) s"
}

# The ✗ line is advisory: main::doctor keeps its exit status. Every
# required tool but yq and jq is a stub that exits 99, kind is the minimum
# release, and the stand-in kubectl answers only the CRD read, so doctor
# reaches no cluster.
@test "sso: lo doctor keeps its exit status with the ✗ line" {
  _sso_setup
  _sso_case_dir "${SSO_FIX}/cases/not-declared"
  _kind_stub 'kind v0.18.0 go1.20.2 linux/amd64\n'
  local bin="${BATS_TEST_TMPDIR}/kind-bin" tool
  for tool in argsh kustomize envsubst docker mkcert; do
    printf '#!/bin/sh\nexit 99\n' > "${bin}/${tool}"
    chmod +x "${bin}/${tool}"
  done
  :args() { :; }
  DOMAIN_NAME=sso.example PATH="${bin}:${PATH}" run main::doctor < /dev/null
  assert_success
  assert_line "$(cat "${CASE}/want")"
  assert_line "doctor: all required checks passed."
  grep -qxF -- "--kubeconfig ${PATH_BASE}/.kubeconfig/sso-test.yaml ${_sso_read}" "${CALLS}" \
    || fail "no CRD read in: $(cat "${CALLS}")"
}

# ── doctor::_suc_check: the kubectl range of system-upgrade-controller ───────
# The cases are shared with the Go twin (internal/cli/cmd_doctor_suc_test.go)
# and with the lint tests of both twins, under
# tests/fixtures/doctor-suc-kubectl/cases/<case>/ (the files are listed in the
# Go test). The check reads the spec only: no kubectl runs.

# _suc_setup — the bootstrap entry parser and a project under BATS_TEST_TMPDIR.
_suc_setup() {
  :usage() { :; }
  :args() { shift; }
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export SUC_FIX="${_PROJECT_ROOT}/tests/fixtures/doctor-suc-kubectl"
  export PATH_BASE="${BATS_TEST_TMPDIR}/project"
  export PATH_CLUSTERS="${PATH_BASE}/clusters"
  export PATH_LOK8S="${PATH_BASE}/.lok8s"
  unset LOK8S_CLUSTER_NAME
}

# _suc_case <name> — run the check for the shared case.
_suc_case() {
  _suc_setup
  local case_dir="${SUC_FIX}/cases/${1}"
  [[ -f "${case_dir}/want" ]] || fail "no case ${1}"
  mkdir -p "${PATH_CLUSTERS}/suc.example"
  cp "${case_dir}/cluster.lok8s.yaml" "${PATH_CLUSTERS}/suc.example/"
  run doctor::_suc_check suc.example
  assert_success
  assert_output "$(cat "${case_dir}/want")"
}

@test "suc: 1.31 is below the range and warns" { _suc_case v1.31; }
@test "suc: 1.32 is below the range and warns" { _suc_case v1.32; }
@test "suc: 1.33 is the first minor of the range" { _suc_case v1.33; }
@test "suc: 1.35 without a v passes on KubeOne" { _suc_case v1.35; }
@test "suc: 1.37 is the last minor of the range" { _suc_case v1.37; }
@test "suc: 1.38 is above the range and warns" { _suc_case v1.38; }
@test "suc: a version with a digest is read without the digest" { _suc_case digest; }
@test "suc: the addon as a map entry counts" { _suc_case map-entry; }
@test "suc: a Lo cluster without a version names the kind default" { _suc_case unset; }
@test "suc: a KubeOne cluster without a version prints no line" { _suc_case unset-kubeone; }
@test "suc: a spec.bootstrap without the addon prints no line" { _suc_case no-entry; }
@test "suc: the addon as a path entry does not count" { _suc_case path-entry; }
@test "suc: a version that is not vX.Y or vX.Y.Z is not checked" { _suc_case not-a-version; }
@test "suc: a version that cannot be read is not checked" { _suc_case unreadable; }
@test "suc: a pre-release tag is not checked" { _suc_case rc; }
@test "suc: a YAML float such as 1.30 is read as written and warns" { _suc_case yaml-float; }
@test "suc: a capital V is not checked" { _suc_case capital-v; }
@test "suc: a minor with a leading zero is not checked" { _suc_case leading-zero; }
@test "suc: a short digest is not checked" { _suc_case short-digest; }
@test "suc: major 2 is outside the range and warns" { _suc_case major-2; }
@test "suc: a version with a leading space is not checked" { _suc_case whitespace; }
@test "suc: the addon with a trailing slash counts" { _suc_case trailing-slash; }
@test "suc: the addon with a trailing /. counts" { _suc_case trailing-dot; }

@test "suc: every shared case has a bats test" {
  local dir name missing=""
  for dir in "${_PROJECT_ROOT}"/tests/fixtures/doctor-suc-kubectl/cases/*/; do
    name="$(basename "${dir}")"
    grep -qE "\{ _suc_case ${name//./\\.}; \}\$" "${BATS_TEST_FILENAME}" || missing+=" ${name}"
  done
  [[ -z "${missing}" ]] || fail "cases without a bats test:${missing}"
}

@test "suc: no active domain and a deploy domain print no line" {
  _suc_setup
  run doctor::_suc_check ""
  assert_success
  assert_output ""
  mkdir -p "${PATH_CLUSTERS}/deploy.example"
  cp "${SUC_FIX}/cases/v1.31/cluster.lok8s.yaml" "${PATH_CLUSTERS}/deploy.example/deploy.lok8s.yaml"
  run doctor::_suc_check deploy.example
  assert_success
  assert_output ""
}

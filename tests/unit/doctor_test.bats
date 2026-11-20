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

# main::doctor gives the active domain (DOMAIN_NAME here, as the entrypoint
# exports it) to the kind line: an older kind is a failure for a domain of
# the Lo driver and a warning for a KubeOne domain. kubectl, mkcert and
# docker are stubs that exit 99, so doctor reaches no cluster. Go twin:
# TestDoctorKindLineReadsTheActiveDomain.
@test "lo doctor reads the active domain for the kind line" {
  _kind_domains
  _kind_stub 'kind v0.17.0 go1.19.4 linux/amd64\n'
  local bin="${BATS_TEST_TMPDIR}/kind-bin" tool
  for tool in kubectl mkcert docker; do
    printf '#!/bin/sh\nexit 99\n' > "${bin}/${tool}"
    chmod +x "${bin}/${tool}"
  done
  :args() { :; }
  local line="kind v0.17.0 — ${_kind_purpose}; ${_kind_need} (fix: b update kind)"
  DOMAIN_NAME=lo.dev PATH="${bin}:${PATH}" run main::doctor < /dev/null
  assert_failure
  assert_line "  ✗ ${line}"
  DOMAIN_NAME=metal.dev PATH="${bin}:${PATH}" run main::doctor < /dev/null
  assert_line "  ! ${line}"
}

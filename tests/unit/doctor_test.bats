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

  # provider contract helpers (read_name/write_config/load) + the doctor lib.
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/provider.sh"
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
  # provider::doctor's ok/warn lines rendered in the lib's ✓/! style.
  assert_output --partial "hcloud API reachable"
  assert_output --partial "Robot creds unset"
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

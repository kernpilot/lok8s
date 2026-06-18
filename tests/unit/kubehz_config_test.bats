#!/usr/bin/env bats
# kubehz_config_test.bats — unit tests for kubehz config parsing and validation

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  # Create domain structure
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev"
}

teardown() {
  teardown_tmpdir
}

# ── read_config: defaults when kubehz block is absent ────

@test "read_config: defaults to hosting=self, access=none when kubehz block absent" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "" ;;
      '.spec.kubehz.access') echo "null" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  [ "${LOK8S_KUBEHZ_HOSTING}" = "self" ]
  [ "${LOK8S_KUBEHZ_ACCESS}" = "none" ]
  [ "${LOK8S_KUBEHZ_API_URL}" = "" ]
}

# ── read_config: hosted with managed access ──────────────

@test "read_config: reads hosted config with apiUrl" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "hosted" ;;
      '.spec.kubehz.apiUrl // ""') echo "https://api.kubehz.dev" ;;
      '.spec.kubehz.access') echo "managed" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  [ "${LOK8S_KUBEHZ_HOSTING}" = "hosted" ]
  [ "${LOK8S_KUBEHZ_ACCESS}" = "managed" ]
  [ "${LOK8S_KUBEHZ_API_URL}" = "https://api.kubehz.dev" ]
}

# ── read_config: self with registered access ─────────────

@test "read_config: reads self-hosted registered config" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "https://api.kubehz.dev" ;;
      '.spec.kubehz.access') echo "registered" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  [ "${LOK8S_KUBEHZ_HOSTING}" = "self" ]
  [ "${LOK8S_KUBEHZ_ACCESS}" = "registered" ]
  [ "${LOK8S_KUBEHZ_API_URL}" = "https://api.kubehz.dev" ]
}

# ── read_config: empty access treated as none ────────────

@test "read_config: empty access string defaults to none" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "" ;;
      '.spec.kubehz.access') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  [ "${LOK8S_KUBEHZ_ACCESS}" = "none" ]
}

# ── validate_config: valid self/none passes ──────────────

@test "validate_config: self/none passes validation" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_success
}

# ── validate_config: valid hosted/managed passes ─────────

@test "validate_config: hosted/managed with apiUrl passes" {
  yq() {
    case "$2" in
      '.spec.runner // ""') echo "hetzner" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="hosted"
  export LOK8S_KUBEHZ_ACCESS="managed"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_success
}

# ── validate_config: invalid hosting value ───────────────

@test "validate_config: rejects invalid hosting value" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="invalid"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "invalid spec.kubehz.hosting: invalid"
}

# ── validate_config: invalid access value ────────────────

@test "validate_config: rejects invalid access value" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="badvalue"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "invalid spec.kubehz.access: badvalue"
}

# ── validate_config: hosted requires apiUrl ──────────────

@test "validate_config: hosted without apiUrl fails" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="hosted"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "spec.kubehz.apiUrl is required when hosting: hosted"
}

@test "validate_config: plain-http apiUrl is rejected" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="hosted"
  export LOK8S_KUBEHZ_ACCESS="managed"
  export LOK8S_KUBEHZ_API_URL="http://api.kubehz.dev"
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "must use HTTPS"
}

# ── validate_config: registered requires apiUrl ──────────

@test "validate_config: registered access without apiUrl fails" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="registered"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "spec.kubehz.apiUrl is required when access: registered"
}

# ── validate_config: managed requires apiUrl ─────────────

@test "validate_config: managed access without apiUrl fails" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="managed"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "spec.kubehz.apiUrl is required when access: managed"
}

# ── validate_config: Lo + hosted requires runner ─────────

@test "validate_config: Lo kind with hosted requires spec.runner" {
  yq() {
    case "$2" in
      .kind) echo "Lo" ;;
      '.spec.runner // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="hosted"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export LOK8S_SPEC_KIND="Lo"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "hosting: hosted with kind: Lo requires spec.runner configuration"
}

# ── validate_config: Lo + hosted with runner passes ──────

@test "validate_config: Lo kind with hosted and runner passes" {
  yq() {
    case "$2" in
      '.spec.runner // ""') echo "hetzner" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="hosted"
  export LOK8S_KUBEHZ_ACCESS="managed"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export LOK8S_SPEC_KIND="Lo"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_success
}

# ── validate_config: self/none with apiUrl passes ────────

@test "validate_config: self/none with apiUrl still passes (apiUrl optional for none)" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_success
}

# ── validate_config: KubeOne + hosted does not require runner ─

@test "validate_config: KubeOne kind with hosted does not require runner" {
  yq() {
    case "$2" in
      '.spec.runner // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="hosted"
  export LOK8S_KUBEHZ_ACCESS="managed"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"

  run kubehz::validate_config
  assert_success
}

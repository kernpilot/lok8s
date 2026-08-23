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

  # Create domain structure. The spec file must EXIST (content is irrelevant to
  # the tests that stub yq): read_config refuses a missing/unreadable file
  # instead of "succeeding" with empty vars.
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev"
  : > "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
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

# ── read_config: a missing spec file is an ERROR ─────────
# The guard `kubehz::read_config … || return 1` in the capi/kubeone drivers is
# only real if read_config can actually fail. It could not, historically: every
# yq failure happened inside an assignment and the trailing `export` reset the
# function's status to 0, so a missing file "succeeded" with empty vars and sent
# the driver down the wrong branch.

@test "read_config: fails on a missing spec file" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::read_config "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/does-not-exist.yaml"
  assert_failure
  assert_output --partial "cannot read cluster spec"
}

@test "read_config: propagates a yq failure instead of exporting empty vars" {
  yq() { return 1; }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::read_config "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
  assert_failure
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
      '.kind // ""') echo "Lo" ;;
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

# ── read_config: upgrade policy defaults ─────────────────
# The upgrades/maintenanceWindow blocks are declarative passthrough — read_config
# only needs the defaults right (channel=none, defer=window, no exclusions) and
# the same null/empty tolerance the other keys get.

@test "read_config: upgrades default to channel=none, defer=window, no exclusions" {
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

  [ "${LOK8S_KUBEHZ_UPGRADES_CHANNEL}" = "none" ]
  [ "${LOK8S_KUBEHZ_UPGRADES_DEFER}" = "window" ]
  [ "${LOK8S_KUBEHZ_MW_EXCLUSIONS}" = "" ]
}

# ── read_config: upgrade policy explicit values ──────────

@test "read_config: reads explicit upgrades channel, defer and exclusions" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "" ;;
      '.spec.kubehz.access') echo "null" ;;
      '.spec.kubehz.upgrades.channel // "none"') echo "minor" ;;
      '.spec.kubehz.upgrades.defer // "window"') echo "immediate" ;;
      '(.spec.kubehz.maintenanceWindow.exclusions // []) | (select(type == "!!seq") // [.]) | .[]') printf '%s\n' "2026-12-20/2027-01-06" "2027-04-03" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  [ "${LOK8S_KUBEHZ_UPGRADES_CHANNEL}" = "minor" ]
  [ "${LOK8S_KUBEHZ_UPGRADES_DEFER}" = "immediate" ]
  [ "${LOK8S_KUBEHZ_MW_EXCLUSIONS}" = "2026-12-20/2027-01-06
2027-04-03" ]
}

# ── validate_config: valid upgrade policy passes ─────────

@test "validate_config: valid upgrades channel/defer and exclusions pass" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"
  export LOK8S_KUBEHZ_UPGRADES_CHANNEL="patch"
  export LOK8S_KUBEHZ_UPGRADES_DEFER="immediate"
  export LOK8S_KUBEHZ_MW_EXCLUSIONS="2026-12-20/2027-01-06
2027-04-03"

  run kubehz::validate_config
  assert_success
}

# ── validate_config: unset upgrade vars use defaults ─────
# validate_config is reachable without read_config; an unset policy means
# "not chosen", never "invalid" — the same tolerance `agent` gets.

@test "validate_config: unset upgrade policy vars pass through defaults" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"
  unset LOK8S_KUBEHZ_UPGRADES_CHANNEL LOK8S_KUBEHZ_UPGRADES_DEFER LOK8S_KUBEHZ_MW_EXCLUSIONS

  run kubehz::validate_config
  assert_success
}

# ── validate_config: invalid upgrades.channel ────────────

@test "validate_config: rejects invalid upgrades.channel" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"
  export LOK8S_KUBEHZ_UPGRADES_CHANNEL="major"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "invalid spec.kubehz.upgrades.channel: major"
}

# ── validate_config: invalid upgrades.defer ──────────────

@test "validate_config: rejects invalid upgrades.defer" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"
  export LOK8S_KUBEHZ_UPGRADES_DEFER="later"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "invalid spec.kubehz.upgrades.defer: later"
}

# ── validate_config: malformed exclusion entry ───────────

@test "validate_config: rejects a malformed maintenanceWindow exclusion" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self"
  export LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_KIND="KubeOne"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"
  export LOK8S_KUBEHZ_MW_EXCLUSIONS="2026-12-20/2027-01-06
christmas week"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "invalid spec.kubehz.maintenanceWindow.exclusions entry: christmas week"
}

# ── exclusions against REAL yq (no stub): fixture-driven shapes ──────────

@test "read_config: real-yq fixture — list, scalar coercion, and absent exclusions" {
  # No yq stub here on purpose: the round-1 review found the parsing was never
  # exercised against the real binary. Three shapes, one contract:
  #   list   → entries verbatim
  #   scalar → coerced to a single entry (validate rejects CONTENT with our
  #            message — the splat must not abort with yq's raw error)
  #   absent → empty
  local f="${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  printf 'spec:\n  kubehz:\n    maintenanceWindow:\n      exclusions: ["2026-01-01", "2026-02-01/2026-02-03"]\n' > "${f}"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  kubehz::read_config "${f}"
  [ "$(printf '%s\n' "${LOK8S_KUBEHZ_MW_EXCLUSIONS}" | wc -l)" -eq 2 ]

  printf 'spec:\n  kubehz:\n    maintenanceWindow:\n      exclusions: "2026-01-01"\n' > "${f}"
  kubehz::read_config "${f}"
  [ "${LOK8S_KUBEHZ_MW_EXCLUSIONS}" = "2026-01-01" ]

  printf 'spec: {}\n' > "${f}"
  kubehz::read_config "${f}"
  [ -z "${LOK8S_KUBEHZ_MW_EXCLUSIONS}" ]
}

@test "validate_config: real-yq fixture — scalar-coerced INVALID exclusion fails with our message" {
  local f="${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
  printf 'spec:\n  kubehz:\n    maintenanceWindow:\n      exclusions: "not-a-date"\n' > "${f}"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  kubehz::read_config "${f}"
  export LOK8S_SPEC_FILE="${f}"
  run kubehz::validate_config
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"maintenanceWindow.exclusions entry: not-a-date"* ]]
}

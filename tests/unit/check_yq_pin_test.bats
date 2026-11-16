#!/usr/bin/env bats
# check_yq_pin_test.bats — hack/check-yq-pin.sh, the CI check that the
# installed yq is the release .bin/b.yaml pins (b install exits 0 when a
# binary fails, and the bats container then runs its own yq).

setup() {
  load "../test_helper"
  setup_tmpdir
  BIN="${BATS_TEST_TMPDIR}/bin"
  mkdir -p "${BIN}"
  cp "${_PROJECT_ROOT}/.bin/b.yaml" "${BIN}/b.yaml"
  # The pin, read from mise.toml (the Go drift test ties it to .bin/b.yaml).
  PIN="v$(sed -n 's/^yq *= *"\(.*\)"/\1/p' "${_PROJECT_ROOT}/mise.toml")"
  [[ "${PIN}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "no yq pin in mise.toml: ${PIN}" >&2; return 1; }
}

# _yq_fake <version> — a yq in BIN that reports <version>.
_yq_fake() {
  printf '#!/usr/bin/env bash\necho "yq (https://github.com/mikefarah/yq/) version %s"\n' "${1}" > "${BIN}/yq"
  chmod +x "${BIN}/yq"
}

@test "check-yq-pin: the pinned yq passes (the real .bin/b.yaml)" {
  _yq_fake "${PIN}"
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_success
  assert_output "yq ${PIN} in ${BIN} is the pinned release"
}

@test "check-yq-pin: another release fails and names both versions" {
  _yq_fake v4.52.5
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_failure
  assert_output "::error::${BIN}/yq is v4.52.5, but ${BIN}/b.yaml pins ${PIN}: b install did not install the pinned yq"
}

@test "check-yq-pin: no yq in the bin directory fails" {
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_failure
  assert_output --partial "${BIN}/yq is not installed"
}

@test "check-yq-pin: a yq whose --version fails fails" {
  printf '#!/usr/bin/env bash\necho "panic: boom" >&2\nexit 2\n' > "${BIN}/yq"
  chmod +x "${BIN}/yq"
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_failure
  assert_output --partial "${BIN}/yq --version failed"
}

@test "check-yq-pin: a b.yaml without a yq version fails, another entry's version does not count" {
  printf 'binaries:\n  kustomize:\n    version: v5.8.1\n  yq: {}\n  jq:\n    version: v1.7\n' > "${BIN}/b.yaml"
  _yq_fake v5.8.1
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_failure
  assert_output "::error::${BIN}/b.yaml pins no yq version (binaries.yq.version)"
}

@test "check-yq-pin: the yq entry's version, quoted and with a comment, is the pin" {
  printf 'binaries:\n  kustomize:\n    version: v5.8.1\n  yq:\n    # the bash readers need it\n    version: "v4.54.3" # pinned\n    groups: [core]\n' > "${BIN}/b.yaml"
  _yq_fake v4.54.3
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_success
  _yq_fake v5.8.1
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_failure
}

@test "check-yq-pin: a yq that differs from the pin only in the patch number fails" {
  local other="${PIN%.*}.$(( ${PIN##*.} + 1 ))"
  _yq_fake "${other}"
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_failure
  assert_output "::error::${BIN}/yq is ${other}, but ${BIN}/b.yaml pins ${PIN}: b install did not install the pinned yq"
}

@test "check-yq-pin: a pre-release of the pinned version is not the pin" {
  _yq_fake "${PIN}-rc1"
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_failure
  assert_output "::error::${BIN}/yq is ${PIN}-rc1, but ${BIN}/b.yaml pins ${PIN}: b install did not install the pinned yq"
}

@test "check-yq-pin: a pin without the leading v matches" {
  printf 'binaries:\n  yq:\n    version: %s\n' "${PIN#v}" > "${BIN}/b.yaml"
  _yq_fake "${PIN}"
  run bash "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${BIN}"
  assert_success
}

@test "check-yq-pin: without an argument it reads the .bin of its repository, as CI runs it" {
  local repo="${BATS_TEST_TMPDIR}/repo"
  mkdir -p "${repo}/hack" "${repo}/.bin"
  cp "${_PROJECT_ROOT}/hack/check-yq-pin.sh" "${repo}/hack/"
  cp "${_PROJECT_ROOT}/.bin/b.yaml" "${repo}/.bin/b.yaml"
  BIN="${repo}/.bin" _yq_fake "${PIN}"
  run bash "${repo}/hack/check-yq-pin.sh"
  assert_success
  assert_output "yq ${PIN} in ${repo}/.bin is the pinned release"
  BIN="${repo}/.bin" _yq_fake v4.0.0
  run bash "${repo}/hack/check-yq-pin.sh"
  assert_failure
}

#!/usr/bin/env bats
# secrets_test.bats — SOPS/age round-trip for .lok8s/libs/secrets.
# Skips when the crypto tools aren't on PATH (e.g. minimal CI containers);
# runs the full encrypt→decrypt round-trip wherever sops + ssh-to-age +
# ssh-keygen are available.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH="${_PROJECT_ROOT}/.bin:${PATH}"
  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_SECRETS="${BATS_TEST_TMPDIR}/.secrets"
  mkdir -p "${PATH_SECRETS}"

  import() { :; };  export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
}

teardown() { teardown_tmpdir; }

require_tools() {
  for t in sops ssh-to-age ssh-keygen; do
    command -v "$t" >/dev/null || skip "$t not installed"
  done
}

@test "secrets: SOPS/age encrypt → decrypt round-trips a value" {
  require_tools
  ssh-keygen -t ed25519 -N '' -C test -f "${BATS_TEST_TMPDIR}/id" -q
  export LOK8S_SSH_KEY="${BATS_TEST_TMPDIR}/id.pub"

  printf 'super-secret-value' > "${PATH_SECRETS}/Secret.app.default.TOKEN"

  run secrets::init --ssh-key "${BATS_TEST_TMPDIR}/id.pub"
  assert_success
  [ -f "${PATH_BASE}/.sops.yaml" ]

  run secrets::encrypt
  assert_success
  [ -f "${PATH_SECRETS}/Secret.app.default.TOKEN.enc" ]

  # Drop the plaintext, then restore it from the .enc with the test identity.
  rm "${PATH_SECRETS}/Secret.app.default.TOKEN"
  SOPS_AGE_KEY="$(ssh-to-age -private-key < "${BATS_TEST_TMPDIR}/id")"
  export SOPS_AGE_KEY
  run secrets::decrypt
  assert_success
  [ "$(cat "${PATH_SECRETS}/Secret.app.default.TOKEN")" = 'super-secret-value' ]
}

@test "secrets: encrypt writes a .secrets/.gitignore that commits only .enc" {
  require_tools
  ssh-keygen -t ed25519 -N '' -C test -f "${BATS_TEST_TMPDIR}/id" -q
  printf 'v' > "${PATH_SECRETS}/Secret.app.default.K"
  secrets::init --ssh-key "${BATS_TEST_TMPDIR}/id.pub"
  run secrets::encrypt
  assert_success
  run cat "${PATH_SECRETS}/.gitignore"
  assert_output --partial '!Secret.*.enc'
}

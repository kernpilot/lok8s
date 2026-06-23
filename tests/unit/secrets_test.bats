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

# --- flat-store shadow / drift detection (no crypto tools needed) -------------
# secrets::check_flat_shadows compares a domain's per-domain store against the
# flat ${PATH_BASE}/.secrets store; PATH_SECRETS already points at the flat one.

_shadow_setup() {
  DOM_DIR="${BATS_TEST_TMPDIR}/clusters/app.example.com"
  mkdir -p "${DOM_DIR}/secrets" "${PATH_SECRETS}"
}

@test "check_flat_shadows: clean when no flat duplicate exists" {
  _shadow_setup
  printf 'v' > "${DOM_DIR}/secrets/Secret.app.default.TOKEN"
  # A global-only flat secret (no per-domain counterpart) must NOT be flagged.
  printf 'global' > "${PATH_SECRETS}/Secret.registries-tls.lok8s-system.tls.crt"
  run secrets::check_flat_shadows "${DOM_DIR}"
  assert_success
  assert_output ''
}

@test "check_flat_shadows: flags an identical flat duplicate as a deprecated shadow" {
  _shadow_setup
  printf 'same' > "${DOM_DIR}/secrets/Secret.app.default.TOKEN"
  printf 'same' > "${PATH_SECRETS}/Secret.app.default.TOKEN"
  run secrets::check_flat_shadows "${DOM_DIR}"
  assert_failure
  assert_output --partial 'Flat-store shadow'
  assert_output --partial 'Secret.app.default.TOKEN'
}

@test "check_flat_shadows: flags a divergent flat duplicate as DRIFT" {
  _shadow_setup
  printf 'per-domain-value' > "${DOM_DIR}/secrets/Secret.app.default.TOKEN"
  printf 'STALE-flat-value' > "${PATH_SECRETS}/Secret.app.default.TOKEN"
  run secrets::check_flat_shadows "${DOM_DIR}"
  assert_failure
  assert_output --partial 'Flat-store DRIFT'
  assert_output --partial 'Secret.app.default.TOKEN'
}

@test "check_flat_shadows: ignores .enc/.sha siblings in the per-domain store" {
  _shadow_setup
  printf 'v'   > "${DOM_DIR}/secrets/Secret.app.default.TOKEN"
  printf 'enc' > "${DOM_DIR}/secrets/Secret.app.default.TOKEN.enc"
  printf 'sha' > "${DOM_DIR}/secrets/Secret.app.default.TOKEN.sha"
  # Flat duplicates of ONLY the .enc/.sha (not the plaintext) are not shadows.
  printf 'enc' > "${PATH_SECRETS}/Secret.app.default.TOKEN.enc"
  printf 'sha' > "${PATH_SECRETS}/Secret.app.default.TOKEN.sha"
  run secrets::check_flat_shadows "${DOM_DIR}"
  assert_success
  assert_output ''
}

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
# flat store, resolved as ${PATH_SECRETS:-${PATH_BASE}/.secrets} (PATH_SECRETS
# overrides the default ${PATH_BASE}/.secrets); these tests set PATH_SECRETS to it.

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

@test "check_flat_shadows: emits exactly one line per shadow" {
  _shadow_setup
  printf 'a' > "${DOM_DIR}/secrets/Secret.app.default.ONE"
  printf 'a' > "${PATH_SECRETS}/Secret.app.default.ONE"   # identical -> shadow
  printf 'b' > "${DOM_DIR}/secrets/Secret.app.default.TWO"
  printf 'X' > "${PATH_SECRETS}/Secret.app.default.TWO"   # differing -> DRIFT
  run secrets::check_flat_shadows "${DOM_DIR}"
  assert_failure
  [ "${#lines[@]}" -eq 2 ]                                # one line per shadow, no dupes/drops
  assert_output --partial 'Flat-store shadow: Secret.app.default.ONE'
  assert_output --partial 'Flat-store DRIFT: Secret.app.default.TWO'
}

@test "check_flat_shadows: honors a custom PATH_SECRETS flat-store location" {
  # Regression for the PR's own case: a manual PATH_SECRETS pointing at a
  # NON-default flat store must still be the store this check compares against.
  # (It previously hard-coded ${PATH_BASE}/.secrets and would have missed this.)
  DOM_DIR="${BATS_TEST_TMPDIR}/clusters/app.example.com"
  mkdir -p "${DOM_DIR}/secrets"
  export PATH_SECRETS="${BATS_TEST_TMPDIR}/custom-flat"
  mkdir -p "${PATH_SECRETS}"
  printf 'same' > "${DOM_DIR}/secrets/Secret.app.default.TOKEN"
  printf 'same' > "${PATH_SECRETS}/Secret.app.default.TOKEN"
  run secrets::check_flat_shadows "${DOM_DIR}"
  assert_failure
  assert_output --partial 'Flat-store shadow: Secret.app.default.TOKEN'
}

@test "check_flat_shadows: under set -u, bails when neither PATH_SECRETS nor PATH_BASE is set" {
  # Both unset must short-circuit BEFORE flat resolves to a bogus "/.secrets".
  # Run under `set -u` (as lo does) so this PROVES the guard: without it the bare
  # ${PATH_BASE} in the flat default would be an unbound-variable abort (non-zero).
  DOM_DIR="${BATS_TEST_TMPDIR}/clusters/app.example.com"
  mkdir -p "${DOM_DIR}/secrets"
  printf 'v' > "${DOM_DIR}/secrets/Secret.app.default.TOKEN"
  unset PATH_SECRETS PATH_BASE
  _under_u() ( set -u; secrets::check_flat_shadows "$@" )
  run _under_u "${DOM_DIR}"
  assert_success
  assert_output ''
}

# ── secrets set — value from stdin ───────────────────────────────────────
# These run the lib STANDALONE through the real argsh runtime (the stubbed
# :args in setup() can't parse positionals), so the value positional's
# :~stdin type is actually exercised. ARGSH_BUILTIN_PATH pins the repo's
# argsh.so so the runtime doesn't try to download one into the tmpdir.
# The inner bash unsets the exported :usage/:args/import STUBS from setup()
# (export -f) — the standalone run must use the real argsh implementations.

secrets_cli() {
  ARGSH_BUILTIN_PATH="${ARGSH_BUILTIN_PATH:-${_PROJECT_ROOT}/.bin/argsh.so}" \
  PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    bash -c 'unset -f :usage :args import 2>/dev/null; exec "${@}"' -- \
    "${_PROJECT_ROOT}/.bin/argsh" "${_PROJECT_ROOT}/.lok8s/libs/secrets" "${@}"
}
secrets_cli_piped() { printf %s "${1}" | secrets_cli "${@:2}"; }

# Skip when the b-managed argsh toolchain isn't installed (fresh clone before
# `b install`) — without the pinned .so argsh would try to DOWNLOAD one into
# the cwd, passing online and failing obscurely offline.
require_argsh_cli() {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh not installed (b install)"
  [ -f "${_PROJECT_ROOT}/.bin/argsh.so" ] || skip "argsh.so not installed (b install)"
}

@test "secrets set: literal value lands in the cache" {
  require_argsh_cli
  run secrets_cli set -n app KEY literal-v
  assert_success
  [ "$(cat "${PATH_SECRETS}/Secret.app.default.KEY")" = "literal-v" ]
}

@test "secrets set: omitted value reads piped stdin" {
  require_argsh_cli
  run secrets_cli_piped piped-v set -n app KEY
  assert_success
  [ "$(cat "${PATH_SECRETS}/Secret.app.default.KEY")" = "piped-v" ]
}

@test "secrets set: explicit - reads piped stdin (argsh with arg-sh/argsh#176)" {
  require_argsh_cli
  # Older argsh parsers reject a bare `-` before the :~stdin type can consume
  # it (the exact error wording varies by version) — probe by OUTCOME: only
  # run when the parser demonstrably stored the piped value via `-`.
  secrets_cli_piped probe set -n probe P - >/dev/null 2>&1 || true
  if [ "$(cat "${PATH_SECRETS}/Secret.probe.default.P" 2>/dev/null)" != "probe" ]; then
    skip "argsh parser predates bare-dash positional support (arg-sh/argsh#176)"
  fi
  run secrets_cli_piped dash-v set -n app KEY -
  assert_success
  [ "$(cat "${PATH_SECRETS}/Secret.app.default.KEY")" = "dash-v" ]
}

# ── secrets set --encrypt ────────────────────────────────────────────────
# `--encrypt` (short -e; alias --enc) encrypts ONLY the just-written file via
# secrets::_encrypt_file (never a whole-store sweep). Gate: ${PATH_BASE}/.sops.yaml
# must exist — its presence == "encryption is configured". These run through the
# real argsh runtime (secrets_cli) so the flag spec is actually parsed.
#
# NOTE on the sops recipient: sops discovers .sops.yaml by walking up from the
# file path, and the repo's own /workspace/.sops.yaml (path_regex 'Secret\..*')
# can shadow the tmpdir one under Docker — so the produced .enc may be encrypted
# to the repo key, not decryptable here. These tests therefore assert .enc
# EXISTENCE / which files were touched (recipient-independent), never a decrypt
# round-trip (that is what the top-of-file round-trip test covers on a dev box).

# The alias/parse tests need NO crypto tools: with .sops.yaml absent the gate
# fails fast, so all three spellings must produce the SAME "No .sops.yaml" error
# AND still have written the plaintext cache (proving the KEY/value positionals
# parsed with the flag consumed — an unknown flag would instead shift them).
@test "secrets set --encrypt: without .sops.yaml → error, non-zero (no .enc)" {
  require_argsh_cli
  run secrets_cli set -n app --encrypt KEY val
  assert_failure
  assert_output --partial 'No .sops.yaml'
  # cache still written (plaintext), but no .enc produced
  [ "$(cat "${PATH_SECRETS}/Secret.app.default.KEY")" = "val" ]
  [ ! -f "${PATH_SECRETS}/Secret.app.default.KEY.enc" ]
}

@test "secrets set --enc/-e: parse the same as --encrypt" {
  require_argsh_cli
  # No .sops.yaml → each spelling fails identically at the encrypt gate; the
  # plaintext cache lands under the right (name,ns,key), proving the token was
  # recognized as the boolean flag (not swallowed as a positional).
  for flag in --encrypt --enc -e; do
    rm -f "${PATH_SECRETS}"/Secret.app.default.*
    run secrets_cli set -n app "${flag}" KEY val
    assert_failure
    assert_output --partial 'No .sops.yaml'
    [ "$(cat "${PATH_SECRETS}/Secret.app.default.KEY")" = "val" ]
  done
}

@test "secrets set --encrypt: encrypts exactly that file, others untouched" {
  require_argsh_cli
  require_tools
  # Configure encryption (gate = ${PATH_BASE}/.sops.yaml present). A tmpdir key
  # keeps it self-contained; the produced .enc's recipient is irrelevant here.
  ssh-keygen -t ed25519 -N '' -C test -f "${BATS_TEST_TMPDIR}/id" -q
  printf 'creation_rules:\n  - path_regex: .*\n    age: %s\n' \
    "$(ssh-to-age < "${BATS_TEST_TMPDIR}/id.pub")" > "${PATH_BASE}/.sops.yaml"

  # A pre-existing sibling plaintext with NO .enc — must stay plaintext-only.
  printf 'sib' > "${PATH_SECRETS}/Secret.other.default.SIB"

  run secrets_cli set -n app --encrypt KEY val
  assert_success
  assert_output --partial 'Set + encrypted app/default/KEY'
  # exactly this file encrypted …
  [ -f "${PATH_SECRETS}/Secret.app.default.KEY.enc" ]
  # … and the sibling was NOT swept up
  [ ! -f "${PATH_SECRETS}/Secret.other.default.SIB.enc" ]
}

# ── secrets set — stale-.enc warning (plaintext write while encryption is on) ──
# When .sops.yaml exists but --encrypt is NOT passed, a plaintext write leaves
# the committed .enc stale — warn (always-on; no quiet level to gate on). Silent
# when .sops.yaml is absent. This path is crypto-free (never invokes sops), so it
# runs without require_tools.
@test "secrets set: warns about a stale .enc when .sops.yaml exists but --encrypt omitted" {
  require_argsh_cli
  : > "${PATH_BASE}/.sops.yaml"   # presence is all the gate checks
  run secrets_cli set -n app KEY val
  assert_success
  assert_output --partial 'wrote plaintext cache only'
  assert_output --partial "run 'lo secrets encrypt' or re-run with --encrypt/-e"
  [ "$(cat "${PATH_SECRETS}/Secret.app.default.KEY")" = "val" ]
  [ ! -f "${PATH_SECRETS}/Secret.app.default.KEY.enc" ]   # plaintext only
}

@test "secrets set: NO stale-.enc warning when .sops.yaml is absent" {
  require_argsh_cli
  # No .sops.yaml under PATH_BASE → encryption not configured → stay quiet.
  run secrets_cli set -n app KEY val
  assert_success
  refute_output --partial 'wrote plaintext cache only'
  assert_output --partial 'Set app/default/KEY'
}

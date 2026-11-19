#!/usr/bin/env bats
# secrets_add_key_test.bats — `lo secrets add-key` (issue #73).
#
# Why this exists
# ---------------
# The command was documented before it was written: secrets::init printed
# "use: lo secrets add-key" and the generated .sops.yaml header said the same,
# for a subcommand that did not exist. Adding a recipient by hand meant editing
# .sops.yaml, `touch`ing every plaintext twin (secrets::encrypt skips files whose
# .enc is newer, so a recipient change re-encrypts nothing), then running encrypt
# per domain — 135 files across 3 stores the last time it was done.
#
# The property that matters is not "the key is in .sops.yaml". It is that the new
# recipient can READ the store: sops only rewrites a file's recipients when that
# file is re-encrypted, so appending the key without re-keying leaves them able
# to decrypt nothing while everything looks configured.
#
# Everything here runs against a THROWAWAY store. These tests must never be able
# to touch clusters/*/secrets — PATH_BASE and PATH_CLUSTERS are redirected into
# BATS_TEST_TMPDIR, and the encrypt step is stubbed, so no sops key is needed and
# no real cache file is reachable. HOME, XDG_CONFIG_HOME (sops reads <config
# dir>/sops/age/keys.txt) and SOPS_AGE_KEY_FILE point into BATS_TEST_TMPDIR too.

setup() {
  load "../test_helper"
  setup_tmpdir
  export HOME="${BATS_TEST_TMPDIR}/home"
  export XDG_CONFIG_HOME="${HOME}/.config"
  export SOPS_AGE_KEY_FILE="${BATS_TEST_TMPDIR}/no-keys.txt"
  unset SOPS_AGE_KEY SOPS_AGE_KEY_CMD SOPS_AGE_SSH_PRIVATE_KEY_FILE
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${HOME}" "${PATH_BASE}" "${PATH_CLUSTERS}/a.dev/secrets" "${PATH_CLUSTERS}/b.dev/secrets"
  import() { :; }
  export -f import

  # Both fixtures are EXACTLY age1 + 58 bech32 chars. The first draft of KEY_B
  # had 59 and the validator rejected it — the fixture was wrong, not the code.
  KEY_A='age1zvkyg2lc7kjhpnjwqpjkwzr9qkxnwqp5xzdmqvxqjhqx0nqwqjqs3xqzq7'
  KEY_B='age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqz'
  cat > "${PATH_BASE}/.sops.yaml" <<YAML
creation_rules:
  - path_regex: 'Secret\\..*'
    age: '${KEY_A}'
YAML
}

teardown() { teardown_tmpdir; }

_load() {
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh" 2>/dev/null || true
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  # :args normally parses the spec; drive the function directly instead.
  ENCRYPT_RAN="${BATS_TEST_TMPDIR}/encrypt.ran"; : > "${ENCRYPT_RAN}"
  secrets::encrypt() { echo ran >> "${ENCRYPT_RAN}"; return 0; }
  command -v sops >/dev/null 2>&1 || sops() { return 0; }
}

# secrets::add-key declares its own locals and lets :args populate them — argsh's
# dynamic-scope contract. So the stub must ASSIGN them, not pre-set them in the
# caller: the function's own `local key=""` runs first and would clobber that
# (which is exactly how the first version of this harness failed, with an empty
# key reported as "not an age key and not a readable file").
_add() { # <key> [all] [skip_orphans]
  local _k="${1}" _all="${2:-0}" _skip="${3:-0}" _dom="${DOMAIN:-}"
  :args() { key="${_k}"; all="${_all}"; skip_orphans="${_skip}"; domain="${_dom}"; }
  secrets::add-key
}

@test "a valid age key is appended to the creation rule" {
  _load
  run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  grep -qF "${KEY_A}" "${PATH_BASE}/.sops.yaml" || { echo "the EXISTING recipient was dropped" >&2; return 1; }
  grep -qF "${KEY_B}" "${PATH_BASE}/.sops.yaml" || { echo "the new recipient was not added" >&2; return 1; }
  grep -q "${KEY_A},${KEY_B}" "${PATH_BASE}/.sops.yaml" || {
    echo "not a comma-separated recipient list:" >&2; cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
}

@test "adding a recipient RE-KEYS the store, it does not just edit yaml" {
  # The whole point: sops rewrites recipients only on re-encryption. A version
  # that edits .sops.yaml and stops leaves the new holder able to read nothing.
  _load
  printf 'v\n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.KEY"
  printf 'enc\n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.KEY.enc"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ -s "${ENCRYPT_RAN}" ] || { echo "encrypt never ran — the recipient holds nothing" >&2; return 1; }
}

@test "re-adding the same key is a no-op and does NOT re-key" {
  # Idempotence matters here beyond tidiness: a needless re-key rewrites every
  # .enc with fresh nonces, which is exactly the diff churn the store fights.
  _load
  run _add "${KEY_A}"
  [ "$status" -eq 0 ]
  [[ "$output" == *"already present"* ]] || { echo "expected an already-present message, got: $output" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ] || { echo "it re-keyed the store for a key that was already there" >&2; return 1; }
  [ "$(grep -cF "${KEY_A}" "${PATH_BASE}/.sops.yaml")" -eq 1 ]
}

@test "an orphan .enc FAILS closed rather than half-adding the recipient" {
  # An .enc with no decrypted twin cannot be re-keyed, so it keeps the OLD
  # recipient list while .sops.yaml claims the new key was added — the new holder
  # would silently be unable to read those files.
  _load
  printf 'enc\n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.orphan.default.KEY.enc"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -ne 0 ] || { echo "an orphan .enc did not fail the command" >&2; return 1; }
  [[ "$output" == *"no decrypted twin"* ]]
  [ ! -s "${ENCRYPT_RAN}" ]
}

@test "--skip-orphans proceeds knowingly" {
  _load
  printf 'enc\n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.orphan.default.KEY.enc"
  DOMAIN=a.dev run _add "${KEY_B}" 0 1
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
}

@test "without flock, add-key refuses before .sops.yaml is touched and makes no lock file" {
  # A run without the lock can undo another run's change. The PATH has the
  # tools of the run and no flock: a link to each one found on PATH.
  _load
  local bin="${BATS_TEST_TMPDIR}/no-flock" tool path
  mkdir -p "${bin}"
  for tool in sops grep sed cat; do
    path="$(type -P "${tool}")" || continue
    ln -s "${path}" "${bin}/${tool}"
  done
  [ ! -e "${bin}/flock" ]
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  _add_path() { local PATH="${1}"; shift; _add "${@}"; }
  DOMAIN=a.dev run _add_path "${bin}" "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] flock not found — add-key and remove-key do not run without the lock of .sops.yaml; nothing was changed
[error]   install flock, or use the lo binary: it locks on every platform" ] || { echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ ! -e "${PATH_BASE}/.sops.yaml.lock" ] || { echo "a lock file was made" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

@test "a malformed age key is refused before .sops.yaml is touched" {
  # A typo here is silent: sops accepts the config and every later encrypt writes
  # a recipient nobody holds.
  _load
  local before; before="$(cat "${PATH_BASE}/.sops.yaml")"
  run _add 'age1nope'
  [ "$status" -ne 0 ] || { echo "a malformed key was accepted" >&2; return 1; }
  [ "$(cat "${PATH_BASE}/.sops.yaml")" = "${before}" ] || {
    echo ".sops.yaml was modified despite the key being rejected" >&2; return 1; }
}

@test "a non-key, non-file argument is refused with a usable message" {
  _load
  run _add '/no/such/path'
  [ "$status" -ne 0 ]
  [[ "$output" == *"not an age key and not a readable file"* ]]
}

@test "a private key argument is refused and not echoed" {
  # The not-a-file error above prints the argument, so a pasted age identity
  # would land in the terminal. The filler is outside the bech32 alphabet: the
  # shape of an age identity, not a key.
  _load
  local fake; fake="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
  local before; before="$(cat "${PATH_BASE}/.sops.yaml")"
  run _add "${fake}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] the key argument looks like a private key — it is not printed
[error] pass an age public key (age1…) or the path to an ed25519 SSH public key" ] || {
    echo "unexpected output (${#output} bytes)" >&2; return 1; }
  [[ "$output" != *"${fake}"* ]]
  [ "$(cat "${PATH_BASE}/.sops.yaml")" = "${before}" ]
  [ ! -s "${ENCRYPT_RAN}" ]
}

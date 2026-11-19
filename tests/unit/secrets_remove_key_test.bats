#!/usr/bin/env bats
# secrets_remove_key_test.bats — `lo secrets remove-key` (issue #137).
#
# add-key in reverse: the recipient leaves every creation rule in .sops.yaml,
# and the stores are re-keyed so that no .enc file lists it. The property that
# matters is that the removed key cannot open the store afterwards. Editing
# .sops.yaml alone changes nothing on disk: sops writes a file's recipients
# only when it encrypts that file.
#
# Everything runs against a THROWAWAY store and throwaway keys. HOME,
# XDG_CONFIG_HOME (sops reads <config dir>/sops/age/keys.txt) and
# SOPS_AGE_KEY_FILE point into BATS_TEST_TMPDIR, and so do PATH_BASE and
# PATH_CLUSTERS. Most tests stub secrets::encrypt; the round trip at the end
# runs the real one and skips without sops, ssh-to-age or ssh-keygen. No test
# prints a private key.

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

  # Public keys of the fixture shape (age1 + 58 bech32 characters).
  RCPT_A='age1zvkyg2lc7kjhpnjwqpjkwzr9qkxnwqp5xzdmqvxqjhqx0nqwqjqs3xqzq7'
  RCPT_B='age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqz'
  _rules "${RCPT_A},${RCPT_B}"
}

teardown() { teardown_tmpdir; }

# _rules <list>... — one creation rule per recipient list. Only the first rule
# matches the cache files; the others must be edited all the same.
_rules() {
  local i=0 l re
  {
    echo "creation_rules:"
    for l in "${@}"; do
      re='Secret\..*'
      (( i == 0 )) || re="Other${i}\\..*"
      printf "  - path_regex: '%s'\n    age: '%s'\n" "${re}" "${l}"
      i=$((i + 1))
    done
  } > "${PATH_BASE}/.sops.yaml"
}

_load() {
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  # The real secrets::encrypt and secrets::path run :args; outside _rm it
  # parses nothing.
  :args() { :; }
  ENCRYPT_LOG="${BATS_TEST_TMPDIR}/encrypt.log"
  : > "${ENCRYPT_LOG}"
  command -v sops >/dev/null 2>&1 || sops() { return 0; }
}

# The stub encrypt logs the domain it ran under and rewrites each plaintext's
# .enc with the recipients of the first rule, as a re-encryption would.
_stub_encrypt() {
  secrets::encrypt() {
    echo "${domain:-<none>}" >> "${ENCRYPT_LOG}"
    local dir plain
    dir="$(secrets::path)" || return 1
    for plain in "${dir}"/Secret.*; do
      [[ -f "${plain}" && "${plain}" != *.enc ]] || continue
      grep -m1 "age:" "${PATH_BASE}/.sops.yaml" > "${plain}.enc"
    done
  }
}

# secrets::remove-key declares its own locals and lets :args fill them
# (argsh's dynamic scope). The stub assigns them only for remove-key itself:
# the helpers it calls (secrets::path, secrets::encrypt) run :args too.
_rm() { # <key> [all] [skip_orphans]
  local _k="${1}" _all="${2:-0}" _skip="${3:-0}"
  local domain="${DOMAIN:-}"
  :args() {
    [[ "${FUNCNAME[1]}" == "secrets::remove-key" ]] || return 0
    key="${_k}"; all="${_all}"; skip_orphans="${_skip}"
  }
  secrets::remove-key
}

_rotate='[warn] a removal does not revoke what the key already read (older .enc files in git history included): rotate those secrets'

@test "the recipient leaves every creation rule and the store is re-keyed" {
  _load
  _stub_encrypt
  _rules "${RCPT_A},${RCPT_B}" "${RCPT_B}, ${RCPT_A}"
  printf 'v' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  ! grep -qF "${RCPT_B}" "${PATH_BASE}/.sops.yaml" || {
    echo "the recipient is still in .sops.yaml:" >&2; cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ "$(grep -cF "age: '${RCPT_A}'" "${PATH_BASE}/.sops.yaml")" -eq 2 ] || {
    echo "the other recipient did not stay in both rules:" >&2; cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev" ] || { echo "encrypt ran under: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
  [[ "$output" == *"removed recipient from .sops.yaml (2 creation rule(s))"* ]]
  [[ "$output" == *"${_rotate}" ]] || { echo "the rotate warning is not the last line: $output" >&2; return 1; }
}

@test "--all re-encrypts every domain store, each under its own domain" {
  # add-key's --all re-encrypts the current store only; for a removal that
  # would leave the other stores open to the removed key.
  _load
  _stub_encrypt
  printf 'v' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K"
  printf 'w' > "${PATH_CLUSTERS}/b.dev/secrets/Secret.app.default.K"
  DOMAIN=a.dev run _rm "${RCPT_B}" 1
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = $'a.dev\nb.dev' ] || { echo "encrypt ran under: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
  [[ "$output" == *"re-keying 2 cache file(s) across 2 store(s)…"* ]]
}

@test "without --all only the current store is re-encrypted" {
  _load
  _stub_encrypt
  printf 'v' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K"
  printf 'w' > "${PATH_CLUSTERS}/b.dev/secrets/Secret.app.default.K"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev" ]
  [ ! -e "${PATH_CLUSTERS}/b.dev/secrets/Secret.app.default.K.enc" ]
}

@test "the last recipient of a rule is refused and nothing is written" {
  _load
  _stub_encrypt
  _rules "${RCPT_A},${RCPT_B}" "${RCPT_B}"
  printf 'v' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K"
  local before; before="$(cat "${PATH_BASE}/.sops.yaml")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] refusing to remove the last recipient of 1 creation rule(s) — nothing could decrypt the files they match
[error]   add another recipient first (lo secrets add-key), or delete the rule by hand" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${PATH_BASE}/.sops.yaml")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "an orphan .enc fails closed before .sops.yaml is written" {
  # A re-key cannot rewrite an .enc without its plaintext, so the file would
  # keep the removed recipient.
  _load
  _stub_encrypt
  printf 'enc\n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.orphan.default.K.enc"
  local before; before="$(cat "${PATH_BASE}/.sops.yaml")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" == *"they would keep the REMOVED"* ]]
  [ "$(cat "${PATH_BASE}/.sops.yaml")" = "${before}" ] || { echo ".sops.yaml was written before the refusal" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "--skip-orphans proceeds and names what the removed key can still open" {
  _load
  _stub_encrypt
  printf 'v' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K"
  printf '%s\n' "${RCPT_B}" > "${PATH_CLUSTERS}/a.dev/secrets/Secret.orphan.default.K.enc"
  DOMAIN=a.dev run _rm "${RCPT_B}" 0 1
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == *"[warn] 1 orphan .enc file(s) still list the removed recipient — its key can still decrypt them"* ]]
}

@test "a recipient in no rule and no .enc is refused" {
  _load
  _stub_encrypt
  _rules "${RCPT_A}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] not a recipient in .sops.yaml or in any .enc file of the store(s) — nothing to remove
[error]   ${RCPT_B}" ] || { echo "$output" >&2; return 1; }
}

@test "a recipient outside a single-quoted age: list is left to the operator" {
  _load
  _stub_encrypt
  printf "creation_rules:\n  - path_regex: 'Secret\\\\..*'\n    age: %s,%s\n" "${RCPT_A}" "${RCPT_B}" > "${PATH_BASE}/.sops.yaml"
  local before; before="$(cat "${PATH_BASE}/.sops.yaml")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [[ "$output" == *"lists the recipient outside a single-quoted age: list — remove it there by hand"* ]]
  [ "$(cat "${PATH_BASE}/.sops.yaml")" = "${before}" ]
}

@test "an .enc the re-key skipped fails the command" {
  # secrets::encrypt skips an .enc that is newer than its plaintext. The
  # command reads the files again instead of trusting the exit code.
  _load
  secrets::encrypt() { echo "${domain}" >> "${ENCRYPT_LOG}"; }
  printf 'v' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K"
  printf '%s\n' "${RCPT_B}" > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  # PATH_CLUSTERS is outside PATH_BASE here, so the path is printed in full.
  [[ "$output" == *"[error] still encrypted to the removed recipient: ${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc"* ]] || {
    echo "$output" >&2; return 1; }
  [[ "$output" != *"done —"* ]]
}

@test "a private key argument is refused and not echoed" {
  # A fixed filler outside the bech32 alphabet: the shape of an age identity,
  # not a key.
  _load
  _stub_encrypt
  local fake; fake="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
  local before; before="$(cat "${PATH_BASE}/.sops.yaml")"
  DOMAIN=a.dev run _rm "${fake}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] the key argument looks like a private key — it is not printed
[error] pass an age public key (age1…) or the path to an ed25519 SSH public key" ] || {
    echo "unexpected output (${#output} bytes)" >&2; return 1; }
  [[ "$output" != *"${fake}"* ]]
  [ "$(cat "${PATH_BASE}/.sops.yaml")" = "${before}" ]
}

@test "the .sops.yaml edit matches the Go twin byte for byte" {
  # The fixture of TestDropRecipient in internal/secrets/rekey_test.go.
  _load
  local f="${BATS_TEST_TMPDIR}/in.yaml" edited rules last
  printf "creation_rules:\n  - path_regex: 'a\\\\..*'\n    age: 'KA, KB ,KC'   # team\n  - path_regex: '.*'\n    age: 'KA,KB,'\n  - path_regex: 'c'\n    age:   'KC'" > "${f}"
  secrets::_drop_recipient "${f}" KB
  [ "${edited}" = "creation_rules:
  - path_regex: 'a\\..*'
    age: 'KA,KC'   # team
  - path_regex: '.*'
    age: 'KA'
  - path_regex: 'c'
    age:   'KC'
" ] || { printf '%s' "${edited}" >&2; return 1; }
  [ "${rules}" -eq 2 ] && [ "${last}" -eq 0 ]
  secrets::_drop_recipient "${f}" KC
  [ "${edited}" = "creation_rules:
  - path_regex: 'a\\..*'
    age: 'KA,KB'   # team
  - path_regex: '.*'
    age: 'KA,KB,'
  - path_regex: 'c'
    age:   ''
" ] || { printf '%s' "${edited}" >&2; return 1; }
  [ "${rules}" -eq 2 ] && [ "${last}" -eq 1 ]
}

@test "round trip: the removed key cannot decrypt the store, the other key can" {
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  # Throwaway keys outside HOME/.ssh. The identity files stay on disk; nothing
  # prints them.
  local k="${BATS_TEST_TMPDIR}/keys" n
  mkdir -p "${k}"
  for n in keep gone; do
    ssh-keygen -q -t ed25519 -N '' -C "${n}" -f "${k}/${n}" </dev/null >/dev/null 2>&1
    ssh-to-age -private-key -i "${k}/${n}" -o "${k}/${n}.age" 2>/dev/null
  done
  local keep gone
  keep="$(ssh-to-age -i "${k}/keep.pub")"
  gone="$(ssh-to-age -i "${k}/gone.pub")"
  _rules "${keep},${gone}"
  local store="${PATH_CLUSTERS}/a.dev/secrets"
  printf 'v' > "${store}/Secret.app.default.K"
  domain=a.dev secrets::encrypt >/dev/null
  _open() { SOPS_AGE_KEY_FILE="${k}/${1}.age" sops decrypt --input-type binary --output-type binary "${store}/Secret.app.default.K.enc" 2>/dev/null; }
  [ "$(_open gone)" = "v" ] || { echo "fixture: the key to remove cannot open the store before the removal" >&2; return 1; }

  DOMAIN=a.dev run _rm "${k}/gone.pub"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  ! _open gone >/dev/null || { echo "the removed key still decrypts the store" >&2; return 1; }
  [ "$(_open keep)" = "v" ] || { echo "the remaining key cannot decrypt the store" >&2; return 1; }
  ! grep -qF "${gone}" "${store}/Secret.app.default.K.enc"
}

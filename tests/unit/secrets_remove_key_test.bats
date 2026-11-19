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
# PATH_CLUSTERS. Most tests stub secrets::_encrypt_file, and their .enc files
# are sops envelopes with no ciphertext (_envelope); the round trip runs the
# real one and skips without sops, ssh-to-age or ssh-keygen. No test prints a
# private key.

setup() {
  load "../test_helper"
  setup_tmpdir
  export HOME="${BATS_TEST_TMPDIR}/home"
  export XDG_CONFIG_HOME="${HOME}/.config"
  export SOPS_AGE_KEY_FILE="${BATS_TEST_TMPDIR}/no-keys.txt"
  unset SOPS_AGE_KEY SOPS_AGE_KEY_CMD SOPS_AGE_SSH_PRIVATE_KEY_FILE PATH_SECRETS LO_SECRETS_LOCK_WAIT
  export LOK8S_SSH_KEY="${HOME}/.ssh/id_ed25519"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${HOME}" "${PATH_BASE}" "${PATH_CLUSTERS}/a.dev/secrets" "${PATH_CLUSTERS}/b.dev/secrets"
  import() { :; }
  export -f import

  # Public keys of the fixture shape (age1 + 58 bech32 characters).
  RCPT_A='age1zvkyg2lc7kjhpnjwqpjkwzr9qkxnwqp5xzdmqvxqjhqx0nqwqjqs3xqzq7'
  RCPT_B='age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqz'
  # RCPT_B as YAML decodes it, while its bytes do not hold it: the first
  # letter as the escape backslash, u, 0061 in a double-quoted string.
  ESC_B="\"$(printf '\134')u0061${RCPT_B:1}\""
  # RCPT_B as a JSON string of an .enc spells it: the same escape, no quotes.
  JESC_B="$(printf '\134')u0061${RCPT_B:1}"
  # The first line of an OpenSSH private key block, no key. Built from two
  # parts, so that the private-key rule of a secret scanner does not match
  # from one PEM literal to the next.
  PEM_HEAD="-----BEGIN OPENSSH PRIVATE ""KEY-----"
  CFG="${PATH_BASE}/.sops.yaml"
  _rules "${RCPT_A},${RCPT_B}"
}

teardown() {
  chmod -R u+rwx "${PATH_CLUSTERS}" 2>/dev/null || true
  teardown_tmpdir
}

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
  } > "${CFG}"
}

_load() {
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  # The real secrets::path runs :args; outside _rm it parses nothing.
  :args() { :; }
  ENCRYPT_LOG="${BATS_TEST_TMPDIR}/encrypt.log"
  : > "${ENCRYPT_LOG}"
  command -v sops >/dev/null 2>&1 || sops() { return 0; }
}

# _envelope <recipient>... — the shape of a binary-mode .enc whose sops
# metadata lists the recipients, with no real ciphertext: enough for the
# recipient scans, not for a decrypt. A recipient goes into the JSON as given,
# so a JSON escape stays one.
_envelope() {
  local r out=""
  for r in "${@}"; do
    out+="${out:+,}{\"recipient\":\"${r}\",\"enc\":\"x\"}"
  done
  printf '{"data":"ENC[x]","sops":{"age":[%s]}}\n' "${out}"
}

# _first_rule_envelope — the envelope of the recipients in the age list of the
# first rule, as sops would write it.
_first_rule_envelope() {
  local line r
  local -a parts=() rcpts=()
  line="$(grep -m1 "age:" "${CFG}")"
  line="${line#*\'}"
  IFS=',' read -r -a parts <<< "${line%%\'*}"
  for r in "${parts[@]}"; do
    r="${r// /}"
    [[ -z "${r}" ]] || rcpts+=("${r}")
  done
  _envelope "${rcpts[@]}"
}

# The stub re-encryption logs each file it ran on (relative to PATH_CLUSTERS)
# and rewrites its .enc with the recipients of the first rule, as sops would.
_stub_encrypt() {
  secrets::_encrypt_file() {
    echo "${1#"${PATH_CLUSTERS}/"}" >> "${ENCRYPT_LOG}"
    _first_rule_envelope > "${1}.enc"
  }
}

# _cache <domain> <name> <recipient> — a plaintext cache file and its .enc
# that lists the recipient, the plaintext the newer of the two (the state
# after a decrypt).
_cache() {
  local f="${PATH_CLUSTERS}/${1}/secrets/Secret.${2}.default.K"
  printf 'v' > "${f}"
  _envelope "${3}" > "${f}.enc"
  touch -t 200001010000 "${f}.enc"
}

# secrets::remove-key declares its own locals and lets :args fill them
# (argsh's dynamic scope). The stub assigns them only for remove-key itself:
# the helpers it calls (secrets::path) run :args too.
_rm() { # <key> [all] [skip_orphans]
  local _k="${1}" _all="${2:-0}" _skip="${3:-0}"
  local domain="${DOMAIN:-}"
  :args() {
    [[ "${FUNCNAME[1]}" == "secrets::remove-key" ]] || return 0
    key="${_k}"; all="${_all}"; skip_orphans="${_skip}"
  }
  secrets::remove-key
}

# _run_lo <argv...> — the real entrypoint of the frozen tree, in a throwaway
# project (the pattern of cli_dispatch_test.bats).
_run_lo() {
  local proj="${BATS_TEST_TMPDIR}/proj"
  mkdir -p "${proj}/clusters"
  run env -C "${proj}" \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    PATH="${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    "${_PROJECT_ROOT}/.lok8s/lo" "${@}"
}

_rotate='[warn] a removal does not revoke what the key already read (older .enc files in git history included): rotate those secrets'
_not_written='[error]   the re-key encrypts with age through the first matching rule: fix the creation rules first. Nothing was written.'

@test "the recipient leaves every creation rule and the store is re-keyed" {
  _load
  _stub_encrypt
  _rules "${RCPT_A},${RCPT_B}" "${RCPT_B}, ${RCPT_A}"
  _cache a.dev app "${RCPT_B}"
  printf 'n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.new.default.K"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  ! grep -qF "${RCPT_B}" "${CFG}" || {
    echo "the recipient is still in .sops.yaml:" >&2; cat "${CFG}" >&2; return 1; }
  [ "$(grep -cF "age: '${RCPT_A}'" "${CFG}")" -eq 2 ] || {
    echo "the other recipient did not stay in both rules:" >&2; cat "${CFG}" >&2; return 1; }
  # Only the .enc that listed the recipient: the new plaintext gets none.
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev/secrets/Secret.app.default.K" ] || { echo "encrypted: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
  [[ "$output" == *"removed recipient from .sops.yaml (2 creation rule(s))"* ]]
  [[ "$output" == *"Encrypted 1 secret(s)"* ]]
  [[ "$output" == *"${_rotate}" ]] || { echo "the rotate warning is not the last line: $output" >&2; return 1; }
}

@test "--all re-encrypts every domain store" {
  # add-key's --all re-encrypts the current store only; for a removal that
  # would leave the other stores open to the removed key.
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache b.dev app "${RCPT_B}"
  DOMAIN=a.dev run _rm "${RCPT_B}" 1
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = $'a.dev/secrets/Secret.app.default.K\nb.dev/secrets/Secret.app.default.K' ] || {
    echo "encrypted: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
  [[ "$output" == *"re-keying 2 cache file(s) across 2 store(s)…"* ]]
}

@test "without --all only the current store is re-keyed, and the stores that still list it are named" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache b.dev app "${RCPT_B}"
  mkdir -p "${PATH_BASE}/.secrets"
  _envelope "${RCPT_B}" > "${PATH_BASE}/.secrets/Secret.flat.default.K.enc"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev/secrets/Secret.app.default.K" ]
  grep -qF "${RCPT_B}" "${PATH_CLUSTERS}/b.dev/secrets/Secret.app.default.K.enc"
  # PATH_CLUSTERS is outside PATH_BASE here, so the store is printed in full.
  [[ "$output" == *"[warn] still listed in 1 .enc file(s) of a store this run did not re-key: ${PATH_CLUSTERS}/b.dev/secrets
[warn]   rerun with --all to re-key every domain store
[warn] still listed in 1 .enc file(s) of the flat store, which --all does not re-key: .secrets
done — commit .sops.yaml and the updated .enc files together
${_rotate}" ]] || { echo "$output" >&2; return 1; }
}

@test "the last recipient of a rule is refused and nothing is written" {
  _load
  _stub_encrypt
  _rules "${RCPT_A},${RCPT_B}" "${RCPT_B}"
  _cache a.dev app "${RCPT_B}"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] refusing to remove the last recipient of 1 creation rule(s) — nothing could decrypt the files they match
[error]   add another recipient first (lo secrets add-key), or delete the rule by hand" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "an orphan .enc fails closed before .sops.yaml is written" {
  # A re-key cannot rewrite an .enc without its plaintext, so the file would
  # keep the removed recipient.
  _load
  _stub_encrypt
  _envelope "${RCPT_A}" "${RCPT_B}" > "${PATH_CLUSTERS}/a.dev/secrets/Secret.orphan.default.K.enc"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" == *"they would keep the REMOVED"* ]]
  [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml was written before the refusal" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "--skip-orphans proceeds and names what the removed key can still open" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _envelope "${RCPT_B}" > "${PATH_CLUSTERS}/a.dev/secrets/Secret.orphan.default.K.enc"
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

@test "the nothing-to-remove refusal names a store that still lists the recipient" {
  _load
  _stub_encrypt
  _rules "${RCPT_A}"
  _cache b.dev app "${RCPT_B}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] not a recipient in .sops.yaml or in any .enc file of the store(s) — nothing to remove
[error]   ${RCPT_B}
[error] still listed in 1 .enc file(s) of a store this run did not re-key: ${PATH_CLUSTERS}/b.dev/secrets
[error]   rerun with --all to re-key every domain store" ] || { echo "$output" >&2; return 1; }
}

@test "a recipient outside a single-quoted age: list is left to the operator" {
  _load
  _stub_encrypt
  printf "creation_rules:\n  - path_regex: 'Secret\\\\..*'\n    age: %s,%s\n" "${RCPT_A}" "${RCPT_B}" > "${CFG}"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [[ "$output" == *"lists the recipient outside a single-quoted age: list — remove it there by hand"* ]]
  [ "$(cat "${CFG}")" = "${before}" ]
}

# _refused <yaml to append> <message> — the recipient is in the bytes of the
# first rule only; the refusal names where the YAML holds it, and nothing is
# written.
_refused() {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  printf '%s' "${1}" >> "${CFG}"
  [ "$(grep -cF "${RCPT_B}" "${CFG}")" -eq 1 ] || { echo "fixture: the recipient must be in the first rule only" >&2; return 1; }
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${CFG} ${2}
[error]   ${RCPT_B}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml was written" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "a recipient in an escaped string or an escaped list item is refused" {
  local msg='lists the recipient in an escaped string or a YAML list, which remove-key does not edit — remove it there by hand'
  _refused "  - path_regex: 'Other\\..*'
    age: ${ESC_B}
" "${msg}"
  _rules "${RCPT_A},${RCPT_B}"
  _refused "  - path_regex: 'Other\\..*'
    age: [${ESC_B}]
" "${msg}"
}

@test "a recipient in key_groups is refused" {
  _refused "  - path_regex: 'Other\\..*'
    key_groups:
      - age:
          - ${ESC_B}
" 'lists the recipient in key_groups — remove-key does not edit key groups; remove it there by hand'
}

@test "a recipient that the edited file still holds through a merge key or an alias is refused" {
  local msg='still lists the recipient after the edit (through an alias or a merge key) — nothing was written; remove it there by hand'
  _refused "  - path_regex: &shared ${ESC_B}
    age: '${RCPT_A}'
  - path_regex: 'Otherx\\..*'
    age: *shared
" "${msg}"
  _rules "${RCPT_A},${RCPT_B}"
  _refused "  - path_regex: 'Othery\\..*'
    age: '${RCPT_A}'
    key: &shared
      age: ${ESC_B}
  - <<: *shared
    path_regex: 'Otherz\\..*'
" "${msg}"
}

@test "a .sops.yaml that does not parse is refused" {
  _load
  _stub_encrypt
  printf '  - path_regex: [\n' >> "${CFG}"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] ${CFG} does not parse as YAML" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
}

# _form_refused <yaml before creation_rules> <second rule> <message> — the
# recipient is in the bytes of the first rule only, and the second rule holds
# a YAML form that yq and the Go twin read differently; the refusal names the
# form, and nothing is written.
_form_refused() {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  printf "%screation_rules:\n  - path_regex: 'Secret\\\\..*'\n    age: '%s,%s'\n%s" "${1}" "${RCPT_A}" "${RCPT_B}" "${2}" > "${CFG}"
  [ "$(grep -cF "${RCPT_B}" "${CFG}")" -eq 1 ] || { echo "fixture: the recipient must be in the first rule only" >&2; return 1; }
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${CFG} ${3}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml was written" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

_tagged='has a YAML tag under creation_rules other than !!str, !!seq or !!map — remove-key does not read tagged values; remove the tag by hand'
_keyed='has a mapping key that is not a string — remove-key does not read such a mapping; quote the key or remove it by hand'

@test "a !!binary recipient is refused (a YAML tag)" {
  local b64; b64="$(printf '%s' "${RCPT_B}" | base64 | tr -d '\n')"
  _form_refused "" "  - path_regex: 'Other\\..*'
    age: !!binary ${b64}
" "${_tagged}"
}

@test "a numeric mapping key is refused" {
  _form_refused "shared: &shared ${ESC_B}
" "  - path_regex: 'Other\\..*'
    age: *shared
    1: x
" "${_keyed}"
}

@test "a bool mapping key is refused" {
  _form_refused "shared: &shared ${ESC_B}
" "  - path_regex: 'Other\\..*'
    age: *shared
    true: x
" "${_keyed}"
}

@test "a null mapping key is refused" {
  _form_refused "shared: &shared ${ESC_B}
" "  - path_regex: 'Other\\..*'
    age: *shared
    ~: x
" "${_keyed}"
}

@test "an .enc that matches no creation rule is refused before any write" {
  _load
  _stub_encrypt
  printf "creation_rules:\n  - path_regex: 'Secret\\\\.app\\\\..*'\n    age: '%s,%s'\n" "${RCPT_A}" "${RCPT_B}" > "${CFG}"
  _cache a.dev app "${RCPT_B}"
  _cache a.dev other "${RCPT_B}"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] no creation rule matches ${PATH_CLUSTERS}/a.dev/secrets/Secret.other.default.K.enc
${_not_written}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "an .enc whose rule has a recipient that is not age is refused before any write" {
  _load
  _stub_encrypt
  printf "creation_rules:\n  - path_regex: 'Secret\\\\.app\\\\..*'\n    age: '%s,%s'\n  - path_regex: 'Secret\\\\.other\\\\..*'\n    age: '%s'\n    pgp: 'FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000'\n" \
    "${RCPT_A}" "${RCPT_B}" "${RCPT_A}" > "${CFG}"
  _cache a.dev app "${RCPT_B}"
  _cache a.dev other "${RCPT_B}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] the creation rule for ${PATH_CLUSTERS}/a.dev/secrets/Secret.other.default.K.enc has a recipient that is not age (pgp, kms, …)
${_not_written}" ] || { echo "$output" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "an .enc whose rule has an age recipient that does not parse is refused before any write" {
  _load
  _stub_encrypt
  local bad rule
  bad="age1$(printf 'b%.0s' {1..58})"
  for rule in "    age: '${RCPT_A},${bad}'" "    key_groups:
      - age: ['${RCPT_A}', '${bad}']"; do
    printf "creation_rules:\n  - path_regex: 'Secret\\\\.app\\\\..*'\n    age: '%s,%s'\n  - path_regex: 'Secret\\\\.other\\\\..*'\n%s\n" \
      "${RCPT_A}" "${RCPT_B}" "${rule}" > "${CFG}"
    _cache a.dev app "${RCPT_B}"
    _cache a.dev other "${RCPT_B}"
    local before; before="$(cat "${CFG}")"
    DOMAIN=a.dev run _rm "${RCPT_B}"
    [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
    [ "$output" = "[error] the creation rule for ${PATH_CLUSTERS}/a.dev/secrets/Secret.other.default.K.enc has an age recipient that does not parse
${_not_written}" ] || { echo "$output" >&2; return 1; }
    [ "$(cat "${CFG}")" = "${before}" ]
    [ ! -s "${ENCRYPT_LOG}" ]
  done
}

# _class <label> <want> <rule body> — the class that secrets::_rules_check
# gives Secret.app.default.K under one creation rule with the body. A
# mismatch names the label only: one body holds a private-key-shaped filler.
_class() {
  local -a none_list=() notage_list=() badage_list=()
  local got=ok
  secrets::_rules_check "${PATH_BASE}/Secret.app.default.K.enc" <<< "$(printf "creation_rules:\n  - path_regex: 'Secret\\\\..*'\n    %s\n" "${3}")" || got=error
  (( ${#none_list[@]} == 0 )) || got=none
  (( ${#notage_list[@]} == 0 )) || got=nonage
  (( ${#badage_list[@]} == 0 )) || got=badage
  [ "${got}" = "${2}" ] || { echo "${1}: got ${got}, want ${2}" >&2; return 1; }
}

@test "the rule check reads the age recipients as sops does" {
  # The rows of TestRuleForReadsTheAgeRecipients (internal/secrets): the Go
  # twin parses each recipient, this one checks its shape.
  _load
  local bad ssh filler pgp
  bad="age1$(printf 'b%.0s' {1..58})"
  ssh="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI$(printf 'A%.0s' {1..43}) test"
  filler="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
  pgp=$'\n'"    pgp: 'FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000'"
  _class "one recipient" ok "age: '${RCPT_A}'"
  _class "a recipient that does not parse" badage "age: '${RCPT_A},${bad}'"
  _class "an empty part of a string" ok "age: '${RCPT_A}, ,${RCPT_A}'"
  _class "an empty item of a list" badage "age: ['${RCPT_A}', '']"
  _class "a list of one empty item" ok "age: ['']"
  _class "an item that is not a string" badage "age: ['${RCPT_A}', 5]"
  _class "an ssh-ed25519 public key" ok "age: '${ssh}'"
  _class "an ssh key of another type" badage "age: 'ssh-dss AAAAB3NzaC1kc3M'"
  _class "a private key in the rule" badage "age: '${filler}'"
  _class "a key group" ok "key_groups:
      - age: ['${RCPT_A}']"
  _class "a key group with a recipient that does not parse" badage "key_groups:
      - age: ['${RCPT_A}', '${bad}']"
  _class "a key group with pgp and a recipient that does not parse" nonage "key_groups:
      - age: ['${bad}']
        pgp: ['FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000']"
  _class "pgp and a recipient that does not parse" badage "age: '${bad}'${pgp}"
  _class "pgp" nonage "age: '${RCPT_A}'${pgp}"
}

@test "an .enc or a store it cannot read fails closed before any write" {
  [ "$(id -u)" -ne 0 ] || skip "root reads every file"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local store="${PATH_CLUSTERS}/a.dev/secrets" before
  before="$(cat "${CFG}")"
  chmod 000 "${store}/Secret.app.default.K.enc"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] cannot read ${store}/Secret.app.default.K.enc
[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  chmod 600 "${store}/Secret.app.default.K.enc"
  chmod 300 "${store}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  chmod 755 "${store}"
  [ "$status" -eq 1 ]
  [ "$output" = "[error] cannot read ${store}
[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "a dangling .enc symlink is refused before any write" {
  # lstat finds the link and stat does not: the file is not absent, and its
  # target may list the recipient.
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local store="${PATH_CLUSTERS}/a.dev/secrets" before
  before="$(cat "${CFG}")"
  ln -s Secret.nowhere.default.K.enc "${store}/Secret.gone.default.K.enc"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] cannot read ${store}/Secret.gone.default.K.enc
[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "with --all, a domain directory it cannot search is refused before any write" {
  [ "$(id -u)" -ne 0 ] || skip "root reads every file"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache b.dev app "${RCPT_B}"
  local before; before="$(cat "${CFG}")"
  chmod 000 "${PATH_CLUSTERS}/b.dev"
  DOMAIN=a.dev run _rm "${RCPT_B}" 1
  chmod 755 "${PATH_CLUSTERS}/b.dev"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] cannot read ${PATH_CLUSTERS}/b.dev/secrets
[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "the select scan reads the parsed metadata: an .enc that spells the recipient as a JSON escape is re-keyed" {
  _load
  _stub_encrypt
  _cache a.dev app "${JESC_B}"
  ! grep -qF "${RCPT_B}" "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc" || {
    echo "fixture: the bytes of the .enc hold the recipient" >&2; return 1; }
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev/secrets/Secret.app.default.K" ] || { echo "encrypted: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
  [[ "$output" == *"re-keying 1 cache file(s) across 1 store(s)…"* ]] || { echo "$output" >&2; return 1; }
}

@test "the post-check reads the parsed metadata: an orphan that spells the recipient as a JSON escape is named" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _envelope "${RCPT_A}" "${JESC_B}" > "${PATH_CLUSTERS}/a.dev/secrets/Secret.orphan.default.K.enc"
  DOMAIN=a.dev run _rm "${RCPT_B}" 0 1
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == *"[warn] 1 orphan .enc file(s) still list the removed recipient — its key can still decrypt them"* ]] || {
    echo "$output" >&2; return 1; }
}

@test "an .enc whose sops metadata does not parse or lists no age recipient is refused before any write" {
  _load
  _stub_encrypt
  local store="${PATH_CLUSTERS}/a.dev/secrets" content before
  for content in 'enc' '{"data":"ENC[x]","sops":{"lastmodified":"x"}}'; do
    _cache a.dev app "${RCPT_B}"
    printf 'b' > "${store}/Secret.bad.default.K"
    printf '%s\n' "${content}" > "${store}/Secret.bad.default.K.enc"
    touch -t 200001010000 "${store}/Secret.bad.default.K.enc"
    before="$(cat "${CFG}")"
    DOMAIN=a.dev run _rm "${RCPT_B}"
    [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
    [ "$output" = "[error] the sops metadata of ${store}/Secret.bad.default.K.enc does not parse or lists no age recipient
[error]   remove-key reads the recipients of each .enc from its sops metadata — fix or restore those files first; nothing was written" ] || {
      echo "$output" >&2; return 1; }
    [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml was written" >&2; return 1; }
    [ ! -s "${ENCRYPT_LOG}" ]
  done
}

# _rm_path <dir> <argv...> — _rm with PATH set to <dir> alone.
_rm_path() {
  local PATH="${1}"
  shift
  _rm "${@}"
}

@test "without flock, remove-key refuses before any write and makes no lock file" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  # A PATH with the tools of the run and no flock: a link to each one found
  # on PATH.
  local bin="${BATS_TEST_TMPDIR}/no-flock" tool path
  mkdir -p "${bin}"
  for tool in sops yq jq grep cat mktemp mv rm cp dirname basename touch readlink; do
    path="$(type -P "${tool}")" || continue
    ln -s "${path}" "${bin}/${tool}"
  done
  for tool in yq jq; do
    [ -e "${bin}/${tool}" ] || skip "${tool} not installed"
  done
  [ ! -e "${bin}/flock" ]
  local enc="${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc"
  cp -p "${CFG}" "${BATS_TEST_TMPDIR}/cfg.saved"
  cp -p "${enc}" "${BATS_TEST_TMPDIR}/enc.saved"
  DOMAIN=a.dev run _rm_path "${bin}" "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] flock not found — add-key and remove-key do not run without the lock of .sops.yaml; nothing was changed
[error]   install flock, or use the lo binary: it locks on every platform" ] || { echo "$output" >&2; return 1; }
  cmp -s "${CFG}" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  cmp -s "${enc}" "${BATS_TEST_TMPDIR}/enc.saved" || { echo "the .enc changed" >&2; return 1; }
  [ ! -e "${CFG}.lock" ] || { echo "a lock file was made" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

# _sops_writes <text> <rc> — a stub sops that writes the text to its --output
# file and returns rc.
_sops_writes() {
  _sops_text="${1}" _sops_rc="${2}"
  sops() {
    local out=""
    while (( ${#} )); do
      [[ "${1}" != --output ]] || { out="${2}"; shift; }
      shift
    done
    printf '%s' "${_sops_text}" > "${out}"
    return "${_sops_rc}"
  }
}

@test "an .enc is written through a temp file and a rename: a sops run that stops half way leaves the old .enc" {
  _load
  local store="${PATH_CLUSTERS}/a.dev/secrets" f
  f="${store}/Secret.app.default.K"
  printf 'v' > "${f}"
  printf 'old\n' > "${f}.enc"
  cp -p "${f}.enc" "${BATS_TEST_TMPDIR}/enc.saved"
  ln "${f}.enc" "${BATS_TEST_TMPDIR}/enc.link"
  _sops_writes 'ne' 1
  run secrets::_encrypt_file "${f}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] Failed to encrypt Secret.app.default.K" ] || { echo "$output" >&2; return 1; }
  cmp -s "${f}.enc" "${BATS_TEST_TMPDIR}/enc.saved" || { echo "the old .enc changed" >&2; return 1; }
  [ -z "$(find "${store}" -name '.*.tmp-*')" ] || { echo "a temp file was left" >&2; return 1; }

  # A run that ends replaces the .enc (mode 0600); the hard link keeps the old
  # bytes.
  _sops_writes 'new' 0
  run secrets::_encrypt_file "${f}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${f}.enc")" = "new" ]
  cmp -s "${BATS_TEST_TMPDIR}/enc.link" "${BATS_TEST_TMPDIR}/enc.saved" || { echo "the .enc was written in place" >&2; return 1; }
  [ "$(stat -c %a "${f}.enc")" = "600" ]
  [ -z "$(find "${store}" -name '.*.tmp-*')" ] || { echo "a temp file was left" >&2; return 1; }
}

@test "a stale cache (an .enc newer than its plaintext) is refused before any write" {
  # The re-key encrypts the plaintext: an older plaintext would overwrite the
  # newer value of the .enc.
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  touch -t 199901010000 "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  # PATH_CLUSTERS is outside PATH_BASE here, so the path is printed in full.
  [ "$output" = "[error] newer than its decrypted twin: ${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc
[error] 1 .enc file(s) are newer than their plaintext: the cache is stale, and the re-key would write the old value back over the newer one
[error]   Run 'lo secrets decrypt' first (in each store), then run this command again. Nothing was written." ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "while another run holds the lock, remove-key refuses and changes nothing" {
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local before fd; before="$(cat "${CFG}")"
  exec {fd}>"${CFG}.lock"
  flock -n "${fd}"
  LO_SECRETS_LOCK_WAIT=0 DOMAIN=a.dev run _rm "${RCPT_B}"
  exec {fd}>&-
  [ "$status" -eq 1 ]
  [ "$output" = "[error] ${CFG}.lock is held by another lo secrets add-key or remove-key — nothing was changed; run it again when that one ends" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test ".sops.yaml is replaced, not rewritten in place, and keeps its mode" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  chmod 640 "${CFG}"
  ln "${CFG}" "${PATH_BASE}/old.yaml"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${PATH_BASE}/old.yaml")" = "${before}" ] || { echo "the hard link sees the new content" >&2; return 1; }
  [ "$(cat "${CFG}")" != "${before}" ]
  [ "$(stat -c %a "${CFG}")" = "640" ]
  [ -z "$(find "${PATH_BASE}" -name '.sops.yaml.tmp-*')" ]
}

@test "a re-key that stops after the write prints the command that resumes it" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache b.dev app "${RCPT_B}"
  secrets::_encrypt_file() {
    [[ "${1}" != */b.dev/* ]] || { error "Failed to encrypt $(basename "${1}")"; return 1; }
    echo "${1#"${PATH_CLUSTERS}/"}" >> "${ENCRYPT_LOG}"
    _first_rule_envelope > "${1}.enc"
  }
  DOMAIN=a.dev run _rm "${RCPT_B}" 1
  [ "$status" -eq 1 ]
  [[ "$output" == *"[error] Failed to encrypt Secret.app.default.K
[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again resumes it:
[error]   lo secrets remove-key ${RCPT_B} --domain a.dev --all" ]] || { echo "$output" >&2; return 1; }

  # Without --all the rerun refuses and names the store that still lists it.
  _stub_encrypt
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ]
  [[ "$output" == *"[error] still listed in 1 .enc file(s) of a store this run did not re-key: ${PATH_CLUSTERS}/b.dev/secrets
[error]   rerun with --all to re-key every domain store" ]] || { echo "$output" >&2; return 1; }
  # The printed command resumes it.
  DOMAIN=a.dev run _rm "${RCPT_B}" 1
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == "recipient not in .sops.yaml; 1 .enc file(s) still list it — re-keying them"* ]]
  ! grep -qF "${RCPT_B}" "${PATH_CLUSTERS}/b.dev/secrets/Secret.app.default.K.enc"
}

@test "removing the caller's own identity warns" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  printf '# created: test\n# public key: %s\n' "${RCPT_B}" > "${SOPS_AGE_KEY_FILE}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == *"[warn] the recipient is your own decrypt identity (${SOPS_AGE_KEY_FILE}): once it is removed, that identity cannot decrypt the re-keyed files"* ]] || {
    echo "$output" >&2; return 1; }
}

@test "a private key argument is refused and not echoed" {
  # A fixed filler outside the bech32 alphabet: the shape of an age identity,
  # not a key.
  _load
  _stub_encrypt
  local fake arg; fake="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
  local before; before="$(cat "${CFG}")"
  for arg in "${fake}" "${PEM_HEAD}" "-----BEGIN"; do
    DOMAIN=a.dev run _rm "${arg}"
    [ "$status" -eq 1 ]
    [ "$output" = "[error] the key argument looks like a private key — it is not printed
[error] pass an age public key (age1…) or the path to an ed25519 SSH public key" ] || {
      echo "unexpected output (${#output} bytes)" >&2; return 1; }
    [[ "$output" != *"${arg}"* ]]
  done
  [ "$(cat "${CFG}")" = "${before}" ]
}

@test "the real entrypoint refuses a private key before :args can print it" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local fake cmd; fake="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
  for cmd in add-key remove-key; do
    _run_lo secrets "${cmd}" "${PEM_HEAD}"
    [ "$status" -ne 0 ] || { echo "${cmd} <PEM line>: rc 0" >&2; return 1; }
    [[ "$output" == *"looks like a private key — it is not printed"* && "$output" != *OPENSSH* ]] || {
      echo "${cmd} <PEM line>: ${output}" >&2; return 1; }
    # shellcheck disable=SC2086 # the four words of the PEM line
    _run_lo secrets "${cmd}" ${PEM_HEAD}
    [ "$status" -ne 0 ] || { echo "${cmd} <PEM words>: rc 0" >&2; return 1; }
    [[ "$output" == *"looks like a private key — it is not printed"* && "$output" != *OPENSSH* ]] || {
      echo "${cmd} <PEM words>: ${output}" >&2; return 1; }
    _run_lo secrets "${cmd}" "${RCPT_A}" "${fake}"
    [ "$status" -ne 0 ] || { echo "${cmd} <age key> <private key>: rc 0" >&2; return 1; }
    [[ "$output" == *"looks like a private key — it is not printed"* && "$output" != *"${fake}"* ]] || {
      echo "${cmd} <age key> <private key>: ${output}" >&2; return 1; }
  done
}

@test "the help of remove-key carries the revocation caveat" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  _run_lo secrets remove-key --help
  [ "$status" -eq 0 ] || { echo "rc ${status}: ${output}" >&2; return 1; }
  [[ "$output" == *"A removal does not revoke what the key already read"* ]] || { echo "$output" >&2; return 1; }
  [[ "$output" == *"Rotate those secrets after the removal."* ]]
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
  [ "${rules}" -eq 2 ] || { echo "rules ${rules}" >&2; return 1; }
  [ "${last}" -eq 0 ] || { echo "last ${last}" >&2; return 1; }
  secrets::_drop_recipient "${f}" KC
  [ "${edited}" = "creation_rules:
  - path_regex: 'a\\..*'
    age: 'KA,KB'   # team
  - path_regex: '.*'
    age: 'KA,KB,'
  - path_regex: 'c'
    age:   ''
" ] || { printf '%s' "${edited}" >&2; return 1; }
  [ "${rules}" -eq 2 ] || { echo "rules ${rules}" >&2; return 1; }
  [ "${last}" -eq 1 ] || { echo "last ${last}" >&2; return 1; }
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
  # The state after a decrypt: the plaintext is the newer file.
  touch "${store}/Secret.app.default.K"
  _open() { SOPS_AGE_KEY_FILE="${k}/${1}.age" sops decrypt --input-type binary --output-type binary "${store}/Secret.app.default.K.enc" 2>/dev/null; }
  [ "$(_open gone)" = "v" ] || { echo "fixture: the key to remove cannot open the store before the removal" >&2; return 1; }

  DOMAIN=a.dev run _rm "${k}/gone.pub"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  ! _open gone >/dev/null || { echo "the removed key still decrypts the store" >&2; return 1; }
  [ "$(_open keep)" = "v" ] || { echo "the remaining key cannot decrypt the store" >&2; return 1; }
  ! grep -qF "${gone}" "${store}/Secret.app.default.K.enc"
}

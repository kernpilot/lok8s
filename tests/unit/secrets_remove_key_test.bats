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
# _summary <rules> <listed> <rekeyed> — the summary of a remove-key run that
# ended, with no orphan that --skip-orphans accepted (Go twin: rmSummary).
_summary() {
  printf '%s\n' "${1} creation rule(s) changed" \
    "${2} .enc file(s) listed the recipient in their labels" \
    "${3} .enc file(s) re-keyed with a new data key" \
    "done — every managed .enc of the selected stores was rewritten: commit them together with .sops.yaml"
}
# _no_label <recipient> — the warning for a key in no rule and on no label.
_no_label() {
  echo "[warn] ${1} is in no creation rule and in no .enc label: remove-key re-keys every managed .enc anyway, and changes no rule"
}
_not_written='[error]   the re-key encrypts with age through the first matching rule: fix the creation rules first. Nothing was written.'
# The scope line of the own-identity check: every run that reaches the check
# prints it, with a match or not (Go twin: scopeWarn).
_scope='[warn] the check for your own decrypt identity reads only SOPS_AGE_KEY_FILE (default ~/.config/sops/age/keys.txt) and the .pub of LOK8S_SSH_KEY (default ~/.ssh/id_ed25519); it does not read SOPS_AGE_KEY, SOPS_AGE_KEY_CMD or other files'

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
$(_summary 1 1 1)
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

@test "a recipient in no rule and on no .enc label warns, re-keys every managed .enc and changes no rule" {
  # Go twin: TestRemoveKeyWarnsAboutAKeyInNoRuleAndOnNoLabel.
  _load
  _stub_encrypt
  _rules "${RCPT_A}"
  _cache a.dev app "${RCPT_A}"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "$(_no_label "${RCPT_B}")
${_scope}
recipient not in .sops.yaml: .sops.yaml is not changed
  ${RCPT_B}
re-keying 1 cache file(s) across 1 store(s)…
Encrypted 1 secret(s)
$(_summary 0 0 1)
${_rotate}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml changed" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev/secrets/Secret.app.default.K" ]
}

@test "a recipient in no rule and on no label of the store: the end names a store that still lists it" {
  _load
  _stub_encrypt
  _rules "${RCPT_A}"
  _cache b.dev app "${RCPT_B}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "$(_no_label "${RCPT_B}")
${_scope}
recipient not in .sops.yaml: .sops.yaml is not changed
  ${RCPT_B}
re-keying 0 cache file(s) across 1 store(s)…
[warn] still listed in 1 .enc file(s) of a store this run did not re-key: ${PATH_CLUSTERS}/b.dev/secrets
[warn]   rerun with --all to re-key every domain store
$(_summary 0 0 0)
${_rotate}" ] || { echo "$output" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "every managed .enc is re-keyed, and the summary counts the files that listed the key apart from the files re-keyed" {
  # Go twin: TestRemoveKeySummaryCountsTheListedAndTheRekeyedFiles.
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache a.dev other "${RCPT_A}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = $'a.dev/secrets/Secret.app.default.K\na.dev/secrets/Secret.other.default.K' ] || {
    echo "encrypted: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
  [[ "$output" == *"removed recipient from .sops.yaml (1 creation rule(s))
  ${RCPT_B}
re-keying 2 cache file(s) across 1 store(s)…
Encrypted 2 secret(s)
$(_summary 1 1 2)
${_rotate}" ]] || { echo "$output" >&2; return 1; }
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

@test "the rule check reads a file name with a newline as one path" {
  # The paths go to yq as one JSON array. In each rule set below, the first
  # rule matches one reading of the name only: the part after the newline
  # alone, or the whole name across the newline.
  _load
  local nl="${PATH_BASE}/Secret.app.default.K"$'\n'"x.enc"
  local pgp="    pgp: 'FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000'"
  local -a none_list=() notage_list=() badage_list=()
  secrets::_rules_check "${nl}" <<< "$(printf "creation_rules:\n  - path_regex: '^x\$'\n    age: '%s'\n%s\n  - path_regex: 'Secret\\\\..*'\n    age: '%s'\n" "${RCPT_A}" "${pgp}" "${RCPT_A}")" || {
    echo "the rule check fails on the name" >&2; return 1; }
  (( ${#none_list[@]} + ${#notage_list[@]} + ${#badage_list[@]} == 0 )) || {
    echo "the name got the rule of the part after its newline" >&2; return 1; }
  secrets::_rules_check "${nl}" <<< "$(printf "creation_rules:\n  - path_regex: \"K\\\\nx\$\"\n    age: '%s'\n%s\n  - path_regex: 'Secret\\\\..*'\n    age: '%s'\n" "${RCPT_A}" "${pgp}" "${RCPT_A}")" || {
    echo "the rule check fails on the name" >&2; return 1; }
  [[ ${#notage_list[@]} -eq 1 && "${notage_list[0]}" == "${nl}" && ${#none_list[@]} -eq 0 ]] || {
    echo "the name did not get the rule that matches it whole" >&2; return 1; }
}

@test "remove-key re-keys a managed file whose name holds a newline" {
  # Go twin: TestRemoveKeyRekeysANameWithANewline.
  _load
  _stub_encrypt
  local store="${PATH_CLUSTERS}/a.dev/secrets" nl
  nl="${store}/Secret.app.default.K"$'\n'"x"
  _cache a.dev app "${RCPT_B}"
  printf 'w' > "${nl}"
  _envelope "${RCPT_B}" > "${nl}.enc"
  touch -t 200001010000 "${nl}.enc"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == *"re-keying 2 cache file(s) across 1 store(s)…"* ]] || { echo "$output" >&2; return 1; }
  ! grep -qF "${RCPT_B}" "${nl}.enc" || { echo "the .enc whose name holds a newline still lists the recipient" >&2; return 1; }
  ! grep -qF "${RCPT_B}" "${store}/Secret.app.default.K.enc" || { echo "the other .enc still lists the recipient" >&2; return 1; }
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

@test "with --all and no domain store it can read, the refusal names each store" {
  # Not "no secret store found": each path may still hold an .enc that lists
  # the recipient (Go twin: TestRemoveKeyAllNamesEveryStoreItCannotRead).
  [ "$(id -u)" -ne 0 ] || skip "root reads every file"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache b.dev app "${RCPT_B}"
  local before; before="$(cat "${CFG}")"
  chmod 000 "${PATH_CLUSTERS}/a.dev" "${PATH_CLUSTERS}/b.dev"
  DOMAIN=a.dev run _rm "${RCPT_B}" 1
  chmod 755 "${PATH_CLUSTERS}/a.dev" "${PATH_CLUSTERS}/b.dev"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] cannot read ${PATH_CLUSTERS}/a.dev/secrets
[error] cannot read ${PATH_CLUSTERS}/b.dev/secrets
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

_duplicate='[error]   a duplicate member can hide a recipient from this check — re-encrypt those files with sops from their plaintext, then run this command again; nothing was written'

# _row <want> <json> — what secrets::_enc_lists prints for RCPT_B on the JSON,
# with "A" and "B" standing for RCPT_A and RCPT_B; an empty want is a refusal
# as metadata that does not parse (rc 1, no output).
_row() {
  local f="${BATS_TEST_TMPDIR}/row.enc" got rc=0
  printf '%s\n' "$(sed -e "s/\"A\"/\"${RCPT_A}\"/g" -e "s/\"B\"/\"${RCPT_B}\"/g" <<< "${2}")" > "${f}"
  got="$(secrets::_enc_lists "${RCPT_B}" "${f}")" || rc=$?
  if [[ -z "${1}" ]]; then
    [[ "${rc}" -eq 1 && -z "${got}" ]] || { echo "${2}: got '${got}' (rc ${rc}), want the metadata refusal" >&2; return 1; }
  else
    [[ "${rc}" -eq 0 && "${got}" == "${1}" ]] || { echo "${2}: got '${got}' (rc ${rc}), want ${1}" >&2; return 1; }
  fi
}

@test "the read of the sops metadata refuses a member twice: the rows of TestEncRecipientsRefusesADuplicateMember" {
  # A map decode and jq keep the last of two members, so the first could hide
  # a recipient. The Go twin reads the same rows.
  _load
  local esc; esc="$(printf '\134')u0073ops"
  _row duplicate '{"data":"x","sops":{"key_groups":[{"age":[{"recipient":"B"},{"recipient":"A"}]}]},"sops":{"age":[{"recipient":"A"}]}}'
  _row duplicate '{"sops":{"age":[{"recipient":"B"}]},"data":"x","sops":{"age":[{"recipient":"A"}]}}'
  _row duplicate '{"sops":{"age":[{"recipient":"B"}]},"'"${esc}"'":{"age":[{"recipient":"A"}]}}'
  _row duplicate '{"sops":{"age":[{"recipient":"B"},{"recipient":"A"}],"age":[{"recipient":"A"}]}}'
  _row duplicate '{"sops":{"key_groups":[{"age":[{"recipient":"B"}]}],"key_groups":[{"age":[{"recipient":"A"}]}]}}'
  _row duplicate '{"sops":{"key_groups":[{"age":[{"recipient":"B"}],"age":[{"recipient":"A"}]}]}}'
  _row duplicate '{"sops":{"age":[{"recipient":"B","recipient":"A","enc":"x"}]}}'
  _row duplicate '{"sops":{"age":[{"recipient":"A","enc":"x","enc":"y"}]}}'
  _row duplicate '{"sops":{"age":[{"recipient":"A"}],"mac":"x","mac":"y"}}'
  _row duplicate '{"sops":{"age":[{"recipient":"A"}],"other":[{"x":{"y":1,"y":2}}]}}'
  _row duplicate '{"sops":{"age":[{"recipient":"A"}],"mac":"x","mac":{"y":1}}}'
  _row duplicate '{"sops":{"age":[{"recipient":"A"}],"mac":{"y":1},"mac":"x"}}'
  _row duplicate '{"sops":{"age":[{"recipient":"A"}],"mac":{},"mac":{"y":1}}}'
  # No duplicate in the metadata: the data payload is not read for one.
  _row clear '{"data":"x","data":"y","sops":{"age":[{"recipient":"A"}]}}'
  _row listed '{"sops":{"key_groups":[{"age":[{"recipient":"A"}]},{"age":[{"recipient":"B"}]}]}}'
  # The shape is read first: a last member that does not parse is the
  # metadata refusal.
  _row '' '{"sops":{"age":[{"recipient":"A"}]},"sops":"x"}'
}

# _duplicate_refused <json> — an .enc of a.dev whose sops metadata holds a
# member twice ("A" and "B" stand for RCPT_A and RCPT_B, B the one to remove):
# a read that keeps the last member does not list RCPT_B, and remove-key
# refuses the file by name before any write.
_duplicate_refused() {
  local enc="${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc" before
  printf 'v' > "${enc%.enc}"
  printf '%s\n' "$(sed -e "s/\"A\"/\"${RCPT_A}\"/g" -e "s/\"B\"/\"${RCPT_B}\"/g" <<< "${1}")" > "${enc}"
  touch -t 200001010000 "${enc}"
  ! jq -r '(.sops.age[]?, .sops.key_groups[]?.age[]?) | .recipient' "${enc}" | grep -qxF "${RCPT_B}" || {
    echo "fixture: a read that keeps the last member lists the recipient: ${1}" >&2; return 1; }
  cp -p "${enc}" "${BATS_TEST_TMPDIR}/enc.saved"
  before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] the sops metadata of ${enc} holds a duplicate member
${_duplicate}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml was written" >&2; return 1; }
  cmp -s "${enc}" "${BATS_TEST_TMPDIR}/enc.saved" || { echo "the .enc changed" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "an .enc whose sops metadata holds a member twice is refused before any write" {
  _load
  _stub_encrypt
  # The recipient twice in the entry of the key to remove; the sops member
  # twice; age twice; key_groups twice.
  _duplicate_refused '{"data":"ENC[x]","sops":{"age":[{"recipient":"B","recipient":"A","enc":"x"},{"recipient":"A","enc":"x"}]}}'
  _duplicate_refused '{"data":"ENC[x]","sops":{"key_groups":[{"age":[{"recipient":"B","enc":"x"},{"recipient":"A","enc":"x"}]}]},"sops":{"age":[{"recipient":"A","enc":"x"}]}}'
  _duplicate_refused '{"data":"ENC[x]","sops":{"age":[{"recipient":"B","enc":"x"},{"recipient":"A","enc":"x"}],"age":[{"recipient":"A","enc":"x"}]}}'
  _duplicate_refused '{"data":"ENC[x]","sops":{"key_groups":[{"age":[{"recipient":"B","enc":"x"},{"recipient":"A","enc":"x"}]}],"key_groups":[{"age":[{"recipient":"A","enc":"x"}]}]}}'
}

@test "a real .enc with a member twice: sops opens one with the removed key, and remove-key refuses both" {
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  # Throwaway keys in the test's temp dir, as in the round trip below.
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
  local store="${PATH_CLUSTERS}/a.dev/secrets" enc compact kept_entry from to
  enc="${store}/Secret.app.default.K.enc"
  printf 'v' > "${enc%.enc}"
  domain=a.dev secrets::encrypt >/dev/null
  _open() { SOPS_AGE_KEY_FILE="${k}/${1}.age" sops decrypt --input-type binary --output-type binary "${enc}" 2>/dev/null; }
  compact="$(jq -c . "${enc}")"
  kept_entry="$(jq -c --arg r "${keep}" '.sops.age[] | select(.recipient == $r)' "${enc}")"
  from="\"recipient\":\"${gone}\""
  to="\"recipient\":\"${gone}\",\"recipient\":\"${keep}\""
  [[ "${compact}" == *"${from}"* && -n "${kept_entry}" ]] || { echo "fixture: the .enc does not list both recipients" >&2; return 1; }

  # The recipient twice in the entry of the removed key: sops reads the last
  # one, and still opens the file with the removed key.
  printf '%s\n' "${compact/"${from}"/"${to}"}" > "${enc}"
  touch "${enc%.enc}"
  [ "$(_open gone)" = "v" ] || { echo "fixture: sops does not open the file with the removed key" >&2; return 1; }
  ! jq -r '.sops.age[].recipient' "${enc}" | grep -qxF "${gone}" || { echo "fixture: jq lists the removed key" >&2; return 1; }
  cp -p "${enc}" "${BATS_TEST_TMPDIR}/enc.saved"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${gone}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] the sops metadata of ${enc} holds a duplicate member
${_duplicate}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml was written" >&2; return 1; }
  cmp -s "${enc}" "${BATS_TEST_TMPDIR}/enc.saved" || { echo "the .enc changed" >&2; return 1; }

  # A second sops member with the kept key only: sops refuses the file, and
  # remove-key names the duplicate.
  local second=",\"sops\":{\"age\":[${kept_entry}]}}"
  [[ "${compact}" == *'}' ]] || { echo "fixture: the compact .enc does not end with an object" >&2; return 1; }
  printf '%s%s\n' "${compact%?}" "${second}" > "${enc}"
  touch "${enc%.enc}"
  ! _open gone >/dev/null || { echo "fixture: sops opens a file with the sops member twice" >&2; return 1; }
  ! _open keep >/dev/null || { echo "fixture: sops opens a file with the sops member twice" >&2; return 1; }
  DOMAIN=a.dev run _rm "${gone}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] the sops metadata of ${enc} holds a duplicate member
${_duplicate}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ] || { echo ".sops.yaml was written" >&2; return 1; }
}

# ── Another .enc in a store ──────────────────────────────────────────────
# remove-key re-keys only the managed Secret.*.enc files at the top of a
# store; any other file whose name ends in .enc, at any depth, is refused by
# its path before any write (Go twin: TestRemoveKeyRefusesAnotherEnc).

_other='[error]   an .enc file that is not re-keyed can keep the removed recipient — nothing was written'

# _other_refused <domain> <rel> — a copy of the managed .enc of the domain at
# <rel> under its store is refused by its path before any write; .sops.yaml
# and both .enc files stay byte-equal. The copy is removed after.
_other_refused() {
  local store="${PATH_CLUSTERS}/${1}/secrets" enc before
  enc="${store}/Secret.app.default.K.enc"
  mkdir -p "$(dirname -- "${store}/${2}")"
  cp -p "${enc}" "${store}/${2}"
  cp -p "${enc}" "${BATS_TEST_TMPDIR}/enc.saved"
  before="$(cat "${CFG}")"
  DOMAIN="${1}" run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "${2}: status $status: $output" >&2; return 1; }
  [ "$output" = "[error] remove-key re-keys only the managed Secret.*.enc files; ${store}/${2} is another .enc file: re-key or remove it by hand, then run again
${_other}" ] || { echo "${2}: $output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ] || { echo "${2}: .sops.yaml was written" >&2; return 1; }
  cmp -s "${enc}" "${BATS_TEST_TMPDIR}/enc.saved" || { echo "${2}: the managed .enc changed" >&2; return 1; }
  cmp -s "${store}/${2}" "${BATS_TEST_TMPDIR}/enc.saved" || { echo "${2}: the other .enc changed" >&2; return 1; }
  [ ! -s "${ENCRYPT_LOG}" ] || { echo "${2}: an encrypt ran" >&2; return 1; }
  rm -f -- "${store}/${2}"
}

@test "another .enc in a store is refused before any write: a copy, a dot name, a file in a subdirectory, a symlinked store" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _other_refused a.dev backup.enc
  _other_refused a.dev .x.enc
  _other_refused a.dev old/Secret.app.default.K.enc
  # A store that is a symlink to a directory is walked; links below it are not.
  mkdir -p "${BATS_TEST_TMPDIR}/real-store" "${PATH_CLUSTERS}/c.dev"
  ln -s "${BATS_TEST_TMPDIR}/real-store" "${PATH_CLUSTERS}/c.dev/secrets"
  _cache c.dev app "${RCPT_B}"
  _other_refused c.dev backup.enc
}

@test "a directory in a store that it cannot read is refused before any write" {
  [ "$(id -u)" -ne 0 ] || skip "root reads every file"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local store="${PATH_CLUSTERS}/a.dev/secrets" before
  mkdir -p "${store}/sub"
  chmod 000 "${store}/sub"
  before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] cannot read ${store}/sub
[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "the other files of a store do not stop the walk: .gitignore, encrypt temp files, restore.d, tls, a directory named *.enc" {
  # The temp file of an encrypt of either twin (.<name>.enc.tmp-<random>)
  # does not end in .enc (Go twin: TestRemoveKeyWalksPastTheOtherFilesOfAStore).
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local store="${PATH_CLUSTERS}/a.dev/secrets" rel
  for rel in .gitignore .Secret.app.default.K.enc.tmp-123456789 .Secret.app.default.K.enc.tmp-AbC123 \
    restore.d/app.yaml tls/tls.crt dir.enc/notes; do
    mkdir -p "$(dirname -- "${store}/${rel}")"
    printf 'x' > "${store}/${rel}"
  done
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" != *"another .enc file"* ]] || { echo "$output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev/secrets/Secret.app.default.K" ] || { echo "encrypted: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
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

@test "the post-check refuses a duplicate member that the re-encrypt wrote, names the file and prints the resume command" {
  # The real secrets::_encrypt_file with a sops stub whose encrypt writes
  # metadata with the age list twice, the first one with the removed
  # recipient (Go twin: TestRemoveKeyPostCheckRefusesADuplicateMember).
  _load
  _cache a.dev app "${RCPT_B}"
  local enc="${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc" dup
  dup="{\"data\":\"ENC[x]\",\"sops\":{\"age\":[{\"recipient\":\"${RCPT_B}\",\"enc\":\"x\"},{\"recipient\":\"${RCPT_A}\",\"enc\":\"x\"}],\"age\":[{\"recipient\":\"${RCPT_A}\",\"enc\":\"x\"}]}}"
  _sops_writes "${dup}" 0
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(cat "${enc}")" = "${dup}" ] || { echo "the .enc is not the file that the stub wrote" >&2; return 1; }
  ! jq -r '.sops.age[].recipient' "${enc}" | grep -qxF "${RCPT_B}" || { echo "fixture: jq lists the removed recipient" >&2; return 1; }
  [[ "$output" == *"Encrypted 1 secret(s)
[error] the sops metadata of ${enc} holds a duplicate member
[error] 0 .enc file(s) still list the removed recipient after the re-key, 1 cannot be read or do not parse
[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again resumes it:
[error]   lo secrets remove-key ${RCPT_B} --domain a.dev" ]] || { echo "$output" >&2; return 1; }
  ! grep -qF "${RCPT_B}" "${CFG}" || { echo ".sops.yaml still lists the recipient: the run stopped before the first write" >&2; return 1; }
}

# _sops_appears <kind> — a sops stub whose encrypt writes the envelope of the
# first rule to its --output file, as sops would, and adds one file to the
# store of a.dev during the run: another .enc (other), a managed .enc with a
# decrypted twin (managed), an orphan (orphan), or a directory that the walk
# cannot read (dir). Each added .enc lists RCPT_B.
_sops_appears() {
  _appears="${1}"
  sops() {
    local out="" s="${PATH_CLUSTERS}/a.dev/secrets"
    while (( ${#} )); do
      [[ "${1}" != --output ]] || { out="${2}"; shift; }
      shift
    done
    _first_rule_envelope > "${out}"
    case "${_appears}" in
      other) _envelope "${RCPT_B}" > "${s}/backup.enc" ;;
      managed) printf 'n' > "${s}/Secret.new.default.K" && _envelope "${RCPT_B}" > "${s}/Secret.new.default.K.enc" ;;
      orphan) _envelope "${RCPT_B}" > "${s}/Secret.orphan.default.K.enc" ;;
      dir) mkdir "${s}/sub" && chmod 000 "${s}/sub" ;;
    esac
  }
}

@test "the post-check refuses a file that appears during the run, names it and prints the resume command" {
  # The post-check repeats the reads and the walk of the check before the
  # first write (Go twin: TestRemoveKeyPostCheckRefusesAFileThatAppears).
  _load
  local s="${PATH_CLUSTERS}/a.dev/secrets" kind want
  local changed='[error] 1 file(s) of the stores changed during the run: the stores must be quiet while remove-key runs'
  for kind in other managed orphan dir; do
    if [[ "${kind}" == dir && "$(id -u)" -eq 0 ]]; then
      continue # root reads every directory
    fi
    chmod -R u+rwx "${PATH_CLUSTERS}"
    rm -rf -- "${s}"
    mkdir -p "${s}"
    _rules "${RCPT_A},${RCPT_B}"
    _cache a.dev app "${RCPT_B}"
    _sops_appears "${kind}"
    case "${kind}" in
      other) want="[error] remove-key re-keys only the managed Secret.*.enc files; ${s}/backup.enc is another .enc file: re-key or remove it by hand, then run again
${changed}" ;;
      managed) want="[error] a managed .enc that this run did not re-key: ${s}/Secret.new.default.K.enc
${changed}" ;;
      orphan) want="[error] an .enc with no decrypted twin after the re-key: ${s}/Secret.orphan.default.K.enc
${changed}" ;;
      dir) want="[error] cannot read ${s}/sub
[error] 0 .enc file(s) still list the removed recipient after the re-key, 1 cannot be read or do not parse" ;;
    esac
    DOMAIN=a.dev run _rm "${RCPT_B}"
    [ "$status" -eq 1 ] || { echo "${kind}: status $status: $output" >&2; return 1; }
    [[ "$output" == *"Encrypted 1 secret(s)
${want}
[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again resumes it:
[error]   lo secrets remove-key ${RCPT_B} --domain a.dev" ]] || { echo "${kind}: $output" >&2; return 1; }
    ! grep -qF "${RCPT_B}" "${s}/Secret.app.default.K.enc" || { echo "${kind}: the managed .enc was not re-keyed" >&2; return 1; }
  done
}

@test "with --skip-orphans, an orphan that appears during the run is a warning" {
  _load
  _cache a.dev app "${RCPT_B}"
  _sops_appears orphan
  DOMAIN=a.dev run _rm "${RCPT_B}" 0 1
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" == *"Encrypted 1 secret(s)
[warn] 1 orphan .enc file(s) still list the removed recipient — its key can still decrypt them
$(_summary 1 1 1)
${_rotate}" ]] || { echo "$output" >&2; return 1; }
}

# ── The .enc path of an encrypt ──────────────────────────────────────────
# secrets::_encrypt_file, which set --encrypt, encrypt and the re-key share,
# writes an .enc only to a regular file or a path that does not exist (Go
# twin: TestSetEncryptRefusesAnEncPathThatIsNotARegularFile).

_enc_path_refusal='is not a regular file (a symlink, a directory, a FIFO or a device): the .enc was not written — remove it, then encrypt again'

# _set_encrypt <proj> — `lo secrets --domain a.dev set -n app --encrypt K v`
# through the real entrypoint in the project, as _run_lo runs it, bounded at
# 60 seconds: a step that opens a FIFO fails the test instead of blocking it.
_set_encrypt() {
  run timeout 60 env -C "${1}" \
    PATH_BASE="${1}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${1}/clusters" \
    PATH="${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    "${_PROJECT_ROOT}/.lok8s/lo" secrets --domain a.dev set -n app --encrypt K v
}

@test "set --encrypt refuses an .enc path that is not a regular file and writes nothing outside the store" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  command -v timeout >/dev/null 2>&1 || skip "timeout not installed"
  command -v mkfifo >/dev/null 2>&1 || skip "mkfifo not installed"
  stub_tools kubectl
  # A sops program that logs its run and writes its --output file: the
  # refusal comes before sops runs.
  local fake="${BATS_TEST_TMPDIR}/fake-sops"
  mkdir -p "${fake}"
  cat > "${fake}/sops" <<'EOF'
#!/usr/bin/env bash
echo "sops $*" >> "${FAKE_SOPS_LOG}"
out=""
while (( $# )); do
  [[ "$1" != --output ]] || { out="$2"; shift; }
  shift
done
printf 'ENC' > "${out}"
EOF
  chmod +x "${fake}/sops"
  export FAKE_SOPS_LOG="${BATS_TEST_TMPDIR}/sops.log"
  export PATH="${fake}:${PATH}"
  local kind proj store enc outside
  for kind in dirlink filelink dir fifo; do
    proj="${BATS_TEST_TMPDIR}/proj-${kind}"
    store="${proj}/clusters/a.dev/secrets"
    enc="${store}/Secret.app.default.K.enc"
    outside="${BATS_TEST_TMPDIR}/outside-${kind}"
    mkdir -p "${store}" "${outside}"
    printf "creation_rules:\n  - path_regex: '%s'\n    age: '%s'\n" 'Secret\..*' "${RCPT_A}" > "${proj}/.sops.yaml"
    case "${kind}" in
      dirlink) mkdir "${outside}/target" && ln -s "${outside}/target" "${enc}" ;;
      filelink) printf 'old' > "${outside}/target" && ln -s "${outside}/target" "${enc}" ;;
      dir) mkdir "${enc}" ;;
      fifo) mkfifo "${enc}" ;;
    esac
    _set_encrypt "${proj}"
    [ "$status" -eq 1 ] || { echo "${kind}: status $status: $output" >&2; return 1; }
    [ "$output" = "[error] clusters/a.dev/secrets/Secret.app.default.K.enc ${_enc_path_refusal}" ] || {
      echo "${kind}: $output" >&2; return 1; }
    [ ! -s "${FAKE_SOPS_LOG}" ] || { echo "${kind}: sops ran" >&2; return 1; }
    [ -f "${store}/Secret.app.default.K" ] || { echo "${kind}: no plaintext cache" >&2; return 1; }
    [ -z "$(find "${store}" -name '.*.tmp-*')" ] || { echo "${kind}: a temp file was left" >&2; return 1; }
    case "${kind}" in
      dirlink)
        [[ -L "${enc}" && "$(readlink "${enc}")" == "${outside}/target" ]] || { echo "dirlink: the symlink changed" >&2; return 1; }
        [ -z "$(ls -A "${outside}/target")" ] || { echo "dirlink: a file was written into the directory outside the store" >&2; return 1; }
        ;;
      filelink)
        [[ -L "${enc}" && "$(readlink "${enc}")" == "${outside}/target" ]] || { echo "filelink: the symlink changed" >&2; return 1; }
        [ "$(cat "${outside}/target")" = "old" ] || { echo "filelink: the file outside the store changed" >&2; return 1; }
        ;;
      dir)
        [[ -d "${enc}" && ! -L "${enc}" && -z "$(ls -A "${enc}")" ]] || { echo "dir: the directory changed" >&2; return 1; }
        ;;
      fifo)
        [ -p "${enc}" ] || { echo "fifo: the FIFO changed" >&2; return 1; }
        ;;
    esac
  done
  assert_no_tool_ran
}

@test "with GNU mv the rename takes -T: a symlink made after the check is replaced, not followed" {
  _load
  secrets::_mv_has_t || skip "mv has no -T (not GNU mv)"
  local store="${PATH_CLUSTERS}/a.dev/secrets" outside="${BATS_TEST_TMPDIR}/outside" f
  f="${store}/Secret.app.default.K"
  printf 'v' > "${f}"
  mkdir -p "${outside}"
  # The sops run makes the .enc path a symlink to a directory outside the
  # store: a change in the window between the check and the rename.
  sops() {
    local out=""
    while (( ${#} )); do
      [[ "${1}" != --output ]] || { out="${2}"; shift; }
      shift
    done
    ln -s "${outside}" "${f}.enc"
    printf 'new' > "${out}"
  }
  run secrets::_encrypt_file "${f}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ -z "$(ls -A "${outside}")" ] || { echo "the rename moved the .enc into the directory that the symlink names" >&2; return 1; }
  [[ -f "${f}.enc" && ! -L "${f}.enc" ]] || { echo "the .enc is not a regular file" >&2; return 1; }
  [ "$(cat "${f}.enc")" = "new" ]
}

# ── A mv that takes -T ───────────────────────────────────────────────────
# The bash encrypt renames each .enc with a mv that takes -T (GNU mv, else
# gmv): a mv without -T moves a file INTO a directory at the .enc path. Each
# command that encrypts refuses before its first write without one, and
# secrets::_encrypt_file refuses before it stages a file. The binary needs no
# mv (Go twin: writeReplacing, os.Rename).

_no_t='[error] this mv has no -T, so the encrypt cannot rename onto the .enc safely: install GNU coreutils (gmv) or use the lo binary'

# _mv_stub <dir> <name> <with_t> — a mv program <dir>/<name> that logs each
# move to MV_STUB_LOG and moves with the real mv (MV_STUB_REAL). Its help text
# names --no-target-directory only when with_t is 1.
_mv_stub() {
  {
    printf '#!%s\n' "${BASH}"
    printf 'name=%q with_t=%q\n' "${2}" "${3}"
    cat <<'EOF'
if [[ "${1:-}" == --help ]]; then
  echo "usage: ${name} [-f] source target"
  (( with_t == 0 )) || echo "  -T, --no-target-directory  treat DEST as a normal file"
  exit 0
fi
echo "${name} $*" >> "${MV_STUB_LOG}"
exec "${MV_STUB_REAL}" "$@"
EOF
  } > "${1}/${2}"
  chmod +x "${1}/${2}"
}

# _no_t_run <path> <command> — one command that encrypts, with PATH set to
# <path>: remove-key, add-key, set --encrypt, encrypt, or _encrypt_file (the
# last guard) on the cache file Secret.new.default.K of a.dev.
_no_t_run() {
  local PATH="${1}" _cmd="${2}" domain="a.dev"
  :args() {
    case "${FUNCNAME[1]}" in
      secrets::remove-key) key="${RCPT_B}" ;;
      secrets::add-key) key="${RCPT_A}" ;;
      secrets::set) name=new; key=K; value=w; encrypt=1 ;;
    esac
  }
  case "${_cmd}" in
    _encrypt_file) secrets::_encrypt_file "${PATH_CLUSTERS}/a.dev/secrets/Secret.new.default.K" ;;
    *) "secrets::${_cmd}" ;;
  esac
}

@test "with a mv without -T and no gmv, each command that encrypts refuses before it writes a file" {
  _load
  # A sops stub that writes its --output file: no sops run may come.
  _sops_writes 'new' 0
  _cache a.dev app "${RCPT_B}"
  printf 'n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.new.default.K"
  # The PATH of the commands: the tools that they run before the encrypt, a
  # mv without -T, and no gmv.
  local bin="${BATS_TEST_TMPDIR}/mv-without-t" tool path cmd
  mkdir -p "${bin}"
  for tool in yq jq flock grep cat mktemp rm cp dirname basename touch readlink find sort chmod; do
    path="$(type -P "${tool}")" || continue
    ln -s "${path}" "${bin}/${tool}"
  done
  for tool in yq jq; do
    [ -e "${bin}/${tool}" ] || skip "${tool} not installed"
  done
  export MV_STUB_LOG="${BATS_TEST_TMPDIR}/mv.log" MV_STUB_REAL
  MV_STUB_REAL="$(type -P mv)" || skip "mv not installed"
  _mv_stub "${bin}" mv 0
  : > "${MV_STUB_LOG}"
  [ ! -e "${bin}/gmv" ]
  cp -a "${PATH_BASE}" "${BATS_TEST_TMPDIR}/base.saved"
  cp -a "${PATH_CLUSTERS}" "${BATS_TEST_TMPDIR}/clusters.saved"
  for cmd in remove-key add-key set encrypt _encrypt_file; do
    run _no_t_run "${bin}" "${cmd}"
    [ "$status" -eq 1 ] || { echo "${cmd}: status $status: $output" >&2; return 1; }
    [ "$output" = "${_no_t}" ] || { echo "${cmd}: $output" >&2; return 1; }
    # .sops.yaml, the plaintexts and every .enc are byte-equal, and no file
    # was added: no lock file, no temp file, no .gitignore.
    diff -r "${BATS_TEST_TMPDIR}/base.saved" "${PATH_BASE}" >&2 || { echo "${cmd}: a file under PATH_BASE changed" >&2; return 1; }
    diff -r "${BATS_TEST_TMPDIR}/clusters.saved" "${PATH_CLUSTERS}" >&2 || { echo "${cmd}: a store file changed" >&2; return 1; }
  done
  [ ! -s "${MV_STUB_LOG}" ] || { echo "the mv stub moved a file: $(cat "${MV_STUB_LOG}")" >&2; return 1; }
}

@test "with a mv without -T, the encrypt renames with a gmv that takes -T" {
  _load
  local store="${PATH_CLUSTERS}/a.dev/secrets" bin="${BATS_TEST_TMPDIR}/mv-without-t" f log
  f="${store}/Secret.app.default.K"
  printf 'v' > "${f}"
  printf 'old\n' > "${f}.enc"
  mkdir -p "${bin}"
  export MV_STUB_LOG="${BATS_TEST_TMPDIR}/mv.log" MV_STUB_REAL
  MV_STUB_REAL="$(type -P mv)" || skip "mv not installed"
  secrets::_mv_has_t "${MV_STUB_REAL}" || skip "mv has no -T (not GNU mv): the gmv stub moves with it"
  # First on the PATH of this test only: a mv without -T and a gmv with -T.
  _mv_stub "${bin}" mv 0
  _mv_stub "${bin}" gmv 1
  : > "${MV_STUB_LOG}"
  local PATH="${bin}:${PATH}"
  ! secrets::_mv_has_t || { echo "fixture: the mv stub takes -T" >&2; return 1; }
  _sops_writes 'new' 0
  run secrets::_encrypt_file "${f}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(cat "${f}.enc")" = "new" ]
  log="$(cat "${MV_STUB_LOG}")"
  [[ "${log}" == "gmv -f -T -- ${store}/.Secret.app.default.K.enc.tmp-"*" ${f}.enc" && "${log}" != *$'\n'* ]] || {
    echo "the moves: ${log}" >&2; return 1; }
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

@test "the stale check reads every managed .enc, also one whose labels do not list the key" {
  # Go twin: TestRemoveKeyRefusesAStaleFileWhoseLabelsDoNotListTheKey.
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache a.dev other "${RCPT_A}"
  touch -t 199901010000 "${PATH_CLUSTERS}/a.dev/secrets/Secret.other.default.K"
  local before; before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] newer than its decrypted twin: ${PATH_CLUSTERS}/a.dev/secrets/Secret.other.default.K.enc
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

  # The re-key touched the twin of each file that it re-keyed, so a rerun
  # does not take it for a stale cache. Without --all the key is in no rule
  # and on no label of the store: the rerun warns, re-keys the store again,
  # ends, and names the store that still lists it (Go twin:
  # TestRemoveKeyResumesAStoppedRekey).
  _stub_encrypt
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" == "$(_no_label "${RCPT_B}")
${_scope}
recipient not in .sops.yaml: .sops.yaml is not changed"*"[warn] still listed in 1 .enc file(s) of a store this run did not re-key: ${PATH_CLUSTERS}/b.dev/secrets
[warn]   rerun with --all to re-key every domain store
$(_summary 0 0 1)
${_rotate}" ]] || { echo "$output" >&2; return 1; }
  # The printed command resumes it.
  DOMAIN=a.dev run _rm "${RCPT_B}" 1
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == "${_scope}
recipient not in .sops.yaml: .sops.yaml is not changed"*"$(_summary 0 1 2)
${_rotate}" ]] || { echo "$output" >&2; return 1; }
  ! grep -qF "${RCPT_B}" "${PATH_CLUSTERS}/b.dev/secrets/Secret.app.default.K.enc"
}

@test "removing the caller's own identity warns" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  printf '# created: test\n# public key: %s\n' "${RCPT_B}" > "${SOPS_AGE_KEY_FILE}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  # The scope line follows: the two sources the check reads, and those it
  # does not.
  [[ "$output" == *"[warn] the recipient is your own decrypt identity (${SOPS_AGE_KEY_FILE}): once it is removed, that identity cannot decrypt the re-keyed files
${_scope}
"* ]] || {
    echo "$output" >&2; return 1; }
  [ "$(grep -cF "${_scope}" <<< "$output")" -eq 1 ]
}

@test "a removal of a key that is not the caller's prints the scope line of the identity check only" {
  # No match warning does not mean that the key is not the caller's: the
  # check does not read every source.
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  printf '# created: test\n# public key: %s\n' "${RCPT_A}" > "${SOPS_AGE_KEY_FILE}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" != *"your own decrypt identity ("* ]] || { echo "$output" >&2; return 1; }
  [ "$(grep -cxF "${_scope}" <<< "$output")" -eq 1 ] || { echo "$output" >&2; return 1; }
  # Before the first write, as in the Go twin.
  [[ "$output" == "${_scope}
removed recipient from .sops.yaml"* ]] || { echo "$output" >&2; return 1; }
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

@test "every managed .enc is re-keyed, also a file whose labels do not list the removed key" {
  # The labels are not trusted to choose the files. The file whose labels do
  # not list the key gets new bytes (a new data key) and still decrypts with
  # the key that stays (Go twin:
  # TestRemoveKeyRekeysAFileWhoseLabelsDoNotListTheKey).
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  local k="${BATS_TEST_TMPDIR}/keys" n
  mkdir -p "${k}"
  for n in keep gone; do
    ssh-keygen -q -t ed25519 -N '' -C "${n}" -f "${k}/${n}" </dev/null >/dev/null 2>&1
    ssh-to-age -private-key -i "${k}/${n}" -o "${k}/${n}.age" 2>/dev/null
  done
  local keep gone store="${PATH_CLUSTERS}/a.dev/secrets"
  keep="$(ssh-to-age -i "${k}/keep.pub")"
  gone="$(ssh-to-age -i "${k}/gone.pub")"
  # other for keep only, app for keep and gone: each encrypted under the rule
  # of its own time.
  printf 'w' > "${store}/Secret.other.default.K"
  printf 'v' > "${store}/Secret.app.default.K"
  _rules "${keep}"
  secrets::_encrypt_file "${store}/Secret.other.default.K" >/dev/null
  _rules "${keep},${gone}"
  secrets::_encrypt_file "${store}/Secret.app.default.K" >/dev/null
  # The state after a decrypt: the plaintexts are the newer files.
  touch "${store}/Secret.other.default.K" "${store}/Secret.app.default.K"
  _open() { SOPS_AGE_KEY_FILE="${k}/${1}.age" sops decrypt --input-type binary --output-type binary "${store}/Secret.${2}.default.K.enc" 2>/dev/null; }
  ! grep -qF "${gone}" "${store}/Secret.other.default.K.enc" || { echo "fixture: the labels of other list the key to remove" >&2; return 1; }
  [ "$(_open gone app)" = "v" ] || { echo "fixture: the key to remove cannot open app" >&2; return 1; }
  cp -p "${store}/Secret.other.default.K.enc" "${BATS_TEST_TMPDIR}/other.saved"

  DOMAIN=a.dev run _rm "${k}/gone.pub"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  ! cmp -s "${store}/Secret.other.default.K.enc" "${BATS_TEST_TMPDIR}/other.saved" || {
    echo "the .enc whose labels do not list the key was not rewritten" >&2; return 1; }
  [ "$(_open keep other)" = "w" ] || { echo "the rewritten .enc does not decrypt with the key that stays" >&2; return 1; }
  ! _open gone app >/dev/null || { echo "the removed key still decrypts app" >&2; return 1; }
  [ "$(_open keep app)" = "v" ] || { echo "the key that stays cannot decrypt app" >&2; return 1; }
}

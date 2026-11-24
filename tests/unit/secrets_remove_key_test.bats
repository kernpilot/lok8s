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
# real one and skips without sops, ssh-to-age or ssh-keygen. Each test that
# runs the real sops carries the tag real-sops: the argsh container of CI has
# no ssh-keygen, so CI runs that tag again on the runner, where a skip fails
# the step. That step also checks the TAP plan of every tagged test under
# tests/unit (1..21): a test that gets or loses the tag changes that number
# in .github/workflows/ci.yml and TESTING.md. No test prints a private key.

setup() {
  load "../test_helper"
  setup_tmpdir
  isolate_secrets_home
  export SOPS_AGE_KEY_FILE="${BATS_TEST_TMPDIR}/no-keys.txt"
  unset LO_SECRETS_LOCK_WAIT
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
  teardown_secrets_home
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
# The envelopes hold no ciphertext, so the compare of each twin with its
# decrypted .enc before the first write passes too (_stub_compare); the twin
# tests run the real compare.
_stub_encrypt() {
  secrets::_encrypt_file() {
    echo "${1#"${PATH_CLUSTERS}/"}" >> "${ENCRYPT_LOG}"
    _first_rule_envelope > "${1}.enc"
  }
  _stub_compare
}

_stub_compare() {
  secrets::_twin_matches() { return 0; }
}

# _stub_sops — the real secrets::_encrypt_file, with sops as a function: the
# encrypt writes the envelope of the first rule to its --output path and logs
# the plaintext (relative to PATH_CLUSTERS), as _stub_encrypt does.
_stub_sops() {
  sops() {
    local -a a=("${@}")
    local i out=""
    for (( i = 0; i + 1 < ${#a[@]}; i++ )); do
      [[ "${a[i]}" != --output ]] || out="${a[i + 1]}"
    done
    [[ -n "${out}" ]] || return 1
    echo "${a[-1]#"${PATH_CLUSTERS}/"}" >> "${ENCRYPT_LOG}"
    _first_rule_envelope > "${out}"
  }
  _stub_compare
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
    "done: every managed .enc of the selected stores was rewritten. Commit them together with .sops.yaml"
}
# _no_label <recipient> — the warning for a key in no rule and on no label.
_no_label() {
  echo "[warn] ${1} is in no creation rule and in no .enc label: remove-key re-keys every managed .enc anyway, and changes no rule"
}
_not_written='[error]   the re-key encrypts with age through the first matching rule: fix the creation rules first. Nothing was written.'
# The scope line of the own-identity check: every run that reaches the check
# prints it, with a match or not (Go twin: scopeWarn).
_scope='[warn] the check for your own decrypt identity reads only SOPS_AGE_KEY_FILE (default ~/.config/sops/age/keys.txt) and the .pub of LOK8S_SSH_KEY (default ~/.ssh/id_ed25519); it does not read SOPS_AGE_KEY, SOPS_AGE_KEY_CMD or other files'

# Without a .sops.yaml the command refuses with one line on stderr that
# names the path, prints nothing on stdout and writes no file (Go twin:
# TestKeyCommandsWithoutSopsYAMLRefuseWithOneLine).
@test "remove-key without a .sops.yaml refuses with one line on stderr and writes nothing" {
  bats_require_minimum_version 1.5.0
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  rm -- "${CFG}"
  local before
  before="$(_state "${PATH_BASE}" "${PATH_CLUSTERS}")"
  DOMAIN=a.dev run --separate-stderr _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output / $stderr" >&2; return 1; }
  [ -z "${output}" ] || { echo "stdout: $output" >&2; return 1; }
  [ "${stderr}" = "[error] ${CFG} not found. Run: lo secrets init" ] || {
    echo "stderr: $stderr" >&2; return 1; }
  [ "$(_state "${PATH_BASE}" "${PATH_CLUSTERS}")" = "${before}" ] || {
    echo "a file changed:" >&2
    diff <(echo "${before}") <(_state "${PATH_BASE}" "${PATH_CLUSTERS}") >&2
    return 1
  }
  [ ! -s "${ENCRYPT_LOG}" ]
}

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
  # The summary counts the orphan apart from the files that the run re-keyed
  # (Go twin: TestRemoveKeySkipOrphansWarns).
  [[ "$output" == *"
1 creation rule(s) changed
2 .enc file(s) listed the recipient in their labels
1 .enc file(s) re-keyed with a new data key
done: every managed .enc of the selected stores but 1 orphan(s) was rewritten. Commit them together with .sops.yaml
${_rotate}" ]] || { echo "$output" >&2; return 1; }
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

# _state <path>... — one line per entry under the paths (its path, type, link
# target and mtime), then the checksum of each regular file: any write shows.
_state() {
  find "${@}" -printf '%p %y %l %T@\n' | LC_ALL=C sort
  find "${@}" -type f -print0 | LC_ALL=C sort -z | xargs -0 -r cksum
}

@test "a symlinked .enc that lists the recipient is refused before any write" {
  # The scan follows the link, but the re-key does not write through one
  # (secrets::_encrypt_file refuses it). Found after the .sops.yaml write, the
  # link would stop the run half way, and each rerun would stop at the same
  # file (Go twin: TestKeyCommandsRefuseASymlinkedEncBeforeAnyWrite).
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  _cache a.dev db "${RCPT_B}"
  local store="${PATH_CLUSTERS}/a.dev/secrets" away="${BATS_TEST_TMPDIR}/elsewhere" before
  mkdir -p "${away}"
  mv -- "${store}/Secret.app.default.K.enc" "${away}/"
  ln -s "${away}/Secret.app.default.K.enc" "${store}/Secret.app.default.K.enc"
  touch -t 200001010001 "${store}/Secret.app.default.K" "${store}/Secret.db.default.K"
  before="$(_state "${CFG}" "${PATH_CLUSTERS}" "${away}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${store}/Secret.app.default.K.enc is a symlink: the re-key does not write an .enc through a link
[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(_state "${CFG}" "${PATH_CLUSTERS}" "${away}")" = "${before}" ] || {
    echo "a file changed:" >&2
    diff <(echo "${before}") <(_state "${CFG}" "${PATH_CLUSTERS}" "${away}") >&2
    return 1
  }
  [ ! -s "${ENCRYPT_LOG}" ]
  # The control: the file in the place of the link. The run removes the
  # recipient and re-keys both files.
  rm -- "${store}/Secret.app.default.K.enc"
  mv -- "${away}/Secret.app.default.K.enc" "${store}/"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = "a.dev/secrets/Secret.app.default.K
a.dev/secrets/Secret.db.default.K" ] || { echo "encrypted: $(cat "${ENCRYPT_LOG}")" >&2; return 1; }
  ! grep -qF "${RCPT_B}" "${CFG}" || { cat "${CFG}" >&2; return 1; }
}

@test "a resumed run refuses a symlinked .enc that does not list the recipient before any write" {
  # A stopped re-key: .sops.yaml no longer lists the recipient, the .enc of
  # app was re-keyed and is now a link to a file that does not list it, and
  # the .enc of db still lists it. The labels do not choose the files, so the
  # link is in the re-key list (Go twin:
  # TestRemoveKeyResumeRefusesASymlinkedEncThatDoesNotListTheKey).
  _load
  _stub_encrypt
  _rules "${RCPT_A}"
  _cache a.dev app "${RCPT_A}"
  _cache a.dev db "${RCPT_B}"
  local store="${PATH_CLUSTERS}/a.dev/secrets" away="${BATS_TEST_TMPDIR}/elsewhere" before
  _envelope "${RCPT_A}" "${RCPT_B}" > "${store}/Secret.db.default.K.enc"
  touch -t 200001010000 "${store}/Secret.db.default.K.enc"
  mkdir -p "${away}"
  mv -- "${store}/Secret.app.default.K.enc" "${away}/"
  ln -s "${away}/Secret.app.default.K.enc" "${store}/Secret.app.default.K.enc"
  ! grep -qF "${RCPT_B}" "${away}/Secret.app.default.K.enc" || { echo "fixture: the link lists the recipient" >&2; return 1; }
  # The lock file is there before the run (the lock writes it), so that the
  # listing of the project shows any other new entry.
  : > "${CFG}.lock"
  before="$(_state "${CFG}" "${PATH_CLUSTERS}" "${away}"; ls -A "${PATH_BASE}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${store}/Secret.app.default.K.enc is a symlink: the re-key does not write an .enc through a link
[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(_state "${CFG}" "${PATH_CLUSTERS}" "${away}"; ls -A "${PATH_BASE}")" = "${before}" ] || {
    echo "a file changed:" >&2
    diff <(echo "${before}") <(_state "${CFG}" "${PATH_CLUSTERS}" "${away}"; ls -A "${PATH_BASE}") >&2
    return 1
  }
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "a .sops.yaml that is a symlink stays a link: its target loses the recipient and keeps its mode" {
  # (Go twin: TestKeyCommandsEditTheTargetOfASymlinkedSopsConfig.)
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local conf="${BATS_TEST_TMPDIR}/conf/sops.yaml" want="${BATS_TEST_TMPDIR}/want.yaml"
  mkdir -p "${conf%/*}"
  mv -- "${CFG}" "${conf}"
  chmod 640 "${conf}"
  ln -s "${conf}" "${CFG}"
  printf "creation_rules:\n  - path_regex: 'Secret\\\\..*'\n    age: '%s'\n" "${RCPT_A}" > "${want}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ -L "${CFG}" ] || { echo "the link was replaced by a file" >&2; return 1; }
  [ "$(readlink "${CFG}")" = "${conf}" ]
  cmp "${conf}" "${want}" || { cat "${conf}" >&2; return 1; }
  [ "$(stat -c %a "${conf}")" = 640 ] || { echo "mode $(stat -c %a "${conf}")" >&2; return 1; }
  # No temp file is left, and the lock is beside the target, not the link.
  [ "$(ls -A "${conf%/*}")" = $'sops.yaml\nsops.yaml.lock' ] || { echo "the dir of the target: $(ls -A "${conf%/*}")" >&2; return 1; }
  [ ! -e "${CFG}.lock" ] || { echo "a lock beside the link" >&2; return 1; }
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

# bats test_tags=real-sops
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
  [ "$output" = "[error] flock not found: nothing was changed. init, add-key and remove-key do not run without the lock of .sops.yaml
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
  _stub_compare
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(cat "${enc}")" = "${dup}" ] || { echo "the .enc is not the file that the stub wrote" >&2; return 1; }
  ! jq -r '.sops.age[].recipient' "${enc}" | grep -qxF "${RCPT_B}" || { echo "fixture: jq lists the removed recipient" >&2; return 1; }
  [[ "$output" == *"Encrypted 1 secret(s)
[error] the sops metadata of ${enc} holds a duplicate member
[error] 0 .enc file(s) still list the removed recipient after the re-key, 1 cannot be read or do not parse
[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again, with an identity that decrypts every file, resumes it:
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
  _stub_compare
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
[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again, with an identity that decrypts every file, resumes it:
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

_enc_path_refusal='is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: the .enc was not written. Remove it, then encrypt again'

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
# secrets::_encrypt_file refuses before it stages a file. set and decrypt
# rename each plaintext the same way, so they refuse too, also set without
# --encrypt. The binary needs no mv (Go twin: writeReplacing, os.Rename).

_no_t='[error] this mv has no -T, so the encrypt cannot rename onto the .enc safely: install GNU coreutils (gmv) or use the lo binary'
_no_t_plain='[error] this mv has no -T, so the plaintext write cannot rename onto the plaintext file safely: install GNU coreutils (gmv) or use the lo binary'
_no_t_decrypt='[error] this mv has no -T, so decrypt cannot rename onto the plaintext file safely: install GNU coreutils (gmv) or use the lo binary'

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

# _no_t_run <path> <command> — one command that renames with a mv -T, with
# PATH set to <path>: remove-key, add-key, set --encrypt, set-plain (set
# without --encrypt), decrypt, encrypt, or _encrypt_file (the last guard) on
# the cache file Secret.new.default.K of a.dev.
_no_t_run() {
  local PATH="${1}" _cmd="${2}" domain="a.dev" _enc=1
  [[ "${_cmd}" != set-plain ]] || { _cmd=set _enc=0; }
  :args() {
    case "${FUNCNAME[1]}" in
      secrets::remove-key) key="${RCPT_B}" ;;
      secrets::add-key) key="${RCPT_A}" ;;
      secrets::set) name=new; key=K; value=w; encrypt="${_enc}" ;;
    esac
  }
  case "${_cmd}" in
    _encrypt_file) secrets::_encrypt_file "${PATH_CLUSTERS}/a.dev/secrets/Secret.new.default.K" ;;
    *) "secrets::${_cmd}" ;;
  esac
}

@test "with a mv without -T and no gmv, each command that encrypts or writes a plaintext refuses before it writes a file" {
  _load
  # A sops stub that writes its --output file: no sops run may come.
  _sops_writes 'new' 0
  _cache a.dev app "${RCPT_B}"
  printf 'n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.new.default.K"
  # The PATH of the commands: the tools that they run before the encrypt, a
  # mv without -T, and no gmv.
  local bin="${BATS_TEST_TMPDIR}/mv-without-t" tool path cmd want
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
  for cmd in remove-key add-key set set-plain decrypt encrypt _encrypt_file; do
    # set renames its plaintext before the .enc, so it names the plaintext
    # write, also with --encrypt.
    case "${cmd}" in
      set | set-plain) want="${_no_t_plain}" ;;
      decrypt) want="${_no_t_decrypt}" ;;
      *) want="${_no_t}" ;;
    esac
    run _no_t_run "${bin}" "${cmd}"
    [ "$status" -eq 1 ] || { echo "${cmd}: status $status: $output" >&2; return 1; }
    [ "$output" = "${want}" ] || { echo "${cmd}: $output" >&2; return 1; }
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

_twin_tail='[error]   the re-key encrypts each twin, so a twin that differs would replace the value of its .enc (lo secrets decrypt keeps a twin that is newer than its .enc: to take the .enc value, remove that twin first). Nothing was written.'
# _same_store <saved dir> — fails when .sops.yaml or a file of the stores is
# not byte-equal to the copy in the saved dir.
_same_store() {
  cmp -s "${CFG}" "${1}/sops.yaml" || { echo ".sops.yaml changed before the refusal" >&2; return 1; }
  diff -r "${1}/clusters" "${PATH_CLUSTERS}" >/dev/null || { echo "a file of the stores changed before the refusal" >&2; return 1; }
}

# _keep_gone <dir> — two throwaway SSH keys, keep and gone, and their age
# identities in dir, outside HOME/.ssh; sets the caller's keep and gone to
# their age recipients. Nothing prints the identities.
_keep_gone() {
  local n
  mkdir -p "${1}"
  for n in keep gone; do
    ssh-keygen -q -t ed25519 -N '' -C "${n}" -f "${1}/${n}" </dev/null >/dev/null 2>&1
    ssh-to-age -private-key -i "${1}/${n}" -o "${1}/${n}.age" 2>/dev/null
  done
  keep="$(ssh-to-age -i "${1}/keep.pub")"
  gone="$(ssh-to-age -i "${1}/gone.pub")"
}

# bats test_tags=real-sops
@test "a twin that differs from its decrypted .enc, an .enc the caller cannot decrypt and a twin it cannot read are refused before any write" {
  # Before the first write, remove-key decrypts each file that it re-keys, in
  # memory, with the identity of the caller, and compares it with its twin:
  # the re-key encrypts the twin, so a twin with another value would replace
  # the value of the .enc. .sops.yaml and every file of the stores stay
  # byte-equal (Go twin: TestRemoveKeyRefusesATwinThatDiffersFromItsEnc).
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  local k="${BATS_TEST_TMPDIR}/keys" n saved="${BATS_TEST_TMPDIR}/saved"
  mkdir -p "${k}" "${saved}"
  for n in keep gone; do
    ssh-keygen -q -t ed25519 -N '' -C "${n}" -f "${k}/${n}" </dev/null >/dev/null 2>&1
    ssh-to-age -private-key -i "${k}/${n}" -o "${k}/${n}.age" 2>/dev/null
  done
  local keep gone store="${PATH_CLUSTERS}/a.dev/secrets"
  keep="$(ssh-to-age -i "${k}/keep.pub")"
  gone="$(ssh-to-age -i "${k}/gone.pub")"
  _rules "${keep},${gone}"
  printf 'v' > "${store}/Secret.app.default.K"
  printf 'w' > "${store}/Secret.other.default.K"
  domain=a.dev secrets::encrypt >/dev/null
  # The twin of app gets another value; both twins are the newer files.
  printf 'x' > "${store}/Secret.app.default.K"
  touch "${store}/Secret.app.default.K" "${store}/Secret.other.default.K"
  cp -p "${CFG}" "${saved}/sops.yaml"
  cp -a "${PATH_CLUSTERS}" "${saved}/clusters"

  DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/keep.age" run _rm "${k}/gone.pub"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${store}/Secret.app.default.K.enc differs from its decrypted twin: run lo secrets decrypt for this store, or encrypt the twin first
${_twin_tail}" ] || { echo "$output" >&2; return 1; }
  _same_store "${saved}"

  # No identity: setup's SOPS_AGE_KEY_FILE names no file, and no SSH key.
  DOMAIN=a.dev run _rm "${k}/gone.pub"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] cannot decrypt ${store}/Secret.app.default.K.enc to compare it with its twin
[error] cannot decrypt ${store}/Secret.other.default.K.enc to compare it with its twin
${_twin_tail}" ] || { echo "$output" >&2; return 1; }
  _same_store "${saved}"

  if [ "$(id -u)" -ne 0 ]; then
    chmod 000 "${store}/Secret.app.default.K"
    DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/keep.age" run _rm "${k}/gone.pub"
    chmod 600 "${store}/Secret.app.default.K"
    [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
    [ "$output" = "[error] cannot read ${store}/Secret.app.default.K
${_twin_tail}" ] || { echo "$output" >&2; return 1; }
    _same_store "${saved}"
  fi
}

# bats test_tags=real-sops
@test "an identity that does not decrypt the .enc is refused by the compare before any write" {
  # A third key, in no rule and on no label: the real sops fails to decrypt,
  # and the compare does not read that failure as a twin that matches, also
  # for an empty twin (a failed decrypt prints nothing, and cmp finds an
  # empty twin equal to it). A marker shows that the decrypt got the third
  # identity from its key file; with no identity, the decrypt does not run
  # and the refusal is the same. .sops.yaml and every file of the stores stay
  # byte-equal (Go twin: TestRemoveKeyRefusesATwinThatDiffersFromItsEnc).
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  local k="${BATS_TEST_TMPDIR}/keys" keep gone saved="${BATS_TEST_TMPDIR}/saved" store="${PATH_CLUSTERS}/a.dev/secrets"
  local id marks="${BATS_TEST_TMPDIR}/marks" refusal
  _keep_gone "${k}"
  ssh-keygen -q -t ed25519 -N '' -C other -f "${k}/other" </dev/null >/dev/null 2>&1
  ssh-to-age -private-key -i "${k}/other" -o "${k}/other.age" 2>/dev/null
  _rules "${keep},${gone}"
  printf 'v' > "${store}/Secret.app.default.K"
  : > "${store}/Secret.empty.default.K"
  domain=a.dev secrets::encrypt >/dev/null
  [ -s "${store}/Secret.empty.default.K.enc" ] || { echo "fixture: the empty twin has no .enc" >&2; return 1; }
  mkdir -p "${saved}"
  cp -p "${CFG}" "${saved}/sops.yaml"
  cp -a "${PATH_CLUSTERS}" "${saved}/clusters"
  # The marker: one line for each decrypt, which names the identity that
  # the decrypt got. Nothing prints the identity.
  id="$(grep -m1 'AGE-SECRET-KEY-' "${k}/other.age")"
  sops() {
    if [[ "${1:-}" == decrypt ]]; then
      if [[ "${SOPS_AGE_KEY:-}" == "${id}" ]]; then echo "the third identity" >> "${marks}"; else echo "another identity" >> "${marks}"; fi
    fi
    command sops "$@"
  }
  refusal="[error] cannot decrypt ${store}/Secret.app.default.K.enc to compare it with its twin
[error] cannot decrypt ${store}/Secret.empty.default.K.enc to compare it with its twin
${_twin_tail}"

  DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/other.age" run _rm "${k}/gone.pub"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "${refusal}" ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${marks}")" = $'the third identity\nthe third identity' ] || { echo "marks: $(cat "${marks}")" >&2; return 1; }
  _same_store "${saved}"

  # No identity: setup's SOPS_AGE_KEY_FILE names no file, and no SSH key.
  command rm -f -- "${marks}"
  DOMAIN=a.dev run _rm "${k}/gone.pub"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "${refusal}" ] || { echo "$output" >&2; return 1; }
  [ ! -e "${marks}" ] || { echo "a decrypt ran with no identity: $(cat "${marks}")" >&2; return 1; }
  _same_store "${saved}"
}

# bats test_tags=real-sops
@test "a twin with the value of its .enc passes the compare when it is the older file, and is re-keyed" {
  # The times of the files do not count: app as lo secrets encrypt leaves
  # it, other set back in time (Go twin:
  # TestRemoveKeyRekeysATwinThatIsOlderThanItsEnc).
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  local k="${BATS_TEST_TMPDIR}/keys" keep gone store="${PATH_CLUSTERS}/a.dev/secrets"
  _keep_gone "${k}"
  _rules "${keep},${gone}"
  printf 'v' > "${store}/Secret.app.default.K"
  printf 'w' > "${store}/Secret.other.default.K"
  domain=a.dev secrets::encrypt >/dev/null
  touch -t 199901010000 "${store}/Secret.other.default.K"
  [[ "${store}/Secret.app.default.K.enc" -nt "${store}/Secret.app.default.K" ]] || {
    echo "fixture: the twin of app is not the older file" >&2; return 1; }
  [[ "${store}/Secret.other.default.K.enc" -nt "${store}/Secret.other.default.K" ]] || {
    echo "fixture: the twin of other is not the older file" >&2; return 1; }
  _open() { SOPS_AGE_KEY_FILE="${k}/${1}.age" sops decrypt --input-type binary --output-type binary "${store}/Secret.${2}.default.K.enc" 2>/dev/null; }

  DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/keep.age" run _rm "${k}/gone.pub"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" == *"$(_summary 1 2 2)
${_rotate}" ]] || { echo "$output" >&2; return 1; }
  ! _open gone app >/dev/null || { echo "the removed key still decrypts app" >&2; return 1; }
  ! _open gone other >/dev/null || { echo "the removed key still decrypts other" >&2; return 1; }
  [ "$(_open keep app)" = "v" ] || { echo "the key that stays cannot decrypt app" >&2; return 1; }
  [ "$(_open keep other)" = "w" ] || { echo "the key that stays cannot decrypt other" >&2; return 1; }
}

# bats test_tags=real-sops
@test "a twin with another value than its .enc is refused by the compare when it is the older file, also one whose labels do not list the key" {
  # The times of the files do not count, and the compare reads every
  # managed file. .sops.yaml and every file of the stores stay byte-equal
  # (Go twin: TestRemoveKeyRefusesAnOlderTwinThatDiffersFromItsEnc).
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  local k="${BATS_TEST_TMPDIR}/keys" keep gone store="${PATH_CLUSTERS}/a.dev/secrets" saved="${BATS_TEST_TMPDIR}/saved"
  _keep_gone "${k}"
  mkdir -p "${saved}"
  # other for keep only, app for keep and gone: each encrypted under the rule
  # of its own time.
  printf 'w' > "${store}/Secret.other.default.K"
  printf 'v' > "${store}/Secret.app.default.K"
  _rules "${keep}"
  secrets::_encrypt_file "${store}/Secret.other.default.K" >/dev/null
  _rules "${keep},${gone}"
  secrets::_encrypt_file "${store}/Secret.app.default.K" >/dev/null
  # The twin of other gets another value, and is set back in time.
  printf 'x' > "${store}/Secret.other.default.K"
  touch -t 199901010000 "${store}/Secret.other.default.K"
  [[ "${store}/Secret.other.default.K.enc" -nt "${store}/Secret.other.default.K" ]] || {
    echo "fixture: the twin of other is not the older file" >&2; return 1; }
  ! grep -qF "${gone}" "${store}/Secret.other.default.K.enc" || { echo "fixture: the labels of other list the key to remove" >&2; return 1; }
  cp -p "${CFG}" "${saved}/sops.yaml"
  cp -a "${PATH_CLUSTERS}" "${saved}/clusters"

  DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/keep.age" run _rm "${k}/gone.pub"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${store}/Secret.other.default.K.enc differs from its decrypted twin: run lo secrets decrypt for this store, or encrypt the twin first
${_twin_tail}" ] || { echo "$output" >&2; return 1; }
  _same_store "${saved}"
}

# bats test_tags=real-sops
@test "after a remove-key the encrypt sweep re-encrypts nothing and the lint check names no secret" {
  # Each re-keyed .enc is the newer file of its pair, as after
  # lo secrets encrypt. The store starts in the state after a decrypt (Go
  # twin: TestRemoveKeyLeavesNothingForTheEncryptSweepOrTheLintCheck).
  command -v sops >/dev/null 2>&1 || skip "sops not installed"
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not installed"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen not installed"
  _load
  local k="${BATS_TEST_TMPDIR}/keys" keep gone store="${PATH_CLUSTERS}/a.dev/secrets" saved="${BATS_TEST_TMPDIR}/saved"
  _keep_gone "${k}"
  mkdir -p "${saved}"
  _rules "${keep},${gone}"
  printf 'v' > "${store}/Secret.app.default.K"
  printf 'w' > "${store}/Secret.other.default.K"
  domain=a.dev secrets::encrypt >/dev/null
  touch "${store}/Secret.app.default.K" "${store}/Secret.other.default.K"

  DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/keep.age" run _rm "${k}/gone.pub"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  # The lint check first: it writes nothing, and the sweep could make the
  # files fresh again.
  domain=a.dev run secrets::check_unencrypted
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ -z "$output" ] || { echo "the lint check after remove-key: $output" >&2; return 1; }
  cp -p "${store}/Secret.app.default.K.enc" "${store}/Secret.other.default.K.enc" "${saved}/"
  # DEBUG names each skip and each encrypt of the sweep.
  DEBUG=1 domain=a.dev run secrets::encrypt
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[debug] Skipped 2 (already up to date)" ] || { echo "the encrypt sweep after remove-key: $output" >&2; return 1; }
  cmp -s "${store}/Secret.app.default.K.enc" "${saved}/Secret.app.default.K.enc" || {
    echo "the encrypt sweep re-encrypted app" >&2; return 1; }
  cmp -s "${store}/Secret.other.default.K.enc" "${saved}/Secret.other.default.K.enc" || {
    echo "the encrypt sweep re-encrypted other" >&2; return 1; }
}

@test "the twin compare keeps the identity and the decrypted value out of a trace" {
  # secrets::_twin_matches with a sops stub that prints the value: under
  # set -x, BASH_XTRACEFD and the RETURN, DEBUG and ERR traps, neither the
  # identity line nor the value is in stdout, stderr or the trace.
  _load
  local store="${PATH_CLUSTERS}/a.dev/secrets"
  export SOPS_AGE_KEY_FILE="${BATS_TEST_TMPDIR}/caller.txt"
  # Not a key: a line of the shape that secrets::decrypt_entry reads, built
  # from parts so that no secret scanner matches the source.
  printf '%s\n' "AGE-SECRET-""KEY-1xtrace-canary-twin-identity" > "${SOPS_AGE_KEY_FILE}"
  sops() { printf '%s' "xtrace-canary-twin-value"; }
  printf '%s' "xtrace-canary-twin-value" > "${store}/Secret.app.default.K"
  printf '%s' "{}" > "${store}/Secret.app.default.K.enc"
  assert_no_trace_leak xtrace-canary-twin secrets::_twin_matches "${store}/Secret.app.default.K.enc"
}

@test "the twin compare returns to its caller with lastpipe on" {
  # BASHOPTS=lastpipe in the environment turns lastpipe on. Then the last
  # part of a pipeline runs in the current shell, and an exit there would
  # end the whole remove-key with the status of the first compare, before
  # any re-key. The caller must get the status and go on.
  _load
  local store="${PATH_CLUSTERS}/a.dev/secrets"
  export SOPS_AGE_KEY_FILE="${BATS_TEST_TMPDIR}/caller.txt"
  printf '%s\n' "AGE-SECRET-""KEY-1lastpipe-identity" > "${SOPS_AGE_KEY_FILE}"
  sops() { printf '%s' "lastpipe-twin-value"; }
  printf '%s' "lastpipe-twin-value" > "${store}/Secret.app.default.K"
  printf '%s' "{}" > "${store}/Secret.app.default.K.enc"
  compare_then_go_on() { secrets::_twin_matches "${1}"; echo "returned $?"; }
  shopt -s lastpipe
  set +m
  run compare_then_go_on "${store}/Secret.app.default.K.enc"
  [ "$output" = "returned 0" ] || { echo "a twin that matches: status $status: $output" >&2; return 1; }
  printf '%s' "another-value" > "${store}/Secret.app.default.K"
  run compare_then_go_on "${store}/Secret.app.default.K.enc"
  [ "$output" = "returned 1" ] || { echo "a twin that differs: status $status: $output" >&2; return 1; }
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
  [ "$output" = "[error] ${CFG}.lock is held by another lo secrets init, add-key or remove-key: nothing was changed. When that run ends, run this command again" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "a lock path that is a symlink is refused before the open, and the file that it names keeps its bytes" {
  # The lock can sit in the directory of the file that a .sops.yaml link
  # names, where another user can write. The open appends and the run never
  # uses a lock that a link names (Go twin:
  # TestKeyCommandsRefuseALockThatIsNotARegularFile).
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local victim="${BATS_TEST_TMPDIR}/victim" before
  printf 'the bytes of another file\n' > "${victim}"
  ln -s "${victim}" "${CFG}.lock"
  before="$(cat "${CFG}")"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${CFG}.lock is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was changed. Remove it, then run this command again" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${victim}")" = 'the bytes of another file' ] || { echo "the file that the link names changed" >&2; return 1; }
  [ "$(readlink "${CFG}.lock")" = "${victim}" ]
  [ "$(cat "${CFG}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]
}

@test "two .sops.yaml links that name one file take one lock, beside that file, also through a chain of links" {
  # A run through the other link holds the lock (in this shell); remove-key
  # through the first link, which gets to the file through a second link, is
  # refused and changes nothing. When the lock is free again, the same run
  # edits the file at the end of the chain (Go twin:
  # TestKeyCommandsTakeOneLockForTwoLinksToOneSopsConfig).
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local conf="${BATS_TEST_TMPDIR}/conf/sops.yaml" mid="${BATS_TEST_TMPDIR}/conf/mid.yaml"
  local other="${BATS_TEST_TMPDIR}/other" before dest="" lockfd p
  mkdir -p "${conf%/*}" "${other}"
  mv -- "${CFG}" "${conf}"
  ln -s "${conf}" "${mid}"
  ln -s "${mid}" "${CFG}"
  ln -s "${conf}" "${other}/.sops.yaml"
  before="$(cat "${conf}")"
  secrets::_config_dest "${other}/.sops.yaml"
  exec {lockfd}>>"${dest}.lock"
  flock -n "${lockfd}"
  LO_SECRETS_LOCK_WAIT=0 DOMAIN=a.dev run _rm "${RCPT_B}"
  exec {lockfd}>&-
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] $(readlink -f -- "${conf}").lock is held by another lo secrets init, add-key or remove-key: nothing was changed. When that run ends, run this command again" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${conf}")" = "${before}" ]
  [ ! -s "${ENCRYPT_LOG}" ]

  LO_SECRETS_LOCK_WAIT=0 DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "the run after the release: status $status: $output" >&2; return 1; }
  grep -qF -- "age: '${RCPT_A}'" "${conf}" || { echo "the file at the end of the chain was not edited" >&2; cat "${conf}" >&2; return 1; }
  [ -L "${CFG}" ] && [ -L "${mid}" ] || { echo "a link was replaced by a file" >&2; return 1; }
  [ -s "${ENCRYPT_LOG}" ]
  for p in "${CFG}" "${other}/.sops.yaml" "${mid}"; do
    [ ! -e "${p}.lock" ] || { echo "a lock beside the link ${p}" >&2; return 1; }
  done
}

@test "the resolution of a .sops.yaml link that names no regular file is refused with one line" {
  # It fails closed: no path comes back to lock beside (Go twin:
  # TestSopsConfigDestRefusesALinkThatNamesNoRegularFile).
  _load
  local target
  mkdir -p "${PATH_BASE}/dir"
  for target in gone.yaml dir; do
    rm -f -- "${CFG}"
    ln -s "${target}" "${CFG}"
    run secrets::_config_dest "${CFG}"
    [ "$status" -eq 1 ] || { echo "${target}: status $status: $output" >&2; return 1; }
    [ "$output" = "[error] ${CFG} does not resolve to a regular file: nothing was changed. Run this command again" ] || {
      echo "${target}: $output" >&2; return 1; }
  done
}

@test "remove-key writes the .sops.yaml file that it locked, also when the link names another file after the open of the lock" {
  # The link is resolved once. A link pointed at b.yaml between the open of
  # the lock and the write does not move the write (Go twin:
  # TestKeyCommandsWriteTheFileThatTheyLocked). The stub encrypt reads the
  # rules through the link, so b.yaml holds the rules of the edit already,
  # plus a comment that the edit of a.yaml does not have.
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local dir="${BATS_TEST_TMPDIR}/conf" before_b
  mkdir -p "${dir}"
  mv -- "${CFG}" "${dir}/a.yaml"
  { echo '# b.yaml'; sed "s|,${RCPT_B}||" "${dir}/a.yaml"; } > "${dir}/b.yaml"
  ln -s "${dir}/a.yaml" "${CFG}"
  before_b="$(cat "${dir}/b.yaml")"
  # secrets::_lock runs flock after the open of the lock file.
  flock() { rm -f -- "${CFG}" && ln -s -- "${dir}/b.yaml" "${CFG}" && command flock "$@"; }
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  grep -qF -- "age: '${RCPT_A}'" "${dir}/a.yaml" || { echo "the file beside the lock was not edited" >&2; cat "${dir}/a.yaml" >&2; return 1; }
  [ "$(cat "${dir}/b.yaml")" = "${before_b}" ] || { echo "the write went to the file that the link names now" >&2; return 1; }
  [ "$(readlink "${CFG}")" = "${dir}/b.yaml" ]
  [ ! -e "${dir}/b.yaml.lock" ]
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

@test "a plaintext dated in the future stays the newer file after the re-key, and an edit of it is encrypted" {
  # The real secrets::_encrypt_file runs around a sops stub. The .enc keeps
  # the time of the encrypt, never a time later than now, and one warning
  # line names the plaintext. An edit after the run gets the time of now, and
  # the encrypt sweep writes the .enc again (Go twin:
  # TestRemoveKeyKeepsAFutureDatedPlaintextNewerAndAnEditIsNotLost).
  _load
  _stub_sops
  _cache a.dev app "${RCPT_B}"
  local plain="${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K" line
  line="[warn] ${plain} has a time in the future: its .enc keeps the time of the encrypt, and each encrypt writes it again. Set the time of the plaintext to now (touch it)"
  touch -t 209901010000 "${plain}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(grep -cxF -- "${line}" <<< "${output}")" = 1 ] || { echo "$output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = a.dev/secrets/Secret.app.default.K ] || { cat "${ENCRYPT_LOG}" >&2; return 1; }
  [[ "${plain}" -nt "${plain}.enc" ]] || { echo "the .enc is dated after the plaintext" >&2; return 1; }
  [ "$(stat -c %Y -- "${plain}.enc")" -le "$(date +%s)" ] || { echo "the .enc has a time in the future" >&2; return 1; }

  printf 'w' > "${plain}"
  run secrets::_encrypt_store "${PATH_CLUSTERS}/a.dev/secrets" "" find
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(cat "${ENCRYPT_LOG}")" = $'a.dev/secrets/Secret.app.default.K\na.dev/secrets/Secret.app.default.K' ] || {
    echo "the encrypt skipped the edit:" >&2; cat "${ENCRYPT_LOG}" >&2; return 1; }
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
[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again, with an identity that decrypts every file, resumes it:
[error]   lo secrets remove-key ${RCPT_B} --domain a.dev --all" ]] || { echo "$output" >&2; return 1; }

  # Each file that the re-key re-keyed is left with the .enc the newer file
  # of its pair, and the rerun takes it all the same: the compare reads the
  # values, not the times. Without --all the key is in no rule and on no
  # label of the store: the rerun warns, re-keys the store again, ends, and
  # names the store that still lists it (Go twin:
  # TestRemoveKeyResumesAStoppedRekey).
  [[ "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K.enc" -nt "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.K" ]] || {
    echo "the re-key left the twin the newer file: a later encrypt sweep would encrypt it again" >&2; return 1; }
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
  [[ "$output" == *"[warn] the recipient is your own decrypt identity (${SOPS_AGE_KEY_FILE}): once it is removed, that identity cannot decrypt the re-keyed files, and a resume of a stopped run needs another identity that decrypts every file
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
    [ "$output" = "[error] an argument looks like a private key: it is not printed
[error] pass the path of the key file instead. add-key and remove-key also take an age public key (age1…)" ] || {
      echo "unexpected output (${#output} bytes)" >&2; return 1; }
    [[ "$output" != *"${arg}"* ]]
  done
  [ "$(cat "${CFG}")" = "${before}" ]
}

# The real entrypoint (the pure-bash parser when argsh.so is absent): the
# refusal comes before :usage and :args can print the argument. That
# includes a key in the --domain value, which the parser of lo takes from
# anywhere in the argv, a key in a flag that no parser knows, and a key as
# a command that lo does not know (that parser prints it).
@test "the real entrypoint refuses a private key before :usage or :args can print it" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local fake cmd; fake="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
  for cmd in add-key remove-key; do
    _run_lo secrets "${cmd}" "${PEM_HEAD}"
    [ "$status" -ne 0 ] || { echo "${cmd} <PEM line>: rc 0" >&2; return 1; }
    [[ "$output" == *"looks like a private key: it is not printed"* && "$output" != *OPENSSH* ]] || {
      echo "${cmd} <PEM line>: ${output}" >&2; return 1; }
    # shellcheck disable=SC2086 # the four words of the PEM line
    _run_lo secrets "${cmd}" ${PEM_HEAD}
    [ "$status" -ne 0 ] || { echo "${cmd} <PEM words>: rc 0" >&2; return 1; }
    [[ "$output" == *"looks like a private key: it is not printed"* && "$output" != *OPENSSH* ]] || {
      echo "${cmd} <PEM words>: ${output}" >&2; return 1; }
    _run_lo secrets "${cmd}" "${RCPT_A}" "${fake}"
    [ "$status" -ne 0 ] || { echo "${cmd} <age key> <private key>: rc 0" >&2; return 1; }
    [[ "$output" == *"looks like a private key: it is not printed"* && "$output" != *"${fake}"* ]] || {
      echo "${cmd} <age key> <private key>: ${output}" >&2; return 1; }
  done
  _lo_refused() {
    _run_lo "${@}"
    [ "$status" -ne 0 ] && [[ "$output" == *"looks like a private key: it is not printed"* && "$output" != *"${fake}"* ]] || {
      echo "lo ${*//${fake}/<private key>}: rc ${status}: ${output//${fake}/<private key>}" >&2; return 1; }
  }
  _lo_refused secrets --domain "${fake}" init
  _lo_refused s --domain "${fake}" init
  _lo_refused --domain "${fake}" secrets list
  _lo_refused secrets "--unknown=${fake}" init
  _lo_refused "${fake}"
}

# The real entrypoint, for lo secrets set: a private key as its value is
# refused before any parser, and the refusal names stdin; the PEM line
# split into words too. A certificate passes the guard: :args of set then
# reads it as a flag (its first character is "-"), and that error is not
# the refusal. The run with the certificate has no store to write to.
@test "the real entrypoint refuses a private key as the value of set with the stdin line, and passes a certificate" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local fake cert want
  fake="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
  cert="-----BEGIN CERTIFICATE-----"$'\n'"MIIBfake"$'\n'"-----END CERTIFICATE-----"
  want="[error] an argument looks like a private key: it is not printed
[error] pass the value on stdin, not as an argument: lo secrets set --name <name> <key> < <file>"
  _run_lo secrets set -n app KEY "${fake}" < /dev/null
  [ "$status" -ne 0 ] && [ "$output" = "${want}" ] || {
    echo "set <private key>: rc ${status}: ${output//${fake}/<private key>}" >&2; return 1; }
  # shellcheck disable=SC2086 # the four words of the PEM line
  _run_lo s s -n app KEY ${PEM_HEAD} < /dev/null
  [ "$status" -ne 0 ] && [ "$output" = "${want}" ] || {
    echo "s s <PEM words>: rc ${status}: ${output}" >&2; return 1; }
  _run_lo secrets set -n tls tls.crt "${cert}" < /dev/null
  [[ "$output" != *"looks like a private key"* ]] || { echo "set <certificate>: refused: ${output}" >&2; return 1; }
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

# bats test_tags=real-sops
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

  # The caller decrypts with the key that stays: remove-key compares each
  # twin with its decrypted .enc before the first write.
  DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/keep.age" run _rm "${k}/gone.pub"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  ! _open gone >/dev/null || { echo "the removed key still decrypts the store" >&2; return 1; }
  [ "$(_open keep)" = "v" ] || { echo "the remaining key cannot decrypt the store" >&2; return 1; }
  ! grep -qF "${gone}" "${store}/Secret.app.default.K.enc"
}

# bats test_tags=real-sops
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

  # The caller decrypts with the key that stays: remove-key compares each
  # twin with its decrypted .enc before the first write.
  DOMAIN=a.dev SOPS_AGE_KEY_FILE="${k}/keep.age" run _rm "${k}/gone.pub"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  ! cmp -s "${store}/Secret.other.default.K.enc" "${BATS_TEST_TMPDIR}/other.saved" || {
    echo "the .enc whose labels do not list the key was not rewritten" >&2; return 1; }
  [ "$(_open keep other)" = "w" ] || { echo "the rewritten .enc does not decrypt with the key that stays" >&2; return 1; }
  ! _open gone app >/dev/null || { echo "the removed key still decrypts app" >&2; return 1; }
  [ "$(_open keep app)" = "v" ] || { echo "the key that stays cannot decrypt app" >&2; return 1; }
}

# The own-identity check reads the .pub of LOK8S_SSH_KEY. A .pub that holds a
# private key is no match, and ssh-to-age does not read it. The control: a
# .pub with a public key is read, and its recipient matches. ssh-to-age is a
# function that logs a call (Go twin: TestDeriveRefusesAPrivateKeyFile; the
# derivation refuses the file for every caller).
@test "remove-key: the own-identity check does not read a .pub that holds a private key" {
  _load
  ssh-to-age() { echo called >> "${BATS_TEST_TMPDIR}/ssh-to-age.called"; echo "${RCPT_A}"; }
  mkdir -p "${HOME}/.ssh"
  printf '%s\nCANARY_SECRET_MATERIAL_DO_NOT_LEAK\n' "${PEM_HEAD}" > "${LOK8S_SSH_KEY}.pub"
  local own=""
  secrets::_own_identity "${RCPT_A}"
  [ -z "${own}" ] || { echo "a private key matched: ${own}" >&2; return 1; }
  [ ! -e "${BATS_TEST_TMPDIR}/ssh-to-age.called" ] || { echo "ssh-to-age read the file" >&2; return 1; }

  printf 'ssh-ed25519 AAAA test\n' > "${LOK8S_SSH_KEY}.pub"
  secrets::_own_identity "${RCPT_A}"
  [ "${own}" = "${LOK8S_SSH_KEY}.pub" ] || { echo "the control did not match: '${own}'" >&2; return 1; }
  [ -e "${BATS_TEST_TMPDIR}/ssh-to-age.called" ] || { echo "the control did not call ssh-to-age" >&2; return 1; }
}

# When LOK8S_SSH_KEY ends in .pub, the own-identity check reads that file,
# not <key>.pub.pub (Go twin: TestOwnIdentityReadsAPubSuffixedKey).
@test "remove-key: the own-identity check reads LOK8S_SSH_KEY itself when it ends in .pub" {
  _load
  ssh-to-age() { cat > "${BATS_TEST_TMPDIR}/ssh-to-age.in"; echo "${RCPT_A}"; }
  local pub="${BATS_TEST_TMPDIR}/keys/work.pub"
  mkdir -p "${pub%/*}"
  printf 'ssh-ed25519 AAAA work\n' > "${pub}"
  export LOK8S_SSH_KEY="${pub}"
  local own=""
  secrets::_own_identity "${RCPT_A}"
  [ "${own}" = "${pub}" ] || { echo "own identity '${own}', want ${pub}" >&2; return 1; }
  cmp -s "${pub}" "${BATS_TEST_TMPDIR}/ssh-to-age.in" || { echo "ssh-to-age did not read ${pub}" >&2; return 1; }
}

# remove-key reads its key file through the same read as add-key: a file
# over 64 KiB, with a NUL byte, or one that cannot be read, is refused with
# the path and the cause, and .sops.yaml stays as it was (Go twin:
# TestKeyFileReadIsCappedAndFailsClosed).
@test "remove-key: a key file over 64 KiB, with a NUL byte, or one that cannot be read, is refused" {
  _load
  ssh-to-age() { echo called >> "${BATS_TEST_TMPDIR}/ssh-to-age.called"; cat > /dev/null; echo "${RCPT_A}"; }
  local canary="CANARY_SECRET_MATERIAL_DO_NOT_LEAK" path before
  local big="${BATS_TEST_TMPDIR}/big.pub" nul="${BATS_TEST_TMPDIR}/nul.pub" closed="${BATS_TEST_TMPDIR}/closed.pub"
  { printf 'ssh-ed25519 AAAA %s' "${canary}"; head -c 65536 /dev/zero | tr '\0' x; } > "${big}"
  printf 'ssh-ed25519 AAAA\0%s\n' "${canary}" > "${nul}"
  local -A cases=(
    ["${big}"]="[error] ${big} is bigger than 64 KiB, which no public key file is: nothing from it is printed"
    ["${nul}"]="[error] ${nul} holds a NUL byte, which no public key file holds: nothing from it is printed"
  )
  if (( EUID != 0 )); then
    printf 'ssh-ed25519 AAAA %s\n' "${canary}" > "${closed}"
    chmod 000 "${closed}"
    cases["${closed}"]="[error] cannot read the key file ${closed}: nothing from it is printed"
  fi
  before="$(cat "${CFG}")"
  for path in "${!cases[@]}"; do
    DOMAIN=a.dev run _rm "${path}"
    [ "$status" -eq 1 ] && [[ "$output" == "${cases[${path}]}"* ]] && [[ "$output" != *"${canary}"* ]] || {
      echo "${path##*/}: status $status: $output" >&2; return 1; }
  done
  [ ! -e "${BATS_TEST_TMPDIR}/ssh-to-age.called" ] || { echo "ssh-to-age ran" >&2; return 1; }
  [ "$(cat "${CFG}")" = "${before}" ]
}

# The own-identity check reads the .pub through the same read: a .pub with
# the key line and more than 64 KiB, or a NUL byte after the key line, is no
# match, and ssh-to-age does not run (Go twin:
# TestOwnIdentityReadIsCappedAndFailsClosed).
@test "remove-key: the own-identity check reads at most 64 KiB, and no NUL byte" {
  _load
  ssh-to-age() { echo called >> "${BATS_TEST_TMPDIR}/ssh-to-age.called"; cat > /dev/null; echo "${RCPT_A}"; }
  local pub="${BATS_TEST_TMPDIR}/keys/work.pub" form own
  mkdir -p "${pub%/*}"
  export LOK8S_SSH_KEY="${pub}"
  printf 'ssh-ed25519 AAAA work\n' > "${pub}"
  own=""
  secrets::_own_identity "${RCPT_A}"
  [ "${own}" = "${pub}" ] || { echo "the plain .pub: own identity '${own}'" >&2; return 1; }
  for form in big nul; do
    case "${form}" in
      big) { printf 'ssh-ed25519 AAAA work\n#'; head -c 65536 /dev/zero | tr '\0' x; } > "${pub}" ;;
      nul) printf 'ssh-ed25519 AAAA work\n\0\n' > "${pub}" ;;
    esac
    rm -f -- "${BATS_TEST_TMPDIR}/ssh-to-age.called"
    own=""
    secrets::_own_identity "${RCPT_A}"
    [ -z "${own}" ] && [ ! -e "${BATS_TEST_TMPDIR}/ssh-to-age.called" ] || {
      echo "${form}: own identity '${own}', or ssh-to-age ran" >&2; return 1; }
  done
}

# The own-identity check gives ssh-to-age the text that it checked, on
# stdin: head (the read) is a function here that removes the .pub after it
# read it, so a second open of the file fails.
@test "remove-key: the own-identity check gives ssh-to-age the checked text, not a second read" {
  _load
  ssh-to-age() { cat > "${BATS_TEST_TMPDIR}/ssh-to-age.in"; echo "${RCPT_A}"; }
  local pub="${BATS_TEST_TMPDIR}/keys/once.pub"
  mkdir -p "${pub%/*}"
  printf 'ssh-ed25519 AAAA once\n' > "${pub}"
  cp -- "${pub}" "${BATS_TEST_TMPDIR}/expect"
  export LOK8S_SSH_KEY="${pub}"
  head() { local rc=0; command head "${@}" || rc=$?; rm -f -- "${LOK8S_SSH_KEY}"; return "${rc}"; }
  local own=""
  secrets::_own_identity "${RCPT_A}"
  unset -f head
  [ "${own}" = "${pub}" ] || { echo "own identity '${own}'" >&2; return 1; }
  [ ! -e "${pub}" ] || { echo "the read did not go through head" >&2; return 1; }
  cmp -s "${BATS_TEST_TMPDIR}/expect" "${BATS_TEST_TMPDIR}/ssh-to-age.in" || {
    echo "ssh-to-age read other bytes than the file held" >&2; return 1; }
}

# The warning about the own identity names the file through
# secrets::_shown_path: a keys file whose path has the shape of a private
# key prints as a placeholder (Go twin: the remove-key case of
# TestKeyPathMessagesHideAPrivateKeyShape).
@test "remove-key: the own-identity warning does not print a key file path with the shape of a private key" {
  _load
  _stub_encrypt
  _cache a.dev app "${RCPT_B}"
  local marker="AGE-SECRET-""KEY-1SHAPEDPATH"
  mkdir -p "${BATS_TEST_TMPDIR}/${marker}"
  export SOPS_AGE_KEY_FILE="${BATS_TEST_TMPDIR}/${marker}/keys.txt"
  printf '# created: test\n# public key: %s\n' "${RCPT_B}" > "${SOPS_AGE_KEY_FILE}"
  DOMAIN=a.dev run _rm "${RCPT_B}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == *"[warn] the recipient is your own decrypt identity ((not printed: it looks like a private key)): once it is removed"* ]] || {
    echo "$output" >&2; return 1; }
  [[ "$output" != *"${marker}"* ]] || { echo "the path reached the output" >&2; return 1; }
}

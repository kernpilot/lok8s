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
# BATS_TEST_TMPDIR, and the encrypt step is stubbed (the whole sweep, or sops
# alone: _load_sweep), so no sops key is needed and no real cache file is
# reachable. HOME, XDG_CONFIG_HOME (sops reads <config
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
  secrets::_encrypt_store() { echo ran >> "${ENCRYPT_RAN}"; return 0; }
  command -v sops >/dev/null 2>&1 || sops() { return 0; }
}

# _load_sweep — the library with the real encrypt sweep (secrets::_encrypt_store
# and secrets::_encrypt_file). Only sops is a stub: a function, so no sops on
# PATH runs. It writes "stub-enc <bytes of the plaintext>" to the --output path
# and logs each plaintext that it encrypts to SOPS_LOG.
_load_sweep() {
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh" 2>/dev/null || true
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  SOPS_LOG="${BATS_TEST_TMPDIR}/sops.log"; : > "${SOPS_LOG}"
  sops() {
    local -a a=("${@}")
    local i out=""
    for (( i = 0; i + 1 < ${#a[@]}; i++ )); do
      [[ "${a[i]}" != --output ]] || out="${a[i + 1]}"
    done
    [[ -n "${out}" ]] || return 1
    printf '%s\n' "${a[-1]}" >> "${SOPS_LOG}"
    { printf 'stub-enc '; cat -- "${a[-1]}"; } > "${out}"
  }
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
  # A rerun reads the sops metadata with jq.
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
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

@test "--all with every domain store a path it cannot read says that no store was found" {
  # add-key does not read those paths; remove-key names them (Go twin:
  # TestAddKeyAllWithEveryStoreUnreadableSaysNoStore).
  [ "$(id -u)" -ne 0 ] || skip "root reads every file"
  _load
  chmod 000 "${PATH_CLUSTERS}/a.dev" "${PATH_CLUSTERS}/b.dev"
  DOMAIN=a.dev run _add "${KEY_B}" 1
  chmod 755 "${PATH_CLUSTERS}/a.dev" "${PATH_CLUSTERS}/b.dev"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "added recipient to .sops.yaml (1 creation rule(s))
  ${KEY_B}
[error] no secret store found to re-key" ] || { echo "$output" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

@test "without flock, add-key refuses before .sops.yaml is touched and makes no lock file" {
  # A run without the lock can undo another run's change. The PATH has the
  # tools of the run and no flock: a link to each one found on PATH (mv too:
  # the check for a mv that takes -T comes before the lock; ls too: the check
  # of the .gitignore before the lock looks up a path that is absent with it).
  _load
  local bin="${BATS_TEST_TMPDIR}/no-flock" tool path
  mkdir -p "${bin}"
  for tool in sops grep sed cat mv ls; do
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

# _state <path>... — one line per entry under the paths (its path, type, link
# target and mtime), then the checksum of each regular file: any write shows.
_state() {
  find "${@}" -printf '%p %y %l %T@\n' | LC_ALL=C sort
  find "${@}" -type f -print0 | LC_ALL=C sort -z | xargs -0 -r cksum
}

@test "a symlinked .enc in the store is refused before any write" {
  # The encrypt does not write through a link (secrets::_encrypt_file refuses
  # one). Found after the .sops.yaml write, the link would stop the run half
  # way, and a rerun would find the recipient present and re-key nothing (Go
  # twin: TestKeyCommandsRefuseASymlinkedEncBeforeAnyWrite).
  _load
  local store="${PATH_CLUSTERS}/a.dev/secrets" away="${BATS_TEST_TMPDIR}/elsewhere" before
  mkdir -p "${away}"
  printf 'v\n' > "${store}/Secret.app.default.KEY"
  printf 'enc %s\n' "${KEY_A}" > "${away}/Secret.app.default.KEY.enc"
  ln -s "${away}/Secret.app.default.KEY.enc" "${store}/Secret.app.default.KEY.enc"
  printf 'w\n' > "${store}/Secret.db.default.KEY"
  printf 'enc %s\n' "${KEY_A}" > "${store}/Secret.db.default.KEY.enc"
  touch -t 200001010000 "${away}/Secret.app.default.KEY.enc" "${store}/Secret.db.default.KEY.enc"
  touch -t 200001010001 "${store}/Secret.app.default.KEY" "${store}/Secret.db.default.KEY"
  before="$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}" "${away}")"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${store}/Secret.app.default.KEY.enc is a symlink: the re-key does not write an .enc through a link
[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}" "${away}")" = "${before}" ] || {
    echo "a file changed:" >&2
    diff <(echo "${before}") <(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}" "${away}") >&2
    return 1
  }
  [ ! -s "${ENCRYPT_RAN}" ]
  # The control: the file in the place of the link. The run adds the
  # recipient and re-keys the store.
  rm -- "${store}/Secret.app.default.KEY.enc"
  mv -- "${away}/Secret.app.default.KEY.enc" "${store}/"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ -s "${ENCRYPT_RAN}" ]
}

# _refused_later <kind> — a store with the pairs app and db. The .enc of db,
# the later pair (or, for kind sha, the .enc of the .sha file of db), is a
# directory (dir, sha), a FIFO (fifo), a unix socket (socket) or a symlink
# (link); for kind mixed the .enc of db is a directory and the .enc of app
# is a symlink. add-key must refuse it before any write, and its last line
# names what it refused. The project (.sops.yaml, the lock file, any temp
# file), the store and the link target stay as they were, and sops does not
# run. The control: with the entry removed, the run adds the recipient, and
# the real sweep (_load_sweep: sops alone is a stub) writes the .enc of each
# plaintext of the store, the .sha.enc too. (Go twins:
# TestAddKeyRefusesEveryEncThatTheEncryptRefusesBeforeAnyWrite and
# TestAddKeyRefusesALinkAndADirectoryInOneStore.)
_refused_later() {
  local kind="${1}" store="${PATH_CLUSTERS}/a.dev/secrets" away="${BATS_TEST_TMPDIR}/elsewhere" enc line tail before plain want=""
  mkdir -p "${away}"
  printf 'v\n' > "${store}/Secret.app.default.KEY"
  printf 'enc %s\n' "${KEY_A}" > "${store}/Secret.app.default.KEY.enc"
  printf 'w\n' > "${store}/Secret.db.default.KEY"
  enc="${store}/Secret.db.default.KEY.enc"
  if [[ "${kind}" == sha ]]; then
    printf 'enc %s\n' "${KEY_A}" > "${enc}"
    printf 's\n' > "${store}/Secret.db.default.KEY.sha"
    enc="${store}/Secret.db.default.KEY.sha.enc"
  fi
  line="${enc} is not a regular file (a directory, a FIFO, a socket or a device): the re-key does not write an .enc there"
  tail="remove or fix each entry named above, then run this command again; nothing was written"
  case "${kind}" in
    dir|sha) mkdir -- "${enc}" ;;
    fifo) mkfifo -- "${enc}" ;;
    socket)
      # A socket path is limited to about 108 bytes and the test dir is
      # longer: bind the short name from inside the store.
      ( cd -- "${store}" && python3 -I -B -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "${enc##*/}" ) 2>/dev/null \
        || skip "cannot bind a unix socket in the test dir"
      [[ -S "${enc}" ]] || skip "the bind made no socket"
      ;;
    link)
      printf 'enc %s\n' "${KEY_A}" > "${away}/Secret.db.default.KEY.enc"
      ln -s "${away}/Secret.db.default.KEY.enc" "${enc}"
      line="${enc} is a symlink: the re-key does not write an .enc through a link"
      tail="replace each link with the file it names, or remove it from the store, then run this command again; nothing was written"
      ;;
    mixed)
      mkdir -- "${enc}"
      mv -- "${store}/Secret.app.default.KEY.enc" "${away}/"
      ln -s "${away}/Secret.app.default.KEY.enc" "${store}/Secret.app.default.KEY.enc"
      line="${store}/Secret.app.default.KEY.enc is a symlink: the re-key does not write an .enc through a link
[error] ${line}"
      tail="replace each link with the file it names, and remove or fix each other entry, then run this command again; nothing was written"
      ;;
  esac
  find "${store}" "${away}" -type f -name '*.enc' -exec touch -t 200001010000 {} +
  find "${store}" -type f ! -name '*.enc' -exec touch -t 200001010001 {} +
  # The lock file is there before the run (the lock writes it), so that the
  # listing of the project shows any other new entry.
  : > "${PATH_BASE}/.sops.yaml.lock"
  before="$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}" "${away}"; ls -A "${PATH_BASE}")"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${line}
[error]   ${tail}" ] || { echo "$output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}" "${away}"; ls -A "${PATH_BASE}")" = "${before}" ] || {
    echo "a file changed:" >&2
    diff <(echo "${before}") <(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}" "${away}"; ls -A "${PATH_BASE}") >&2
    return 1
  }
  [ ! -s "${SOPS_LOG}" ] || { echo "sops ran on:" >&2; cat "${SOPS_LOG}" >&2; return 1; }
  rm -r -- "${enc}"
  if [[ "${kind}" == mixed ]]; then
    rm -- "${store}/Secret.app.default.KEY.enc"
    mv -- "${away}/Secret.app.default.KEY.enc" "${store}/"
  fi
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  # Each plaintext of the store, in glob order, and its new .enc: a regular
  # file with the stub's bytes. No temp file is left.
  for plain in "${store}"/Secret.*; do
    [[ "${plain}" != *.enc ]] || continue
    want+="${want:+$'\n'}${plain}"
    [[ -f "${plain}.enc" && ! -L "${plain}.enc" ]] || { echo "${plain}.enc is not a regular file" >&2; return 1; }
    [ "$(cat "${plain}.enc")" = "stub-enc $(cat "${plain}")" ] || { echo "${plain}.enc was not written" >&2; return 1; }
  done
  [ "$(cat "${SOPS_LOG}")" = "${want}" ] || { echo "sops ran on:" >&2; cat "${SOPS_LOG}" >&2; return 1; }
  [ -z "$(find "${store}" -name '.*.tmp-*')" ] || { echo "a temp file was left" >&2; return 1; }
  [[ "${kind}" != sha ]] || grep -qxF "${store}/Secret.db.default.KEY.sha" "${SOPS_LOG}" || { echo "the .sha file was not encrypted" >&2; return 1; }
}

# A device node needs root to make, so no test makes one: secrets::_enc_path_ok
# refuses it as it refuses a FIFO.
@test "the .enc of a later pair that is a directory is refused before any write" {
  _load_sweep
  _refused_later dir
}

@test "the .enc of a later pair that is a FIFO is refused before any write" {
  command -v mkfifo >/dev/null 2>&1 || skip "mkfifo not installed"
  _load_sweep
  _refused_later fifo
}

@test "the .enc of a later pair that is a symlink is refused before any write" {
  _load_sweep
  _refused_later link
}

@test "the .enc of a .sha file that is a directory is refused before any write" {
  _load_sweep
  _refused_later sha
}

@test "the .enc of a later pair that is a unix socket is refused before any write" {
  command -v python3 >/dev/null 2>&1 || skip "python3 not installed: no tool to bind a unix socket"
  _load_sweep
  _refused_later socket
}

@test "a symlinked .enc and a directory .enc in one store: the last line names both" {
  _load_sweep
  _refused_later mixed
}

# _long_name_refused — the body of the long-name tests, after the library is
# loaded. The lstat of the .enc fails with ENAMETOOLONG, and [[ -e ]] reads
# that as a path that does not exist. The check must not: the encrypt would
# stop after the .sops.yaml write, and a rerun would find the recipient
# present (Go twin: TestAddKeyRefusesAnEncPathThatItCannotLookUpBeforeAnyWrite).
# It lists the project with command ls, so an ls function of the test does
# not run here.
_long_name_refused() {
  local store="${PATH_CLUSTERS}/a.dev/secrets" max name before
  max="$(getconf NAME_MAX "${store}" 2>/dev/null)" || max=255
  [[ "${max}" =~ ^[0-9]+$ ]] || skip "the test dir has no NAME_MAX"
  name="Secret.app.default."
  while (( ${#name} < max - 2 )); do name+=K; done
  { printf 'v\n' > "${store}/${name}"; } 2>/dev/null || skip "the filesystem refuses a name of ${#name} bytes"
  ! ( : > "${store}/${name}.enc" ) 2>/dev/null || skip "the filesystem takes a name of $(( ${#name} + 4 )) bytes"
  printf 'w\n' > "${store}/Secret.db.default.KEY"
  printf 'enc %s\n' "${KEY_A}" > "${store}/Secret.db.default.KEY.enc"
  touch -t 200001010000 "${store}/Secret.db.default.KEY.enc"
  touch -t 200001010001 "${store}/${name}" "${store}/Secret.db.default.KEY"
  # The lock file is there before the run (the lock writes it), so that the
  # listing of the project shows any other new entry.
  : > "${PATH_BASE}/.sops.yaml.lock"
  before="$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}"; command ls -A "${PATH_BASE}")"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${store}/${name}.enc cannot be looked up (an error other than \"no such file or directory\", for example a name that is too long): the re-key does not write an .enc there
[error]   remove or fix each entry named above, then run this command again; nothing was written" ] || {
    echo "$output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}"; command ls -A "${PATH_BASE}")" = "${before}" ] || {
    echo "a file changed:" >&2
    diff <(echo "${before}") <(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}"; command ls -A "${PATH_BASE}") >&2
    return 1
  }
  [ ! -s "${ENCRYPT_RAN}" ]
  # The control: without the long name, the run adds the recipient.
  rm -- "${store}/${name}"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ -s "${ENCRYPT_RAN}" ]
}

# _ls_then_missing <arg>... — runs ls, then lists a path that does not
# exist: the last line of its stderr is always "No such file or directory",
# whatever the first listing found.
_ls_then_missing() {
  command ls "${@}"
  command ls -ld -- "${BATS_TEST_TMPDIR}/no-such-entry"
}

@test "a plaintext whose .enc name is too long is refused before any write" {
  _load
  _long_name_refused
}

@test "the long name is refused when a shell function named ls adds a line of its own" {
  # secrets::_absent runs the ls command (command ls), not the function: the
  # message of the function ends in "No such file or directory".
  _load
  ls() { _ls_then_missing "${@}"; }
  _long_name_refused
}

@test "the long name is refused when an ls alias is set before the library is read" {
  # Bash expands an alias when it reads the library, also after LC_ALL=C. It
  # does not expand the word after command.
  shopt -s expand_aliases
  alias ls=_ls_then_missing
  _load
  _long_name_refused
}

@test "the encrypt reads the list that add-key checked, not a new listing of the store" {
  # No .gitignore, a dangling plaintext link to .gitignore and a dangling .enc
  # link. The check leaves the plaintext out: the link names nothing yet. The
  # encrypt writes the .gitignore, and the link then names a regular file. A
  # new listing would take it and stop at its .enc, after the .sops.yaml
  # write. The checked list leaves both links as they were (Go twin:
  # TestAddKeyEncryptsTheListThatItChecked).
  _load_sweep
  local store="${PATH_CLUSTERS}/a.dev/secrets"
  [ ! -e "${store}/.gitignore" ]
  ln -s .gitignore "${store}/Secret.app.default.KEY"
  ln -s missing "${store}/Secret.app.default.KEY.enc"
  printf 'w\n' > "${store}/Secret.db.default.KEY"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ "$(readlink "${store}/Secret.app.default.KEY")" = .gitignore ]
  [ "$(readlink "${store}/Secret.app.default.KEY.enc")" = missing ]
  [ -f "${store}/.gitignore" ] || { echo "the encrypt wrote no .gitignore" >&2; return 1; }
  [ "$(cat "${SOPS_LOG}")" = "${store}/Secret.db.default.KEY" ] || { echo "sops ran on:" >&2; cat "${SOPS_LOG}" >&2; return 1; }
  [ "$(cat "${store}/Secret.db.default.KEY.enc")" = "stub-enc w" ]
}

@test "a .gitignore of the store that is a symlink is refused before any write, also for a recipient that is already there" {
  # The check comes before the lock file, the .sops.yaml write and the
  # touch of the plaintexts (Go twin: TestAddKeyRefusesABadGitignoreBeforeAnyWrite).
  _load
  local store="${PATH_CLUSTERS}/a.dev/secrets" outside="${BATS_TEST_TMPDIR}/elsewhere/gitignore" key before
  mkdir -p "${outside%/*}"
  printf '# not the rules of lok8s\n' > "${outside}"
  ln -s "${outside}" "${store}/.gitignore"
  printf 'v' > "${store}/Secret.app.default.K"
  touch -t 200001010000 "${store}/Secret.app.default.K"
  for key in "${KEY_B}" "${KEY_A}"; do
    before="$(_state "${PATH_BASE}" "${store}" "${outside%/*}")"
    DOMAIN=a.dev run _add "${key}"
    [ "$status" -eq 1 ] || { echo "${key}: status $status: $output" >&2; return 1; }
    [ "$output" = "[error] ${store}/.gitignore is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was written — remove it, then run this command again" ] || {
      echo "$output" >&2; return 1; }
    [ "$(_state "${PATH_BASE}" "${store}" "${outside%/*}")" = "${before}" ] || { echo "${key}: a write" >&2; return 1; }
    [ ! -e "${PATH_BASE}/.sops.yaml.lock" ] || { echo "${key}: a lock file" >&2; return 1; }
    [ ! -s "${ENCRYPT_RAN}" ]
  done
}

@test "add-key encrypts every file of the list that it checked, also one whose .enc is dated in the future" {
  # The run sets the plaintexts to now, so an .enc dated in the future stays
  # the newer file, and a sweep that skips it would leave it without the new
  # recipient (Go twin: TestAddKeyRekeysAnEncDatedInTheFuture).
  _load_sweep
  local store="${PATH_CLUSTERS}/a.dev/secrets"
  printf 'v' > "${store}/Secret.app.default.K"
  printf 'old' > "${store}/Secret.app.default.K.enc"
  touch -t 209901010000 "${store}/Secret.app.default.K.enc"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$(cat "${SOPS_LOG}")" = "${store}/Secret.app.default.K" ] || { echo "sops ran on:" >&2; cat "${SOPS_LOG}" >&2; return 1; }
  [ "$(cat "${store}/Secret.app.default.K.enc")" = "stub-enc v" ]
  [ "$(stat -c %Y -- "${store}/Secret.app.default.K.enc")" -le "$(date +%s)" ] || { echo "the .enc has a time in the future" >&2; return 1; }
}

@test "a domain whose store does not resolve is refused before any write, with --all and without" {
  # secrets::path says why, and the last line says that .sops.yaml did not
  # change and what to do. Before, the run added the recipient, then
  # stopped, and a rerun found the recipient present and re-keyed nothing
  # (Go twin: TestAddKeyRefusesAStoreThatDoesNotResolveBeforeAnyWrite).
  _load
  source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"
  local store="${PATH_CLUSTERS}/c.dev/secrets" before all
  mkdir -p "${PATH_CLUSTERS}/c.dev"
  printf 'v\n' > "${PATH_CLUSTERS}/a.dev/secrets/Secret.app.default.KEY"
  : > "${PATH_BASE}/.sops.yaml.lock"
  before="$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}"; command ls -A "${PATH_BASE}")"
  for all in 0 1; do
    DOMAIN=c.dev run _add "${KEY_B}" "${all}"
    [ "$status" -eq 1 ] || { echo "all=${all}: status $status: $output" >&2; return 1; }
    [ "$output" = "[error] domain c.dev has no secrets store: ${store} does not exist, and this project keeps a store per domain (${PATH_CLUSTERS}/a.dev/secrets). lo does not fall back to ${PATH_SECRETS:-${PATH_BASE}/.secrets}.
[error]   Create the empty directory ${store} to generate new values for c.dev.
[error]   add-key did not change .sops.yaml: create that directory, or select a domain that has a store, then run this command again" ] || {
      echo "all=${all}: $output" >&2; return 1; }
    [ "$(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}"; command ls -A "${PATH_BASE}")" = "${before}" ] || {
      echo "all=${all}: a file changed:" >&2
      diff <(echo "${before}") <(_state "${PATH_BASE}/.sops.yaml" "${PATH_CLUSTERS}"; command ls -A "${PATH_BASE}") >&2
      return 1
    }
    [ ! -s "${ENCRYPT_RAN}" ]
  done
  # The control: with the store made, the run adds the recipient.
  mkdir "${store}"
  DOMAIN=c.dev run _add "${KEY_B}" 1
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ -s "${ENCRYPT_RAN}" ]
}

@test "a .sops.yaml that is a symlink stays a link: its target gets the recipient and keeps its mode" {
  # sed -i would put a regular file in the place of the link and leave the
  # target as it was (Go twin: TestKeyCommandsEditTheTargetOfASymlinkedSopsConfig).
  _load
  local conf="${BATS_TEST_TMPDIR}/conf/sops.yaml" want="${BATS_TEST_TMPDIR}/want.yaml"
  mkdir -p "${conf%/*}"
  mv -- "${PATH_BASE}/.sops.yaml" "${conf}"
  chmod 640 "${conf}"
  ln -s "${conf}" "${PATH_BASE}/.sops.yaml"
  printf "creation_rules:\n  - path_regex: 'Secret\\\\..*'\n    age: '%s,%s'\n" "${KEY_A}" "${KEY_B}" > "${want}"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ -L "${PATH_BASE}/.sops.yaml" ] || { echo "the link was replaced by a file" >&2; return 1; }
  [ "$(readlink "${PATH_BASE}/.sops.yaml")" = "${conf}" ]
  cmp "${conf}" "${want}" || { cat "${conf}" >&2; return 1; }
  [ "$(stat -c %a "${conf}")" = 640 ] || { echo "mode $(stat -c %a "${conf}")" >&2; return 1; }
  # No temp file is left, and the lock is beside the target, not the link.
  [ "$(ls -A "${conf%/*}")" = $'sops.yaml\nsops.yaml.lock' ] || { echo "the dir of the target: $(ls -A "${conf%/*}")" >&2; return 1; }
  [ ! -e "${PATH_BASE}/.sops.yaml.lock" ] || { echo "a lock beside the link" >&2; return 1; }
}

@test "a lock in a directory that it cannot write is refused, also for a recipient that is already there, and the line names the directory" {
  # With a .sops.yaml link the lock goes beside the file that the link
  # names (Go twin: TestKeyCommandsNameTheDirectoryOfALockThatCannotBeOpened).
  [ "$(id -u)" -ne 0 ] || skip "root writes every directory"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  local dir="${BATS_TEST_TMPDIR}/conf" before
  mkdir -p "${dir}"
  mv -- "${PATH_BASE}/.sops.yaml" "${dir}/sops.yaml"
  ln -s "${dir}/sops.yaml" "${PATH_BASE}/.sops.yaml"
  dir="$(readlink -f -- "${dir}")"
  before="$(cat "${dir}/sops.yaml")"
  chmod 555 "${dir}"
  DOMAIN=a.dev run _add "${KEY_A}"
  chmod 755 "${dir}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] cannot open the lock file ${dir}/sops.yaml.lock — the lock goes beside the file that .sops.yaml names, so lo needs write access to ${dir}; nothing was changed" ] || {
    echo "$output" >&2; return 1; }
  [ "$(cat "${dir}/sops.yaml")" = "${before}" ]
  [ "$(ls -A "${dir}")" = sops.yaml ] || { echo "the dir of the target: $(ls -A "${dir}")" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

@test "add-key writes the .sops.yaml file that it locked, also when the link names another file after the open of the lock" {
  # The link is resolved once. A link pointed at b.yaml between the open of
  # the lock and the write does not move the write (Go twin:
  # TestKeyCommandsWriteTheFileThatTheyLocked).
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  local dir="${BATS_TEST_TMPDIR}/conf" before_b
  mkdir -p "${dir}"
  mv -- "${PATH_BASE}/.sops.yaml" "${dir}/a.yaml"
  { echo '# b.yaml'; cat -- "${dir}/a.yaml"; } > "${dir}/b.yaml"
  ln -s "${dir}/a.yaml" "${PATH_BASE}/.sops.yaml"
  before_b="$(cat "${dir}/b.yaml")"
  # secrets::_lock runs flock after the open of the lock file.
  flock() { rm -f -- "${PATH_BASE}/.sops.yaml" && ln -s -- "${dir}/b.yaml" "${PATH_BASE}/.sops.yaml" && command flock "$@"; }
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${dir}/a.yaml" || { echo "the file beside the lock was not edited" >&2; cat "${dir}/a.yaml" >&2; return 1; }
  [ "$(cat "${dir}/b.yaml")" = "${before_b}" ] || { echo "the write went to the file that the link names now" >&2; return 1; }
  [ "$(readlink "${PATH_BASE}/.sops.yaml")" = "${dir}/b.yaml" ]
  [ ! -e "${dir}/b.yaml.lock" ]
  [ -s "${ENCRYPT_RAN}" ]
}

@test "add-key writes the .gitignore of the store before .sops.yaml: a .gitignore that it cannot write stops the run before any change" {
  # The encrypt writes the .gitignore too, but after the write of .sops.yaml:
  # a .gitignore that it cannot write (the store is read-only) stopped the
  # run there, with the recipient in .sops.yaml and no .enc re-keyed. The
  # run after the fix adds the recipient and re-keys (Go twin:
  # TestAddKeyWritesTheGitignoreBeforeTheConfig).
  [ "$(id -u)" -ne 0 ] || skip "root writes every directory"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  local store="${PATH_CLUSTERS}/a.dev/secrets" before tail
  printf 'v' > "${store}/Secret.app.default.K"
  printf 'old' > "${store}/Secret.app.default.K.enc"
  printf '# old\n' > "${store}/.gitignore"
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  before="$(_state "${store}")"
  chmod 555 "${store}"
  DOMAIN=a.dev run _add "${KEY_B}"
  chmod 755 "${store}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  tail="[error] cannot write ${store}/.gitignore: it was not changed
[error]   add-key did not change .sops.yaml or an .enc file: fix the cause above, then run this command again"
  [[ "${output}" == *$'\n'"${tail}" && "${output}" != *"added recipient"* ]] || { echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ "$(_state "${store}")" = "${before}" ] || { echo "a store file changed" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]

  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "the run after the fix: status $status: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ "$(cat "${SOPS_LOG}")" = "${store}/Secret.app.default.K" ] || { echo "sops ran on:" >&2; cat "${SOPS_LOG}" >&2; return 1; }
  grep -qxF '!Secret.*.enc' "${store}/.gitignore"
}

# _envelope <recipient>... — the JSON envelope of an .enc (the binary store of
# sops) with a full entry for each recipient, as sops writes it: an enc that
# is an armored age block, a lastmodified and a mac. secrets::_enc_lists
# reads it, with added too.
_envelope() {
  local r sep=""
  printf '{"data":"ENC[x]","sops":{"age":['
  for r in "${@}"; do
    printf '%s{"recipient":"%s","enc":"-----BEGIN AGE ENCRYPTED FILE-----\\nYWdl\\n-----END AGE ENCRYPTED FILE-----\\n"}' "${sep}" "${r}"
    sep=,
  done
  printf '],"lastmodified":"2026-01-01T00:00:00Z","mac":"ENC[x]"}}\n'
}

# _sops_envelope — after _load_sweep: a sops function that writes, to the
# --output path, the envelope of each recipient of the age: line of
# .sops.yaml, and logs each plaintext to SOPS_LOG.
_sops_envelope() {
  sops() {
    local -a a=("${@}") rcpts=()
    local i out="" list
    for (( i = 0; i + 1 < ${#a[@]}; i++ )); do
      [[ "${a[i]}" != --output ]] || out="${a[i + 1]}"
    done
    [[ -n "${out}" ]] || return 1
    list="$(sed -nE "s/^[[:space:]]*age:[[:space:]]*'([^']*)'.*/\1/p" "${PATH_BASE}/.sops.yaml")"
    IFS=, read -r -a rcpts <<< "${list}"
    printf '%s\n' "${a[-1]}" >> "${SOPS_LOG}"
    _envelope "${rcpts[@]}" > "${out}"
  }
}

@test "a rerun finishes the re-key of a run that stopped after its write of .sops.yaml, and the run after that changes nothing" {
  # The first run stops at the orphan check (an .enc with no plaintext twin),
  # after .sops.yaml got the recipient. The rerun that its message names
  # (--skip-orphans) finds the recipient present and the .enc files of the
  # sweep without it: it re-keys them and does not write .sops.yaml again:
  # the file keeps its inode and its mtime, not only its bytes (Go twin:
  # TestAddKeyRerunFinishesAReKeyThatAnEarlierRunLeft).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" p before id
  for p in app db; do
    printf 'v' > "${store}/Secret.${p}.default.K"
    _envelope "${KEY_A}" > "${store}/Secret.${p}.default.K.enc"
  done
  _envelope "${KEY_A}" > "${store}/Secret.orphan.default.K.enc"

  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" == *"no decrypted twin"* ]] || { echo "$output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || {
    echo "fixture: the first run did not write .sops.yaml before it stopped" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  id="$(_file_id "${PATH_BASE}/.sops.yaml")"

  DOMAIN=a.dev run _add "${KEY_B}" 0 1
  [ "$status" -eq 0 ] || { echo "the rerun: status $status: $output" >&2; return 1; }
  [ "$output" = "[warn] orphan (no decrypted twin, cannot re-key): ${store}/Secret.orphan.default.K.enc
recipient already present in .sops.yaml, but 2 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key
  ${KEY_B}
Encrypted 2 secret(s)
done — commit .sops.yaml and the updated .enc files together" ] || { echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo "the rerun wrote .sops.yaml" >&2; return 1; }
  [ "$(_file_id "${PATH_BASE}/.sops.yaml")" = "${id}" ] || { echo "the rerun wrote or replaced .sops.yaml" >&2; return 1; }
  [ "$(LC_ALL=C sort "${SOPS_LOG}")" = "${store}/Secret.app.default.K
${store}/Secret.db.default.K" ] || { echo "sops ran on:" >&2; cat "${SOPS_LOG}" >&2; return 1; }
  for p in app db; do
    [ "$(secrets::_enc_lists "${KEY_B}" "${store}/Secret.${p}.default.K.enc" added)" = listed ] || {
      echo "${p}: the .enc does not list the recipient" >&2; return 1; }
  done

  # The orphan still does not list the recipient: without --skip-orphans the
  # run refuses it, as the first run did.
  before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
  : > "${SOPS_LOG}"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "the run without --skip-orphans: status $status: $output" >&2; return 1; }
  [ "$output" = "[warn] orphan (no decrypted twin, cannot re-key): ${store}/Secret.orphan.default.K.enc
[error] 1 encrypted file(s) have no decrypted twin — they would keep the OLD
[error]   recipient list while .sops.yaml claims the new key was added.
[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it." ] || { echo "$output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "the refused run changed a file" >&2; return 1; }

  DOMAIN=a.dev run _add "${KEY_B}" 0 1
  [ "$status" -eq 0 ] || { echo "the last run: status $status: $output" >&2; return 1; }
  [ "$output" = "[warn] orphan (no decrypted twin, cannot re-key): ${store}/Secret.orphan.default.K.enc
recipient already present in .sops.yaml — nothing to add
  ${KEY_B}" ] || { echo "$output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "the last run changed a file" >&2; return 1; }
  [ "$(_file_id "${PATH_BASE}/.sops.yaml")" = "${id}" ] || { echo "a run wrote or replaced .sops.yaml" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]
}

# _file_id <path> — the device, the inode and the mtime of the file at
# <path>: a write in place changes the mtime, and a replace (a rename over
# the name) changes the inode, also when the bytes stay the same.
_file_id() {
  stat -L -c '%d:%i %y' -- "${1}"
}

# _finished_store — after _load_sweep and _sops_envelope: .sops.yaml lists
# KEY_A and KEY_B, and the store a.dev holds the pairs app and db, each .enc
# a full envelope of both keys (the state after a finished add-key), dated
# in the past.
_finished_store() {
  local store="${PATH_CLUSTERS}/a.dev/secrets" p
  sed -i "s|age: '${KEY_A}'|age: '${KEY_A},${KEY_B}'|" "${PATH_BASE}/.sops.yaml"
  for p in app db; do
    printf 'v' > "${store}/Secret.${p}.default.K"
    _envelope "${KEY_A}" "${KEY_B}" > "${store}/Secret.${p}.default.K.enc"
  done
  touch -t 200001010000 "${store}/Secret.app.default.K.enc" "${store}/Secret.db.default.K.enc"
}

@test "a rerun re-keys only the .enc files that are not finished, and the other .enc keeps its bytes and its mtime" {
  # The .enc of app is missing, holds the label of the recipient alone, has
  # no mac or an empty lastmodified, has an enc that is not an armored age
  # block (or has no footer line), or lists the old key only. The rerun
  # re-keys that file only: a plaintext that is older than a pulled .enc
  # does not replace it (Go twin:
  # TestAddKeyRerunReKeysOnlyTheFilesThatAreNotFinished).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" app db kind before id
  app="${store}/Secret.app.default.K.enc"
  db="${store}/Secret.db.default.K.enc"
  for kind in missing label no-mac no-lastmodified not-armored no-footer old; do
    _finished_store
    case "${kind}" in
      missing) rm -- "${app}" ;;
      label) printf '{"data":"ENC[x]","sops":{"age":[{"recipient":"%s"}]}}\n' "${KEY_B}" > "${app}" ;;
      no-mac) jq -c 'del(.sops.mac)' "${db}" > "${app}" ;;
      no-lastmodified) jq -c '.sops.lastmodified = ""' "${db}" > "${app}" ;;
      not-armored) jq -c --arg r "${KEY_B}" '(.sops.age[] | select(.recipient == $r) | .enc) = "x"' "${db}" > "${app}" ;;
      no-footer) jq -c --arg r "${KEY_B}" '(.sops.age[] | select(.recipient == $r) | .enc) |= rtrimstr("-----END AGE ENCRYPTED FILE-----\n")' "${db}" > "${app}" ;;
      old) _envelope "${KEY_A}" > "${app}" ;;
    esac
    before="$(_state "${db}" "${PATH_BASE}/.sops.yaml")"
    id="$(_file_id "${PATH_BASE}/.sops.yaml")"
    : > "${SOPS_LOG}"
    DOMAIN=a.dev run _add "${KEY_B}"
    [ "$status" -eq 0 ] || { echo "${kind}: status $status: $output" >&2; return 1; }
    [ "$output" = "recipient already present in .sops.yaml, but 1 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key
  ${KEY_B}
Encrypted 1 secret(s)
done — commit .sops.yaml and the updated .enc files together" ] || { echo "${kind}: $output" >&2; return 1; }
    [ "$(cat "${SOPS_LOG}")" = "${store}/Secret.app.default.K" ] || { echo "${kind}: sops ran on: $(cat "${SOPS_LOG}")" >&2; return 1; }
    [ "$(secrets::_enc_lists "${KEY_B}" "${app}" added)" = listed ] || { echo "${kind}: app was not re-keyed" >&2; return 1; }
    [ "$(_state "${db}" "${PATH_BASE}/.sops.yaml")" = "${before}" ] || { echo "${kind}: the .enc of db or .sops.yaml changed" >&2; return 1; }
    [ "$(_file_id "${PATH_BASE}/.sops.yaml")" = "${id}" ] || { echo "${kind}: the rerun wrote or replaced .sops.yaml" >&2; return 1; }
  done
}

@test "a rerun fails when the creation rule does not list the key: the key is only in a comment of .sops.yaml" {
  # The rerun finds the key present and re-keys both files, but sops
  # encrypts them to the rule. The run reads the sweep again after the
  # encrypt, names each file and fails, with no "done" line (Go twin:
  # TestAddKeyRerunFailsWhenTheRuleDoesNotListTheKey).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" p
  for p in app db; do
    printf 'v' > "${store}/Secret.${p}.default.K"
    _envelope "${KEY_A}" > "${store}/Secret.${p}.default.K.enc"
  done
  { printf '# the next key: %s\n' "${KEY_B}"; cat -- "${PATH_BASE}/.sops.yaml"; } > "${BATS_TEST_TMPDIR}/cfg.new"
  mv -- "${BATS_TEST_TMPDIR}/cfg.new" "${PATH_BASE}/.sops.yaml"
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "recipient already present in .sops.yaml, but 2 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key
  ${KEY_B}
Encrypted 2 secret(s)
[error] the sops metadata of ${store}/Secret.app.default.K.enc does not pass the recipient envelope check after the re-key
[error] the sops metadata of ${store}/Secret.db.default.K.enc does not pass the recipient envelope check after the re-key
[error]   the key is in .sops.yaml, but the creation rule of these files may not list it (for example, the key is only in a comment): fix .sops.yaml, then run this command again" ] || {
    echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
}

@test "an orphan-only store: a rerun refuses the orphan without --skip-orphans, as the first run does, and an orphan that shows the recipient needs no re-key" {
  # (Go twin: TestAddKeyRerunRefusesAnOrphanOnlyStoreWithoutSkipOrphans.)
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" refusal before
  _envelope "${KEY_A}" > "${store}/Secret.orphan.default.K.enc"
  refusal="[warn] orphan (no decrypted twin, cannot re-key): ${store}/Secret.orphan.default.K.enc
[error] 1 encrypted file(s) have no decrypted twin — they would keep the OLD
[error]   recipient list while .sops.yaml claims the new key was added.
[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it."
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "the first run: status $status: $output" >&2; return 1; }
  [[ "$output" == *"${refusal}" ]] || { echo "the first run: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || {
    echo "fixture: the first run did not write .sops.yaml before it stopped" >&2; return 1; }
  before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "the rerun: status $status: $output" >&2; return 1; }
  [ "$output" = "${refusal}" ] || { echo "the rerun: $output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "the rerun changed a file" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]

  # The control: an orphan that shows the recipient.
  _envelope "${KEY_A}" "${KEY_B}" > "${store}/Secret.orphan.default.K.enc"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "the control: status $status: $output" >&2; return 1; }
  [ "$output" = "recipient already present in .sops.yaml — nothing to add
  ${KEY_B}" ] || { echo "the control: $output" >&2; return 1; }
}

@test "a rerun refuses a sweep .enc that is a symlink or a directory before it decides that nothing is left to do" {
  # (Go twin: TestAddKeyRerunRefusesAnEncThatIsNotARegularFile.)
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" app kind want before
  app="${store}/Secret.app.default.K.enc"
  for kind in link dir; do
    _finished_store
    rm -rf -- "${app}"
    if [[ "${kind}" == link ]]; then
      ln -s Secret.db.default.K.enc "${app}"
      want="[error] ${app} is a symlink: the re-key does not write an .enc through a link
[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written"
    else
      mkdir -- "${app}"
      want="[error] ${app} is not a regular file (a directory, a FIFO, a socket or a device): the re-key does not write an .enc there
[error]   remove or fix each entry named above, then run this command again; nothing was written"
    fi
    before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
    DOMAIN=a.dev run _add "${KEY_B}"
    [ "$status" -eq 1 ] || { echo "${kind}: status $status: $output" >&2; return 1; }
    [ "$output" = "${want}" ] || { echo "${kind}: $output" >&2; return 1; }
    [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "${kind}: a file changed" >&2; return 1; }
    [ ! -s "${SOPS_LOG}" ]
  done
}

@test "a rerun with nothing to do writes a missing .gitignore of the store before it says nothing to add" {
  # (Go twin: TestAddKeyRerunWritesAMissingGitignore.)
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets"
  _finished_store
  rm -f -- "${store}/.gitignore"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "recipient already present in .sops.yaml — nothing to add
  ${KEY_B}" ] || { echo "$output" >&2; return 1; }
  grep -qxF '!Secret.*.enc' "${store}/.gitignore" || { echo "no .gitignore was written" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]
}

@test "a store that add-key cannot list is refused before any write, with the key in .sops.yaml and without it" {
  # No read permission on the store: the lookup of the .gitignore in it still
  # works (Go twin: TestAddKeyRefusesAStoreThatItCannotList).
  [ "$(id -u)" -ne 0 ] || skip "root reads every directory"
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" key before
  printf 'v' > "${store}/Secret.app.default.K"
  _envelope "${KEY_A}" > "${store}/Secret.app.default.K.enc"
  for key in "${KEY_B}" "${KEY_A}"; do
    before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
    chmod 300 "${store}"
    DOMAIN=a.dev run _add "${key}"
    chmod 755 "${store}"
    [ "$status" -eq 1 ] || { echo "${key}: status $status: $output" >&2; return 1; }
    [ "$output" = "[error] cannot list the store ${store} (not a directory, no read or search permission, or the read of the directory failed): add-key did not change .sops.yaml; fix the store, then run this command again" ] || {
      echo "${key}: $output" >&2; return 1; }
    [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "${key}: a file changed" >&2; return 1; }
    [ ! -s "${SOPS_LOG}" ]
  done
}

@test "a rerun refuses an .enc larger than 16 MiB with one line that names it, and reads one of exactly 16 MiB" {
  # (Go twin: TestAddKeyRerunRefusesAnEncLargerThanTheLimit.)
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" app before
  app="${store}/Secret.app.default.K.enc"
  _finished_store
  head -c 16777217 /dev/zero > "${app}"
  before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] ${app} is larger than 16 MiB, the size limit of the metadata check: add-key does not read its sops metadata and stops here; restore or remove that file, then run this command again" ] || {
    echo "$output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "a file changed" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]

  # The control: exactly 16 MiB is read (it does not parse: the rerun re-keys it).
  head -c 16777216 /dev/zero > "${app}"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "the control: status $status: $output" >&2; return 1; }
  [ "$(cat "${SOPS_LOG}")" = "${store}/Secret.app.default.K" ] || { echo "the control: sops ran on: $(cat "${SOPS_LOG}")" >&2; return 1; }
}

@test "without jq, a rerun refuses before any change with two lines that name jq" {
  # The Go binary reads the metadata with encoding/json and needs no jq.
  # command is a function here that finds no jq.
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  command() { [[ "${1}" == -v && "${2}" == jq ]] && return 1; builtin command "${@}"; }
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  DOMAIN=a.dev run _add "${KEY_A}"
  unset -f command
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] jq not found: add-key reads the sops metadata of each .enc with jq; nothing was changed
[error]   install jq (b install), or use the lo binary: it needs no jq" ] || { echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

_no_fd='[error] bash cannot test an open file through /dev/fd (on Linux, /proc/self/fd): add-key and remove-key do not run without that test of the lock of .sops.yaml; nothing was changed
[error]   mount /proc, or use the lo binary: it tests the lock without /dev/fd'

@test "when bash cannot test a descriptor through /dev/fd, add-key refuses before the lock file with a line that names the cause" {
  # The test after the open of the lock reads the descriptor through /dev/fd
  # (on Linux, /proc/self/fd). Without it, the run made the lock file and
  # then stopped with the line of a lock path that is not a regular file.
  # Here secrets::_fd_ok is a function that fails; the next test runs
  # without /proc. remove-key takes the same lock (secrets::_lock).
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  secrets::_fd_ok || { echo "fixture: bash cannot test /dev/fd on this host" >&2; return 1; }
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  secrets::_fd_ok() { return 1; }
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "${_no_fd}" ] || { echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ ! -e "${PATH_BASE}/.sops.yaml.lock" ] || { echo "a lock file was made" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

@test "in a mount namespace with no /proc, add-key refuses before the lock file with the line of a /dev/fd that bash cannot test" {
  # A tmpfs over /proc in a user and mount namespace of the test: bash then
  # finds no /proc/self/fd for /dev/fd/<n>.
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  command -v unshare >/dev/null 2>&1 || skip "unshare not installed"
  unshare --user --map-current-user --keep-caps --mount true 2>/dev/null || skip "no user namespace here"
  local script="${BATS_TEST_TMPDIR}/no-proc.sh"
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  cat > "${script}" <<'EOF'
mount -t tmpfs none /proc 2>/dev/null || exit 99
source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh" 2>/dev/null || true
source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
secrets::_encrypt_store() { return 0; }
sops() { return 0; }
:args() { key="${KEY_B}"; all=0; skip_orphans=0; domain=a.dev; }
secrets::add-key
EOF
  run env _PROJECT_ROOT="${_PROJECT_ROOT}" KEY_B="${KEY_B}" unshare --user --map-current-user --keep-caps --mount bash "${script}"
  [ "$status" -ne 99 ] || skip "a user namespace cannot mount over /proc here"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "${_no_fd}" ] || { echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ ! -e "${PATH_BASE}/.sops.yaml.lock" ] || { echo "a lock file was made" >&2; return 1; }
}

# The refusal of the test after the open of the lock: @lock@ is the lock
# path. The open (>>) can have made an empty file through a link, so the
# lines say so; the Go twin opens with O_NOFOLLOW and prints the line of the
# check.
_lock_changed='[error] @lock@ changed after its check (a symlink or another file is there now): the command stopped before it wrote .sops.yaml or the store
[error]   but the open can have made an empty file at the path that a symlink there names: remove the lock path and that file, then run this command again'

@test "a lock link put there after the check is refused by the test after the open, and the file that it names keeps its bytes" {
  # The check of the lock path (secrets::_enc_path_ok) and the open are two
  # steps. A link put there between them is followed by the open (>>, which
  # writes nothing), and the test after the open refuses the run with the
  # line of the check. Here the check is a function that puts the link there
  # after it passed (Go twin: TestKeyCommandsRefuseALockLinkPutThereAfterTheCheck,
  # where the open does not follow the link). remove-key takes the same lock.
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  local _t_lock="${PATH_BASE}/.sops.yaml.lock" _t_victim="${BATS_TEST_TMPDIR}/elsewhere/victim"
  mkdir -p "${_t_victim%/*}"
  printf 'the bytes of another file\n' > "${_t_victim}"
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  eval "_enc_path_ok_real() $(declare -f secrets::_enc_path_ok | tail -n +2)"
  secrets::_enc_path_ok() {
    _enc_path_ok_real "${@}" || return
    [[ "${1}" == "${_t_lock}" ]] || return 0
    ln -s -- "${_t_victim}" "${_t_lock}"
  }
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "${_lock_changed//@lock@/${_t_lock}}" ] || { echo "$output" >&2; return 1; }
  [ -L "${_t_lock}" ] || { echo "fixture: the link was not put there" >&2; return 1; }
  [ "$(cat "${_t_victim}")" = 'the bytes of another file' ] || { echo "the file that the link names changed" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

@test "another regular file put at the lock path after the open is refused by the inode test after the open" {
  # The path is no link, and the file there is a regular file, so only the
  # inode test (-ef) of secrets::_opened_ok tells it from the opened file.
  # The wrapper puts the new file there after the open and before the test.
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load
  local _t_lock="${PATH_BASE}/.sops.yaml.lock" calls="${BATS_TEST_TMPDIR}/opened.calls"
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  eval "_opened_ok_real() $(declare -f secrets::_opened_ok | tail -n +2)"
  secrets::_opened_ok() {
    : >> "${calls}"
    printf 'another file\n' > "${_t_lock}.new" && mv -f -- "${_t_lock}.new" "${_t_lock}" || return
    _opened_ok_real "${@}"
  }
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ -e "${calls}" ] || { echo "fixture: the test after the open did not run" >&2; return 1; }
  [ "$output" = "${_lock_changed//@lock@/${_t_lock}}" ] || { echo "$output" >&2; return 1; }
  [[ -f "${_t_lock}" && ! -L "${_t_lock}" && "$(cat "${_t_lock}")" == 'another file' ]] || {
    echo "fixture: the lock path is not the other regular file" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

# _fail_listing <dir> <after> — a find function of the test: each listing of
# <dir> (the one checked listing of secrets::_store_names, -maxdepth 1)
# after the first <after> ones prints nothing and exits 1, as a read of the
# directory that fails (EIO). Each listing of <dir> adds a byte to
# ${BATS_TEST_TMPDIR}/listings; every other find runs the real one.
_fail_listing() {
  _t_dir="${1}" _t_after="${2}" _t_listings="${BATS_TEST_TMPDIR}/listings"
  : > "${_t_listings}"
  find() {
    if [[ " ${*} " == *" -maxdepth 1 "* && " ${*} " == *" ${_t_dir} "* ]]; then
      printf x >> "${_t_listings}"
      (( $(wc -c < "${_t_listings}") > _t_after )) || { command find "${@}"; return; }
      echo "find: ${_t_dir}: Input/output error" >&2
      return 1
    fi
    command find "${@}"
  }
}

# _listings — the count of the listings of the directory of _fail_listing.
_listings() {
  wc -c < "${BATS_TEST_TMPDIR}/listings"
}

_unlistable='[error] cannot list the store @store@ (not a directory, no read or search permission, or the read of the directory failed): @outcome@; fix the store, then run this command again'

@test "a read of the store that fails is refused before any write, with the key in .sops.yaml and without it" {
  # The mode and the search of the store are as they are: only the listing
  # fails (a find function of the test). An empty list would let the run
  # report the re-key as done (Go twin: TestAddKeyRefusesAStoreWhoseListingFails).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" key before id want
  printf 'v' > "${store}/Secret.app.default.K"
  _envelope "${KEY_A}" > "${store}/Secret.app.default.K.enc"
  want="${_unlistable//@store@/${store}}"
  want="${want//@outcome@/add-key did not change .sops.yaml}"
  for key in "${KEY_B}" "${KEY_A}"; do
    before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
    id="$(_file_id "${PATH_BASE}/.sops.yaml")"
    _fail_listing "${store}" 0
    DOMAIN=a.dev run _add "${key}"
    unset -f find
    [ "$status" -eq 1 ] || { echo "${key}: status $status: $output" >&2; return 1; }
    [ "$output" = "${want}" ] || { echo "${key}: $output" >&2; return 1; }
    [ "$(_listings)" -eq 1 ] || { echo "${key}: the store was listed $(_listings) times" >&2; return 1; }
    [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "${key}: a file changed" >&2; return 1; }
    [ "$(_file_id "${PATH_BASE}/.sops.yaml")" = "${id}" ] || { echo "${key}: .sops.yaml was written" >&2; return 1; }
    [ ! -s "${SOPS_LOG}" ]
  done
}

@test "add-key lists the store of the sweep once: the first run and a rerun re-key from that listing" {
  # Each listing of the store after the first fails (a find function of the
  # test). A second listing would find an empty store and report the re-key
  # as done (Go twin: TestAddKeyListsTheStoreOnce).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" p
  for p in app db; do
    printf 'v' > "${store}/Secret.${p}.default.K"
    _envelope "${KEY_A}" > "${store}/Secret.${p}.default.K.enc"
  done
  _fail_listing "${store}" 1
  DOMAIN=a.dev run _add "${KEY_B}"
  unset -f find
  [ "$status" -eq 0 ] || { echo "the first run: status $status: $output" >&2; return 1; }
  [ "$output" = "added recipient to .sops.yaml (1 creation rule(s))
  ${KEY_B}
re-keying 2 cache file(s) across 1 store(s)…
Encrypted 2 secret(s)
done — commit .sops.yaml and the updated .enc files together" ] || { echo "the first run: $output" >&2; return 1; }
  [ "$(_listings)" -eq 1 ] || { echo "the first run: the store was listed $(_listings) times" >&2; return 1; }
  for p in app db; do
    [ "$(secrets::_enc_lists "${KEY_B}" "${store}/Secret.${p}.default.K.enc" added)" = listed ] || {
      echo "the first run: ${p} was not re-keyed" >&2; return 1; }
  done

  rm -- "${store}/Secret.app.default.K.enc"
  : > "${SOPS_LOG}"
  _fail_listing "${store}" 1
  DOMAIN=a.dev run _add "${KEY_B}"
  unset -f find
  [ "$status" -eq 0 ] || { echo "the rerun: status $status: $output" >&2; return 1; }
  [ "$output" = "recipient already present in .sops.yaml, but 1 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key
  ${KEY_B}
Encrypted 1 secret(s)
done — commit .sops.yaml and the updated .enc files together" ] || { echo "the rerun: $output" >&2; return 1; }
  [ "$(_listings)" -eq 1 ] || { echo "the rerun: the store was listed $(_listings) times" >&2; return 1; }
  [ "$(cat "${SOPS_LOG}")" = "${store}/Secret.app.default.K" ] || { echo "the rerun: sops ran on: $(cat "${SOPS_LOG}")" >&2; return 1; }
}

@test "--all: a listing of another store that fails is refused with a line that names that store" {
  # A rerun refuses it before any write. The first run refuses it after its
  # write of .sops.yaml (the orphan check of the first run comes after that
  # write), with no .enc re-keyed and no plaintext touched (Go twin:
  # TestAddKeyAllRefusesAnotherStoreWhoseListingFails).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" other="${PATH_CLUSTERS}/b.dev/secrets" key before want outcome
  printf 'v' > "${store}/Secret.app.default.K"
  _envelope "${KEY_A}" "${KEY_B}" > "${store}/Secret.app.default.K.enc"
  printf 'x' > "${other}/Secret.x.default.K"
  touch -t 200001010000 "${store}/Secret.app.default.K" "${other}/Secret.x.default.K"
  # The .gitignore that the first run writes before .sops.yaml.
  secrets::_ensure_gitignore "${store}"
  for key in "${KEY_A}" "${KEY_B}"; do
    before="$(_state "${store}" "${other}")"
    cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
    _fail_listing "${other}" 0
    DOMAIN=a.dev run _add "${key}" 1
    unset -f find
    outcome="add-key did not change .sops.yaml"
    [[ "${key}" == "${KEY_A}" ]] || outcome="add-key wrote .sops.yaml and stopped before the re-key"
    want="${_unlistable//@store@/${other}}"
    want="${want//@outcome@/${outcome}}"
    [[ "${key}" == "${KEY_A}" ]] || want="added recipient to .sops.yaml (1 creation rule(s))
  ${KEY_B}
${want}"
    [ "$status" -eq 1 ] || { echo "${key}: status $status: $output" >&2; return 1; }
    [ "$output" = "${want}" ] || { echo "${key}: $output" >&2; return 1; }
    [ "$(_listings)" -eq 1 ] || { echo "${key}: the other store was listed $(_listings) times" >&2; return 1; }
    [ "$(_state "${store}" "${other}")" = "${before}" ] || { echo "${key}: a file of a store changed" >&2; return 1; }
    if [[ "${key}" == "${KEY_A}" ]]; then
      cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo "${key}: .sops.yaml changed" >&2; return 1; }
    else
      grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { echo "the first run did not write .sops.yaml" >&2; return 1; }
    fi
    [ ! -s "${SOPS_LOG}" ]
  done
}

@test "an orphan .enc that is a symlink to a path that does not exist is counted as an orphan, not skipped" {
  # It is in the store, and a re-key cannot rewrite it. The first run and a
  # rerun refuse it without --skip-orphans; with --skip-orphans the rerun
  # finishes the re-key and warns about it (Go twin:
  # TestAddKeyCountsADanglingOrphanLink).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" p orphan warning refusal before
  orphan="${store}/Secret.orphan.default.K.enc"
  for p in app db; do
    printf 'v' > "${store}/Secret.${p}.default.K"
    _envelope "${KEY_A}" > "${store}/Secret.${p}.default.K.enc"
  done
  ln -s Secret.missing.enc "${orphan}"
  warning="[warn] orphan (no decrypted twin, cannot re-key): ${orphan}"
  refusal="${warning}
[error] 1 encrypted file(s) have no decrypted twin — they would keep the OLD
[error]   recipient list while .sops.yaml claims the new key was added.
[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it."
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "the first run: status $status: $output" >&2; return 1; }
  [[ "$output" == *"${refusal}" ]] || { echo "the first run: $output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || {
    echo "fixture: the first run did not write .sops.yaml before it stopped" >&2; return 1; }
  before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 1 ] || { echo "the rerun: status $status: $output" >&2; return 1; }
  [ "$output" = "${refusal}" ] || { echo "the rerun: $output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "the rerun changed a file" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]

  DOMAIN=a.dev run _add "${KEY_B}" 0 1
  [ "$status" -eq 0 ] || { echo "the run with --skip-orphans: status $status: $output" >&2; return 1; }
  [[ "$output" == "${warning}"$'\n'* ]] || { echo "the run with --skip-orphans: $output" >&2; return 1; }
  [ "$(LC_ALL=C sort "${SOPS_LOG}")" = "${store}/Secret.app.default.K
${store}/Secret.db.default.K" ] || { echo "sops ran on: $(cat "${SOPS_LOG}")" >&2; return 1; }
}

@test "the shared fixture of the envelope parser: an entry of another recipient with ENC does not make a rerun re-key the .enc" {
  # After a finished run, the .enc of app holds the good entry of the
  # recipient and, before it, an entry of another recipient whose enc is
  # spelled "ENC", and the plaintext of app holds another value than the
  # .enc. Only the entries of the recipient are read for their enc, so the
  # rerun finds nothing to do and the .enc keeps its bytes (Go twin:
  # TestAddKeyRerunReadsOnlyTheEntriesOfTheRecipient).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" app before id
  app="${store}/Secret.app.default.K.enc"
  _finished_store
  jq -c --arg o 'age1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz' \
    '.sops.age = [{recipient: $o, ENC: "-----BEGIN AGE ENCRYPTED FILE-----\nx\n-----END AGE ENCRYPTED FILE-----\n"}] + .sops.age' \
    "${store}/Secret.db.default.K.enc" > "${app}"
  touch -t 200001010000 "${app}"
  printf 'not the value of the .enc' > "${store}/Secret.app.default.K"
  # The .gitignore that a rerun writes when it is missing.
  secrets::_ensure_gitignore "${store}"
  before="$(_state "${PATH_BASE}/.sops.yaml" "${store}")"
  id="$(_file_id "${PATH_BASE}/.sops.yaml")"
  DOMAIN=a.dev run _add "${KEY_B}"
  [ "$status" -eq 0 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "recipient already present in .sops.yaml — nothing to add
  ${KEY_B}" ] || { echo "$output" >&2; return 1; }
  [ "$(_state "${PATH_BASE}/.sops.yaml" "${store}")" = "${before}" ] || { echo "a file changed" >&2; return 1; }
  [ "$(_file_id "${PATH_BASE}/.sops.yaml")" = "${id}" ] || { echo ".sops.yaml was written" >&2; return 1; }
  [ ! -s "${SOPS_LOG}" ]
}

@test "a .sops.yaml that is a link to the .gitignore of the store is refused before any change, and the file keeps its bytes" {
  # The .gitignore holds valid rule YAML (with the key, and without it) and
  # no allow line, so its write would replace the file of .sops.yaml (Go
  # twin: TestAddKeyRefusesAConfigThatIsTheGitignore).
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  command -v flock >/dev/null 2>&1 || skip "flock not installed"
  _load_sweep
  _sops_envelope
  local store="${PATH_CLUSTERS}/a.dev/secrets" key gitignore before dest
  gitignore="${store}/.gitignore"
  printf 'v' > "${store}/Secret.app.default.K"
  _envelope "${KEY_A}" > "${store}/Secret.app.default.K.enc"
  for key in "${KEY_A}" "${KEY_B}"; do
    cp -- "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/rules.yaml"
    rm -f -- "${PATH_BASE}/.sops.yaml" "${gitignore}"
    cp -- "${BATS_TEST_TMPDIR}/rules.yaml" "${gitignore}"
    ln -s -- "${gitignore}" "${PATH_BASE}/.sops.yaml"
    # The lock sits beside the file that .sops.yaml names.
    : > "${gitignore}.lock"
    dest="$(readlink -f -- "${PATH_BASE}/.sops.yaml")"
    before="$(_state "${store}")"
    DOMAIN=a.dev run _add "${key}"
    [ "$status" -eq 1 ] || { echo "${key}: status $status: $output" >&2; return 1; }
    [ "$output" = "[error] .sops.yaml names ${dest}, and add-key writes that file as ${gitignore}: nothing was changed; give .sops.yaml a file of its own, then run this command again" ] || {
      echo "${key}: $output" >&2; return 1; }
    [ "$(_state "${store}")" = "${before}" ] || { echo "${key}: a file of the store changed" >&2; return 1; }
    cmp -s "${gitignore}" "${BATS_TEST_TMPDIR}/rules.yaml" || { echo "${key}: the file of .sops.yaml changed" >&2; return 1; }
    [ -L "${PATH_BASE}/.sops.yaml" ] || { echo "${key}: .sops.yaml is no longer a link" >&2; return 1; }
    [ ! -s "${SOPS_LOG}" ]
  done
}

@test "a flock failure that is not a wait that ran out says that it cannot lock, not that another command holds the lock" {
  # flock is a function of the test that exits 66 (a sysexits code); flock
  # -E gives a lock that another process holds its own code. The held lock
  # itself is in tests/unit/secrets_remove_key_test.bats.
  _load
  cp -p "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved"
  flock() { return 66; }
  DOMAIN=a.dev run _add "${KEY_B}"
  unset -f flock
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [ "$output" = "[error] cannot lock ${PATH_BASE}/.sops.yaml.lock (flock exited with 66): nothing was changed" ] || {
    echo "$output" >&2; return 1; }
  cmp -s "${PATH_BASE}/.sops.yaml" "${BATS_TEST_TMPDIR}/cfg.saved" || { echo ".sops.yaml changed" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
}

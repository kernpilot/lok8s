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
  # the check for a mv that takes -T comes before the lock).
  _load
  local bin="${BATS_TEST_TMPDIR}/no-flock" tool path
  mkdir -p "${bin}"
  for tool in sops grep sed cat mv; do
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

@test "--all in a domain whose store does not resolve stops after the write and encrypts nothing" {
  # The check before the write lists no file, so the encrypt does not list a
  # store either: it says why the store does not resolve (Go twin:
  # TestAddKeyAllWithAnUnresolvedStoreEncryptsNothing).
  _load
  source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"
  mkdir -p "${PATH_CLUSTERS}/c.dev"
  DOMAIN=c.dev run _add "${KEY_B}" 1
  [ "$status" -eq 1 ] || { echo "status $status: $output" >&2; return 1; }
  [[ "$output" == *"[error] domain c.dev has no secrets store"* ]] || { echo "$output" >&2; return 1; }
  grep -qF "age: '${KEY_A},${KEY_B}'" "${PATH_BASE}/.sops.yaml" || { cat "${PATH_BASE}/.sops.yaml" >&2; return 1; }
  [ ! -s "${ENCRYPT_RAN}" ]
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
  [ "$(ls -A "${conf%/*}")" = sops.yaml ] || { echo "a temp file was left: $(ls -A "${conf%/*}")" >&2; return 1; }
}

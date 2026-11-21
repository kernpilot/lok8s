#!/usr/bin/env bash
# parity-test.sh — differential test between the Go lo and the argsh lo.
#
# For every ported command, runs BOTH implementations (the Go binary, and the
# same binary routed to the frozen tree by the project file) against a
# synthetic project and diffs stdout, stderr, and exit codes. Lines listed in
# an ALLOW_DIFF pattern are permitted to differ (deliberate divergences, e.g.
# `lo version` no longer reporting a bash version).
#
# Usage: hack/parity-test.sh [path-to-go-lo]   (default: bin/lo)
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"

# Synthetic project: three domains covering the driver/deploy/malformed axes.
parity::new_project "${PROJ}"
mkdir -p "${PROJ}/clusters/alpha.dev" "${PROJ}/clusters/beta.cloud" "${PROJ}/clusters/gamma.app"
printf 'kind: Lo\nmetadata:\n  name: alpha\n' > "${PROJ}/clusters/alpha.dev/cluster.lok8s.yaml"
printf 'kind: KubeOne\nmetadata:\n  name: beta\n' > "${PROJ}/clusters/beta.cloud/cluster.lok8s.yaml"
printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${PROJ}/clusters/gamma.app/deploy.lok8s.yaml"

# lo use — full surface.
check - use
check - use alpha.dev
check - use
check - use --domain beta.cloud
check - use nonexistent.dev
check - use '../evil'
check - use gamma.app

# lo version — the Go binary intentionally drops the `bash` row. It runs
# each tool that it finds, docker included, so the case runs with the
# probe stubs (parity::probe_case): the tool rows show the stub line.
parity::probe_case "${PROJ}" check '^bash ' version

# A project with NO .lok8s whose project file says `default: bash`: a
# routing needs the tree IN the project, so the binary refuses with the
# exact message, writes nothing into the project and extracts nothing
# into the cache (XDG_CACHE_HOME): the embedded copy serves the provider
# plugins and the drivers fallback, never a routing.
BARE="${WORK}/bare"
mkdir -p "${BARE}/clusters" "${BARE}/.bin" "${WORK}/cache"
ln -s "${ROOT}/.bin/argsh" "${BARE}/.bin/argsh"
[[ -e "${ROOT}/.bin/argsh.so" ]] && ln -s "${ROOT}/.bin/argsh.so" "${BARE}/.bin/argsh.so"
parity::implementation "${BARE}" bash
bare_rc=0
(cd "${BARE}" && XDG_CACHE_HOME="${WORK}/cache" "${LO_BIN}" version </dev/null >"${WORK}/bare.out" 2>"${WORK}/bare.err") || bare_rc=$?
want="lo: implementation bash: the tree ${BARE}/.lok8s/lo is missing. Run \"lo assets eject bash\", or set spec.implementation.default: go."
if (( bare_rc == 1 )) && [[ "$(cat "${WORK}/bare.err")" == "${want}" && ! -s "${WORK}/bare.out" ]]; then
  echo "ok: a routing without the tree in the project is refused (rc 1, the exact message)"
else
  fail "routing without a tree: rc=${bare_rc} stderr=$(cat "${WORK}/bare.err")"
fi
if [[ -e "${WORK}/cache/lok8s" ]]; then
  fail "a refused routing extracted the embedded tree into the cache"
else
  echo "ok: nothing extracted into the cache"
fi
if [[ -e "${BARE}/.lok8s" ]]; then
  fail "a refused routing wrote .lok8s into a project without one"
else
  echo "ok: the bare project got no .lok8s"
fi

# ── lo secrets ───────────────────────────────────────────────────────────────
# Stateful commands (init writes .sops.yaml, set/encrypt mutate the store), so
# each implementation gets its OWN project clone and state builds up per impl;
# outputs are diffed with the project dir normalized to PROJ. Crypto parity is
# asserted by CROSS-decrypt (a bash-encrypted .enc decrypted by the Go impl
# and vice versa) — never by comparing .enc bytes (sops encryption is
# nondeterministic).

export HOME="${WORK}/home"          # isolate ~/.config/sops/age/keys.txt
# sops reads <config dir>/sops/age/keys.txt, and on Linux the config dir is
# $XDG_CONFIG_HOME when it is set, not $HOME/.config: pin it into WORK too.
export XDG_CONFIG_HOME="${HOME}/.config"
mkdir -p "${HOME}"
ssh-keygen -t ed25519 -N '' -C parity -f "${WORK}/home/key" -q
export LOK8S_SSH_KEY="${WORK}/home/key"   # private key (decrypt derivation)
# Every other age identity source sops reads from the environment.
unset SOPS_AGE_KEY SOPS_AGE_KEY_FILE SOPS_AGE_KEY_CMD SOPS_AGE_SSH_PRIVATE_KEY_FILE || true

for impl in go bash; do
  parity::new_project "${WORK}/sec-${impl}"
  mkdir -p "${WORK}/sec-${impl}/clusters/alpha.dev/secrets"
  printf 'kind: Lo\nmetadata:\n  name: alpha\n' > "${WORK}/sec-${impl}/clusters/alpha.dev/cluster.lok8s.yaml"
done

# sec_check <argv...> — run in both secrets clones, diff rc/stdout/stderr.
sec_check() {
  PARITY_DIR_GO="${WORK}/sec-go" PARITY_DIR_BASH="${WORK}/sec-bash" check - "$@"
}

# sec_state <relpath> — the file must be byte-identical across the two clones.
sec_state() {
  parity::state_same "${WORK}/sec-bash/${1}" "${WORK}/sec-go/${1}" "${1}" || failures=$((failures + 1))
}

# sec_prep <argv...> — a fixture step, not a parity case: run in both clones,
# each under its own implementation, output discarded. A non-zero rc fails.
sec_prep() {
  local impl rc
  for impl in go bash; do
    parity::implementation "${WORK}/sec-${impl}" "${impl}"
    rc=0
    (cd "${WORK}/sec-${impl}" && "${LO_BIN}" "$@" </dev/null >/dev/null 2>&1) || rc=$?
    (( rc == 0 )) || fail "fixture (${impl}): lo $* exited ${rc}"
  done
}

sec_check secrets path
sec_check secrets --domain alpha.dev path
sec_check secrets path --domain alpha.dev
sec_check s path
# beta.dev: a domain directory without its own store, in a project that keeps
# a store per domain (alpha.dev). No flat fallback: the store-reading
# commands stop with the refusal; `set` creates the store first, then works.
for impl in go bash; do
  mkdir -p "${WORK}/sec-${impl}/clusters/beta.dev"
  printf 'kind: Lo\nmetadata:\n  name: beta\n' > "${WORK}/sec-${impl}/clusters/beta.dev/cluster.lok8s.yaml"
done
sec_check secrets --domain beta.dev path
if ! grep -qF "[error] domain beta.dev has no secrets store: PROJ/clusters/beta.dev/secrets does not exist, and this project keeps a store per domain (PROJ/clusters/alpha.dev/secrets)." "${WORK}/go.err"; then
  fail "secrets path for a domain without a store: no refusal"
fi
sec_check secrets --domain beta.dev list
sec_check secrets --domain beta.dev decrypt
sec_check secrets --domain beta.dev set -n app TOKEN beta-secret
sec_check secrets --domain beta.dev path
sec_state clusters/beta.dev/secrets/Secret.app.default.TOKEN
sec_check secrets set -n app JWT_SECRET s3cr3t
sec_check secrets set --domain alpha.dev -n app -s myns TOKEN dom-secret
sec_check secrets encrypt              # no .sops.yaml yet → error
sec_check secrets init --ssh-key "${WORK}/home/key.pub"
sec_check secrets init --ssh-key "${WORK}/home/key.pub"   # idempotent branch
sec_state .sops.yaml
sec_check secrets set -n app -e ENC1 enc-value            # set + single-file encrypt
# The key ENC makes the plaintext Secret.app.default.ENC, a name that ends in
# .enc in another letter case. The rule !Secret.*.enc can let git commit it,
# so set refuses the key before it writes a file, also with -e.
sec_check secrets set -n app -e ENC enc-value
for impl in go bash; do
  [[ ! -e "${WORK}/sec-${impl}/.secrets/Secret.app.default.ENC" ]] \
    || fail "secrets set (${impl}): a plaintext with a reserved name"
done
sec_check secrets set -n app --enc ENC2 enc-value2        # --enc alias
sec_check secrets set -n app JWT_SECRET s3cr3t2           # stale-.enc warning path
sec_check secrets e                                       # sweep (alias)
sec_check secrets encrypt                                 # all fresh → silent
sec_check secrets encrypt --name nope                     # no match → loud failure
sec_check secrets encrypt --name ../../etc                # traversal guard
sec_check secrets list
sec_check secrets l
sec_check secrets list --domain alpha.dev
sec_check secrets env --name app
sec_check secrets env --name app -s myns --domain alpha.dev
sec_check secrets env --name nope
sec_check secrets print JWT_SECRET
sec_check secrets print app
sec_check secrets print --only-one app
sec_check secrets print zzz
sec_check secrets allow                                   # no .sha files yet
for impl in go bash; do
  printf 'deadbeef  \n' > "${WORK}/sec-${impl}/.secrets/Secret.gen.default.X.sha"
  printf 'cafe\n' > "${WORK}/sec-${impl}/.secrets/other.sha"
done
sec_check secrets allow
sec_state .secrets/.bash-allow
sec_state .secrets/.gitignore
# A negation below the block of lo: set refuses before the plaintext, with
# the line number and not the rule. The .gitignore keeps its bytes.
for impl in go bash; do
  cp -p -- "${WORK}/sec-${impl}/.secrets/.gitignore" "${WORK}/sec-${impl}.gitignore.saved"
  printf '!README.md\n' >> "${WORK}/sec-${impl}/.secrets/.gitignore"
done
sec_check secrets set -n app GITIGNORE_REFUSED refused-value
sec_state .secrets/.gitignore
for impl in go bash; do
  [[ ! -e "${WORK}/sec-${impl}/.secrets/Secret.app.default.GITIGNORE_REFUSED" ]] \
    || fail "secrets set (${impl}): a plaintext despite a refused .gitignore"
  mv -f -- "${WORK}/sec-${impl}.gitignore.saved" "${WORK}/sec-${impl}/.secrets/.gitignore"
done
sec_check secrets add-key /no/such/file
sec_check secrets add-key age1nope
KEY2="$("${ROOT}/.bin/ssh-to-age" < "${WORK}/home/key.pub")"
sec_check secrets add-key "${KEY2}"                       # already present: encrypts the .sha with no .enc
ssh-keygen -t ed25519 -N '' -C parity2 -f "${WORK}/home/key2" -q
sec_check secrets add-key "${WORK}/home/key2.pub"         # re-key + sweep
sec_state .sops.yaml

# sec_no_echo <text> <label> — no output of the last sec_check contains text.
sec_no_echo() {
  if grep -qF -- "${1}" "${WORK}/go.out" "${WORK}/go.err" "${WORK}/bash.out" "${WORK}/bash.err"; then
    fail "${2}: the output echoes the argument"
  fi
}

# A private key pasted as the argument: refused by add-key and remove-key,
# never echoed. A fixed filler outside the bech32 alphabet: the shape of an
# age identity, not a key.
FAKE_ID="AGE-SECRET-KEY-1$(printf 'O%.0s' {1..58})"
sec_check secrets add-key "${FAKE_ID}"
sec_no_echo "${FAKE_ID}" "secrets add-key <private key>"
sec_check secrets remove-key "${FAKE_ID}"
sec_no_echo "${FAKE_ID}" "secrets remove-key <private key>"
# The same before any parser can print it: a PEM line (it looks like a
# flag), the PEM block split into words, and a private key as an extra
# argument. PEM_HEAD is built from two parts, so that the private-key rule of
# a secret scanner does not match from one PEM literal to the next.
PEM_HEAD="-----BEGIN OPENSSH PRIVATE ""KEY-----"
for cmd in add-key remove-key; do
  sec_check secrets "${cmd}" "${PEM_HEAD}"
  sec_no_echo "OPENSSH" "secrets ${cmd} <PEM line>"
  # shellcheck disable=SC2086 # the four words of the PEM line
  sec_check secrets "${cmd}" ${PEM_HEAD}
  sec_no_echo "OPENSSH" "secrets ${cmd} <PEM words>"
  sec_check secrets "${cmd}" "${KEY2}" "${FAKE_ID}"
  sec_no_echo "${FAKE_ID}" "secrets ${cmd} <age key> <private key>"
done

# remove-key: the refusals first. None of them may write .sops.yaml.
KEY2B="$("${ROOT}/.bin/ssh-to-age" < "${WORK}/home/key2.pub")"
for impl in go bash; do
  cp -p "${WORK}/sec-${impl}/.sops.yaml" "${WORK}/sops-${impl}.saved"
done

# sec_refusal <label> <yaml> <argv...> — writes .sops.yaml (the saved one with
# the yaml appended, or the yaml alone when it starts with creation_rules:)
# in both clones, runs the case, checks that .sops.yaml did not change, and
# puts the saved one back.
sec_refusal() {
  local label="${1}" yaml="${2}" impl
  shift 2
  for impl in go bash; do
    if [[ "${yaml}" == creation_rules:* ]]; then
      printf '%s' "${yaml}" > "${WORK}/sec-${impl}/.sops.yaml"
    else
      cp -p "${WORK}/sops-${impl}.saved" "${WORK}/sec-${impl}/.sops.yaml"
      printf '%s' "${yaml}" >> "${WORK}/sec-${impl}/.sops.yaml"
    fi
    cp -p "${WORK}/sec-${impl}/.sops.yaml" "${WORK}/sops-${impl}.case"
  done
  sec_check "$@"
  for impl in go bash; do
    cmp -s "${WORK}/sops-${impl}.case" "${WORK}/sec-${impl}/.sops.yaml" \
      || fail "secrets remove-key (${impl}): .sops.yaml changed by the ${label} refusal"
    cp -p "${WORK}/sops-${impl}.saved" "${WORK}/sec-${impl}/.sops.yaml"
  done
}

# The YAML read: key2 where the line edit does not look (an escaped string,
# a key group, an alias). ESC2 is key2 with its first letter as the escape
# backslash, u, 0061 in a double-quoted string.
ESC2="\"$(printf '\134')u0061${KEY2B:1}\""
sec_refusal "escaped string" "  - path_regex: 'Other\\..*'
    age: ${ESC2}
" secrets remove-key "${WORK}/home/key2.pub"
sec_refusal "key_groups" "  - path_regex: 'Other\\..*'
    key_groups:
      - age:
          - ${ESC2}
" secrets remove-key "${WORK}/home/key2.pub"
sec_refusal "alias" "  - path_regex: &shared ${ESC2}
    age: '${KEY2}'
  - path_regex: 'Otherx\\..*'
    age: *shared
" secrets remove-key "${WORK}/home/key2.pub"

# The lock: while another run holds .sops.yaml.lock, both commands refuse
# at once (LO_SECRETS_LOCK_WAIT=0) and change nothing.
if command -v flock >/dev/null 2>&1; then
  exec {lock_go}>"${WORK}/sec-go/.sops.yaml.lock" {lock_bash}>"${WORK}/sec-bash/.sops.yaml.lock"
  if flock -n "${lock_go}" && flock -n "${lock_bash}"; then
    export LO_SECRETS_LOCK_WAIT=0
    sec_check secrets remove-key "${WORK}/home/key2.pub"
    sec_check secrets add-key "${WORK}/home/key.pub"
    unset LO_SECRETS_LOCK_WAIT
  else
    fail "secrets lock fixture: the harness cannot take the lock"
  fi
  exec {lock_go}>&- {lock_bash}>&-
  for impl in go bash; do
    cmp -s "${WORK}/sops-${impl}.saved" "${WORK}/sec-${impl}/.sops.yaml" \
      || fail "secrets (${impl}): .sops.yaml changed while the lock was held"
  done
fi
sec_check secrets remove-key age1nope
sec_check secrets remove-key /no/such/file
sec_check secrets remove-key "age1$(printf 'q%.0s' {1..57})z"   # valid shape, in no rule and on no label: a warning, not a refusal
for impl in go bash; do                                   # a second rule that lists only key2
  printf "  - path_regex: 'Other\\\\..*'\n    age: '%s'\n" "${KEY2B}" >> "${WORK}/sec-${impl}/.sops.yaml"
  cp -p "${WORK}/sec-${impl}/.sops.yaml" "${WORK}/sops-${impl}.last"
done
sec_check secrets remove-key "${WORK}/home/key2.pub"      # the last recipient of a rule
for impl in go bash; do
  cmp -s "${WORK}/sops-${impl}.last" "${WORK}/sec-${impl}/.sops.yaml" \
    || fail "secrets remove-key (${impl}): .sops.yaml changed by the last-recipient refusal"
  cp -p "${WORK}/sops-${impl}.saved" "${WORK}/sec-${impl}/.sops.yaml"
  printf 'enc\n' > "${WORK}/sec-${impl}/.secrets/Secret.orphan.default.X.enc"
done
sec_check secrets remove-key "${WORK}/home/key2.pub"      # an orphan .enc
for impl in go bash; do
  cmp -s "${WORK}/sops-${impl}.saved" "${WORK}/sec-${impl}/.sops.yaml" \
    || fail "secrets remove-key (${impl}): .sops.yaml changed by the orphan refusal"
  rm -f "${WORK}/sec-${impl}/.secrets/Secret.orphan.default.X.enc"
done

# --all: every domain store is re-keyed (add-key's --all re-encrypts the
# current store only).
sec_check secrets --domain alpha.dev encrypt
sec_check secrets --domain beta.dev encrypt
for impl in go bash; do
  grep -qF "${KEY2B}" "${WORK}/sec-${impl}/clusters/beta.dev/secrets/Secret.app.default.TOKEN.enc" \
    || fail "secrets remove-key fixture (${impl}): the beta.dev store does not list key2"
done
# Right after an encrypt each .enc is newer than its plaintext, and the
# plaintext holds the value of the .enc. The compare reads the values, not
# the file times, so a newer .enc is no reason to refuse.
sec_check secrets --domain alpha.dev remove-key "${WORK}/home/key2.pub"
sec_prep secrets --domain alpha.dev decrypt
sec_prep secrets --domain beta.dev decrypt
# An .enc of the store that matches no creation rule, a rule with a
# recipient that is not age, or a rule with an age recipient that does not
# parse (NOT_RCPT: the length of one, in a letter outside bech32): the re-key
# would stop half way.
NOT_RCPT="age1$(printf 'b%.0s' {1..58})"
sec_refusal "no rule" "creation_rules:
  - path_regex: 'Other\\..*'
    age: '${KEY2},${KEY2B}'
" secrets --domain alpha.dev remove-key "${WORK}/home/key2.pub"
sec_refusal "pgp rule" "creation_rules:
  - path_regex: 'Secret\\..*'
    age: '${KEY2},${KEY2B}'
    pgp: 'FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000'
" secrets --domain alpha.dev remove-key "${WORK}/home/key2.pub"
sec_refusal "age recipient that does not parse" "creation_rules:
  - path_regex: 'Secret\\..*'
    age: '${KEY2},${KEY2B},${NOT_RCPT}'
" secrets --domain alpha.dev remove-key "${WORK}/home/key2.pub"
sec_refusal "key-group recipient that does not parse" "creation_rules:
  - path_regex: 'Secret\\..*'
    key_groups:
      - age: ['${KEY2}', '${NOT_RCPT}']
  - path_regex: 'Other\\..*'
    age: '${KEY2},${KEY2B}'
" secrets --domain alpha.dev remove-key "${WORK}/home/key2.pub"
sec_check secrets remove-key "${WORK}/home/key2.pub" --all
sec_state .sops.yaml
for impl in go bash; do
  if grep -lF "${KEY2B}" "${WORK}/sec-${impl}"/clusters/*/secrets/Secret.*.enc; then
    fail "secrets remove-key --all (${impl}): a domain store still lists the removed recipient"
  fi
  grep -qF "${KEY2B}" "${WORK}/sec-${impl}/.secrets/Secret.app.default.JWT_SECRET.enc" \
    || fail "secrets remove-key --all (${impl}): the flat store was re-keyed too"
done
# The recipient is in no rule now, but the flat store's .enc files still list
# it: a second run re-keys them, once the cache is fresh.
sec_prep secrets decrypt
sec_check secrets remove-key "${WORK}/home/key2.pub"
for impl in go bash; do
  if grep -rlF --include='*.enc' "${KEY2B}" "${WORK}/sec-${impl}/.secrets" "${WORK}/sec-${impl}/clusters"; then
    fail "secrets remove-key (${impl}): an .enc file still lists the removed recipient"
  fi
done

# Cross-decrypt: wipe the plaintexts, then decrypt the go-encrypted store
# with the BASH impl and the bash-encrypted store with the GO impl. Both must
# restore the same values — that is the interoperability contract.
rm "${WORK}/sec-go/.secrets/Secret.app.default.JWT_SECRET" \
   "${WORK}/sec-bash/.secrets/Secret.app.default.JWT_SECRET"
cross_rc=0
parity::implementation "${WORK}/sec-go" bash
parity::implementation "${WORK}/sec-bash" go
(cd "${WORK}/sec-go" && "${LO_BIN}" secrets decrypt >/dev/null 2>&1) || cross_rc=$?
(cd "${WORK}/sec-bash" && "${LO_BIN}" secrets decrypt >/dev/null 2>&1) || cross_rc=$?
if (( cross_rc == 0 )) \
  && [[ "$(cat "${WORK}/sec-go/.secrets/Secret.app.default.JWT_SECRET")" == "s3cr3t2" ]] \
  && [[ "$(cat "${WORK}/sec-bash/.secrets/Secret.app.default.JWT_SECRET")" == "s3cr3t2" ]]; then
  echo "ok: secrets cross-decrypt (bash⇄go interoperability)"
else
  fail "secrets cross-decrypt"
fi
sec_check secrets decrypt                                 # freshness skip → counted, all equal

# A kept plaintext that differs from its .enc: decrypt names it (never the
# value), counts both kinds of skip and exits 0.
for impl in go bash; do
  printf 'local-edit' > "${WORK}/sec-${impl}/.secrets/Secret.app.default.JWT_SECRET"
  touch -t 202001010000 "${WORK}/sec-${impl}/.secrets/Secret.app.default.JWT_SECRET.enc"
done
sec_check secrets decrypt
if grep -q 'local-edit' "${WORK}/go.out" "${WORK}/go.err" "${WORK}/bash.out" "${WORK}/bash.err"; then
  fail "secrets decrypt printed a plaintext value"
fi
if ! grep -qx '\[warn\] Secret.app.default.JWT_SECRET: the plaintext is newer than Secret.app.default.JWT_SECRET.enc and differs from it. Decrypt kept the plaintext.' "${WORK}/go.err"; then
  fail "secrets decrypt did not name the differing plaintext"
fi

# An .enc that does not decrypt: both print one [error] line with the file
# name only, never the sops stderr (a MAC mismatch prints two unkeyed SHA-512
# digests of the plaintext). The file sorts last, so the run reaches it.
for impl in go bash; do
  printf '{"data":"ENC[fake]","sops":{}}' > "${WORK}/sec-${impl}/.secrets/Secret.zz.default.BAD.enc"
done
sec_check secrets decrypt
if ! grep -qx '\[error\] Failed to decrypt Secret.zz.default.BAD.enc' "${WORK}/bash.err" \
  || grep -v '^\[' "${WORK}/bash.err" | grep -q .; then
  fail "secrets decrypt of a bad .enc: not exactly the [error] line"
fi
for impl in go bash; do
  rm -f "${WORK}/sec-${impl}/.secrets/Secret.zz.default.BAD.enc" "${WORK}/sec-${impl}/.secrets/Secret.zz.default.BAD"
done

report

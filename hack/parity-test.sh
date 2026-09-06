#!/usr/bin/env bash
# parity-test.sh — differential test between the Go lo and the argsh lo.
#
# For every ported command, runs BOTH implementations (the Go binary, and the
# same binary with LO_IMPL=bash forcing the argsh passthrough) against a
# synthetic project and diffs stdout, stderr, and exit codes. Lines listed in
# an ALLOW_DIFF pattern are permitted to differ (deliberate divergences, e.g.
# `lo version` no longer reporting a bash version).
#
# Usage: hack/parity-test.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# The harness must run against its synthetic project ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they silently redirect BOTH implementations — and the secrets section's
# WRITES — into that live repo instead of ${WORK}.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_SSH_KEY SOPS_AGE_KEY SOPS_AGE_KEY_FILE DEBUG

# Pin the C locale: bash glob expansion sorts by LC_COLLATE, and the Go port
# lists stores in byte order (= C collation). Under e.g. en_US.UTF-8 the two
# orderings differ for mixed-case names — a cosmetic listing-order divergence
# this differential harness must not trip over.
export LC_ALL=C

# Synthetic project: three domains covering the driver/deploy/malformed axes.
mkdir -p "${WORK}/proj/clusters/alpha.dev" "${WORK}/proj/clusters/beta.cloud" "${WORK}/proj/clusters/gamma.app"
printf 'kind: Lo\nmetadata:\n  name: alpha\n' > "${WORK}/proj/clusters/alpha.dev/cluster.lok8s.yaml"
printf 'kind: KubeOne\nmetadata:\n  name: beta\n' > "${WORK}/proj/clusters/beta.cloud/cluster.lok8s.yaml"
printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${WORK}/proj/clusters/gamma.app/deploy.lok8s.yaml"
ln -s "${ROOT}/.lok8s" "${WORK}/proj/.lok8s"
ln -s "${ROOT}/.bin" "${WORK}/proj/.bin"

failures=0

# check <allow-diff-regex|-> <argv...>
check() {
  local allow="${1}"; shift
  local go_rc=0 bash_rc=0
  (cd "${WORK}/proj" && "${LO_BIN}" "$@" >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${WORK}/proj" && LO_IMPL=bash "${LO_BIN}" "$@" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?

  local ok=1
  if (( go_rc != bash_rc )); then
    echo "FAIL: lo $* — rc: bash=${bash_rc} go=${go_rc}"
    ok=0
  fi
  local stream
  for stream in out err; do
    local diff_out
    if [[ "${allow}" == "-" ]]; then
      diff_out="$(diff "${WORK}/bash.${stream}" "${WORK}/go.${stream}" || true)"
    else
      diff_out="$(diff <(grep -vE "${allow}" "${WORK}/bash.${stream}") \
                       <(grep -vE "${allow}" "${WORK}/go.${stream}") || true)"
    fi
    if [[ -n "${diff_out}" ]]; then
      echo "FAIL: lo $* — std${stream} differs:"
      echo "${diff_out}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  if (( ok )); then
    echo "ok: lo $*"
  else
    failures=$((failures + 1))
  fi
}

# lo use — full surface.
check - use
check - use alpha.dev
check - use
check - use --domain beta.cloud
check - use nonexistent.dev
check - use '../evil'
check - use gamma.app

# lo version — the Go binary intentionally drops the `bash` row.
check '^bash ' version

# ── lo secrets ───────────────────────────────────────────────────────────────
# Stateful commands (init writes .sops.yaml, set/encrypt mutate the store), so
# each implementation gets its OWN project clone and state builds up per impl;
# outputs are diffed with the project dir normalized to PROJ. Crypto parity is
# asserted by CROSS-decrypt (a bash-encrypted .enc decrypted by the Go impl
# and vice versa) — never by comparing .enc bytes (sops encryption is
# nondeterministic).

export HOME="${WORK}/home"          # isolate ~/.config/sops/age/keys.txt
mkdir -p "${HOME}"
ssh-keygen -t ed25519 -N '' -C parity -f "${WORK}/home/key" -q
export LOK8S_SSH_KEY="${WORK}/home/key"   # private key (decrypt derivation)
unset SOPS_AGE_KEY SOPS_AGE_KEY_FILE || true

for impl in go bash; do
  mkdir -p "${WORK}/sec-${impl}/clusters/alpha.dev/secrets"
  printf 'kind: Lo\nmetadata:\n  name: alpha\n' > "${WORK}/sec-${impl}/clusters/alpha.dev/cluster.lok8s.yaml"
  ln -s "${ROOT}/.lok8s" "${WORK}/sec-${impl}/.lok8s"
  ln -s "${ROOT}/.bin" "${WORK}/sec-${impl}/.bin"
done

# sec_check <argv...> — run in both secrets clones, diff rc/stdout/stderr.
sec_check() {
  local go_rc=0 bash_rc=0
  (cd "${WORK}/sec-go" && "${LO_BIN}" "$@" </dev/null >"${WORK}/sgo.out" 2>"${WORK}/sgo.err") || go_rc=$?
  (cd "${WORK}/sec-bash" && LO_IMPL=bash "${LO_BIN}" "$@" </dev/null >"${WORK}/sbash.out" 2>"${WORK}/sbash.err") || bash_rc=$?
  sed -i "s|${WORK}/sec-go|PROJ|g" "${WORK}/sgo.out" "${WORK}/sgo.err"
  sed -i "s|${WORK}/sec-bash|PROJ|g" "${WORK}/sbash.out" "${WORK}/sbash.err"

  local ok=1
  if (( go_rc != bash_rc )); then
    echo "FAIL: lo $* — rc: bash=${bash_rc} go=${go_rc}"
    ok=0
  fi
  local stream diff_out
  for stream in out err; do
    diff_out="$(diff "${WORK}/sbash.${stream}" "${WORK}/sgo.${stream}" || true)"
    if [[ -n "${diff_out}" ]]; then
      echo "FAIL: lo $* — std${stream} differs:"
      echo "${diff_out}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  if (( ok )); then
    echo "ok: lo $*"
  else
    failures=$((failures + 1))
  fi
}

# sec_state <relpath> — the file must be byte-identical across the two clones.
sec_state() {
  if diff -q "${WORK}/sec-bash/$1" "${WORK}/sec-go/$1" >/dev/null 2>&1; then
    echo "ok: state $1"
  else
    echo "FAIL: state $1 differs between implementations"
    diff "${WORK}/sec-bash/$1" "${WORK}/sec-go/$1" | head -10 | sed 's/^/  /' || true
    failures=$((failures + 1))
  fi
}

sec_check secrets path
sec_check secrets --domain alpha.dev path
sec_check secrets path --domain alpha.dev
sec_check s path
sec_check secrets set -n app JWT_SECRET s3cr3t
sec_check secrets set --domain alpha.dev -n app -s myns TOKEN dom-secret
sec_check secrets encrypt              # no .sops.yaml yet → error
sec_check secrets init --ssh-key "${WORK}/home/key.pub"
sec_check secrets init --ssh-key "${WORK}/home/key.pub"   # idempotent branch
sec_state .sops.yaml
sec_check secrets set -n app -e ENC enc-value             # set + single-file encrypt
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
sec_check secrets add-key /no/such/file
sec_check secrets add-key age1nope
KEY2="$("${ROOT}/.bin/ssh-to-age" < "${WORK}/home/key.pub")"
sec_check secrets add-key "${KEY2}"                       # already present → no-op
ssh-keygen -t ed25519 -N '' -C parity2 -f "${WORK}/home/key2" -q
sec_check secrets add-key "${WORK}/home/key2.pub"         # re-key + sweep
sec_state .sops.yaml

# Cross-decrypt: wipe the plaintexts, then decrypt the go-encrypted store
# with the BASH impl and the bash-encrypted store with the GO impl. Both must
# restore the same values — that is the interoperability contract.
rm "${WORK}/sec-go/.secrets/Secret.app.default.JWT_SECRET" \
   "${WORK}/sec-bash/.secrets/Secret.app.default.JWT_SECRET"
cross_rc=0
(cd "${WORK}/sec-go" && LO_IMPL=bash "${LO_BIN}" secrets decrypt >/dev/null 2>&1) || cross_rc=$?
(cd "${WORK}/sec-bash" && "${LO_BIN}" secrets decrypt >/dev/null 2>&1) || cross_rc=$?
if (( cross_rc == 0 )) \
  && [[ "$(cat "${WORK}/sec-go/.secrets/Secret.app.default.JWT_SECRET")" == "s3cr3t2" ]] \
  && [[ "$(cat "${WORK}/sec-bash/.secrets/Secret.app.default.JWT_SECRET")" == "s3cr3t2" ]]; then
  echo "ok: secrets cross-decrypt (bash⇄go interoperability)"
else
  echo "FAIL: secrets cross-decrypt"
  failures=$((failures + 1))
fi
sec_check secrets decrypt                                 # freshness skip → silent

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"

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

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"

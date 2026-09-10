#!/usr/bin/env bash
# parity.sh: run every parity harness (hack/parity-*.sh) against one lo
# binary and summarize: one line per harness with its ok/fail counts, the
# full log of every harness that failed, exit 1 if any did.
#
# Each harness is a differential run of the Go binary against the argsh
# passthrough (LO_IMPL=bash) over a synthetic project; see
# hack/lib/parity.sh for the shared machinery. The binary is resolved to an
# absolute path: every harness cd's into its synthetic project before
# invoking it.
#
# Usage: hack/parity.sh <path-to-lo-binary>   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ "${LO_BIN}" == /* ]] || LO_BIN="${PWD}/${LO_BIN}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

# The CI order: the cheap, cluster-free surfaces first.
HARNESSES=(test build configure audit loop leaves ops kubehz operator orchestrate)

LOGS="$(mktemp -d)"
trap 'rm -rf "${LOGS}"' EXIT

echo "parity: ${LO_BIN} ($("${LO_BIN}" --version 2>/dev/null | head -1))"
failed=0
for name in "${HARNESSES[@]}"; do
  log="${LOGS}/${name}.log"
  rc=0
  bash "${ROOT}/hack/parity-${name}.sh" "${LO_BIN}" >"${log}" 2>&1 || rc=$?
  ok="$(grep -c '^ok:' "${log}" || true)"
  bad="$(grep -c '^FAIL:' "${log}" || true)"
  if (( rc == 0 )); then
    printf '  ok    parity-%-12s %4d ok\n' "${name}" "${ok}"
  else
    printf '  FAIL  parity-%-12s %4d ok, %d FAIL, rc %d\n' "${name}" "${ok}" "${bad}" "${rc}"
    sed 's/^/        /' "${log}"
    failed=$((failed + 1))
  fi
done

if (( failed )); then
  echo "parity: ${failed} harness(es) failed"
  exit 1
fi
echo "parity: all ${#HARNESSES[@]} harnesses passed"

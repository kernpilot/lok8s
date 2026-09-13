#!/usr/bin/env bash
# parity.sh: run every parity harness (hack/parity-*.sh) against one lo
# binary and summarize: one line per harness with its ok/fail counts, the
# full log of every harness that failed, exit 1 if any did.
#
# Each harness is a differential run of the Go binary against the argsh
# passthrough (the project file's spec.implementation) over a synthetic project; see
# hack/lib/parity.sh for the shared machinery. The binary is resolved to an
# absolute path: every harness cd's into its synthetic project before
# invoking it.
#
# A harness that exits 0 without one "ok:" line fails the run too: a run
# that compared nothing (a wrong PATH, a preamble that skipped every case)
# proves nothing and used to look green.
#
# Usage: hack/parity.sh <path-to-lo-binary>   (default: bin/lo)
# PARITY_HARNESS_DIR and PARITY_HARNESSES (space-separated names) override
# the harness directory and list; the runner's own test uses them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ "${LO_BIN}" == /* ]] || LO_BIN="${PWD}/${LO_BIN}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

HARNESS_DIR="${PARITY_HARNESS_DIR:-${ROOT}/hack}"
# The CI order: the cheap, cluster-free surfaces first.
HARNESSES=(test build configure audit loop leaves ops kubehz operator orchestrate)
if [[ -n "${PARITY_HARNESSES:-}" ]]; then
  read -r -a HARNESSES <<<"${PARITY_HARNESSES}"
fi

LOGS="$(mktemp -d)"
trap 'rm -rf "${LOGS}"' EXIT

echo "parity: ${LO_BIN} ($("${LO_BIN}" --version 2>/dev/null | head -1))"
failed=0
for name in "${HARNESSES[@]}"; do
  log="${LOGS}/${name}.log"
  rc=0
  bash "${HARNESS_DIR}/parity-${name}.sh" "${LO_BIN}" >"${log}" 2>&1 || rc=$?
  ok="$(grep -c '^ok:' "${log}" || true)"
  bad="$(grep -c '^FAIL:' "${log}" || true)"
  if (( rc == 0 && ok > 0 )); then
    printf '  ok    parity-%-12s %4d ok\n' "${name}" "${ok}"
  elif (( rc == 0 )); then
    # rc is 0 and ok is 0 on this branch by construction: name the harness
    # and the fixed values, no placeholders.
    printf '  FAIL  parity-%-12s rc 0, no ok: line (the run compared nothing)\n' "${name}"
    sed 's/^/        /' "${log}"
    failed=$((failed + 1))
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

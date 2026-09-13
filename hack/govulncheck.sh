#!/usr/bin/env bash
# hack/govulncheck.sh — the vulnerability gate over every Go module in the
# tree, with a tracked allowlist for findings that have no fix yet.
#
# Runs four scans (root core, root -tags inprocess, kustomize/, ai/lochat/)
# and fails on any REACHABLE finding (a symbol-level trace, what govulncheck
# reports as "Your code is affected by") that is not listed in
# .github/govulncheck-ignore.json, or that is listed but past its review_by
# date. Every scan runs before the verdict, so one log shows everything.
#
# Needs govulncheck and jq on PATH. Usage: bash hack/govulncheck.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IGNORE="${ROOT}/.github/govulncheck-ignore.json"
[[ -f "${IGNORE}" ]] || { echo "error: ${IGNORE} missing" >&2; exit 2; }
command -v govulncheck >/dev/null || { echo "error: govulncheck not on PATH" >&2; exit 2; }
command -v jq >/dev/null || { echo "error: jq not on PATH" >&2; exit 2; }

# Every module is scanned with the ROOT go.mod toolchain — the one the
# release builds all three with (release.yml: setup-go, go-version-file:
# go.mod). Without the pin GOTOOLCHAIN=auto would scan kustomize/ (go 1.25)
# and ai/lochat/ (go 1.22) with whatever Go is local, and the stdlib
# verdict would describe a binary nobody ships.
GOTOOLCHAIN="go$(sed -n 's/^go //p' "${ROOT}/go.mod")"
export GOTOOLCHAIN
echo "toolchain: ${GOTOOLCHAIN}"

today="$(date -u +%F)"
# id → reason for entries still within their review window; expired ones
# are reported and fail below like any unlisted finding.
allowed="$(jq -r --arg today "${today}" \
  '.ignore[] | select(.review_by >= $today) | .id' "${IGNORE}")"
expired="$(jq -r --arg today "${today}" \
  '.ignore[] | select(.review_by < $today) | "\(.id) (review_by \(.review_by))"' "${IGNORE}")"

rc=0
scan() {
  local label="${1}" dir="${2}"; shift 2
  echo "::group::govulncheck ${label}"
  # The human-readable report first (its exit status is not the verdict).
  (cd "${dir}" && govulncheck "$@" ./...) || true
  # The verdict comes from the JSON stream: one finding per reachable
  # (osv, symbol) pair; module-level entries (empty function) are informational.
  local reachable
  reachable="$(cd "${dir}" && govulncheck -format json "$@" ./... 2>/dev/null \
    | jq -r 'select(.finding) | select((.finding.trace[0].function // "") != "") | .finding.osv' \
    | sort -u)"
  echo "::endgroup::"
  local id
  for id in ${reachable}; do
    if grep -qx "${id}" <<<"${allowed}"; then
      echo "allowed  ${label}: ${id} (listed in .github/govulncheck-ignore.json)"
    else
      echo "FAIL     ${label}: ${id} is reachable and not allowlisted"
      rc=1
    fi
  done
  [[ -n "${reachable}" ]] || echo "clean    ${label}"
}

scan "root (core)" "${ROOT}"
scan "root (-tags inprocess)" "${ROOT}" -tags inprocess
scan "kustomize" "${ROOT}/kustomize"
scan "ai/lochat" "${ROOT}/ai/lochat"

if [[ -n "${expired}" ]]; then
  echo "FAIL     allowlist entries past their review date:"
  while IFS= read -r line; do printf '           %s\n' "${line}"; done <<<"${expired}"
  rc=1
fi
exit "${rc}"

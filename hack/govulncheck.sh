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

# Every module is scanned with the ROOT go.mod toolchain. The release
# builds all three modules with that toolchain (release.yml: setup-go,
# go-version-file: go.mod). GOTOOLCHAIN=auto uses the local Go when it is
# the same as or newer than a module's `go` line. Then the stdlib verdict
# describes a binary that nobody ships. The pin makes all four scans use
# the Go version of the root `go` line.
GOTOOLCHAIN="go$(sed -n 's/^go //p' "${ROOT}/go.mod")"
export GOTOOLCHAIN
echo "toolchain: ${GOTOOLCHAIN}"

today="$(date -u +%F)"
# "<id> <module>" for entries still within their review window; expired
# ones are reported and fail below like any unlisted finding. The module
# is part of the match: one advisory can cover several modules (e.g.
# containerd v1 with no fix and containerd/v2 with one), and an entry
# accepts only the module it names.
allowed="$(jq -r --arg today "${today}" \
  '.ignore[] | select(.review_by >= $today) | "\(.id) \(.module)"' "${IGNORE}")"
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
  # trace[0] is the vulnerable symbol, so its module is the vulnerable one.
  local reachable
  reachable="$(cd "${dir}" && govulncheck -format json "$@" ./... 2>/dev/null \
    | jq -r 'select(.finding) | select((.finding.trace[0].function // "") != "") | "\(.finding.osv) \(.finding.trace[0].module)"' \
    | sort -u)"
  echo "::endgroup::"
  local id module
  while read -r id module; do
    [[ -n "${id}" ]] || continue
    if grep -qxF "${id} ${module}" <<<"${allowed}"; then
      echo "allowed  ${label}: ${id} in ${module} (listed in .github/govulncheck-ignore.json)"
    else
      echo "FAIL     ${label}: ${id} in ${module} is reachable and not allowlisted"
      rc=1
    fi
  done <<<"${reachable}"
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

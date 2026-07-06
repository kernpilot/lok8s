#!/usr/bin/env bash
# build-addons-index.sh — emit the machine-readable framework-addon index JSON
# on stdout:
#
#   {generatedAt, addons: [{name, chartVersion?, appVersion?, category?}, …]}
#
# Built from .lok8s/addons/* metadata (the khelm chart.yaml version pin + the
# lok8s.dev/category label) via the SAME helpers `lo addons` uses, so the
# published index can never drift from the CLI's own view. The Deploy Docs
# workflow writes it to docs/public/addons-index.json, so it is served at
# https://lok8s.io/addons-index.json — the kubehz platform polls it there to
# compute availableUpdates for ClusterInventory, without needing git access.
#
# DETERMINISTIC: addons are emitted in C-locale name order with a stable key
# order; generatedAt honors SOURCE_DATE_EPOCH, falls back to the last git
# commit touching .lok8s/addons (stable per commit), then to the wall clock.
#
# Needs only bash + yq (mikefarah v4) + jq — both preinstalled on GitHub
# ubuntu-24.04 runners (verified: runner-images Ubuntu2404-Readme lists
# jq 1.7 + yq 4.53.3).
set -euo pipefail
export LC_ALL=C

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ADDONS_DIR="${ROOT}/.lok8s/addons"

command -v yq >/dev/null 2>&1 || { echo "error: yq (mikefarah v4) is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "error: jq is required" >&2; exit 1; }
[[ -d "${ADDONS_DIR}" ]] || { echo "error: addons dir not found: ${ADDONS_DIR}" >&2; exit 1; }

# Reuse the CLI's own metadata extractors (addons::_version / addons::_category)
# instead of re-implementing them. The lib is an argsh module — stub `import`
# and source the verbose helpers it expects; its main:: guard does not fire on
# a plain source ($0 != the lib path, ARGSH_SOURCE unset here).
import() { :; }
# shellcheck source=/dev/null
source "${ROOT}/.lok8s/utils/verbose.sh"
# shellcheck source=/dev/null
source "${ROOT}/.lok8s/libs/addons"

_generated_at() {
  local epoch="${SOURCE_DATE_EPOCH:-}"
  if [[ -z "${epoch}" ]]; then
    # Stable per commit: the last commit touching the addons tree.
    epoch="$(git -C "${ROOT}" log -1 --format=%ct -- .lok8s/addons 2>/dev/null || true)"
  fi
  if [[ -n "${epoch}" ]]; then
    TZ=UTC printf '%(%Y-%m-%dT%H:%M:%SZ)T\n' "${epoch}"
  else
    date -u +%Y-%m-%dT%H:%M:%SZ
  fi
}

addons_json="[]"
for dir in "${ADDONS_DIR}"/*/; do
  [[ -d "${dir}" ]] || continue
  name="$(basename "${dir}")"
  chart_version="$(addons::_version "${dir}")"
  [[ "${chart_version}" != "-" ]] || chart_version=""
  category="$(addons::_category "${dir}")"
  [[ "${category}" != "-" ]] || category=""
  # appVersion: not tracked by the khelm chart.yaml pins today — the key is
  # part of the index contract and appears as soon as the pins carry it.
  addons_json="$(jq --arg n "${name}" --arg cv "${chart_version}" --arg cat "${category}" \
    '. += [({name: $n}
           + (if $cv  != "" then {chartVersion: $cv} else {} end)
           + (if $cat != "" then {category: $cat}    else {} end))]' \
    <<< "${addons_json}")"
done

jq -n --arg generatedAt "$(_generated_at)" --argjson addons "${addons_json}" \
  '{generatedAt: $generatedAt, addons: ($addons | sort_by(.name))}'

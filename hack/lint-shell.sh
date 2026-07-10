#!/usr/bin/env bash
# hack/lint-shell.sh [warning|braces|all] — the ONE shellcheck entrypoint.
# CI (.github/workflows/ci.yml) and `npm run lint` both call this, so the
# file discovery lives here only and the linted sets cannot drift.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# *.sh files (sourced libs often lack a shebang) UNION the extensionless
# argsh/bash scripts a -name filter alone would skip. Overlap is fine.
files() {
  {
    find .lok8s operator/hooks docs/.vitepress hack -type f -name '*.sh'
    grep -rlE '^#!/usr/bin/env (argsh|bash)' .lok8s operator/hooks docs/.vitepress hack
  } | sort -u
}

pass="${1:-all}"

if [[ "${pass}" == "warning" || "${pass}" == "all" ]]; then
  files | xargs -r shellcheck --shell=bash --severity=warning
fi

if [[ "${pass}" == "braces" || "${pass}" == "all" ]]; then
  # SC2250 is style-severity — the warning pass filters it out, so brace
  # enforcement needs its own allowlist pass. --include gates both the
  # report and the exit code to the listed check (also on in .shellcheckrc
  # for editor integration).
  files | xargs -r shellcheck --shell=bash --enable=require-variable-braces --include=SC2250
fi

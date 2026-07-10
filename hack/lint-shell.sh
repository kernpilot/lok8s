#!/usr/bin/env bash
# hack/lint-shell.sh — the ONE lint entrypoint: `argsh lint` runs shellcheck
# (honoring .shellcheckrc: SC2250 brace style + the repo-wide suppressions)
# PLUS argsh-lint (argsh idiom checks: inert :args hubs, flag/local
# mismatches, args-field shadowing, unresolved imports). CI and
# `npm run lint` both call this, so the file discovery lives here only and
# the linted sets cannot drift.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Without a local shellcheck + argsh-lint pair, argsh forwards the run to its
# container — pin the image so local runs and CI lint with identical tools
# (arg-sh/argsh 0.10.0 bundles shellcheck 0.11.0 + argsh-lint 0.1.0; b cannot
# yet install a second asset from one repo, so the container is the pin).
export ARGSH_DOCKER_IMAGE="${ARGSH_DOCKER_IMAGE:-ghcr.io/arg-sh/argsh:0.10.0}"

# *.sh files (sourced libs often lack a shebang) UNION the extensionless
# argsh/bash scripts a -name filter alone would skip. Overlap is fine.
{
  find .lok8s operator/hooks docs/.vitepress hack -type f -name '*.sh'
  grep -rlE '^#!/usr/bin/env (argsh|bash)' .lok8s operator/hooks docs/.vitepress hack
} | sort -u | xargs -r ./.bin/argsh lint

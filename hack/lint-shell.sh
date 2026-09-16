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
# container — DIGEST-pinned (tags are mutable; the container executes over the
# repo, so it gets the same supply-chain bar as a downloaded binary). The tag
# is a human-readable comment only; the digest is the pin. 0.10.0 bundles
# the shellcheck v0.11.0 + argsh-lint v0.1.0 pair — when bumping the vendored
# .bin/argsh, bump this digest with it. (b cannot yet install a second asset
# from one repo, so the container is how argsh-lint ships.)
# ghcr.io/arg-sh/argsh:0.10.0 =
export ARGSH_DOCKER_IMAGE="${ARGSH_DOCKER_IMAGE:-ghcr.io/arg-sh/argsh@sha256:99037ed056fe6271b2c020a78387e376c49d9700c23128d2fe6dbbc37b4a2c49}"

# *.sh files (sourced libs often lack a shebang) UNION the extensionless
# argsh/bash scripts a -name filter alone would skip. Overlap is fine.
# grep exits 1 on zero matches — tolerate exactly that (the find half may
# still yield files) while real errors (exit 2) kill the run with their
# original status.
#
# tests/ carries its own set and is linted too: the e2e harness drives
# docker, kind and `lo` against a machine with live clusters on it, which
# is the last shell in this repo that should go unchecked. Two exclusions
# there, both deliberate:
#
#   *.bats           bats syntax, not bash. `@test "..." {` is a parse
#                    error to shellcheck, so the suites are not linted as
#                    shell; their helpers (*.bash, *.sh) are.
#   */.lok8s         a per-run copy of the frozen tree that a routing e2e
#                    leg makes inside the scenario directory. It is
#                    gitignored, it is byte-identical to .lok8s/ which is
#                    already in the set above, and linting it would
#                    report every finding N times over.
SETS=(.lok8s .archive operator/hooks docs/.vitepress hack install tests)
{
  find "${SETS[@]}" -type f \( -name '*.sh' -o -name '*.bash' \) -print
  grep -rlE '^#!/usr/bin/env (argsh|bash)' --exclude='*.bats' "${SETS[@]}" \
    || { rc="${?}"; [[ "${rc}" -eq 1 ]] || exit "${rc}"; }
} | sort -u | grep -v '^tests/e2e/[^/]*/\.lok8s/' | xargs -r ./.bin/argsh lint

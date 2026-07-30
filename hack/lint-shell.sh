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
{
  find .lok8s operator/hooks docs/.vitepress hack install -type f -name '*.sh'
  grep -rlE '^#!/usr/bin/env (argsh|bash)' .lok8s operator/hooks docs/.vitepress hack install \
    || { rc="${?}"; [[ "${rc}" -eq 1 ]] || exit "${rc}"; }
} | sort -u | xargs -r ./.bin/argsh lint

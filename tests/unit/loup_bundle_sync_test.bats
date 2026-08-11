#!/usr/bin/env bats
# loup_bundle_sync_test.bats — the published installer must agree with its source.
#
# Why this exists
# ---------------
# `docs/public/lo-up` is a GENERATED artifact, committed to the repo and served
# to `curl … | sh` from https://lok8s.io/lo-up. `install/build` regenerates it
# from `install/lo-up`. Nothing in CI notices when the two disagree (issue #112,
# part 2): ci.yml's `e2e-lo-up` job runs `lo up`, which is a different thing
# entirely. So an edit to install/lo-up that is never rebuilt keeps shipping the
# OLD script to every new user, indefinitely and silently.
#
# The ideal gate is byte-exact — `install/build && git diff --exit-code` — and it
# is viable: the build is deterministic (verified, two runs, identical sha256).
# It is not what this file does, because the build needs the arg-sh/argsh
# checkout AND its `minifier`, which is a Rust crate in that repo. CI has
# neither, and a gate that skips in CI is the failure mode this repo keeps
# finding rather than a gate. See the PR for that follow-up.
#
# What this pins instead needs nothing but the two files, so it runs everywhere
# CI already runs bats: the required-binary list is the part of lo-up that
# actually drifted, and the minifier preserves those names because they are
# string literals in a `for bin in …` loop. If someone adds a binary to the
# source list and forgets to rebuild, this goes red.

setup() {
  load "../test_helper"
  BUNDLE="${_PROJECT_ROOT}/docs/public/lo-up"
  SOURCE="${_PROJECT_ROOT}/install/lo-up"
}

# The `for bin in argsh kubectl jq yq envsubst; do` line inside
# loup::verify_materialised — the authoritative list in the source.
_required_bins() {
  sed -n '/^loup::verify_materialised()/,/^}/p' "${SOURCE}" \
    | sed -n 's/^[[:space:]]*for bin in \(.*\); do$/\1/p'
}

@test "the source declares a non-empty required-binary list" {
  # ANTI-VACUITY. Every assertion below iterates this list; if the parse breaks
  # (the loop is reworded, the function renamed) they would all pass over an
  # empty set and this file would silently stop testing anything.
  local bins; bins="$(_required_bins)"
  [ -n "${bins}" ] || {
    echo "could not parse the 'for bin in …' list out of loup::verify_materialised" >&2
    echo "in ${SOURCE} — the parser below is stale, not the code." >&2
    return 1
  }
  local -a arr; read -r -a arr <<< "${bins}"
  [ "${#arr[@]}" -ge 5 ] || {
    echo "only ${#arr[@]} required binaries parsed (${bins}) — expected at least 5." >&2
    return 1
  }
}

@test "the published bundle is a real bundle, not a stub" {
  # Second anti-vacuity guard: a truncated or missing artifact would let the
  # grep-based assertions below fail for the wrong reason, or pass trivially if
  # they were ever loosened.
  [ -f "${BUNDLE}" ] || { echo "missing ${BUNDLE}" >&2; return 1; }
  local size; size=$(wc -c < "${BUNDLE}")
  [ "${size}" -gt 10000 ] || {
    echo "${BUNDLE} is only ${size} bytes — that is not the built installer." >&2
    return 1
  }
}

# The same list as it survives minification. The minifier renames the loop
# VARIABLE but not the string literals, so `for bin in argsh kubectl jq yq …`
# ships as `for a1 in argsh kubectl jq yq …` — the names are intact and
# comparable. Anchored on `in argsh` because argsh is always first. The class
# includes `-` so a future required binary like ssh-to-age (already in b.yaml's
# core group) doesn't truncate the capture and fail for the wrong reason.
_bundle_bins() {
  grep -om1 'in argsh[a-z0-9 -]*' "${BUNDLE}" | sed 's/^in //; s/ *$//'
}

@test "the published bundle's required-binary list matches the source exactly" {
  # THE gate. An earlier version of this test asked only whether each binary
  # appeared ANYWHERE in the 79KB bundle, and it could not fail: the argsh
  # runtime bundled inside carries its own "envsubst is required for -t" message,
  # so a stale bundle passed. Mutation is what exposed it — restoring the
  # pre-fix bundle left the test green. Compare the LISTS, not the substrings.
  local want; want="$(_required_bins)"
  local have; have="$(_bundle_bins)"

  [ -n "${have}" ] || {
    echo "could not find the required-binary list inside ${BUNDLE}." >&2
    echo "Either the bundle is not built from this source, or minification changed" >&2
    echo "shape and _bundle_bins needs updating — do not weaken this to a substring." >&2
    return 1
  }

  [ "${want}" = "${have}" ] || {
    echo "the published installer is STALE — its binary list disagrees with the source:" >&2
    echo "  install/lo-up      : ${want}" >&2
    echo "  docs/public/lo-up  : ${have}" >&2
    echo "Every 'curl | sh' user is being served the old script." >&2
    echo "Rebuild it:  ARGSH_SRC=/path/to/arg-sh/argsh install/build" >&2
    return 1
  }
}

@test "envsubst specifically is in the published installer's checked list" {
  # A correctness claim, not just a sync one: `b install` renders manifests
  # through envsubst (b.yaml's core group maps renvsubst→envsubst), so an install
  # missing it breaks on the first `lo use` while lo-up reports success.
  # libs/doctor already treats it as required. A verification step that passes on
  # a broken env is worse than none.
  #
  # Checked against the LIST, for the reason above — a bare grep for the word
  # matches the argsh runtime's own message and proves nothing.
  local have; have="$(_bundle_bins)"
  [[ " ${have} " == *" envsubst "* ]] || {
    echo "the published installer's checked list is [${have}] — no envsubst, so a" >&2
    echo "green install can still have broken templating (issue #112 part 1)." >&2
    return 1
  }
}

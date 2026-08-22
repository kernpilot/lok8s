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
# The byte-exact gate NOW EXISTS: the `loup-bundle` job in ci.yml checks
# arg-sh/argsh out at install/argsh.pin, downloads the pinned `minifier`
# release asset, runs install/build and diffs. What blocked it was not the
# toolchain — the minifier ships as a release asset and gettext is one apt
# package — but the fact that the bundle embeds the runtime's version and
# commit, so a rebuild from a different argsh revision differs for a reason
# that has nothing to do with lo-up. install/argsh.pin records that revision.
#
# This file is the half that needs NOTHING but the repo, so it runs everywhere
# bats runs, including on a machine that cannot build. It pins two things: the
# required-binary list (the part of lo-up that actually drifted), and the
# agreement between the pin file and the runtime baked into the bundle. The
# second is what keeps the pin honest — a rebuild from an unpinned argsh, or a
# pin bump without a rebuild, fails here even when the byte-exact job does not
# run.

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


# ── the pin agrees with what is actually in the bundle ────

_pin_value() {
  sed -n "s/^${1}=//p" "${_PROJECT_ROOT}/install/argsh.pin"
}

@test "install/argsh.pin records a runtime revision" {
  # ANTI-VACUITY for the two tests below: both compare against these values, so
  # an unparseable or empty pin would make them pass over nothing.
  local ref describe
  ref="$(_pin_value ref)"
  describe="$(_pin_value describe)"
  [[ "${ref}" =~ ^[0-9a-f]{40}$ ]] || {
    echo "install/argsh.pin has no 40-char ref (got '${ref}')" >&2
    return 1
  }
  [ -n "${describe}" ] || { echo "install/argsh.pin has no describe" >&2; return 1; }
}

@test "the published bundle carries the PINNED argsh runtime" {
  # The bundle embeds `ARGSH_VERSION="…"` from the argsh checkout install/build
  # ran against. If it disagrees with the pin, either the bundle was built from
  # an unpinned runtime or the pin was bumped without rebuilding — and the
  # byte-exact CI job would then fail for a reason nobody can reproduce.
  local want; want="$(_pin_value describe)"
  local have
  have="$(grep -om1 'ARGSH_VERSION="[^"$]*"' "${BUNDLE}" | sed 's/^ARGSH_VERSION="//; s/"$//')"

  [ -n "${have}" ] || {
    echo "no ARGSH_VERSION literal in ${BUNDLE} — the bundle is not built from" >&2
    echo "install/lo-up.min.tmpl, or the template no longer stamps it." >&2
    return 1
  }
  [ "${want}" = "${have}" ] || {
    echo "the published installer was built from a DIFFERENT argsh runtime:" >&2
    echo "  install/argsh.pin  : ${want}" >&2
    echo "  docs/public/lo-up  : ${have}" >&2
    echo "Rebuild:  ARGSH_SRC=/path/to/arg-sh/argsh install/build" >&2
    return 1
  }
}

@test "the bundle's argsh commit is the pinned one" {
  local want; want="$(_pin_value ref)"
  local have
  have="$(grep -om1 'ARGSH_COMMIT_SHA="[0-9a-f]*"' "${BUNDLE}" \
    | sed 's/^ARGSH_COMMIT_SHA="//; s/"$//')"

  [ -n "${have}" ] || { echo "no ARGSH_COMMIT_SHA literal in ${BUNDLE}" >&2; return 1; }
  [[ "${want}" == "${have}"* ]] || {
    echo "the published installer's argsh commit is not the pinned one:" >&2
    echo "  install/argsh.pin  : ${want}" >&2
    echo "  docs/public/lo-up  : ${have}" >&2
    return 1
  }
}

@test "install/build refuses to build from an unpinned argsh revision" {
  # The pin is only worth something if the build enforces it. Grepping the
  # source rather than running it: the build needs the argsh checkout and the
  # minifier, which is exactly what this file cannot assume.
  local build="${_PROJECT_ROOT}/install/build"
  grep -q 'argsh.pin' "${build}" || {
    echo "install/build no longer reads install/argsh.pin — the pin is now" >&2
    echo "documentation, not a constraint." >&2
    return 1
  }
  grep -q 'argsh revision does not match install/argsh.pin' "${build}" || {
    echo "install/build no longer FAILS on a pin mismatch." >&2
    return 1
  }
}

@test "CI rebuilds the bundle and diffs it" {
  # The other half. Without a workflow step, nothing rebuilds and the two tests
  # above only prove the bundle is self-consistent, not current.
  local ci="${_PROJECT_ROOT}/.github/workflows/ci.yml"
  grep -q 'install/build' "${ci}" || {
    echo "no CI step runs install/build — docs/public/lo-up can drift from" >&2
    echo "install/lo-up with nothing to notice (issue #112 part 2)." >&2
    return 1
  }
  grep -q 'git diff --exit-code -- docs/public/lo-up' "${ci}" || {
    echo "CI builds the bundle but never diffs it against the committed one." >&2
    return 1
  }
}

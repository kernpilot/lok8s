#!/usr/bin/env bats
# import_convention_test.bats — every argsh import in .lok8s uses the `^` prefix.
#
# Why this exists
# ---------------
# POST-REVIEW finding 8 (issue #2) reported "import prefix usage is inconsistent
# across libs (`^libs/...` vs relative)" and counted 76 prefixed against 47
# relative. Re-counting closed it: the bash tree is already uniform. All 139
# argsh imports use `^`, and the "relative" ones are TypeScript ESM imports in
# `.lok8s/libs/init.d/test/` — the Playwright scaffolding lok8s writes into a
# user's project. Those are a different language with a different module system
# and MUST stay relative.
#
# So the decision was to keep the convention and make it enforceable rather than
# rewrite 123 import lines. A mechanical rewrite carries real regression risk —
# `^` resolves against PATH_SCRIPTS while a bare path resolves against the
# importing file, and the two differ for anything sourced outside the
# `lo` entrypoint — for no functional gain. AGENTS.md states the rule; this
# test is what makes it hold.

setup() {
  load "../test_helper"
}

# Bash/argsh sources under .lok8s. init.d is excluded by name: it is scaffolding
# for a user's project, not framework code, and it is TypeScript.
_argsh_sources() {
  find "${_PROJECT_ROOT}/.lok8s" -type f \
    ! -path '*/init.d/*' \
    ! -name '*.yaml' ! -name '*.yml' ! -name '*.json' ! -name '*.ts' \
    -print0 \
    | xargs -0 grep -lE '^#!/usr/bin/env (argsh|bash)|^# shellcheck shell=bash' 2>/dev/null
}

@test "the framework really does use argsh imports" {
  # ANTI-VACUITY. The gate below asserts an ABSENCE; without this it would pass
  # just as happily over a tree with no imports at all, or a broken file sweep.
  local count
  count=$(_argsh_sources | xargs grep -c '^import \^' 2>/dev/null \
    | awk -F: '{ n += $NF } END { print n + 0 }')
  [ "${count}" -ge 100 ] || {
    echo "only ${count} '^'-prefixed imports found — the sweep is broken, not" >&2
    echo "the convention." >&2
    return 1
  }
}

@test "no argsh import omits the ^ prefix" {
  # `^` resolves against PATH_SCRIPTS; a bare path resolves against the
  # importing file. Both work from `lo`, which is why a mixed tree goes
  # unnoticed until something is sourced from elsewhere.
  local hits
  hits=$(_argsh_sources | xargs grep -nE '^[[:space:]]*import[[:space:]]+[^^[:space:]]' 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "these imports are not '^'-prefixed:" >&2
    echo "${hits}" >&2
    echo "The convention is 'import ^libs/foo' / 'import ^utils/bar' —" >&2
    echo "see AGENTS.md. (issue #2, finding 8)" >&2
    return 1
  }
}

@test "AGENTS.md states the convention this test enforces" {
  # A gate with no written rule is a trap: the next contributor learns the rule
  # from a red CI run instead of the guide.
  grep -q 'import \^' "${_PROJECT_ROOT}/AGENTS.md" || {
    echo "AGENTS.md no longer documents the '^' import prefix." >&2
    return 1
  }
}

#!/usr/bin/env bash
# tests/lib/bats_libs.bash — locate and load bats-support/bats-assert.
#
# The ONE loader shared by tests/test_helper.bash (unit/operator) and
# tests/e2e/lib/helpers.bash, so both suites resolve libraries identically
# and worktree fixes cannot drift between them. The sourcing file must set
# _PROJECT_ROOT before calling _load_bats_libs.
#
# No vendored submodules — `argsh test` provides bats + bats-support +
# bats-assert; system installs and the argsh Docker image are also honored.
_load_bats_libs() {
  # Candidate directories include the standard locations where argsh's
  # Docker image (and system installs) place bats-support/bats-assert…
  #
  # …plus the MAIN working tree's .bin/lib, which is what makes the suite
  # runnable from a `git worktree`. The trap is that .bin/ *is* checked out
  # there — it holds tracked b.yaml/b.lock — while .bin/lib is installed at
  # runtime by `b install` and gitignored, so it exists only in the main tree.
  # ${_PROJECT_ROOT}/.bin/lib therefore resolves to a real directory that is
  # missing the libraries, and every test dies in setup() with
  # "Could not find library 'bats-support'" — 1156 failures, 0 passes, which
  # reads like a catastrophically broken branch rather than a missing path
  # (AUDIT.md r830/r834/r835 lost time to exactly this three times).
  #
  # `--git-common-dir` is the resolver: in a worktree it points at the main
  # repo's .git, in a normal checkout at ./.git, so the same expression covers
  # both and the loops below simply skip it when it holds no bats-support.
  local main_root="" common
  if common="$(git -C "${_PROJECT_ROOT}" rev-parse --git-common-dir 2>/dev/null)"; then
    [[ "${common}" == /* ]] || common="${_PROJECT_ROOT}/${common}"
    main_root="$(cd "${common}/.." 2>/dev/null && pwd)" || main_root=""
  fi

  # One candidate list feeds both the BATS_LIB_PATH loop and the bats<1.5
  # fallback, so the two can never disagree. The main-root entry only means
  # anything from a linked worktree — skip it when it repeats the project
  # root (normal checkout).
  local -a lib_dirs=(
    /usr/lib /usr/local/lib "${HOME}/.local/lib" /opt/homebrew/lib
    "${_PROJECT_ROOT}/.bin/lib"
  )
  if [[ -n "${main_root}" && "${main_root}" != "${_PROJECT_ROOT}" ]]; then
    lib_dirs+=("${main_root}/.bin/lib")
  fi

  local d
  for d in "${lib_dirs[@]}"; do
    [[ -d "${d}/bats-support" ]] || continue
    [[ ":${BATS_LIB_PATH:-}:" == *":${d}:"* ]] || BATS_LIB_PATH="${BATS_LIB_PATH:+${BATS_LIB_PATH}:}${d}"
  done
  export BATS_LIB_PATH

  # bats_load_library (bats >= 1.5)
  if declare -F bats_load_library &>/dev/null; then
    bats_load_library bats-support
    bats_load_library bats-assert
    return 0
  fi
  # Direct load fallback (bats < 1.5) — same candidate list as above.
  for d in "${lib_dirs[@]}"; do
    if [[ -f "${d}/bats-support/load.bash" ]] && [[ -f "${d}/bats-assert/load.bash" ]]; then
      load "${d}/bats-support/load.bash"
      load "${d}/bats-assert/load.bash"
      return 0
    fi
  done
  echo "error: bats-support/bats-assert not found. Run tests via: argsh test" >&2
  return 1
}

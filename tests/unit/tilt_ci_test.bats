#!/usr/bin/env bats
# tilt_ci_test.bats — unit tests for the headless bring-up path.
#
# `lo up` (interactive) backgrounds `tilt up` via tilt::up, which returns
# immediately and exits non-zero in a non-TTY context even though Tilt
# started — no scriptable "build + deploy + wait-ready + real exit status".
# `lo up --ci` (and `lo tilt ci`) instead run `tilt ci` in the foreground:
# it builds + deploys all resources, waits for readiness, then exits 0 on
# success / non-zero on failure. These tests pin the exact invocation.
#
# `tilt` is stubbed via a PATH shim (NOT a bash function) so it is also
# honored through tilt::up's `nohup tilt ...` (nohup execs the real binary,
# bypassing shell functions). The shim:
#   - answers `tilt doctor` with "Env: kind" so the kind-env guard passes,
#   - records every other invocation's argv (one line per call) to $TILT_CAP.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"

  # argsh's `:args`/`:usage` builtins (from argsh.so) are NOT loaded when a
  # lib is `source`d in a bats context — every unit test in this suite
  # bypasses them (cf. build_test.bats drives build::targets, not the
  # `:args`-gated main::build). To exercise tilt::ci's post-parse body
  # (the `--timeout` passthrough) we install a minimal `:args` shim that
  # reproduces what real argsh does for the one flag that matters here:
  # it populates `timeout` from --timeout <v> | --timeout=<v> | -t <v>.
  # (Verified against the real argsh runtime: `'timeout|t'` parses all
  # three forms identically.)
  :args() {
    shift  # drop the description (first positional)
    while (( $# )); do
      case "$1" in
        --timeout=*|--timeout|-t)
          if [[ "$1" == *=* ]]; then timeout="${1#*=}"; else timeout="${2:-}"; shift; fi
          ;;
      esac
      shift
    done
  }
  export -f :args

  source "${_PROJECT_ROOT}/.lok8s/libs/tilt"

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}"
  printf 'Tiltfile\n' > "${PATH_BASE}/Tiltfile"

  # Deterministic port — bypass tilt::port's domain hashing.
  export TILT_PORT=14242

  # PATH shim for `tilt` (records argv; survives nohup exec).
  export TILT_CAP="${BATS_TEST_TMPDIR}/tilt.cap"
  : > "${TILT_CAP}"
  local shim="${BATS_TEST_TMPDIR}/shimbin"
  mkdir -p "${shim}"
  cat > "${shim}/tilt" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == doctor ]]; then echo "Env: kind"; exit 0; fi
# Liveness probe (tilt::running): the apiserver is "down" unless TILT_RUNNING
# is set. NOT recorded to TILT_CAP — it's a probe, not an action, so action
# assertions (up/ci/trigger) stay clean.
if [[ "$1" == get && "$2" == session ]]; then
  [[ -n "${TILT_RUNNING:-}" ]] && exit 0
  echo "Error: No tilt apiserver found: tilt-${TILT_PORT:-?}" >&2; exit 1
fi
printf '%s\n' "$*" >> "${TILT_CAP}"
exit 0
SH
  chmod +x "${shim}/tilt"
  PATH="${shim}:${PATH}"
}

teardown() {
  teardown_tmpdir
}

# Wait briefly for tilt::up's backgrounded `nohup tilt up &` to write a line.
_wait_for_capture() {
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    [[ -s "${TILT_CAP}" ]] && return 0
    sleep 0.1
  done
  return 1
}

# ── tilt::ci — the headless invocation ───────────────────

@test "tilt::ci runs 'tilt ci' (not 'tilt up') with --file and --port" {
  run tilt::ci
  assert_success

  run cat "${TILT_CAP}"
  # Must invoke the `ci` subcommand, foreground, with the standard flags.
  assert_output --partial "ci "
  assert_output --partial "--port=14242"
  assert_output --partial "--file=${PATH_BASE}/Tiltfile"
  # Headless must NOT shell out to interactive `tilt up`.
  refute_output --partial "up "
}

@test "tilt::ci passes --timeout through to 'tilt ci' when given" {
  run tilt::ci --timeout 90s
  assert_success

  run cat "${TILT_CAP}"
  assert_output --partial "ci "
  assert_output --partial "--timeout 90s"
}

@test "tilt::ci omits --timeout when none is given" {
  run tilt::ci
  assert_success

  run cat "${TILT_CAP}"
  refute_output --partial "--timeout"
}

@test "tilt::ci returns the exit status of 'tilt ci' (non-zero on failure)" {
  # Re-point the shim so `tilt ci` fails (readiness/build failure).
  local shim="${BATS_TEST_TMPDIR}/shimbin"
  cat > "${shim}/tilt" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == doctor ]]; then echo "Env: kind"; exit 0; fi
printf '%s\n' "$*" >> "${TILT_CAP}"
exit 7
SH
  chmod +x "${shim}/tilt"

  run tilt::ci
  assert_failure 7
}

# ── tilt::up — the interactive invocation (default, unchanged) ──

@test "tilt::up backgrounds 'tilt up' (not 'tilt ci')" {
  run tilt::up
  assert_success

  _wait_for_capture
  run cat "${TILT_CAP}"
  assert_output --partial "up "
  assert_output --partial "--port=14242"
  assert_output --partial "--file=${PATH_BASE}/Tiltfile"
  refute_output --partial "ci "
}

# ── tilt::up — reload an already-running instance instead of duplicating ──

@test "tilt::running reflects the apiserver: down → false, up → true" {
  run tilt::running 14242
  assert_failure                       # shim: no apiserver by default
  export TILT_RUNNING=1
  run tilt::running 14242
  assert_success
}

@test "tilt::up reloads the Tiltfile (no duplicate 'tilt up') when Tilt is already running" {
  export TILT_RUNNING=1                # apiserver answers → already up
  run tilt::up
  assert_success
  assert_output --partial "reloading Tiltfile"

  run cat "${TILT_CAP}"
  assert_output --partial "trigger (Tiltfile)"
  assert_output --partial "--port 14242"
  refute_output --partial "up --port"   # did NOT background a second instance
}

@test "tilt::reload triggers the (Tiltfile) resource on the given port" {
  run tilt::reload 14242
  assert_success
  run cat "${TILT_CAP}"
  assert_output --partial "trigger (Tiltfile) --port 14242"
}

# ── main::up dispatch — --ci routes to tilt ci, default to tilt up ──
#
# main::up lives in .lok8s/lo, which executes main() at source time (the
# standalone guard fires under bash `source`), so it can't be sourced in
# isolation the way a lib can. We exercise the *real* tilt::ci / tilt::up
# (sourced above, containing the literal `tilt ci` / `tilt up` calls) and
# drive them through a faithful reproduction of main::up's post-reconcile
# dispatch: reconcile always runs; --ci → tilt::ci, else → tilt::up.

# Mirror of main::up's branch (kept in lockstep with .lok8s/lo main::up).
_main_up() {
  local ci="${1:-0}" timeout="${2:-}"
  local reconciled=0
  provision::dispatch() { reconciled=1; }   # stub the infra step
  provision::dispatch "test.lok8s.dev"

  if (( ci )); then
    if [[ -n "${timeout}" ]]; then
      tilt::ci --timeout "${timeout}"
    else
      tilt::ci
    fi
  else
    tilt::up
  fi
  echo "reconciled=${reconciled}"
}

@test "main::up --ci dispatches to 'tilt ci' after reconcile" {
  run _main_up 1
  assert_success
  assert_output --partial "reconciled=1"   # infra reconcile still ran

  run cat "${TILT_CAP}"
  assert_output --partial "ci "
  refute_output --partial "up "
}

@test "main::up without --ci dispatches to 'tilt up' after reconcile" {
  run _main_up 0
  assert_success
  assert_output --partial "reconciled=1"

  _wait_for_capture
  run cat "${TILT_CAP}"
  assert_output --partial "up "
  refute_output --partial "ci "
}

@test "main::up --ci --timeout forwards the timeout to 'tilt ci'" {
  run _main_up 1 120s
  assert_success

  run cat "${TILT_CAP}"
  assert_output --partial "ci "
  assert_output --partial "--timeout 120s"
}

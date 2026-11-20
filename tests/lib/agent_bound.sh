# shellcheck shell=sh
# agent_bound.sh — the pod's `timeout` for a test run of the heartbeat
# script, and a record of each kubectl and curl call with its bound.
#
# A test prepends this file to the extracted script (the runner is POSIX
# sh). The kubectl and curl stubs of the test call stub_call first. The
# assertion is assert_agent_calls_bounded (tests/lib/agent_bound.bash).
#
#   STUB_TIMEOUT_FORM_LOG  each timeout call in another form than
#                          `timeout -k 2 SECS` (5, 10, 15 or 30)
#   STUB_CALL_LOG          one line per kubectl and curl call: "bound ..."
#                          under the timeout stub, "UNBOUND ..." without it

# timeout: the pod's `timeout -k 2 SECS CMD ARGS` (the BusyBox applet and
# GNU coreutils take the same form). It runs the command as a shell
# function, so the stub of the test answers it and no real tool runs. Any
# other form, such as a call without the kill-after, goes to
# STUB_TIMEOUT_FORM_LOG and fails (rc 125), so it does not pass unseen.
timeout() {
  case "$1:$2:$3" in
    -k:2:5|-k:2:10|-k:2:15|-k:2:30) ;;
    *)
      printf '%s\n' "$*" >> "${STUB_TIMEOUT_FORM_LOG}"
      echo "stub: timeout without -k 2: $*" >&2
      return 125 ;;
  esac
  shift 3
  # POSIX sh has no `local`: save STUB_BOUND, set it for the call, put it
  # back. stub_call reads it with ${STUB_BOUND:-}, so an empty value is
  # the same as no value.
  _stub_bound_was="${STUB_BOUND:-}"
  STUB_BOUND=1
  "$@"
  _stub_rc=$?
  STUB_BOUND="${_stub_bound_was}"
  return "${_stub_rc}"
}

# stub_call TOOL ARGS: one line in STUB_CALL_LOG. STUB_BOUND is set only
# while the timeout stub runs the call, so a call that the script starts
# without the bound is "UNBOUND". The arguments hold no secret: the script
# gives secrets on stdin.
stub_call() {
  if [ -n "${STUB_BOUND:-}" ]; then
    printf 'bound %s\n' "$*" >> "${STUB_CALL_LOG}"
  else
    printf 'UNBOUND %s\n' "$*" >> "${STUB_CALL_LOG}"
  fi
}

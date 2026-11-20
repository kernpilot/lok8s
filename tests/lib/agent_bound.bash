# shellcheck shell=bash
# agent_bound.bash — the assertion for tests/lib/agent_bound.sh.

# assert_agent_calls_bounded: each kubectl and curl call of the runs so far
# ran under the timeout stub, in the `timeout -k 2 SECS` form. It fails on
# a call without the bound, on a timeout call in another form, and on a
# call log without a kubectl or a curl call: an empty log proves nothing.
assert_agent_calls_bounded() {
  if [[ -s "${STUB_TIMEOUT_FORM_LOG}" ]]; then
    echo "timeout calls in another form than 'timeout -k 2 SECS':" >&2
    cat "${STUB_TIMEOUT_FORM_LOG}" >&2
    return 1
  fi
  if grep -q '^UNBOUND ' "${STUB_CALL_LOG}" 2>/dev/null; then
    echo "calls without the bound:" >&2
    grep '^UNBOUND ' "${STUB_CALL_LOG}" >&2
    return 1
  fi
  if ! grep -q '^bound kubectl ' "${STUB_CALL_LOG}" 2>/dev/null \
    || ! grep -q '^bound curl ' "${STUB_CALL_LOG}" 2>/dev/null; then
    echo "the call log (${STUB_CALL_LOG}) has no kubectl call or no curl call" >&2
    return 1
  fi
}

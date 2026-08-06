#!/usr/bin/env bats
# secret_material_containment_test.bats — a tool that prints key material on
# failure must never have its stderr reach a terminal or a CI log.
#
# Why this exists
# ---------------
# `ssh-to-age` echoes the ENTIRE input file in its error message:
#
#     ssh-to-age: failed to convert '-----BEGIN OPENSSH PRIVATE KEY-----
#     <the whole key>
#
# Verified against the pinned binary, not assumed (2026-08-06): feeding it a file
# containing a canary string and capturing stderr reproduces the canary verbatim.
# That is how an SSH private key blob reached session transcripts on 2026-06-26
# and 2026-07-12/13 (FRICTION 2026-07-15; scrubbed, and the key was
# passphrase-encrypted).
#
# lok8s already gets this right — `secrets::decrypt` redirects that stderr — and
# the same measurement proves the redirect is what does it: with `2>/dev/null`
# the canary appears 0 times in the output, without it, once. Nothing enforced
# it, though. A future call site that drops the redirect reintroduces a silent
# secret leak, and the failure looks like an ordinary error message.
#
# So this pins the property rather than the fix: every invocation that feeds a
# PRIVATE key to ssh-to-age must contain its stderr. The public-key derivation is
# deliberately exempt — leaking a public key is not a leak.

setup() {
  load "../test_helper"
  LOK8S_DIR="${_PROJECT_ROOT}/.lok8s"
}

# Real invocations only: not comments, not `command -v` probes, and not the
# error text that TELLS a user which command to run by hand.
_private_key_invocations() {
  grep -rn 'ssh-to-age' "${LOK8S_DIR}" 2>/dev/null \
    | grep -- '-private-key' \
    | grep -v '^\s*#' \
    | grep -vE ':[0-9]+:\s*#' \
    | grep -v 'command -v' \
    | grep -v 'error ' \
    || true
}

@test "every ssh-to-age private-key invocation contains its stderr" {
  local found=0 leaky=()
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    found=$((found + 1))
    # Containment = the tool's stderr does not reach the caller's stderr.
    # `2>/dev/null` and `2>&1` into a captured substitution both qualify.
    if [[ ! "${line}" =~ 2\>/dev/null ]] && [[ ! "${line}" =~ 2\>\&1 ]]; then
      leaky+=("${line}")
    fi
  done < <(_private_key_invocations)

  # A pattern that matches nothing must fail, not pass: if the call site is
  # renamed or moved, this test would otherwise report "all clear" over an empty
  # set — which is exactly how a guard stops guarding.
  [ "${found}" -ge 1 ] || {
    echo "no ssh-to-age -private-key invocation found under ${LOK8S_DIR}" >&2
    echo "the call site moved or was renamed; this test is no longer watching anything" >&2
    return 1
  }

  [ "${#leaky[@]}" -eq 0 ] || {
    echo "ssh-to-age -private-key WITHOUT stderr containment — it prints the whole" >&2
    echo "key file on failure, so this leaks the private key into the terminal/CI log:" >&2
    printf '  %s\n' "${leaky[@]}" >&2
    return 1
  }
}

@test "the containment is real: the tool does leak when stderr is not redirected" {
  # The test above asserts a shape. This one proves the shape MATTERS, against
  # the actual binary — otherwise it is a style rule dressed as a security check.
  command -v ssh-to-age >/dev/null 2>&1 || skip "ssh-to-age not on PATH"

  local key="${BATS_TEST_TMPDIR}/fake_key"
  cat > "${key}" <<'EOF'
-----BEGIN OPENSSH PRIVATE KEY-----
CANARY_SECRET_MATERIAL_DO_NOT_LEAK
-----END OPENSSH PRIVATE KEY-----
EOF

  # Unredirected: the canary reaches the caller.
  local unguarded
  unguarded="$(ssh-to-age -private-key < "${key}" 2>&1 || true)"
  [[ "${unguarded}" == *CANARY_SECRET_MATERIAL* ]] || {
    echo "ssh-to-age no longer echoes its input on failure — the containment rule" >&2
    echo "above may be obsolete, or this binary changed. Re-check before relaxing it." >&2
    return 1
  }

  # Redirected exactly as lok8s does it: nothing escapes.
  local guarded
  guarded="$( { ssh-to-age -private-key < "${key}" 2>/dev/null || true; } 2>&1 )"
  [[ "${guarded}" != *CANARY_SECRET_MATERIAL* ]] || {
    echo "the 2>/dev/null redirect did NOT contain the key material" >&2
    return 1
  }
}

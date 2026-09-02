#!/usr/bin/env bats
# loup_verify_test.bats — unit tests for .lok8s/legacy/install/lo-up's post-step verification
#
# lo-up used to trust `b`'s exit code. `b` has shipped more than one silent
# rc-0 no-op (fentas/b#180, #184), each of which left a fresh machine with an
# "installed" env containing nothing — the failure only surfaced much later as
# a missing binary. These tests pin the two guards that check the artefacts.

setup() {
  load "../test_helper"
  setup_tmpdir

  # lo-up is an argsh script; source only the pieces under test. `:args` and
  # the argsh runtime are not needed because main() is never called.
  _say()  { printf '%s\n' "${*}" >&2; }
  _info() { _say "• ${*}"; }
  _ok()   { _say "✓ ${*}"; }
  _warn() { _say "! ${*}"; }
  _die()  { _say "✗ ${*}"; exit 1; }

  # The functions read these from main()'s scope.
  LOUP_REPO="github.com/kernpilot/lok8s"
  profile="local"
  git_ref="main"
  PATH_BIN="${BATS_TEST_TMPDIR}/.bin"
  mkdir -p "${PATH_BIN}"

  # Pull in the two functions without executing the script.
  eval "$(sed -n '/^loup::bin_dir()/,/^}/p;/^loup::verify_registered()/,/^}/p;/^loup::verify_materialised()/,/^}/p' \
    "${_PROJECT_ROOT}/.lok8s/legacy/install/lo-up")"

  cd "${BATS_TEST_TMPDIR}" || return 1
}

# --- loup::verify_registered ---

@test "verify_registered fails when b.yaml was never created" {
  run loup::verify_registered
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"never registered"* ]]
}

@test "verify_registered fails when b.yaml has no lok8s env (the rc-0 no-op)" {
  mkdir -p .bin
  printf 'binaries: {}\n' > .bin/b.yaml
  run loup::verify_registered
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"registered no lok8s env"* ]]
  # The message must tell the user how to retry.
  [[ "${output}" == *"b env add github.com/kernpilot/lok8s#local"* ]]
}

@test "verify_registered passes once the env is recorded" {
  mkdir -p .bin
  printf 'envs:\n  git@github.com:kernpilot/lok8s#local: {}\n' > .bin/b.yaml
  run loup::verify_registered
  [ "${status}" -eq 0 ]
}

# --- loup::verify_materialised ---

# A core binary already on PATH satisfies the check by design (users may have
# kubectl/jq system-wide), so the missing-binary cases must run with a PATH
# that holds nothing but coreutils.
bare_path() {
  PATH="/usr/bin:/bin" "${@}"
}

# NOTE: /usr/bin commonly HAS jq and yq, so bare_path does not hide them. Only
# argsh is reliably absent from a system PATH — use it for missing-binary cases.

materialise() {
  mkdir -p .lok8s
  printf '#!/bin/sh\n' > .lok8s/lo
  local bin
  for bin in argsh kubectl jq yq envsubst; do
    printf '#!/bin/sh\n' > "${PATH_BIN}/${bin}"
    chmod +x "${PATH_BIN}/${bin}"
  done
}

@test "verify_materialised fails when the framework was not vendored" {
  materialise
  rm -rf .lok8s
  run loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"vendored framework"* ]]
}

@test "verify_materialised fails when the lo entrypoint is missing" {
  materialise
  rm -f .lok8s/lo
  run loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *".lok8s/lo"* ]]
}

@test "verify_materialised fails when a core binary is missing" {
  materialise
  rm -f "${PATH_BIN}/argsh"
  run bare_path loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"argsh"* ]]
}

@test "the missing-binary line survives a trailing slash on PATH_BIN" {
  # `${PATH_BIN##*/}` on ".../.bin/" is EMPTY, so the line read "/argsh"
  # (issue #112 part 4). The basename is computed in one place now —
  # loup::bin_dir — and both display sites call it.
  materialise
  rm -f "${PATH_BIN}/argsh"
  PATH_BIN="${PATH_BIN}/"
  run bare_path loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *".bin/argsh"* ]] || {
    echo "expected '.bin/argsh' in the message, got:" >&2
    echo "${output}" >&2
    return 1
  }
}

@test ".lok8s/legacy/install/lo-up has ONE basename-of-PATH_BIN expression" {
  # The drift gate for part 4. Two display sites carried the same expression
  # and only one of them was ever fixed.
  # loup::bin_dir strips the trailing slash before the basename, so any raw
  # ${PATH_BIN##*/} left in the file is a second copy with the old bug.
  # Comment lines are excluded — the fix is explained in one.
  local hits
  hits=$(grep -n '\${PATH_BIN##\*/}' "${_PROJECT_ROOT}/.lok8s/legacy/install/lo-up" \
    | grep -v '^[0-9]*:#' || true)
  [ -z "${hits}" ] || {
    echo "a raw \${PATH_BIN##*/} is back — it renders empty on a trailing slash:" >&2
    echo "${hits}" >&2
    echo "Call loup::bin_dir instead." >&2
    return 1
  }
  grep -q '^loup::bin_dir()' "${_PROJECT_ROOT}/.lok8s/legacy/install/lo-up" || {
    echo "loup::bin_dir is gone — this gate is measuring nothing." >&2
    return 1
  }
}

@test "verify_materialised reports every missing piece at once" {
  materialise
  rm -rf .lok8s
  # argsh, not jq/yq: those are commonly installed system-wide, which the
  # ambient-PATH fallback would (correctly) accept even under bare_path.
  rm -f "${PATH_BIN}/argsh"
  run bare_path loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"vendored framework"* ]]
  [[ "${output}" == *".lok8s/lo"* ]]
  [[ "${output}" == *"argsh"* ]]
}

@test "verify_materialised passes on a complete env" {
  materialise
  run loup::verify_materialised
  [ "${status}" -eq 0 ]
}

@test "verify_materialised accepts a core binary already on PATH" {
  materialise
  rm -f "${PATH_BIN}/kubectl"
  local stub="${BATS_TEST_TMPDIR}/onpath"
  mkdir -p "${stub}"
  printf '#!/bin/sh\n' > "${stub}/kubectl"
  chmod +x "${stub}/kubectl"
  PATH="${stub}:${PATH}" run loup::verify_materialised
  [ "${status}" -eq 0 ]
}

# --- wiring ---
#
# The tests above source the functions in isolation, so they stay green even if
# main() stops calling them. Pin the call sites too.

@test "lo-up calls verify_registered outside the bootstrap branch" {
  local src="${_PROJECT_ROOT}/.lok8s/legacy/install/lo-up"
  grep -q '^  loup::verify_registered$' "${src}" \
    || fail "loup::verify_registered is not called at main()'s top level — an env with a vendored .lok8s but no b.yaml entry would report ready"
}

@test "lo-up calls verify_materialised after b install" {
  local src="${_PROJECT_ROOT}/.lok8s/legacy/install/lo-up"
  grep -A 1 '^  b install' "${src}" | grep -q 'loup::verify_materialised' \
    || fail "loup::verify_materialised does not follow 'b install' — a no-op install would report ready"
}

@test "the published bundle is in sync with .lok8s/legacy/install/lo-up" {
  # docs/public/lo-up is a generated artifact served to `curl … | sh`. A stale
  # bundle means the guards exist in the repo but not in what users run.
  local bundle="${_PROJECT_ROOT}/docs/public/lo-up"
  [ -s "${bundle}" ] || fail "docs/public/lo-up is missing or empty"
  grep -q 'registered no lok8s env' "${bundle}" \
    || fail "docs/public/lo-up predates the verification guards — re-run .lok8s/legacy/install/build"
  grep -q '</dev/tty' "${bundle}" \
    || fail "docs/public/lo-up lost the </dev/tty redirect — the confirm prompt would silently auto-yes"
}

# --- behaviour of the PUBLISHED bundle ---
#
# The greps above pin the source; these drive the artifact users actually run.
# That catches what source-level assertions cannot: a minifier that renames a
# `:args` destination leaves the spec string (and so `--help`) untouched while
# the flag silently stops working.

_stub_project() {
  local dir="${1}"
  mkdir -p "${dir}/.bin"
  cat > "${dir}/.bin/b" <<'STUB'
#!/bin/sh
echo "${@}" >> "${B_PROBE_LOG}"
exit 0
STUB
  chmod +x "${dir}/.bin/b"
  printf 'binaries: {}\n' > "${dir}/.bin/b.yaml"
}

@test "bundle: --profile and --git-ref reach 'b env add'" {
  local dir="${BATS_TEST_TMPDIR}/proj"
  _stub_project "${dir}"
  B_PROBE_LOG="${dir}/calls" PATH="${dir}/.bin:${PATH}" LOUP_URL="file:///dev/null" \
    run bash "${_PROJECT_ROOT}/docs/public/lo-up" -y --profile kustomize --git-ref v9.9.9 --dir "${dir}"
  # Exits at the verification guard — the stub installs nothing. The log is the
  # signal: the flag values had to survive minification to get there.
  run cat "${dir}/calls"
  assert_output --partial "#kustomize"
  assert_output --partial "v9.9.9"
}

@test "bundle: a b that exits 0 without doing anything does NOT report a ready env" {
  local dir="${BATS_TEST_TMPDIR}/proj"
  _stub_project "${dir}"
  B_PROBE_LOG="${dir}/calls" PATH="${dir}/.bin:${PATH}" LOUP_URL="file:///dev/null" \
    run bash "${_PROJECT_ROOT}/docs/public/lo-up" -y --dir "${dir}"
  assert_failure
  refute_output --partial "lok8s env ready"
  assert_output --partial "registered no lok8s env"
}

@test "bundle: LOUP_REPO override is not told to re-run the command that just ran" {
  local dir="${BATS_TEST_TMPDIR}/proj"
  _stub_project "${dir}"
  # A fork registers under its own org; the guard must match on LOUP_REPO, not a
  # hardcoded kernpilot/lok8s.
  printf 'envs:\n  git@github.com:acme/lok8s#local: {}\n' > "${dir}/.bin/b.yaml"
  B_PROBE_LOG="${dir}/calls" PATH="${dir}/.bin:${PATH}" LOUP_URL="file:///dev/null" \
    LOUP_REPO="github.com/acme/lok8s" \
    run bash "${_PROJECT_ROOT}/docs/public/lo-up" -y --dir "${dir}"
  refute_output --partial "registered no lok8s env"
}

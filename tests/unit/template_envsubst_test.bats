#!/usr/bin/env bats
# template_envsubst_test.bats — the envsubst flavor shim (template::envsubst)
#
# lok8s restricts substitution to explicit variable lists. GNU gettext
# envsubst takes the list as a SHELL-FORMAT positional arg; renvsubst
# (b's curated envsubst) rejects positional args entirely and wants
# --variable filters. The shim must translate transparently — these tests
# pin both dialects with fake flavor binaries, plus the real system GNU
# envsubst when available.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"

  # Fake binaries directory, prepended per-test.
  FAKE_BIN="${BATS_TEST_TMPDIR}/bin"
  mkdir -p "${FAKE_BIN}"
}

# A fake GNU gettext envsubst: identifies as GNU and records how it was
# invoked; substitution itself is out of scope for the dispatch tests.
_fake_gnu() {
  cat > "${FAKE_BIN}/envsubst" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "--version" ]]; then
  echo "envsubst (GNU gettext-runtime) 0.22"
  exit 0
fi
printf '%s\n' "$@" > "${ARGS_OUT}"
cat > /dev/null
echo "gnu-called"
SH
  chmod +x "${FAKE_BIN}/envsubst"
  export ARGS_OUT="${BATS_TEST_TMPDIR}/args"
  PATH="${FAKE_BIN}:${PATH}"
  _TEMPLATE_ENVSUBST_FLAVOR=""
}

# A fake renvsubst: real renvsubst prints its bare version for --version
# (no "GNU gettext" marker) and hard-rejects positional args — the exact
# behavior that broke `lo build` when b aliased it onto envsubst.
_fake_renvsubst() {
  cat > "${FAKE_BIN}/envsubst" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "--version" ]]; then
  echo "0.10.0"
  exit 0
fi
for a in "$@"; do
  [[ "${a}" == --variable=* || "${a}" == --prefix=* || "${a}" == --suffix=* ]] || {
    echo "ERROR: Unknown flag: ${a}" >&2
    exit 1
  }
done
printf '%s\n' "$@" > "${ARGS_OUT}"
cat > /dev/null
echo "renvsubst-called"
SH
  chmod +x "${FAKE_BIN}/envsubst"
  export ARGS_OUT="${BATS_TEST_TMPDIR}/args"
  PATH="${FAKE_BIN}:${PATH}"
  _TEMPLATE_ENVSUBST_FLAVOR=""
}

@test "flavor detection: GNU gettext" {
  _fake_gnu
  run template::envsubst_flavor
  assert_success
  assert_output "gnu"
}

@test "flavor detection: renvsubst (anything non-GNU)" {
  _fake_renvsubst
  run template::envsubst_flavor
  assert_success
  assert_output "renvsubst"
}

@test "GNU dialect: SHELL-FORMAT string passed through as one arg" {
  _fake_gnu
  run bash -c '
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    echo "x" | template::envsubst "\${VAR_A} \${VAR_B} "
  '
  assert_success
  assert_output "gnu-called"
  run cat "${ARGS_OUT}"
  assert_output '${VAR_A} ${VAR_B} '
}

@test "renvsubst dialect: SHELL-FORMAT translated to --variable filters" {
  _fake_renvsubst
  run bash -c '
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    echo "x" | template::envsubst "\${VAR_A} \${VAR_B} "
  '
  assert_success
  assert_output "renvsubst-called"
  run cat "${ARGS_OUT}"
  assert_line --index 0 "--variable=VAR_A"
  assert_line --index 1 "--variable=VAR_B"
}

@test "empty whitelist replaces NOTHING under renvsubst (no filterless call)" {
  # renvsubst without filters substitutes EVERYTHING — an empty whitelist must
  # therefore never reach it. The fake exits 1 on any non-filter invocation,
  # so plain passthrough proves envsubst was never called.
  _fake_renvsubst
  run bash -c '
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    printf "keep=\$HOME\n" | template::envsubst ""
  '
  assert_success
  assert_output 'keep=$HOME'
}

@test "whitespace-only whitelist is treated as empty" {
  _fake_renvsubst
  run bash -c '
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    printf "keep=\${OTHER}\n" | template::envsubst "  "
  '
  assert_success
  assert_output 'keep=${OTHER}'
}

@test "real GNU envsubst: listed vars substituted, others literal" {
  command -v envsubst > /dev/null || skip "no envsubst on PATH"
  envsubst --version 2> /dev/null | grep -q "GNU gettext" ||
    skip "system envsubst is not GNU gettext"
  _TEMPLATE_ENVSUBST_FLAVOR=""
  export LOK8S_SPEC_T1="hello"
  run bash -c '
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    printf "a=\${LOK8S_SPEC_T1} b=\${NOT_LISTED} c=\$HOME\n" \
      | template::envsubst "\${LOK8S_SPEC_T1} "
  '
  assert_success
  assert_output 'a=hello b=${NOT_LISTED} c=$HOME'
}

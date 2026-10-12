#!/usr/bin/env bats
# bootstrap_sandbox_test.bats — the whole-project --dry-run isolation
# sandbox (review round 2, item A): bootstrap::_sandbox_copy_tree's
# symlink handling and bootstrap::_sandbox_build's env redirection/unset
# and cleanup. Go twin: internal/bootstrap/isolation_test.go.

setup() {
  load "../test_helper"
  setup_tmpdir
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
}

teardown() {
  teardown_tmpdir
}

@test "bootstrap::_sandbox_copy_tree copies regular files" {
  local src="${BATS_TEST_TMPDIR}/src" dst="${BATS_TEST_TMPDIR}/dst"
  mkdir -p "${src}/a"
  printf 'hello\n' > "${src}/a/b.txt"
  run bootstrap::_sandbox_copy_tree "${src}" "${dst}"
  assert_success
  [ "$(cat "${dst}/a/b.txt")" = "hello" ]
}

@test "bootstrap::_sandbox_copy_tree skips .git" {
  local src="${BATS_TEST_TMPDIR}/src" dst="${BATS_TEST_TMPDIR}/dst"
  mkdir -p "${src}/.git"
  printf 'ref: refs/heads/main\n' > "${src}/.git/HEAD"
  printf 'x\n' > "${src}/keep.txt"
  run bootstrap::_sandbox_copy_tree "${src}" "${dst}"
  assert_success
  [ ! -e "${dst}/.git" ]
  [ -f "${dst}/keep.txt" ]
}

# A symlink whose target stays inside root is copied AS ITS TARGET's
# content — a real file in the copy, not a symlink — fixing a
# cross-directory reference the project itself holds.
@test "bootstrap::_sandbox_copy_tree dereferences an internal symlink" {
  local src="${BATS_TEST_TMPDIR}/src" dst="${BATS_TEST_TMPDIR}/dst"
  mkdir -p "${src}/real"
  printf 'content\n' > "${src}/real/file.txt"
  ln -s "${src}/real" "${src}/link"
  run bootstrap::_sandbox_copy_tree "${src}" "${dst}"
  assert_success
  [ ! -L "${dst}/link" ]
  [ -d "${dst}/link" ]
  [ "$(cat "${dst}/link/file.txt")" = "content" ]
}

# A symlink whose target resolves OUTSIDE root is refused outright.
@test "bootstrap::_sandbox_copy_tree refuses an escaping symlink" {
  local src="${BATS_TEST_TMPDIR}/src" dst="${BATS_TEST_TMPDIR}/dst" outside="${BATS_TEST_TMPDIR}/outside"
  mkdir -p "${src}" "${outside}"
  printf 'nope\n' > "${outside}/secret.txt"
  ln -s "${outside}/secret.txt" "${src}/escape"
  run bootstrap::_sandbox_copy_tree "${src}" "${dst}"
  assert_failure
}

@test "bootstrap::_sandbox_copy_tree refuses a relative escaping symlink" {
  local src="${BATS_TEST_TMPDIR}/src" dst="${BATS_TEST_TMPDIR}/dst"
  mkdir -p "${src}/sub"
  ln -s "../../etc/passwd" "${src}/sub/escape"
  run bootstrap::_sandbox_copy_tree "${src}" "${dst}"
  assert_failure
}

# bootstrap::_sandbox_build copies the project, redirects HOME/XDG_*/
# CAROOT/HELM_*/PATH_SECRETS, and unsets KUBECONFIG/KUBERNETES_*/*_TOKEN —
# all live in THIS shell for the rest of the test (dynamic scoping; no
# subshell here, matching how bootstrap::apply itself calls it).
@test "bootstrap::_sandbox_build redirects env and unsets cluster credentials" {
  mkdir -p "${PATH_CLUSTERS}/x.dev"
  echo 'kind: Lo' > "${PATH_CLUSTERS}/x.dev/cluster.lok8s.yaml"
  export HOME=/real/home
  export KUBECONFIG=/real/kubeconfig.yaml
  export KUBERNETES_SERVICE_HOST=10.0.0.1
  export HCLOUD_TOKEN=real-token

  # Not via `run`: bootstrap::_sandbox_build exports into the CALLING
  # shell, and the assertions below need those exports to have landed
  # HERE, not in a `run` subshell that already exited.
  bootstrap::_sandbox_build "x.dev"

  [ "${HOME}" != "/real/home" ]
  [ -z "${KUBECONFIG:-}" ]
  [ -z "${KUBERNETES_SERVICE_HOST:-}" ]
  [ -z "${HCLOUD_TOKEN:-}" ]
  [[ "${PATH_SECRETS}" == "${PATH_CLUSTERS}/x.dev/secrets" ]]
  [ -f "${PATH_BASE}/clusters/x.dev/cluster.lok8s.yaml" ]

  bootstrap::_sandbox_cleanup
  [ ! -e "${bootstrap_sandbox_root}" ]
}

# A symlink AT PATH_BIN pointing outside the project (the b-managed
# toolchain, commonly a shared install via PATH_BIN) must be TOLERATED —
# .bin is excluded from the copy entirely. This is exactly the shape
# hack/parity-orchestrate.sh's fixture takes (.bin/argsh symlinked to the
# repo's real toolchain install) and it broke the sandbox on its first
# parity run.
@test "bootstrap::_sandbox_build tolerates a PATH_BIN symlink escape" {
  local orig_bin="${PATH_BIN:-${PATH_BASE}/.bin}"
  export PATH_BIN="${orig_bin}"
  mkdir -p "${PATH_CLUSTERS}/x.dev" "${PATH_BIN}"
  local outside; outside="$(mktemp -d)"
  printf '#!/bin/sh\n' > "${outside}/argsh"
  ln -s "${outside}/argsh" "${PATH_BIN}/argsh"
  bootstrap::_sandbox_build "x.dev"
  [ ! -e "${PATH_BASE}/.bin" ]
  [[ "${PATH_BIN}" == "${orig_bin}" ]]
  bootstrap::_sandbox_cleanup
}

@test "bootstrap::_sandbox_build refuses an escaping symlink in the project" {
  mkdir -p "${PATH_CLUSTERS}/x.dev"
  # A SEPARATE mktemp -d, not nested under BATS_TEST_TMPDIR (== PATH_BASE)
  # — it must be truly outside the project for this to test anything.
  local outside; outside="$(mktemp -d)"
  printf 'nope\n' > "${outside}/secret.txt"
  ln -s "${outside}/secret.txt" "${PATH_BASE}/escape"
  run bootstrap::_sandbox_build "x.dev"
  assert_failure
  rm -rf "${outside}"
}

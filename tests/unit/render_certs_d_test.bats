#!/usr/bin/env bats
# render_certs_d_test.bats — lo::write_certs_d must refresh the containerd
# certs.d tree IN PLACE (preserve the directory's inode). A kind node
# bind-mounts clusters/<domain>/.containerd/certs.d; `rm -rf`ing the dir gives it
# a new inode while the running node's mount still points at the deleted one, so
# the node sees an EMPTY certs.d → containerd falls back to HTTPS:443 (the
# registry serves HTTP:80) → ImagePullBackOff on every re-`lo up`. These tests
# guard that regression: the dir inode is stable across runs, and stale host
# entries are still cleared.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export DOMAIN_NAME="test.dev"
  mkdir -p "${PATH_CLUSTERS}/${DOMAIN_NAME}"

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/defaults.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/render.sh"

  # Isolate the dir-refresh behavior: plain (non-TLS) registries, no entries.
  registry::is_tls() { return 1; }
  registry::each() { :; }
}

_certs_d() { echo "${PATH_CLUSTERS}/${DOMAIN_NAME}/.containerd/certs.d"; }

@test "write_certs_d creates the certs.d dir" {
  lo::write_certs_d || fail "write_certs_d returned non-zero"
  assert [ -d "$(_certs_d)" ]
}

@test "write_certs_d preserves the certs.d dir inode across runs (bind-mount safe)" {
  lo::write_certs_d
  local certs_d before after
  certs_d="$(_certs_d)"
  before=$(stat -c '%i' "${certs_d}")
  lo::write_certs_d
  after=$(stat -c '%i' "${certs_d}")
  assert_equal "${before}" "${after}"
}

@test "write_certs_d clears stale host entries on re-run" {
  lo::write_certs_d
  local certs_d
  certs_d="$(_certs_d)"
  mkdir -p "${certs_d}/stale.registry"
  echo "old" >"${certs_d}/stale.registry/hosts.toml"
  lo::write_certs_d
  assert [ ! -e "${certs_d}/stale.registry" ]
}

@test "write_certs_d self-protects the generated tree with a committable .gitignore" {
  # The whole .containerd tree is regenerated derived state — its entries are
  # never committed. The generator drops a .gitignore that ignores everything
  # (*) but keeps itself (!.gitignore) so the protection is committable and
  # travels with the repo, so consumer projects need no hand-maintained rule.
  lo::write_certs_d
  local gitignore
  gitignore="$(dirname "$(_certs_d)")/.gitignore"
  assert [ -f "${gitignore}" ]
  run grep -Fxq '*' "${gitignore}"
  assert_success
  run grep -Fxq '!.gitignore' "${gitignore}"
  assert_success
}

@test "write_certs_d .gitignore survives the in-place certs.d refresh" {
  # The .gitignore lives in the .containerd parent, so the certs.d clear
  # (find -mindepth 1 -delete) must not remove it across re-runs.
  lo::write_certs_d
  lo::write_certs_d
  assert [ -f "$(dirname "$(_certs_d)")/.gitignore" ]
}

@test "write_certs_d does not clobber an already-conforming .gitignore" {
  # The sentinel guard (grep !.gitignore) rewrites only a missing or pre-fix
  # (*-only) file; a conforming one is left untouched — proven by a user marker
  # surviving a re-run (asserts content-preservation, not just existence).
  lo::write_certs_d
  local gi
  gi="$(dirname "$(_certs_d)")/.gitignore"
  printf '*\n!.gitignore\n# user-added marker\n' >"${gi}"
  lo::write_certs_d
  run grep -Fxq '# user-added marker' "${gi}"
  assert_success
}

#!/usr/bin/env bats
# secrets_store_resolution_test.bats — which secrets store a command reads, for
# the frozen bash tree. Findings from the kubehz.in.net in-place recreate
# (2026-10-23/24):
#
#   - the KubeOne addon render ran before bootstrap::apply exported the domain
#     store, so the secrets plugin rendered the hcloud CCM Secret from the
#     stale flat .secrets/ store; and it read KUSTOMIZE_PLUGIN_HOME raw, so with
#     it unset the cilium render failed "unable to find plugin root";
#   - the flat-store fallback was silent: the first run of a new domain
#     directory would generate a new sops-age key and new tokens into it;
#   - provider credentials were read from plaintext only, so a worktree store
#     with only the committed .enc failed.
#
# Go twins: internal/domain/store_test.go, internal/build (TestArtifactsRefuses…),
# internal/bootstrap/store_test.go, internal/secrets/store_test.go,
# internal/provision/creds_enc_test.go.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export PATH="${_PROJECT_ROOT}/.bin:${PATH}"
  mkdir -p "${PATH_CLUSTERS}"

  import() { :; }
  :usage() { :; }
  :args()  { shift; }
  export -f import :usage :args
}

teardown() { teardown_tmpdir; }

# _per_domain_project: a.dev keeps its own store, new.dev is a domain
# directory without one.
_per_domain_project() {
  mkdir -p "${PATH_CLUSTERS}/a.dev/secrets" "${PATH_CLUSTERS}/new.dev"
  printf 'kind: Lo\nmetadata:\n  name: newc\n' > "${PATH_CLUSTERS}/new.dev/cluster.lok8s.yaml"
}

# _refusal: the first line of the refusal for new.dev.
_refusal() { echo "[error] domain new.dev has no secrets store: ${PATH_CLUSTERS}/new.dev/secrets does not exist"; }

# ── domain::check_secrets_store ─────────────────────────────────────────────

@test "check_secrets_store: a domain without a store in a per-domain project is refused" {
  _per_domain_project
  run domain::check_secrets_store new.dev
  assert_failure
  assert_line "[error] domain new.dev has no secrets store: ${PATH_CLUSTERS}/new.dev/secrets does not exist, and this project keeps a store per domain (${PATH_CLUSTERS}/a.dev/secrets). lo does not use the flat .secrets/ store for new.dev."
  assert_line "[error]   To generate new values, create the empty directory ${PATH_CLUSTERS}/new.dev/secrets. To use the values of another domain, copy its .enc files into that directory, then run \"lo secrets decrypt --domain new.dev\"."
}

@test "check_secrets_store: the legacy flat project, a domain with a store, and no domain dir pass" {
  mkdir -p "${PATH_CLUSTERS}/only.dev"
  run domain::check_secrets_store only.dev        # no clusters/*/secrets at all
  assert_success
  assert_output ""
  _per_domain_project
  mkdir -p "${PATH_CLUSTERS}/new.dev/secrets"
  run domain::check_secrets_store new.dev         # its own store
  assert_success
  assert_output ""
  run domain::check_secrets_store lok8s.dev       # no domain directory
  assert_success
  assert_output ""
  run domain::check_secrets_store ""
  assert_success
}

# ── secrets::path ────────────────────────────────────────────────────────────

@test "secrets::path: no flat fallback for a domain without a store; set creates the store" {
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  _per_domain_project
  mkdir -p "${PATH_BASE}/.secrets"
  local domain=new.dev
  run secrets::path
  assert_failure
  assert_line --partial "$(_refusal)"
  refute_output --partial "${PATH_BASE}/.secrets"
  # The documented way out: the store exists, the path resolves to it.
  secrets::_ensure_store
  run secrets::path
  assert_success
  assert_output "${PATH_CLUSTERS}/new.dev/secrets"
}

@test "secrets::path: a project with only the flat store keeps using it" {
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  mkdir -p "${PATH_CLUSTERS}/only.dev"
  unset PATH_SECRETS
  local domain=only.dev
  run secrets::path
  assert_success
  assert_output "${PATH_BASE}/.secrets"
}

# ── build::artifacts ─────────────────────────────────────────────────────────

_build_setup() {
  source "${_PROJECT_ROOT}/.lok8s/utils/targets.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"
  template::envsubst_whitelist() { echo ""; }
  export KCALLED="${BATS_TEST_TMPDIR}/kustomize.called"
  kustomize() {
    : > "${KCALLED}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'
  }
  export -f template::envsubst_whitelist kustomize
  printf 'resources: []\n' > "${PATH_CLUSTERS}/new.dev/kustomization.yaml"
}

@test "build::artifacts: a domain without a store stops before the render; --no-secrets renders" {
  _per_domain_project
  _build_setup
  export PATH_SECRETS="${PATH_BASE}/.secrets"
  run build::artifacts new.dev
  assert_failure
  assert_line --partial "$(_refusal)"
  [ ! -e "${KCALLED}" ] || { echo "kustomize ran"; return 1; }
  [ ! -e "${PATH_CLUSTERS}/new.dev/artifacts.yaml" ]

  LOK8S_BUILD_NO_SECRETS=1 run build::artifacts new.dev
  assert_success
  [ -e "${KCALLED}" ]
  [ -s "${PATH_CLUSTERS}/new.dev/artifacts.yaml" ]
}

# ── bootstrap::apply ─────────────────────────────────────────────────────────

@test "bootstrap::apply: a domain without a store stops before any render or apply" {
  _per_domain_project
  export KCALLED="${BATS_TEST_TMPDIR}/tool.called"
  kustomize() { : > "${KCALLED}"; }
  kubectl() { : > "${KCALLED}"; }
  export -f kustomize kubectl
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  mkdir -p "${PATH_BASE}/.kubeconfig"
  : > "${PATH_BASE}/.kubeconfig/newc.yaml"
  printf 'kind: Lo\nmetadata:\n  name: newc\nspec:\n  bootstrap:\n    - cilium\n' > "${PATH_CLUSTERS}/new.dev/cluster.lok8s.yaml"
  export PATH_SECRETS="${PATH_BASE}/.secrets"
  run bootstrap::apply new.dev "${PATH_CLUSTERS}/new.dev/cluster.lok8s.yaml" "${PATH_BASE}/.kubeconfig/newc.yaml"
  assert_failure
  assert_line --partial "$(_refusal)"
  [ ! -e "${KCALLED}" ] || { echo "a tool ran"; return 1; }
}

# ── kubeone::render_addons ───────────────────────────────────────────────────

_render_addons_setup() {
  command -v yq &>/dev/null || skip "yq not available"
  provider::detect() { echo "hetzner"; }
  export RCALLS="${BATS_TEST_TMPDIR}/render.calls"
  addons::render() {
    printf '%s|%s\n' "${1##*/}" "${PATH_SECRETS:-<unset>}" >> "${RCALLS}"
    echo "# stub addon"
  }
  export -f provider::detect addons::render
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
  export HCLOUD_TOKEN="test-token"
  unset HROBOT_USER HROBOT_PASSWORD || true
  work_dir="${BATS_TEST_TMPDIR}/work"
  mkdir -p "${work_dir}"
  printf 'apiVersion: kubeone.k8c.io/v1beta2\nkind: KubeOneCluster\ncloudProvider:\n  hetzner: {}\napiEndpoint:\n  host: 10.0.0.1\n' > "${work_dir}/kubeone.yaml"
}

# The render runs inside kubeone::apply, BEFORE bootstrap::apply exports the
# domain store; the Go binary calls it through its provider bridge with the
# flat store in PATH_SECRETS. Both render with the domain store now.
@test "render_addons: the addon render reads the domain's store, not the inherited flat one" {
  _render_addons_setup
  mkdir -p "${PATH_CLUSTERS}/k1.cloud/secrets"
  printf 'kind: KubeOne\nmetadata:\n  name: k1\nspec:\n  bootstrap: []\n' > "${PATH_CLUSTERS}/k1.cloud/cluster.lok8s.yaml"
  export PATH_SECRETS="${PATH_BASE}/.secrets"
  run kubeone::render_addons "${work_dir}" "${PATH_CLUSTERS}/k1.cloud/cluster.lok8s.yaml"
  assert_success
  run cat "${RCALLS}"
  assert_line "cilium|${PATH_CLUSTERS}/k1.cloud/secrets"
  assert_line "ccm|${PATH_CLUSTERS}/k1.cloud/secrets"
}

@test "render_addons: a domain without a store in a per-domain project stops before any render" {
  _render_addons_setup
  mkdir -p "${PATH_CLUSTERS}/a.dev/secrets" "${PATH_CLUSTERS}/k1.cloud"
  printf 'kind: KubeOne\nmetadata:\n  name: k1\nspec:\n  bootstrap: []\n' > "${PATH_CLUSTERS}/k1.cloud/cluster.lok8s.yaml"
  run kubeone::render_addons "${work_dir}" "${PATH_CLUSTERS}/k1.cloud/cluster.lok8s.yaml"
  assert_failure
  assert_line --partial "[error] domain k1.cloud has no secrets store: ${PATH_CLUSTERS}/k1.cloud/secrets does not exist"
  [ ! -e "${RCALLS}" ] || { echo "an addon rendered"; return 1; }
}

# addons::render (the one render behind the bootstrap and the KubeOne addons)
# gives its kustomize child <project>/.kustomize when KUSTOMIZE_PLUGIN_HOME is
# unset, the value the Go binary gives every kustomize child.
@test "addons::render: an unset KUSTOMIZE_PLUGIN_HOME defaults to the project .kustomize" {
  command -v yq &>/dev/null || skip "yq not available"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/addons"
  local addon="${BATS_TEST_TMPDIR}/addon"
  mkdir -p "${addon}"
  printf 'resources: []\n' > "${addon}/kustomization.yaml"
  export KHOME="${BATS_TEST_TMPDIR}/khome"
  kustomize() {
    printf '%s\n' "${KUSTOMIZE_PLUGIN_HOME:-<unset>}" > "${KHOME}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'
  }
  envsubst() { [[ "${1:-}" != --version ]] || return 0; cat; }
  export -f kustomize envsubst
  unset KUSTOMIZE_PLUGIN_HOME
  run addons::render "${addon}" kubeone hetzner ""
  assert_success
  [ "$(cat "${KHOME}")" = "${PATH_BASE}/.kustomize" ]
  # A set value is kept.
  export KUSTOMIZE_PLUGIN_HOME="${BATS_TEST_TMPDIR}/elsewhere"
  run addons::render "${addon}" kubeone hetzner ""
  assert_success
  [ "$(cat "${KHOME}")" = "${BATS_TEST_TMPDIR}/elsewhere" ]
}

# ── provision::load_provider_creds ───────────────────────────────────────────

_require_crypto() {
  for t in sops ssh-to-age ssh-keygen; do
    command -v "$t" >/dev/null || skip "$t not installed"
  done
}

# _enc_creds <k=v>...: write the creds, encrypt them with `secrets::encrypt`
# (the production producer) to a throwaway key, then drop the plaintexts:
# what a fresh worktree holds before `lo secrets decrypt`.
_enc_creds() {
  ssh-keygen -t ed25519 -N '' -C test -f "${BATS_TEST_TMPDIR}/id" -q
  export LOK8S_SSH_KEY="${BATS_TEST_TMPDIR}/id"
  export HOME="${BATS_TEST_TMPDIR}/home"
  unset SOPS_AGE_KEY SOPS_AGE_KEY_FILE || true
  printf 'creation_rules:\n  - path_regex: Secret\\..*\n    age: %s\n' \
    "$(ssh-to-age < "${BATS_TEST_TMPDIR}/id.pub")" > "${PATH_BASE}/.sops.yaml"
  local store="${PATH_CLUSTERS}/test.lok8s.dev/secrets" kv
  mkdir -p "${store}"
  for kv in "$@"; do
    printf '%s' "${kv#*=}" > "${store}/Secret.hetzner.provisioning.${kv%%=*}"
  done
  local domain=test.lok8s.dev
  secrets::encrypt >/dev/null
  local f
  for f in "${store}"/Secret.hetzner.provisioning.*; do
    [[ "${f}" == *.enc ]] || rm "${f}"
  done
}

@test "load_provider_creds: an .enc-only store is decrypted in memory, no plaintext written" {
  _require_crypto
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  _enc_creds HCLOUD_TOKEN=tok-enc HROBOT_USER=rob-usr HROBOT_PASSWORD=rob-pwd
  unset HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD ROBOT_USER ROBOT_PASSWORD HETZNER_ROBOT_USER HETZNER_ROBOT_PASSWORD
  local store="${PATH_CLUSTERS}/test.lok8s.dev/secrets"
  [ -f "${store}/Secret.hetzner.provisioning.HCLOUD_TOKEN.enc" ]
  [ ! -e "${store}/Secret.hetzner.provisioning.HCLOUD_TOKEN" ]

  provision::load_provider_creds test.lok8s.dev 2> "${BATS_TEST_TMPDIR}/stderr"
  [ "${HCLOUD_TOKEN}" = "tok-enc" ]
  [ "${HETZNER_ROBOT_USER}" = "rob-usr" ]
  [ "${ROBOT_PASSWORD}" = "rob-pwd" ]
  [ ! -s "${BATS_TEST_TMPDIR}/stderr" ]
  run ls "${store}"
  refute_output --regexp 'provisioning\.[A-Z_]+$'
}

@test "load_provider_creds: an .enc no local identity decrypts is skipped with a warning by name" {
  _require_crypto
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  _enc_creds HCLOUD_TOKEN=tok-enc
  export LOK8S_SSH_KEY="${BATS_TEST_TMPDIR}/no-such-key"
  unset HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD
  run provision::load_provider_creds test.lok8s.dev
  assert_success
  assert_output "[warn] cannot read Secret.hetzner.provisioning.HCLOUD_TOKEN: the plaintext is missing, and no local age identity decrypts its .enc. Set HCLOUD_TOKEN, or set SOPS_AGE_KEY or LOK8S_SSH_KEY to an identity of the store."
  refute_output --partial "tok-enc"
}

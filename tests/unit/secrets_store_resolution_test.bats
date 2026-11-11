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
  # The message names the store lo would otherwise use; an explicit
  # PATH_SECRETS does not override the refusal.
  export PATH_SECRETS="/custom/flat"
  run domain::check_secrets_store new.dev
  assert_failure
  assert_output "[error] domain new.dev has no secrets store: ${PATH_CLUSTERS}/new.dev/secrets does not exist, and this project keeps a store per domain (${PATH_CLUSTERS}/a.dev/secrets). lo does not fall back to /custom/flat.
[error]   Create the empty directory ${PATH_CLUSTERS}/new.dev/secrets to generate new values for new.dev."
}

# With neither PATH_SECRETS nor PATH_BASE set (a bare unit context), the
# named fallback is ./.secrets, never /.secrets.
@test "check_secrets_store: names ./.secrets when PATH_SECRETS and PATH_BASE are unset" {
  _per_domain_project
  ( unset PATH_SECRETS PATH_BASE
    run domain::check_secrets_store new.dev
    assert_failure
    assert_line --partial "lo does not fall back to ./.secrets."
  )
}

# The other store is the first in byte order (LC_ALL=C), as the Go twin
# names it, whatever the locale's collation.
@test "check_secrets_store: names the byte-order first store in any locale" {
  mkdir -p "${PATH_CLUSTERS}/a.dev/secrets" "${PATH_CLUSTERS}/B.dev/secrets" "${PATH_CLUSTERS}/new.dev"
  local loc
  for loc in C en_US.UTF-8 C.UTF-8; do
    LC_ALL="${loc}" run domain::check_secrets_store new.dev
    assert_failure
    assert_line --partial "a store per domain (${PATH_CLUSTERS}/B.dev/secrets)"
  done
}

@test "export_secrets_store: exports the domain store; refuses without one" {
  _per_domain_project
  export PATH_SECRETS="${PATH_BASE}/.secrets"
  run domain::export_secrets_store new.dev
  assert_failure
  [ "${PATH_SECRETS}" = "${PATH_BASE}/.secrets" ]
  domain::export_secrets_store a.dev
  [ "${PATH_SECRETS}" = "${PATH_CLUSTERS}/a.dev/secrets" ]
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
  refute_line "${PATH_BASE}/.secrets"
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

# _creds_setup: the provision lib, an .enc-only store entry for HCLOUD_TOKEN
# (fake content: these cases stub sops or never reach it), no ambient creds.
_creds_setup() {
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  export HOME="${BATS_TEST_TMPDIR}/home"
  export LOK8S_SSH_KEY="${BATS_TEST_TMPDIR}/no-such-key"
  unset SOPS_AGE_KEY SOPS_AGE_KEY_FILE HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD \
    ROBOT_USER ROBOT_PASSWORD HETZNER_ROBOT_USER HETZNER_ROBOT_PASSWORD || true
  STORE="${PATH_CLUSTERS}/test.lok8s.dev/secrets"
  mkdir -p "${STORE}"
  printf '{"data":"ENC[fake]","sops":{}}' > "${STORE}/Secret.hetzner.provisioning.HCLOUD_TOKEN.enc"
}

_entry="Secret.hetzner.provisioning.HCLOUD_TOKEN"
_identity_hint="Set HCLOUD_TOKEN, or set SOPS_AGE_KEY, SOPS_AGE_KEY_FILE or LOK8S_SSH_KEY to an identity of the store."

# The decrypt runs in memory through sops: the stub records its argv and the
# identity it got, prints the value, and the value is exported. No plaintext
# reaches the store. No crypto tools needed (CI's bats container has no
# ssh-keygen); the real-crypto twin is the next case and the Go test.
@test "load_provider_creds: an .enc-only store is decrypted in memory (stub sops)" {
  _creds_setup
  export SOPS_AGE_KEY="stub-identity" SOPS_LOG="${BATS_TEST_TMPDIR}/sops.log"
  sops() { printf '%s|%s\n' "${SOPS_AGE_KEY}" "$*" >> "${SOPS_LOG}"; printf 'tok-enc'; }
  export -f sops
  provision::load_provider_creds test.lok8s.dev 2> "${BATS_TEST_TMPDIR}/stderr"
  [ "${HCLOUD_TOKEN}" = "tok-enc" ]
  [ ! -s "${BATS_TEST_TMPDIR}/stderr" ]
  run cat "${SOPS_LOG}"
  assert_output "stub-identity|decrypt --input-type binary --output-type binary ${STORE}/${_entry}.enc"
  run ls "${STORE}"
  refute_output --regexp 'provisioning\.[A-Z_]+$'
}

_require_crypto() {
  for t in sops ssh-to-age ssh-keygen; do
    command -v "$t" >/dev/null || skip "$t not installed"
  done
}

# The same with the real sops and an identity derived from LOK8S_SSH_KEY.
# The .enc is made by secrets::encrypt (the production producer) to a
# throwaway key.
@test "load_provider_creds: an .enc-only store is decrypted in memory (real sops)" {
  _require_crypto
  _creds_setup
  ssh-keygen -t ed25519 -N '' -C test -f "${BATS_TEST_TMPDIR}/id" -q
  export LOK8S_SSH_KEY="${BATS_TEST_TMPDIR}/id"
  printf 'creation_rules:\n  - path_regex: Secret\\..*\n    age: %s\n' \
    "$(ssh-to-age < "${BATS_TEST_TMPDIR}/id.pub")" > "${PATH_BASE}/.sops.yaml"
  rm "${STORE}/${_entry}.enc"
  printf 'tok-enc' > "${STORE}/Secret.hetzner.provisioning.HCLOUD_TOKEN"
  printf 'rob-usr' > "${STORE}/Secret.hetzner.provisioning.HROBOT_USER"
  local domain=test.lok8s.dev
  secrets::encrypt >/dev/null
  rm "${STORE}/Secret.hetzner.provisioning.HCLOUD_TOKEN" "${STORE}/Secret.hetzner.provisioning.HROBOT_USER"

  provision::load_provider_creds test.lok8s.dev 2> "${BATS_TEST_TMPDIR}/stderr"
  [ "${HCLOUD_TOKEN}" = "tok-enc" ]
  [ "${HETZNER_ROBOT_USER}" = "rob-usr" ]
  [ ! -s "${BATS_TEST_TMPDIR}/stderr" ]
  run ls "${STORE}"
  refute_output --regexp 'provisioning\.[A-Z_]+$'
}

# A failed decrypt is skipped with a warning by name (never a value) that
# names the cause class. No crypto tools needed.
@test "load_provider_creds: no local age identity" {
  _creds_setup
  run provision::load_provider_creds test.lok8s.dev
  assert_success
  assert_output "[warn] cannot read ${_entry}: the plaintext is missing, and no local age identity was found for its .enc. ${_identity_hint}"
}

@test "load_provider_creds: the .enc does not decrypt" {
  _creds_setup
  export SOPS_AGE_KEY="stub-identity"
  sops() { echo "MAC mismatch. File has 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" >&2; return 1; }
  export -f sops
  run provision::load_provider_creds test.lok8s.dev
  assert_success
  assert_output "[warn] cannot read ${_entry}: the plaintext is missing, and its .enc does not decrypt with the local age identity (another identity, or a damaged file). ${_identity_hint}"
  [ -z "${HCLOUD_TOKEN:-}" ]
}

@test "load_provider_creds: the .enc cannot be read" {
  [[ "${EUID}" -ne 0 ]] || skip "root reads a 0000 file"
  _creds_setup
  export SOPS_AGE_KEY="stub-identity"
  chmod 000 "${STORE}/${_entry}.enc"
  run provision::load_provider_creds test.lok8s.dev
  chmod 600 "${STORE}/${_entry}.enc"
  assert_success
  assert_output "[warn] cannot read ${_entry}: the plaintext is missing, and its .enc cannot be read. Set HCLOUD_TOKEN, or make ${_entry}.enc readable."
}

@test "load_provider_creds: sops is not installed" {
  _creds_setup
  export SOPS_AGE_KEY="stub-identity"
  local tb="${BATS_TEST_TMPDIR}/nosops"
  mkdir -p "${tb}"
  ln -s "$(command -v cat)" "${tb}/cat"
  PATH="${tb}" run provision::load_provider_creds test.lok8s.dev
  assert_success
  assert_output "[warn] cannot read ${_entry}: the plaintext is missing, and sops is not installed. Set HCLOUD_TOKEN, or install sops (b install)."
}

# ── lo k8s infrastructure / platform ─────────────────────────────────────────

# They rendered with the inherited PATH_SECRETS, the flat store even for a
# domain with its own store. Now they read the domain store, and a domain
# without one stops before the render.
@test "k8s::infrastructure: renders with the domain store; refuses a domain without one" {
  source "${_PROJECT_ROOT}/.lok8s/libs/k8s"
  export KLOG="${BATS_TEST_TMPDIR}/kustomize.log"
  kustomize() { echo "${PATH_SECRETS:-<unset>}" >> "${KLOG}"; echo "kind: ConfigMap"; }
  export -f kustomize
  _per_domain_project
  mkdir -p "${PATH_CLUSTERS}/new.dev/targets"
  export PATH_SECRETS="${PATH_BASE}/.secrets"
  local domain=new.dev
  run k8s::infrastructure
  assert_failure
  assert_line --partial "$(_refusal)"
  [ ! -e "${KLOG}" ] || { echo "kustomize ran"; return 1; }
  [ ! -e "${PATH_CLUSTERS}/new.dev/artifacts/infrastructure.yaml" ]

  mkdir -p "${PATH_CLUSTERS}/new.dev/secrets"
  run k8s::infrastructure
  assert_success
  run cat "${KLOG}"
  assert_output "${PATH_CLUSTERS}/new.dev/secrets"
}

@test "k8s::platform: a domain without a store stops before the render" {
  source "${_PROJECT_ROOT}/.lok8s/libs/k8s"
  export KLOG="${BATS_TEST_TMPDIR}/kustomize.log"
  kustomize() { : > "${KLOG}"; echo "kind: ConfigMap"; }
  export -f kustomize
  _per_domain_project
  mkdir -p "${PATH_BASE}/.k8s/overlays/new.dev"
  local domain=new.dev
  run k8s::platform
  assert_failure
  assert_line --partial "$(_refusal)"
  [ ! -e "${KLOG}" ] || { echo "kustomize ran"; return 1; }
}

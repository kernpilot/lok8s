#!/usr/bin/env bats
# kubeone_csi_test.bats — spec.network.csi makes KubeOne's bundled provider CSI
# opt-in. Verifies (a) kubeone::extract_vars reads spec.network.csi into
# CSI_PLUGIN (default "external"), and (b) kubeone::render_addons writes an EMPTY
# csi-hetzner/ override dir on the default (external/empty) path — which shadows +
# no-op's KubeOne's embedded csi-hetzner — and does NOT create it for csi: hetzner
# (opt-in ⇒ KubeOne deploys its bundled hcloud CSI). Uses the real yq from
# `argsh test`.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  import() { :; }
  export -f import
  # verbose.sh (error/debug/warn) already loaded by test_helper.

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
}

teardown() {
  teardown_tmpdir
}

_load_kubeone_config() {
  # extract_vars calls provider::detect; render_addons calls addons::render.
  # Stub both so the config sources + runs without real infra/khelm.
  provider::detect() { echo "hetzner"; }
  addons::render() { echo "# stub addon"; }
  export -f provider::detect addons::render
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
}

# Minimal KubeOne spec; optional `csi: <value>` under spec.network.
_kubeone_spec() {
  local csi="$1" path="${BATS_TEST_TMPDIR}/kubeone-cluster.yaml"
  cat >"${path}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: ko-test
spec:
  kubernetes:
    version: "v1.35.5"
  provider:
    hetzner: {}
  network:
    podSubnet: 10.244.0.0/16
YAML
  if [[ -n "${csi}" ]]; then
    printf '    csi: %s\n' "${csi}" >>"${path}"
  fi
  echo "${path}"
}

# Minimal Hetzner manifest render_addons needs (cloudProvider.hetzner +
# apiEndpoint.host, no static workers so the Robot-cred path is skipped).
_kubeone_manifest() {
  local work_dir="$1"; mkdir -p "${work_dir}"
  cat >"${work_dir}/kubeone.yaml" <<'YAML'
apiVersion: kubeone.k8c.io/v1beta2
kind: KubeOneCluster
cloudProvider:
  hetzner: {}
apiEndpoint:
  host: "1.2.3.4"
  port: 6443
YAML
}

# ── extract_vars: spec.network.csi → CSI_PLUGIN ──────────

@test "extract_vars: absent spec.network.csi ⇒ CSI_PLUGIN=external (Ceph-first default)" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec "")
  kubeone::extract_vars "${spec}"
  [ "${CSI_PLUGIN}" = "external" ]
}

@test "extract_vars: spec.network.csi=hetzner ⇒ CSI_PLUGIN=hetzner (opt-in)" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec "hetzner")
  kubeone::extract_vars "${spec}"
  [ "${CSI_PLUGIN}" = "hetzner" ]
}

# ── render_addons: CSI override gating ───────────────────

@test "render_addons: default (unset CSI_PLUGIN) creates an EMPTY csi-hetzner override dir" {
  _load_kubeone_config
  unset CSI_PLUGIN
  export HCLOUD_TOKEN="dummy"
  local wd="${BATS_TEST_TMPDIR}/wd-default"; _kubeone_manifest "${wd}"
  run kubeone::render_addons "${wd}"
  assert_success
  [ -d "${wd}/addons/csi-hetzner" ]
  # Override MUST be empty (KubeOne skips an empty dir ⇒ embedded CSI no-op'd).
  [ -z "$(ls -A "${wd}/addons/csi-hetzner")" ]
}

@test "render_addons: csi=external creates the empty csi-hetzner override dir" {
  _load_kubeone_config
  export CSI_PLUGIN="external" HCLOUD_TOKEN="dummy"
  local wd="${BATS_TEST_TMPDIR}/wd-external"; _kubeone_manifest "${wd}"
  run kubeone::render_addons "${wd}"
  assert_success
  [ -d "${wd}/addons/csi-hetzner" ]
}

@test "render_addons: csi=hetzner does NOT create the override (KubeOne deploys its bundled CSI)" {
  _load_kubeone_config
  export CSI_PLUGIN="hetzner" HCLOUD_TOKEN="dummy"
  local wd="${BATS_TEST_TMPDIR}/wd-hetzner"; _kubeone_manifest "${wd}"
  run kubeone::render_addons "${wd}"
  assert_success
  [ ! -d "${wd}/addons/csi-hetzner" ]
}

@test "render_addons: invalid csi value errors out" {
  _load_kubeone_config
  export CSI_PLUGIN="bogus" HCLOUD_TOKEN="dummy"
  local wd="${BATS_TEST_TMPDIR}/wd-bogus"; _kubeone_manifest "${wd}"
  run kubeone::render_addons "${wd}"
  assert_failure
  assert_output --partial "invalid spec.network.csi"
  [ ! -d "${wd}/addons/csi-hetzner" ]
}

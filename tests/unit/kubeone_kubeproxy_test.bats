#!/usr/bin/env bats
# kubeone_kubeproxy_test.bats — spec.network.kubeProxy toggles whether kubeadm
# installs kube-proxy. Verifies (a) kubeone::extract_vars maps the field to
# KUBE_PROXY_SKIP (absent/"enabled" ⇒ false; "disabled" ⇒ true; invalid ⇒ error),
# and (b) the KubeOne manifest template substitutes it into
# clusterNetwork.kubeProxy.skipInstallation. Uses the real yq/envsubst from
# `argsh test`. The Cilium kube-proxy-replacement side is set separately in the
# cilium addon values (kubeProxyReplacement + bpf.masquerade) and is deliberately
# NOT this driver's concern — the two sides are opted into independently.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  import() { :; }
  export -f import
  # verbose.sh (error/debug/warn) already loaded by test_helper.

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
  if ! command -v jq &>/dev/null; then skip "jq not available (template::envsubst native path)"; fi
}

teardown() {
  teardown_tmpdir
}

_load_kubeone_config() {
  # extract_vars calls provider::detect. Stub it so the config sources + runs
  # without real infra.
  provider::detect() { echo "hetzner"; }
  addons::render() { echo "# stub addon"; }
  export -f provider::detect addons::render
  # `import` is stubbed in setup, so source what config's imports would provide:
  # utils/template (template::envsubst, used by the manifest render) + oidc.
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
}

# Minimal KubeOne spec; optional `kubeProxy: <value>` under spec.network.
_kubeone_spec() {
  local kube_proxy="$1" path="${BATS_TEST_TMPDIR}/kubeone-cluster.yaml"
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
  if [[ -n "${kube_proxy}" ]]; then
    printf '    kubeProxy: %s\n' "${kube_proxy}" >>"${path}"
  fi
  echo "${path}"
}

# ── extract_vars: spec.network.kubeProxy → KUBE_PROXY_SKIP ──────────

@test "extract_vars: absent spec.network.kubeProxy ⇒ KUBE_PROXY_SKIP=false (kube-proxy installed)" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec "")
  kubeone::extract_vars "${spec}"
  [ "${KUBE_PROXY_SKIP}" = "false" ]
}

@test "extract_vars: spec.network.kubeProxy=enabled ⇒ KUBE_PROXY_SKIP=false" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec "enabled")
  kubeone::extract_vars "${spec}"
  [ "${KUBE_PROXY_SKIP}" = "false" ]
}

@test "extract_vars: spec.network.kubeProxy=disabled ⇒ KUBE_PROXY_SKIP=true (kubeadm skips kube-proxy)" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec "disabled")
  kubeone::extract_vars "${spec}"
  [ "${KUBE_PROXY_SKIP}" = "true" ]
}

@test "extract_vars: invalid spec.network.kubeProxy errors out" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec "bogus")
  run kubeone::extract_vars "${spec}"
  assert_failure
  assert_output --partial "invalid spec.network.kubeProxy"
}

# ── manifest template: KUBE_PROXY_SKIP → clusterNetwork.kubeProxy.skipInstallation ──

@test "manifest template substitutes KUBE_PROXY_SKIP into skipInstallation" {
  _load_kubeone_config   # sources utils/template → template::envsubst available
  local tmpl="${_PROJECT_ROOT}/.lok8s/drivers/kubeone/cluster/core/kubeone.yaml"
  export CLUSTER_NAME=ko-test K8S_VERSION=v1.35.5 CLOUD_PROVIDER=hetzner
  export POD_SUBNET=10.244.0.0/16 SERVICE_SUBNET=10.96.0.0/12 CNI_PLUGIN=external
  export KUBE_PROXY_SKIP=true
  # Same wrapper the driver's manifest render uses (config: template::envsubst).
  run template::envsubst '${CLUSTER_NAME} ${K8S_VERSION} ${CLOUD_PROVIDER} ${POD_SUBNET} ${SERVICE_SUBNET} ${CNI_PLUGIN} ${KUBE_PROXY_SKIP}' < "${tmpl}"
  assert_success
  assert_output --partial "skipInstallation: true"
  # substituted, NOT left literal (proves ${KUBE_PROXY_SKIP} is in the whitelist)
  refute_output --partial 'skipInstallation: ${KUBE_PROXY_SKIP}'
}

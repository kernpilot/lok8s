#!/usr/bin/env bats
# kubeone_addon_values_test.bats — issue #157: the addons kubeone::render_addons
# writes for `kubeone apply` must carry the CLUSTER's inline bootstrap values
# (`values:`/`valueFiles:`), the same overlay the bootstrap path applies. The
# incident this pins: an inline-only cilium setting reverted mid-node-walk
# because render_addons rendered from the framework value stack alone.
#
# addons::render is stubbed to RECORD its arguments; the assertions read the
# recorded inline-values arg — wiring proof. The value-extraction semantics are
# bootstrap::_parse_entry's own (tested in bootstrap_test.bats), reused via
# bootstrap::inline_values, so there is no second parser to drift.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  import() { :; }
  export -f import
  if ! command -v yq &>/dev/null; then skip "yq not available"; fi

  provider::detect() { echo "hetzner"; }
  addons::render() {
    # Record every call: dir|kind|provider|inline (inline may be multi-line —
    # record it base64'd so one line = one call).
    printf '%s|%s|%s|%s\n' "$1" "$2" "$3" "$(printf '%s' "${4:-}" | base64 | tr -d '\n' )" \
      >> "${BATS_TEST_TMPDIR}/render.calls"
    echo "# stub addon"
  }
  export -f provider::detect addons::render
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"

  export HCLOUD_TOKEN="test-token"
  unset HROBOT_USER HROBOT_PASSWORD || true

  work_dir="${BATS_TEST_TMPDIR}/work"
  mkdir -p "${work_dir}" "${PATH_CLUSTERS}/test.lok8s.dev"
  cat > "${work_dir}/kubeone.yaml" <<'YAML'
apiVersion: kubeone.k8c.io/v1beta2
kind: KubeOneCluster
cloudProvider:
  hetzner: {}
apiEndpoint:
  host: 10.0.0.1
YAML
}

teardown() {
  teardown_tmpdir
}

_cluster_yaml() {
  # $1 = the spec.bootstrap YAML block (indented list), may be empty.
  local path="${PATH_CLUSTERS}/test.lok8s.dev/cluster.lok8s.yaml"
  { echo "apiVersion: lok8s.dev/v1alpha1"
    echo "kind: Cluster"
    echo "metadata: {name: test}"
    echo "spec:"
    if [[ -n "${1:-}" ]]; then
      echo "  bootstrap:"
      printf '%s\n' "${1}"
    else
      echo "  bootstrap: []"
    fi
  } > "${path}"
  echo "${path}"
}

_inline_of() {
  # Decode the recorded inline arg of the call whose addon dir ends in $1.
  # The call line MUST exist — "never rendered" and "rendered with empty
  # inline" are different verdicts, and a silent grep-miss would green both.
  local line
  line=$(grep "addons/${1}|" "${BATS_TEST_TMPDIR}/render.calls" | head -1)
  [[ -n "${line}" ]] || { echo "NO-RENDER-CALL-FOR-${1}" >&2; return 1; }
  awk -F'|' '{print $4}' <<<"${line}" | base64 -d
}

@test "render_addons: the cluster's inline cilium values ride the kubeone render (issue #157)" {
  local cy; cy=$(_cluster_yaml '    - cilium:
        values:
          kubeProxyReplacement: false
          policyAuditMode: true')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  local inline; inline=$(_inline_of cilium)
  [ "$(yq -r '.kubeProxyReplacement' <<<"${inline}")" = "false" ]
  [ "$(yq -r '.policyAuditMode' <<<"${inline}")" = "true" ]
}

@test "render_addons: no bootstrap entry for cilium -> empty inline (unchanged behavior)" {
  local cy; cy=$(_cluster_yaml '')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  [ -z "$(_inline_of cilium)" ]
}

@test "render_addons: no cluster yaml at all (legacy call shape) still renders — under set -u like the argsh runtime" {
  # bats does not run `set -u`, the argsh entrypoint DOES — a bare "${2}"
  # in the signature would pass here and crash in production (review find).
  # A shell FUNCTION wrapper keeps the sourced definitions in scope (a
  # `bash -c` child would see none of them).
  _legacy_call() { set -u; kubeone::render_addons "${1}"; }
  run _legacy_call "${work_dir}"
  [ "${status}" -eq 0 ]
  [ -z "$(_inline_of cilium)" ]
}

@test "render_addons: ccm merges the cluster's values ON TOP of the robot-derived inline" {
  export HROBOT_USER="u" HROBOT_PASSWORD="p"
  yq -i '.cloudProvider.hetzner.networkID = "net-1"' "${work_dir}/kubeone.yaml"
  local cy; cy=$(_cluster_yaml '    - ccm:
        values:
          env:
            HCLOUD_LOAD_BALANCERS_ENABLED: {value: "false"}')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  local inline; inline=$(_inline_of ccm)
  # Driver-derived facts survive…
  [ "$(yq -r '.env.ROBOT_ENABLED.value' <<<"${inline}")" = "true" ]
  [ "$(yq -r '.env.HCLOUD_NETWORK.value' <<<"${inline}")" = "net-1" ]
  # …and the cluster's explicit intent lands on top.
  [ "$(yq -r '.env.HCLOUD_LOAD_BALANCERS_ENABLED.value' <<<"${inline}")" = "false" ]
}

@test "render_addons: on a COLLIDING key the cluster's ccm value wins (precedence, not just merge)" {
  # Non-colliding keys green under either operand order — this collision is
  # the only assertion that pins 'explicit cluster intent beats derived
  # facts' (swap the merge operands and it fails).
  export HROBOT_USER="u" HROBOT_PASSWORD="p"
  local cy; cy=$(_cluster_yaml '    - ccm:
        values:
          env:
            HCLOUD_NETWORK_ROUTES_ENABLED: {value: "true"}')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  local inline; inline=$(_inline_of ccm)
  [ "$(yq -r '.env.HCLOUD_NETWORK_ROUTES_ENABLED.value' <<<"${inline}")" = "true" ]
}

@test "bootstrap::inline_values: a same-named ./targets entry does not shadow the framework addon" {
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/targets/cilium"
  local cy; cy=$(_cluster_yaml '    - ./targets/cilium
    - cilium:
        values:
          kubeProxyReplacement: false')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" cilium
  [ "${status}" -eq 0 ]
  [ "$(yq -r '.kubeProxyReplacement' <<<"${output}")" = "false" ]
}

@test "bootstrap::inline_values: legacy whole-map form extracts too" {
  local cy; cy=$(_cluster_yaml '    - cilium:
        encryption:
          enabled: true')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" cilium
  [ "${status}" -eq 0 ]
  [ "$(yq -r '.encryption.enabled' <<<"${output}")" = "true" ]
}

@test "bootstrap::inline_values: valueFiles pre-merge with values on top (cluster-dir relative)" {
  cat > "${PATH_CLUSTERS}/test.lok8s.dev/extra.yaml" <<'YAML'
kubeProxyReplacement: true
tunnel: disabled
YAML
  local cy; cy=$(_cluster_yaml '    - cilium:
        valueFiles: [./extra.yaml]
        values:
          kubeProxyReplacement: false')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" cilium
  [ "${status}" -eq 0 ]
  [ "$(yq -r '.kubeProxyReplacement' <<<"${output}")" = "false" ] # values: wins
  [ "$(yq -r '.tunnel' <<<"${output}")" = "disabled" ]            # file survives
}

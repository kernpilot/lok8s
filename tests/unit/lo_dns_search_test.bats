#!/usr/bin/env bats
# lo_dns_search_test.bats — the kind config drops the host's DNS search domains.
#
# Why this exists
# ---------------
# Docker copies the search line of the host's resolv.conf into every kind node,
# and kubelet appended it to the search list of every pod (lanisce/ops:
# `search <ns>.svc.cluster.local svc.cluster.local cluster.local fritz.box
# tail6fced5.ts.net`, ndots:5). The Codex CLI (a musl build) then failed every
# request after 5 s. `networking.dnsSearch: []` makes kind start each node with
# KIND_DNS_SEARCH="", and the node entrypoint removes the search lines from
# /etc/resolv.conf on every boot, before kubelet starts.
#
# The bash render is compared byte for byte with the goldens the Go tests use
# (internal/driver/lo/testdata/kindconfig_*.golden), so both implementations
# render the same kind config. Go twin: internal/driver/lo/dnssearch_test.go.

setup() {
  load "../test_helper"
  setup_tmpdir
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  # The golden harness (internal/driver/lo/render_test.go: goldenDriver).
  export PATH_CLUSTERS="/test/clusters" DOMAIN_NAME="test.lok8s.dev" LOK8S_MAX_CONCURRENT_DOWNLOADS=3
  export LOK8S_CP_COUNT=1 LOK8S_WORKER_COUNT=0 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID
  GOLDEN="${_PROJECT_ROOT}/internal/driver/lo/testdata"
}

teardown() { teardown_tmpdir; }

# _same_as_golden <golden> [spec] — the bash render equals the golden file.
_same_as_golden() {
  diff -u "${GOLDEN}/${1}" <(lo::render_kind_config test-shared v1.31.4 lok8s "${2:-}") || {
    echo "the bash render differs from ${1} (the Go render's golden)" >&2
    return 1
  }
}

# _empty_dns_search <file> — networking.dnsSearch is an EMPTY LIST: a missing
# key or a null keeps the host's search domains (kind passes KIND_DNS_SEARCH
# only for a set list).
_empty_dns_search() {
  [ "$(yq -r '.networking.dnsSearch | tag' "${1}")" == '!!seq' ] &&
    [ "$(yq -r '.networking.dnsSearch | length' "${1}")" == 0 ] || {
      echo "networking.dnsSearch is not an empty list:" >&2; cat "${1}" >&2; return 1
    }
}

@test "render: the default config equals the golden and drops the host search domains" {
  _same_as_golden kindconfig_default.golden
  lo::render_kind_config test-shared v1.31.4 lok8s "" > "${BATS_TEST_TMPDIR}/kind.yaml"
  _empty_dns_search "${BATS_TEST_TMPDIR}/kind.yaml"
}

@test "render: host ports, multinode, OIDC and extra mounts equal their goldens" {
  LOK8S_HOST_PORTS=true _same_as_golden kindconfig_hostports.golden
  LOK8S_CP_COUNT=3 LOK8S_WORKER_COUNT=2 _same_as_golden kindconfig_multinode.golden
  LOK8S_HOST_PORTS=true LOK8S_SPEC_OIDC_ISSUER="https://id.kubehz.dev" LOK8S_SPEC_OIDC_CLIENTID="kubectl-client" \
    _same_as_golden kindconfig_oidc.golden
  cat > "${BATS_TEST_TMPDIR}/mounts.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-shared
spec:
  nodes:
    extraMounts:
      - hostPath: /src/app
        containerPath: /app
        readOnly: true
      - hostPath: /data
        containerPath: /data
YAML
  LOK8S_EXTRA_MOUNTS_COUNT=2 _same_as_golden kindconfig_extramounts.golden "${BATS_TEST_TMPDIR}/mounts.yaml"
}

@test "lo provision: kind create gets a config without the host search domains" {
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters" PATH_BASE="${BATS_TEST_TMPDIR}/base"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: lotest\nspec:\n  runtime: kind\n  kubernetes:\n    version: v1.31.4\n' \
    > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  :args() { domain="test.dev"; }
  lo::read_config() { :; }
  lo::validate_ips() { :; }
  lo::network() { :; }
  registry::is_shared() { return 1; }
  lo::registries_tls_cert() { :; }
  lo::registries() { :; }
  lo::write_certs_d() { :; }
  lo::write_oidc_auth_config() { :; }
  lo::registries_tls_nudge() { :; }
  _lo_extract_kubeconfig() { echo "${PATH_BASE}/.kubeconfig/lotest.yaml"; }
  lo::coredns() { :; }
  lo::apply_local_registry_hosting() { :; }
  lo::registry_configmap() { :; }
  kapply::run() { local phase="${1}" fn="${2}"; shift 2; "${fn}" "${@}"; }
  kind() {
    if [[ "${1:-} ${2:-}" == "create cluster" ]]; then
      local prev="" arg
      for arg in "${@}"; do
        [[ "${prev}" == "--config" ]] && cat "${arg}" > "${BATS_TEST_TMPDIR}/kind-config"
        prev="${arg}"
      done
    fi
    return 0
  }

  driver::provision test.dev
  [ -s "${BATS_TEST_TMPDIR}/kind-config" ] || { echo "kind create got no config" >&2; return 1; }
  _empty_dns_search "${BATS_TEST_TMPDIR}/kind-config"
}

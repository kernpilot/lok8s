#!/usr/bin/env bats
# lo_dns_search_test.bats — networking.dnsSearch in the kind config and the
# spec.network.dnsSearch reader. The reason for the key is at
# lo::render_kind_config (.lok8s/drivers/lo/utils/render.sh).
#
# The bash render is compared byte for byte with the goldens the Go tests use
# (internal/driver/lo/testdata/kindconfig_*.golden), so both implementations
# render the same kind config. The reader's error lines are pinned with the
# same text as the Go twin. Go twin: internal/driver/lo/dnssearch_test.go.

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
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID LOK8S_NETWORK_DNS_SEARCH
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

# _dns_search <file> [name…] — networking.dnsSearch is a LIST that holds
# exactly the given names (none: the empty list). A missing key or a null
# keeps the host's search domains: kind passes KIND_DNS_SEARCH only for a
# set list.
_dns_search() {
  local file="${1}"; shift
  local want got
  want="$(printf '%s\n' "${@}")"
  got="$(yq -r '.networking.dnsSearch[]' "${file}")"
  [ "$(yq -r '.networking.dnsSearch | tag' "${file}")" == '!!seq' ] && [ "${got}" == "${want}" ] || {
    echo "networking.dnsSearch is not the list [${*}]:" >&2; cat "${file}" >&2; return 1
  }
}

# _spec <spec-body> — a cluster spec under the test dir; prints its path.
_spec() {
  printf 'kind: Lo\nmetadata:\n  name: dns\nspec:\n%s' "${1}" > "${BATS_TEST_TMPDIR}/spec.yaml"
  echo "${BATS_TEST_TMPDIR}/spec.yaml"
}

# _stub_tools — docker, kind and kubectl FIRST on PATH: each logs its argv and
# fails. A provision test calls _no_tool_ran at its end, so a real tool can
# never touch the host's live clusters, and a call that a stub catches fails
# the test.
_stub_tools() {
  local tool
  mkdir -p "${BATS_TEST_TMPDIR}/stub-bin"
  for tool in docker kind kubectl; do
    printf '#!/usr/bin/env bash\necho "%s $*" >> "%s"\nexit 97\n' \
      "${tool}" "${BATS_TEST_TMPDIR}/tools.log" > "${BATS_TEST_TMPDIR}/stub-bin/${tool}"
    chmod +x "${BATS_TEST_TMPDIR}/stub-bin/${tool}"
  done
  export PATH="${BATS_TEST_TMPDIR}/stub-bin:${PATH}"
}

_no_tool_ran() {
  [ ! -s "${BATS_TEST_TMPDIR}/tools.log" ] || {
    echo "a tool on PATH ran (only the function stubs may answer):" >&2
    cat "${BATS_TEST_TMPDIR}/tools.log" >&2
    return 1
  }
}

@test "render: the default config equals the golden and drops the host search domains" {
  _same_as_golden kindconfig_default.golden
  lo::render_kind_config test-shared v1.31.4 lok8s "" > "${BATS_TEST_TMPDIR}/kind.yaml"
  _dns_search "${BATS_TEST_TMPDIR}/kind.yaml"
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

@test "render: spec.network.dnsSearch replaces the empty list (the shared golden)" {
  export LOK8S_NETWORK_DNS_SEARCH="corp.example.com example.ts.net"
  _same_as_golden kindconfig_dnssearch.golden
  lo::render_kind_config test-shared v1.31.4 lok8s "" > "${BATS_TEST_TMPDIR}/kind.yaml"
  _dns_search "${BATS_TEST_TMPDIR}/kind.yaml" corp.example.com example.ts.net
}

@test "read_dns_search_config: no key, a null, [] and a non-map spec.network export the empty list" {
  local body
  for body in '  cluster:\n    domain: lok8s.dev\n' '  network: lok8s\n' '  network:\n    name: n\n' \
    '  network:\n    dnsSearch: ~\n' '  network:\n    dnsSearch:\n' '  network:\n    dnsSearch: []\n'; do
    export LOK8S_NETWORK_DNS_SEARCH="stale.example"
    # shellcheck disable=SC2059 # the bodies carry \n escapes on purpose
    lo::read_dns_search_config "$(_spec "$(printf "${body}")")" 2> "${BATS_TEST_TMPDIR}/err"
    [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "body ${body}:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
    [ "${LOK8S_NETWORK_DNS_SEARCH}" == "" ] || {
      echo "body ${body}: exported '${LOK8S_NETWORK_DNS_SEARCH}', want ''" >&2; return 1
    }
  done
}

@test "read_dns_search_config: DNS names are exported as written, space-separated" {
  local label63 name253
  label63="$(printf 'a%.0s' {1..63})"
  name253="${label63}.${label63}.${label63}.$(printf 'b%.0s' {1..61})"
  [ "${#name253}" -eq 253 ]
  lo::read_dns_search_config "$(_spec "  network:
    dnsSearch:
      - corp.example.com
      - \"fritz.box\"
      - 'lan'
      - 1and1.example
      - a-b.example
      - xn--bcher-kva.example
      - Corp.Example.com
      - corp.example.com.
      - ${label63}.example
      - ${name253}
      - ${name253}.
")"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "corp.example.com fritz.box lan 1and1.example a-b.example xn--bcher-kva.example Corp.Example.com corp.example.com. ${label63}.example ${name253} ${name253}." ]
}

@test "read_dns_search_config: aliases read like their value (an anchored list, aliased entries, an anchored spec.network)" {
  printf 'kind: Lo\nshared: &s [corp.example.com, x.example]\nspec:\n  network:\n    dnsSearch: *s\n' > "${BATS_TEST_TMPDIR}/a.yaml"
  lo::read_dns_search_config "${BATS_TEST_TMPDIR}/a.yaml"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "corp.example.com x.example" ]

  printf 'kind: Lo\none: &o corp.example.com\nspec:\n  network:\n    dnsSearch: [*o, y.example, *o]\n' > "${BATS_TEST_TMPDIR}/b.yaml"
  lo::read_dns_search_config "${BATS_TEST_TMPDIR}/b.yaml"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "corp.example.com y.example corp.example.com" ]

  printf 'kind: Lo\nnet: &n\n  name: x\n  dnsSearch: [a.example]\nspec:\n  network: *n\n' > "${BATS_TEST_TMPDIR}/c.yaml"
  lo::read_dns_search_config "${BATS_TEST_TMPDIR}/c.yaml"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "a.example" ]
}

@test "read_dns_search_config: 32 entries pass, more fail with the count to remove" {
  local n body i
  for n in 32 33 34; do
    body="  network:
    dnsSearch:
"
    for (( i = 0; i < n; i++ )); do body+="      - d${i}.example
"; done
    local rc=0 want=""
    lo::read_dns_search_config "$(_spec "${body}")" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
    case "${n}" in
      33) want="error: spec.network.dnsSearch has 33 entries. The limit is 32. Remove 1 entry." ;;
      34) want="error: spec.network.dnsSearch has 34 entries. The limit is 32. Remove 2 entries." ;;
    esac
    if [[ -z "${want}" ]]; then
      [ "${rc}" -eq 0 ] || { echo "32 entries rejected:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
      [ "$(wc -w <<< "${LOK8S_NETWORK_DNS_SEARCH}")" -eq 32 ] || { echo "32 entries: exported '${LOK8S_NETWORK_DNS_SEARCH}'" >&2; return 1; }
    else
      [ "${rc}" -ne 0 ] || { echo "${n} entries accepted" >&2; return 1; }
      [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "${want}" ] || { echo "${n} entries:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
    fi
  done
}

@test "read_dns_search_config: a value that is not a list fails, the value in plain words" {
  local value observed
  for value in 'corp.example.com|the text '"'"'corp.example.com'"'"'' '{a: b}|a map' 'true|the boolean true' '5|the number 5'; do
    observed="${value#*|}"; value="${value%%|*}"
    local rc=0
    lo::read_dns_search_config "$(_spec "  network:
    dnsSearch: ${value}
")" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
    [ "${rc}" -ne 0 ]
    [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "error: spec.network.dnsSearch must be a list of DNS names. Found ${observed}. Write a YAML list, for example [corp.example.com]." ] || {
      echo "dnsSearch: ${value}:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1
    }
    [ "${LOK8S_NETWORK_DNS_SEARCH}" == "" ]
  done
}

@test "read_dns_search_config: every bad entry gets the line of its cause (the Go golden, byte for byte)" {
  # The shared fixture holds one entry per failure cause, an aliased entry,
  # a "…\n" string and a | block scalar (a newline the value keeps).
  local rc=0
  lo::read_dns_search_config "${GOLDEN}/dnssearch_bad.yaml" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ]
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "" ]
  diff -u "${GOLDEN}/dnssearch_bad.golden" "${BATS_TEST_TMPDIR}/err"
}

@test "read_config and driver::export stop at a failed read (errexit off, as provision runs them)" {
  local spec rc
  spec="$(_spec "  network:
    dnsSearch: [corp_example]
")"
  lo::read_network_config() { return 0; }
  lo::read_node_config()    { return 0; }
  lo::read_lb_config()      { return 0; }
  rc=0; lo::read_config "${spec}" 2>/dev/null || rc=$?
  [ "${rc}" -ne 0 ] || { echo "read_config returned 0 for a rejected dnsSearch" >&2; return 1; }

  # A failed EARLIER reader stops the read too: the dnsSearch reader runs
  # last, and its success must not become the verdict.
  lo::read_dns_search_config() { return 0; }
  lo::read_network_config() { return 1; }
  rc=0; lo::read_config "${spec}" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "read_config returned 0 after a failed network read" >&2; return 1; }
  lo::read_network_config() { return 0; }
  lo::read_node_config() { return 1; }
  rc=0; lo::read_config "${spec}" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "read_config returned 0 after a failed node read" >&2; return 1; }

  # driver::export: a failed read must not hand over to export_spec_envs.
  :args() { domain="test.dev"; }
  lo::read_config() { return 1; }
  lo::export_spec_envs() { echo "export_spec_envs ran" >&2; return 0; }
  rc=0; driver::export test.dev 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "driver::export returned 0 after a failed read" >&2; return 1; }
  if grep -q "export_spec_envs ran" "${BATS_TEST_TMPDIR}/err"; then
    echo "driver::export went on to export_spec_envs after a failed read" >&2; return 1
  fi
}

# _provision_stubs — every collaborator of driver::provision stubbed to
# SUCCEED, except the render and the DNS search reader (the real ones).
# `kind create` copies its config file to ${BATS_TEST_TMPDIR}/kind-config.
_provision_stubs() {
  _stub_tools
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters" PATH_BASE="${BATS_TEST_TMPDIR}/base"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: lotest\nspec:\n  runtime: kind\n  kubernetes:\n    version: v1.31.4\n%s' \
    "${1:-}" > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  :args() { domain="test.dev"; }
  # The real reader for spec.network.dnsSearch; the network/registry readers
  # would write .registries.json, which this test does not need.
  lo::read_config() { lo::read_dns_search_config "${1}"; }
  lo::validate_ips() { :; }
  lo::network() { :; }
  registry::is_shared() { return 1; }
  lo::registries_tls_cert() { :; }
  lo::registries() { :; }
  lo::write_certs_d() { :; }
  lo::write_oidc_auth_config() { :; }
  lo::registries_tls_nudge() { :; }
  _lo_extract_kubeconfig() { echo "${PATH_BASE}/.kubeconfig/lotest.yaml"; }
  # The node-ip heal gate runs `docker network inspect`; no node has a
  # registry NIC here.
  lo::nodes_on_registry_network() { return 1; }
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
}

@test "lo provision: kind create gets a config without the host search domains" {
  _provision_stubs
  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -eq 0 ]
  [ -s "${BATS_TEST_TMPDIR}/kind-config" ] || { echo "kind create got no config" >&2; return 1; }
  _dns_search "${BATS_TEST_TMPDIR}/kind-config"
  _no_tool_ran
}

@test "lo provision: kind create gets the spec.network.dnsSearch list" {
  _provision_stubs "  network:
    dnsSearch: [corp.example.com, example.ts.net]
"
  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -eq 0 ]
  _dns_search "${BATS_TEST_TMPDIR}/kind-config" corp.example.com example.ts.net
  _no_tool_ran
}

@test "lo provision: a bad dnsSearch entry stops before kind create" {
  _provision_stubs "  network:
    dnsSearch: [corp_example]
"
  lo::network() { echo "lo::network ran" >&2; }
  local rc=0
  driver::provision test.dev 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "driver::provision returned 0 for a rejected dnsSearch" >&2; return 1; }
  grep -qF "error: spec.network.dnsSearch[0] has a character that is not valid in a DNS name: 'corp_example'." "${BATS_TEST_TMPDIR}/err"
  if grep -q "lo::network ran" "${BATS_TEST_TMPDIR}/err"; then
    echo "the provision went on to the docker network after the rejected read" >&2; return 1
  fi
  [ ! -e "${BATS_TEST_TMPDIR}/kind-config" ]
  _no_tool_ran
}

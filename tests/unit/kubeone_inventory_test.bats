#!/usr/bin/env bats
# kubeone_inventory_test.bats — unit tests for the KubeOne driver's
# _append_inventory (the kubeone.yaml host-inventory builder).
#
# Pins the descriptor-anchoring contract (2026-07-10 incident: label-discovered
# machine-controller VMs were swept into staticWorkers.hosts and kubeone apply
# tried to SSH/adopt live MachineDeployment nodes):
#   1. controlPlane.hosts / staticWorkers.hosts contain ONLY servers declared
#      in the descriptor's server[] array (matched by name). VMs discovered by
#      the cluster label but NOT declared are skipped with a warning.
#   2. A per-server `#labels` entry (comma-separated k=v) is emitted as the
#      host's labels: map so KubeOne's labelNodes task syncs Node labels on
#      every apply; a bare `key-` entry keeps KubeOne's addRemoveKeyValues
#      removal suffix (`"key-": ""`).
#   3. Descriptors without #labels / without undeclared VMs render exactly the
#      previous host shape (no labels key, kubelet blocks unchanged).
#
# Uses the real jq/yq/envsubst from `argsh test`.

setup() {
  load "../test_helper"
  setup_tmpdir

  command -v jq &>/dev/null || skip "jq required"
  command -v yq &>/dev/null || skip "yq required"
  command -v envsubst &>/dev/null || skip "envsubst required"

  import() { :; }; export -f import
  :usage() { :; }; export -f :usage
  :args() { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"

  # The builder verifies the ssh private key file exists.
  SSH_KEY="${BATS_TEST_TMPDIR}/id_test"
  touch "${SSH_KEY}"

  DESCRIPTOR="${BATS_TEST_TMPDIR}/hetzner.json"

  # Seed manifest as kubeone::generate_config would leave it (cloudProvider
  # set; the builder appends apiEndpoint + hosts to it).
  MANIFEST="${BATS_TEST_TMPDIR}/kubeone.yaml"
  cat > "${MANIFEST}" <<'YAML'
apiVersion: kubeone.k8c.io/v1beta2
kind: KubeOneCluster
cloudProvider:
  hetzner: {}
YAML
}

teardown() { teardown_tmpdir; }

# _node <name> <role> — one provider::output inventory row
_node() {
  jq -n --arg n "$1" --arg r "$2" \
    '{name: $n, role: $r, group: "default", public_ip: "203.0.113.9",
      private_ip: "10.0.0.9", access: "default", ssh_user: "root", ssh_port: 22}'
}

# _stub_provider_output <nodes_json> — provider::output returns the standard
# envelope (api endpoint + access + the given nodes; empty network id).
_stub_provider_output() {
  _PO_JSON=$(jq -n --arg key "${SSH_KEY}" --argjson nodes "$1" \
    '{api: {endpoint: "198.51.100.1", privateEndpoint: "", port: 6443},
      access: [{id: "default", type: "ssh", user: "root", port: 22,
                privateKey: $key, publicKey: ""}],
      nodes: $nodes,
      network: {id: "", name: "", cidr: ""}}')
  provider::output() { echo "${_PO_JSON}"; }
}

@test "undeclared label-discovered VMs (machine-controller) are excluded from staticWorkers with a warning" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [
    { "name": "cp-0", "type": "cx43" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10" }
  ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node cp-0 control-plane)" \
    --argjson b "$(_node worker-0 worker)" \
    --argjson c "$(_node base-fsn1-6c7bd8b74dxkzmzc-abcde worker)" \
    '[$a, $b, $c]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success
  assert_output --partial "skipping base-fsn1-6c7bd8b74dxkzmzc-abcde (role=worker)"
  assert_output --partial "not declared in the descriptor's server[]"

  run yq -r '[.staticWorkers.hosts[].hostname] | join(",")' "${MANIFEST}"
  assert_output "worker-0"
}

@test "control-plane selection is descriptor-anchored too" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [ { "name": "cp-0", "type": "cx43" } ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node cp-0 control-plane)" \
    --argjson b "$(_node foreign-cp control-plane)" \
    '[$a, $b]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success
  assert_output --partial "skipping foreign-cp (role=control-plane)"

  run yq -r '[.controlPlane.hosts[].hostname] | join(",")' "${MANIFEST}"
  assert_output "cp-0"
  # the foreign CP's private IP must not leak into the apiserver SANs either
  run yq -r '.apiEndpoint.alternativeNames | length' "${MANIFEST}"
  assert_output "1"
}

@test "descriptor #labels are emitted as the host labels map (workers AND control planes)" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [
    { "name": "cp-0", "type": "cx43",
      "#labels": "role.kubehz.cloud/platform=true" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10",
      "#labels": "lok8s.dev/cluster=prod,tier.kubehz.cloud/pro=true,class.kubehz.cloud/metal=true" }
  ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node cp-0 control-plane)" \
    --argjson b "$(_node worker-0 worker)" \
    '[$a, $b]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success

  run yq -r '.staticWorkers.hosts[0].labels["lok8s.dev/cluster"]' "${MANIFEST}"
  assert_output "prod"
  run yq -r '.staticWorkers.hosts[0].labels["tier.kubehz.cloud/pro"]' "${MANIFEST}"
  assert_output "true"
  run yq -r '.staticWorkers.hosts[0].labels["class.kubehz.cloud/metal"]' "${MANIFEST}"
  assert_output "true"
  run yq -r '.controlPlane.hosts[0].labels["role.kubehz.cloud/platform"]' "${MANIFEST}"
  assert_output "true"
}

@test "a key- removal suffix (KubeOne addRemoveKeyValues) passes through as an empty value" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10",
      "#labels": "tier.kubehz.cloud/pro=true,old.example.com/retired-" }
  ] }
JSON
  _stub_provider_output "$(jq -n --argjson a "$(_node worker-0 worker)" '[$a]')"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success

  run yq -r '.staticWorkers.hosts[0].labels | has("old.example.com/retired-")' "${MANIFEST}"
  assert_output "true"
  run yq -r '.staticWorkers.hosts[0].labels["old.example.com/retired-"]' "${MANIFEST}"
  assert_output ""
}

@test "descriptor without #labels renders the previous host shape unchanged" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [
    { "name": "cp-0", "type": "cx43" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10" }
  ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node cp-0 control-plane)" \
    --argjson b "$(_node worker-0 worker)" \
    '[$a, $b]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success
  refute_output --partial "skipping"

  # no labels key on any host, kubelet reservations intact
  run yq -r '.controlPlane.hosts[0] | has("labels")' "${MANIFEST}"
  assert_output "false"
  run yq -r '.staticWorkers.hosts[0] | has("labels")' "${MANIFEST}"
  assert_output "false"
  run yq -r '.controlPlane.hosts[0].kubelet.maxPods' "${MANIFEST}"
  assert_output "110"
  run yq -r '.staticWorkers.hosts[0].kubelet.maxPods' "${MANIFEST}"
  assert_output "250"
  run yq -r '.staticWorkers.hosts[0].hostname' "${MANIFEST}"
  assert_output "worker-0"
  run yq -r '.staticWorkers.hosts[0].sshPrivateKeyFile' "${MANIFEST}"
  assert_output "${SSH_KEY}"
}

@test "a descriptor with no server[] refuses to build an inventory from labels alone" {
  echo '{ "cluster_name": "prod" }' > "${DESCRIPTOR}"
  _stub_provider_output "$(jq -n --argjson a "$(_node cp-0 control-plane)" '[$a]')"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_failure
  assert_output --partial "declares no server[]"
}

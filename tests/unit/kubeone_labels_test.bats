#!/usr/bin/env bats
# kubeone_labels_test.bats — the node-tier label plumbing: hetzner.json #labels
# -> provider::output node.labels -> kubeone staticWorkers/controlPlane
# hosts[].labels -> (kubeone labelNodes) -> Node labels.
#
# Pins the contract that _append_inventory renders a `labels:` block on each
# host from the provider node's `labels` map, sorted + deterministic, and emits
# NO block when a node carries no labels. This is what makes the
# tier.kubehz.cloud/* taxonomy reach (and survive `lo recover` on) the Node,
# since kubeone's labelNodes task syncs HostConfig.labels -> Node labels on both
# apply and recover.

setup() {
  load "../test_helper"
  setup_tmpdir

  # FAIL-HARD (not skip) if jq/yq are missing: these tests exercise the label
  # RENDER/PARSE path and are worthless without them. A silent skip would ship
  # a false-green suite ("1..N ok" that validated nothing). Run via
  # `PATH_BIN=$PWD/.bin ./.bin/argsh test …` (CI does; mounts the b toolchain).
  command -v jq &>/dev/null || { echo "FATAL: jq required — run tests with PATH_BIN=\$PWD/.bin (mounts the b toolchain)" >&2; return 1; }
  command -v yq &>/dev/null || { echo "FATAL: yq required — run tests with PATH_BIN=\$PWD/.bin (mounts the b toolchain)" >&2; return 1; }

  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  # Neutralize the argsh framework glue the driver sources at load time.
  import() { :; }; export -f import
  :usage() { :; }; export -f :usage
  :args()  { shift; }; export -f :args
  error()  { echo "ERROR: $*" >&2; return 1; }; export -f error
  debug()  { :; }; export -f debug

  # A dummy ssh private key so the inventory ssh-key preflight passes.
  export _SSH_KEY="${BATS_TEST_TMPDIR}/id"
  : > "${_SSH_KEY}"

  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"
}

teardown() { teardown_tmpdir; }

# Stub provider::output with a fixed inventory: one CP (no extra labels) + two
# workers, one carrying the full pilot tier taxonomy, one with none.
_stub_output() {
  provider::output() {
    cat <<JSON
{
  "api": {"endpoint": "lb.example", "privateEndpoint": "10.0.1.5", "port": 6443},
  "access": [{"privateKey": "${_SSH_KEY}"}],
  "nodes": [
    {"name": "cp-0", "role": "control-plane", "public_ip": "192.0.2.1", "private_ip": "10.0.1.1", "ssh_user": "root", "ssh_port": 22, "labels": {}},
    {"name": "w-0", "role": "worker", "public_ip": "192.0.2.10", "private_ip": "10.0.1.10", "ssh_user": "root", "ssh_port": 22,
     "labels": {"tier.kubehz.cloud/free": "true", "tier.kubehz.cloud/pro": "true", "capability.kubehz.cloud/monitoring": "true", "role.kubehz.cloud/platform": "true"}},
    {"name": "w-1", "role": "worker", "public_ip": "192.0.2.11", "private_ip": "10.0.1.11", "ssh_user": "root", "ssh_port": 22, "labels": {}},
    {"name": "w-evil", "role": "worker", "public_ip": "192.0.2.12", "private_ip": "10.0.1.12", "ssh_user": "root", "ssh_port": 22,
     "labels": {"x.kubehz.cloud/inject": "a\"\n      role: control-plane\n      # b"}}
  ],
  "network": {"id": "", "name": "", "cidr": ""}
}
JSON
  }
  export -f provider::output
}

_render() {
  _stub_output
  local manifest="${BATS_TEST_TMPDIR}/kubeone.yaml"
  # Minimal base manifest (no cloudProvider.hetzner so the CCM/networkID branch
  # is skipped — we only exercise the host/label rendering).
  cat > "${manifest}" <<'YAML'
apiVersion: kubeone.k8c.io/v1beta2
kind: KubeOneCluster
cloudProvider:
  none: {}
YAML
  # The inventory is DESCRIPTOR-ANCHORED: only server[]-declared names are
  # emitted as hosts (kubeone_inventory_test.bats pins that contract), so the
  # descriptor must declare every stubbed node for the render under test.
  cat > "${BATS_TEST_TMPDIR}/config.json" <<'JSON'
{ "cluster_name": "test",
  "server": [
    { "name": "cp-0" }, { "name": "w-0" }, { "name": "w-1" }, { "name": "w-evil" }
  ] }
JSON
  _append_inventory "${BATS_TEST_TMPDIR}/config.json" "${manifest}"
  echo "${manifest}"
}

@test "worker with tier labels renders a labels: block synced to the Node" {
  manifest=$(_render)
  run yq -o json '.staticWorkers.hosts[] | select(.hostname == "w-0") | .labels' "${manifest}"
  assert_success
  assert_output --partial '"tier.kubehz.cloud/free": "true"'
  assert_output --partial '"tier.kubehz.cloud/pro": "true"'
  assert_output --partial '"capability.kubehz.cloud/monitoring": "true"'
  assert_output --partial '"role.kubehz.cloud/platform": "true"'
}

@test "worker with no labels renders NO labels block (kubelet still present)" {
  manifest=$(_render)
  run yq -r '.staticWorkers.hosts[] | select(.hostname == "w-1") | has("labels")' "${manifest}"
  assert_success
  assert_output "false"
  # kubelet block is still rendered for the label-less worker.
  run yq -r '.staticWorkers.hosts[] | select(.hostname == "w-1") | .kubelet.maxPods' "${manifest}"
  assert_output "250"
}

@test "control-plane with no extra labels renders NO labels block" {
  manifest=$(_render)
  run yq -r '.controlPlane.hosts[] | select(.hostname == "cp-0") | has("labels")' "${manifest}"
  assert_success
  assert_output "false"
}

@test "rendered labels are deterministic (sorted by key)" {
  manifest=$(_render)
  run yq -o json -I=0 '.staticWorkers.hosts[] | select(.hostname == "w-0") | (.labels | keys)' "${manifest}"
  assert_success
  # jq/yq sort_by(.key) => capability < role < tier(free) < tier(pro)
  assert_output '["capability.kubehz.cloud/monitoring","role.kubehz.cloud/platform","tier.kubehz.cloud/free","tier.kubehz.cloud/pro"]'
}

@test "the rendered manifest is valid YAML kubeone can parse" {
  manifest=$(_render)
  run yq -e '.staticWorkers.hosts | length' "${manifest}"
  assert_success
  assert_output "3"
}

@test "YAML injection: a value with a quote+newline stays a single escaped scalar (no manifest break)" {
  manifest=$(_render)
  # The whole manifest must still parse — a raw "\(.value)" interpolation would
  # break YAML here (the embedded `role: control-plane` / newline would smuggle
  # in structure). @json keeps it one escaped scalar.
  run yq -e '.staticWorkers.hosts | length' "${manifest}"
  assert_success
  assert_output "3"
  # The malicious value round-trips VERBATIM as the label value, not as injected
  # YAML structure: the literal string (quote + newline + would-be keys) is the
  # value of x.kubehz.cloud/inject and nothing more.
  run yq -o json '.staticWorkers.hosts[] | select(.hostname == "w-evil") | .labels["x.kubehz.cloud/inject"]' "${manifest}"
  assert_success
  assert_output $'"a\\"\\n      role: control-plane\\n      # b"'
  # And the injected text did NOT become a real key on the host.
  run yq -r '.staticWorkers.hosts[] | select(.hostname == "w-evil") | has("role")' "${manifest}"
  assert_output "false"
}

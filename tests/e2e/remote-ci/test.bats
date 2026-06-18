#!/usr/bin/env bats
# E2E: remote-ci — verify CI mode: repo sync + remote lo provision +
# build a trivial nginx app via tilt ci on the remote VM.
#
# Gated by E2E=1 AND E2E_REMOTE=1.
# Uses cloud.d/ci module for full toolchain install on the VM.

_E2E_SSH_KEY_DIR=""
_E2E_SERVER_IP=""

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  [[ "${E2E_REMOTE:-}" == "1" ]] || skip "e2e: set E2E_REMOTE=1 to run remote provider tests"

  if [[ -z "${HCLOUD_TOKEN:-}" ]] && [[ -f "${_PROJECT_ROOT}/.env" ]]; then
    local _token
    _token=$(grep -oP 'LOK8S_E2E_HCLOUD_TOKEN=\K.*' "${_PROJECT_ROOT}/.env" 2>/dev/null || echo "")
    [[ -z "${_token}" ]] || export HCLOUD_TOKEN="${_token}"
  fi

  e2e::require_tools docker kind kustomize yq hcloud rsync
  [[ -n "${HCLOUD_TOKEN:-}" ]] || skip "e2e: HCLOUD_TOKEN not set"

  local ctx
  ctx=$(hcloud context active 2>/dev/null || echo "")
  [[ -n "${ctx}" ]] || skip "e2e: no active hcloud context"

  e2e::init "${BATS_TEST_DIRNAME}" 130.lok8s.dev

  # Clean up stale resources
  if command -v hcloud &>/dev/null && [[ -n "${HCLOUD_TOKEN:-}" ]]; then
    local selector="lok8s.dev/cluster=e2e-ci"
    for resource in load-balancer server; do
      local ids
      ids=$(hcloud "${resource}" list -o json -l "${selector}" 2>/dev/null | jq -r '.[].id' 2>/dev/null || echo "")
      for id in ${ids}; do
        hcloud "${resource}" delete "${id}" 2>/dev/null || true
      done
    done
  fi

  # Generate ephemeral SSH keypair
  _E2E_SSH_KEY_DIR="${PATH_BASE}/.ssh-e2e"
  rm -rf "${_E2E_SSH_KEY_DIR}"
  mkdir -p "${_E2E_SSH_KEY_DIR}"
  ssh-keygen -t ed25519 -f "${_E2E_SSH_KEY_DIR}/id_ed25519" -N "" -q

  # Write hetzner.json with cloud.d/ci module for full toolchain
  local cluster_dir="${PATH_CLUSTERS}/130.lok8s.dev"
  cat > "${cluster_dir}/hetzner.json" <<JSON
{
  "cluster_name": "e2e-ci",
  "cloudInit": {
    "modules": "ci"
  },
  "ssh-key": [
    { "name": "e2e-ci", "public-key-from-file": "${_E2E_SSH_KEY_DIR}/id_ed25519.pub" }
  ],
  "network": [
    { "name": "e2e-ci", "ip-range": "10.0.0.0/8",
      "#subnets": [
        { "network-zone": "eu-central", "type": "cloud", "ip-range": "10.0.0.0/24" }
      ]
    }
  ],
  "server": [
    { "name": "e2e-ci-cp-0", "type": "cx33", "image": "ubuntu-24.04",
      "location": "fsn1", "ssh-key": [0], "network": 0,
      "label": "lok8s.dev/cluster=e2e-ci,lok8s.dev/role=control-plane"
    }
  ]
}
JSON

  # SSH config for the ephemeral key
  mkdir -p "${HOME}/.ssh"
  cat > "${HOME}/.ssh/config_e2e_ci" <<EOF
Host *
  IdentityFile ${_E2E_SSH_KEY_DIR}/id_ed25519
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  ConnectTimeout 10
  LogLevel ERROR
  ControlMaster auto
  ControlPath /tmp/lok8s-ssh-%h.sock
  ControlPersist 600
EOF
  if [[ -f "${HOME}/.ssh/config" ]]; then
    cp "${HOME}/.ssh/config" "${HOME}/.ssh/config.bak.e2e-ci"
  fi
  cat "${HOME}/.ssh/config_e2e_ci" > "${HOME}/.ssh/config.tmp"
  [[ ! -f "${HOME}/.ssh/config" ]] || cat "${HOME}/.ssh/config" >> "${HOME}/.ssh/config.tmp"
  mv "${HOME}/.ssh/config.tmp" "${HOME}/.ssh/config"

  e2e::provision

  # Cache the server IP for subsequent tests
  _E2E_SERVER_IP=$(hcloud server list -l "lok8s.dev/cluster=e2e-ci" -o json | jq -r '.[0].public_net.ipv4.ip')
  export _E2E_SERVER_IP
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"

  if [[ -z "${HCLOUD_TOKEN:-}" ]] && [[ -f "${_PROJECT_ROOT}/.env" ]]; then
    local _token
    _token=$(grep -oP 'LOK8S_E2E_HCLOUD_TOKEN=\K.*' "${_PROJECT_ROOT}/.env" 2>/dev/null || echo "")
    [[ -z "${_token}" ]] || export HCLOUD_TOKEN="${_token}"
  fi

  e2e::init "${BATS_TEST_DIRNAME}" 130.lok8s.dev
  e2e::destroy

  if command -v hcloud &>/dev/null && [[ -n "${HCLOUD_TOKEN:-}" ]]; then
    local selector="lok8s.dev/cluster=e2e-ci"
    for resource in load-balancer server placement-group firewall ssh-key network; do
      local ids
      ids=$(hcloud "${resource}" list -o json -l "${selector}" 2>/dev/null | jq -r '.[].id' 2>/dev/null || echo "")
      for id in ${ids}; do
        hcloud "${resource}" delete "${id}" 2>/dev/null || true
      done
    done
  fi

  if [[ -f "${HOME}/.ssh/config.bak.e2e-ci" ]]; then
    mv "${HOME}/.ssh/config.bak.e2e-ci" "${HOME}/.ssh/config"
  fi
  rm -f "${HOME}/.ssh/config_e2e_ci"
  rm -rf "${_E2E_SSH_KEY_DIR:-/tmp/nonexistent}"
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 130.lok8s.dev
  # Resolve server IP for each test (setup_file exports don't cross bats process boundary)
  _E2E_SERVER_IP=$(hcloud server list -l "lok8s.dev/cluster=e2e-ci" -o json | jq -r '.[0].public_net.ipv4.ip')
}

# Helper: run a command on the remote VM with the lok8s env.
# Usage: e2e_remote <command...>
e2e_remote() {
  ssh "root@${_E2E_SERVER_IP}" "cd /workspace && \
    export DOMAIN_NAME=130.lok8s.dev && \
    export PATH_BASE=/workspace && \
    export PATH_LOK8S=/workspace/.lok8s && \
    export PATH_CLUSTERS=/workspace/clusters && \
    export PATH_BIN=/workspace/.bin && \
    export KUBECONFIG=/workspace/.kubeconfig/e2e-ci.yaml && \
    export KUSTOMIZE_PLUGIN_HOME=/workspace/.kustomize && \
    export PATH=\"/workspace/.lok8s:/workspace/.bin:\${PATH}\" && \
    $*"
}

@test "hetzner VM was provisioned with CI tools" {
  run hcloud server list -l "lok8s.dev/cluster=e2e-ci" -o columns=name -o noheader
  assert_success
  assert_output --partial "e2e-ci-cp-0"
}

@test "repo was synced to the remote VM" {
  run ssh "root@${_E2E_SERVER_IP}" "test -f /workspace/.lok8s/lo && test -d /workspace/clusters"
  assert_success
}

@test "kind cluster is running on the remote VM" {
  run ssh "root@${_E2E_SERVER_IP}" "docker ps --filter 'name=e2e-ci-control-plane' --format '{{.Status}}'"
  assert_success
  assert_output --partial "Up"
}

@test "tilt ci builds the app and pod reaches Running" {
  # Run tilt ci on the remote VM — builds the image, pushes to
  # lok8s.local, deploys the kustomize target, waits for healthy.
  # tilt must run from the scenario dir (relative paths + services.yaml discovery)
  local scenario="/workspace/tests/e2e/remote-ci"
  run e2e_remote "cd ${scenario} && PATH_BASE=${scenario} PATH_CLUSTERS=${scenario}/clusters tilt ci --port 0"
  assert_success
  assert_output --partial "All workloads are healthy"

  # Verify the pod is actually Running in-cluster
  run e2e_remote "kubectl get pod -l app=app -o jsonpath='{.items[0].status.phase}'"
  assert_success
  assert_output --partial "Running"
}

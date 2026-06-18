#!/usr/bin/env bats
# E2E: remote-lo — verify Lo cluster provisioned on a remote Hetzner VM.
#
# Gated by E2E=1 AND E2E_REMOTE=1 (costs real money, ~3 min).
#
# The test generates an ephemeral SSH keypair, writes the provider
# config (hetzner.yaml) with the key path, and lets the provider
# create the VM with that key injected via cloud-init.

_E2E_SSH_KEY_DIR=""

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  [[ "${E2E_REMOTE:-}" == "1" ]] || skip "e2e: set E2E_REMOTE=1 to run remote provider tests"

  # Load token from .env
  if [[ -z "${HCLOUD_TOKEN:-}" ]] && [[ -f "${_PROJECT_ROOT}/.env" ]]; then
    local _token
    _token=$(grep -oP 'LOK8S_E2E_HCLOUD_TOKEN=\K.*' "${_PROJECT_ROOT}/.env" 2>/dev/null || echo "")
    [[ -z "${_token}" ]] || export HCLOUD_TOKEN="${_token}"
  fi

  e2e::require_tools docker kind kustomize yq hcloud ssh-keygen
  [[ -n "${HCLOUD_TOKEN:-}" ]] || skip "e2e: HCLOUD_TOKEN not set"

  local ctx
  ctx=$(hcloud context active 2>/dev/null || echo "")
  [[ -n "${ctx}" ]] || skip "e2e: no active hcloud context"

  e2e::init "${BATS_TEST_DIRNAME}" 129.lok8s.dev

  # Clean up any leftover resources from a previous failed run
  # (cloud-init only runs on first boot — stale VMs won't have
  # the updated sshd config).
  if command -v hcloud &>/dev/null && [[ -n "${HCLOUD_TOKEN:-}" ]]; then
    local selector="lok8s.dev/cluster=e2e-remote"
    for resource in load-balancer server; do
      local ids
      ids=$(hcloud "${resource}" list -o json -l "${selector}" 2>/dev/null | jq -r '.[].id' 2>/dev/null || echo "")
      for id in ${ids}; do
        hcloud "${resource}" delete "${id}" 2>/dev/null || true
      done
    done
  fi

  # Generate ephemeral SSH keypair for this test run
  _E2E_SSH_KEY_DIR="${PATH_BASE}/.ssh-e2e"
  rm -rf "${_E2E_SSH_KEY_DIR}"
  mkdir -p "${_E2E_SSH_KEY_DIR}"
  ssh-keygen -t ed25519 -f "${_E2E_SSH_KEY_DIR}/id_ed25519" -N "" -q

  # Write the provider config with the generated key path
  local cluster_dir="${PATH_CLUSTERS}/129.lok8s.dev"
  # Write a hetzner.json descriptor (the native format for the
  # provider's hetzner::create loop). The descriptor references
  # resources by index — e.g. server.ssh-key: [0] means "use
  # the first ssh-key entry".
  cat > "${cluster_dir}/hetzner.json" <<JSON
{
  "cluster_name": "e2e-remote",
  "ssh-key": [
    { "name": "e2e-remote", "public-key-from-file": "${_E2E_SSH_KEY_DIR}/id_ed25519.pub" }
  ],
  "network": [
    { "name": "e2e-remote", "ip-range": "10.0.0.0/8",
      "#subnets": [
        { "network-zone": "eu-central", "type": "cloud", "ip-range": "10.0.0.0/24" }
      ]
    }
  ],
  "server": [
    { "name": "e2e-remote-cp-0", "type": "cx33", "image": "ubuntu-24.04",
      "location": "fsn1", "ssh-key": [0], "network": 0,
      "label": "lok8s.dev/cluster=e2e-remote,lok8s.dev/role=control-plane"
    }
  ]
}
JSON

  # Configure SSH to use the generated key + skip host verification
  export GIT_SSH_COMMAND="ssh -i ${_E2E_SSH_KEY_DIR}/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
  mkdir -p "${HOME}/.ssh"
  cat > "${HOME}/.ssh/config_e2e_remote" <<EOF
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
  # Prepend to SSH config so Docker's SSH transport picks it up
  if [[ -f "${HOME}/.ssh/config" ]]; then
    cp "${HOME}/.ssh/config" "${HOME}/.ssh/config.bak.e2e"
  fi
  cat "${HOME}/.ssh/config_e2e_remote" > "${HOME}/.ssh/config.tmp"
  [[ ! -f "${HOME}/.ssh/config" ]] || cat "${HOME}/.ssh/config" >> "${HOME}/.ssh/config.tmp"
  mv "${HOME}/.ssh/config.tmp" "${HOME}/.ssh/config"

  e2e::provision
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"

  # Reload token
  if [[ -z "${HCLOUD_TOKEN:-}" ]] && [[ -f "${_PROJECT_ROOT}/.env" ]]; then
    local _token
    _token=$(grep -oP 'LOK8S_E2E_HCLOUD_TOKEN=\K.*' "${_PROJECT_ROOT}/.env" 2>/dev/null || echo "")
    [[ -z "${_token}" ]] || export HCLOUD_TOKEN="${_token}"
  fi

  e2e::init "${BATS_TEST_DIRNAME}" 129.lok8s.dev
  e2e::destroy

  # Belt and braces: clean up Hetzner resources by label
  if command -v hcloud &>/dev/null && [[ -n "${HCLOUD_TOKEN:-}" ]]; then
    local selector="lok8s.dev/cluster=e2e-remote"
    for resource in load-balancer server placement-group firewall ssh-key network; do
      local ids
      ids=$(hcloud "${resource}" list -o json -l "${selector}" 2>/dev/null | jq -r '.[].id' 2>/dev/null || echo "")
      for id in ${ids}; do
        hcloud "${resource}" delete "${id}" 2>/dev/null || true
      done
    done
  fi

  # Restore SSH config
  if [[ -f "${HOME}/.ssh/config.bak.e2e" ]]; then
    mv "${HOME}/.ssh/config.bak.e2e" "${HOME}/.ssh/config"
  fi
  rm -f "${HOME}/.ssh/config_e2e_remote"

  # Clean up ephemeral SSH key
  rm -rf "${_E2E_SSH_KEY_DIR:-/tmp/nonexistent}"
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 129.lok8s.dev
}

@test "hetzner VM was provisioned" {
  run hcloud server list -l "lok8s.dev/cluster=e2e-remote" -o columns=name -o noheader
  assert_success
  assert_output --partial "e2e-remote-cp-0"
}

@test "kind cluster is running on the remote VM" {
  # Set DOCKER_HOST to the remote VM (same as provision did)
  local server_ip
  server_ip=$(hcloud server list -l "lok8s.dev/cluster=e2e-remote" -o json | jq -r '.[0].public_net.ipv4.ip')
  export DOCKER_HOST="ssh://root@${server_ip}"

  run docker ps --filter "name=e2e-remote-control-plane" --format '{{.Status}}'
  assert_success
  assert_output --partial "Up"
}

@test "cluster is reachable via SSH tunnel" {
  # The kubeconfig is at .kubeconfig/<metadata.name>.yaml
  local kc="${PATH_BASE}/.kubeconfig/e2e-remote.yaml"
  assert [ -f "${kc}" ]

  # Should point at localhost (SSH tunnel), not the remote IP
  run cat "${kc}"
  assert_success
  assert_output --partial "127.0.0.1"

  run kubectl --kubeconfig "${kc}" get nodes
  assert_success
  assert_output --partial "Ready"
}

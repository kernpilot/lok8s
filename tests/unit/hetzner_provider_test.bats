#!/usr/bin/env bats
# hetzner_provider_test.bats — the OPTIONAL Hetzner provider hooks
# provider::rebuild (reset existing nodes to a fresh-install state) and
# provider::doctor (read-only, advisory infrastructure diagnosis).
#
# All external effects (hcloud CLI, Robot REST via curl, SSH probes,
# ssh-keygen) are replaced with recording fakes — no real network, no real
# node is ever touched. The fakes log their argv so the tests can assert
# exactly which commands were issued (CP-vs-worker selection, descriptor
# image/cloud-init sourcing, rescue/reset, idempotence, failure propagation)
# and that NOTHING destructive (disk wipe) is ever run.

setup() {
  load "../test_helper"
  setup_tmpdir

  # Point PATH_LOK8S at the REAL framework so the provider main sources its
  # real json.sh/resources.sh/cloud-config and the framework cloud-init lib.
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export CLOUD_LOG_FILE="${BATS_TEST_TMPDIR}/hetzner.log"

  import() { :; }
  export -f import

  # Recording fakes' log files.
  export HCLOUD_LOG="${BATS_TEST_TMPDIR}/hcloud.log"
  export CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
  export SSH_LOG="${BATS_TEST_TMPDIR}/ssh.log"
  : > "${HCLOUD_LOG}"; : > "${CURL_LOG}"; : > "${SSH_LOG}"

  # Robot API base + creds (overridable defaults consumed by the provider).
  export HROBOT_API="https://robot.test"
  export HROBOT_USER="ruser" HROBOT_PASSWORD="rpass"
  export HCLOUD_TOKEN="tok"

  # Barrier: fail fast in tests (no real waiting).
  export PROVIDER_REBUILD_WAIT_TRIES=3 PROVIDER_REBUILD_WAIT_INTERVAL=0

  _install_fakes
  _write_descriptor

  if ! command -v jq &>/dev/null; then skip "jq not available"; fi
  if ! command -v yq &>/dev/null; then skip "yq not available"; fi

  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
}

teardown() { teardown_tmpdir; }

# ── recording fakes ──────────────────────────────────────
_install_fakes() {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server list")
        echo '[{"name":"cp-0","status":"running","public_net":{"ipv4":{"ip":"192.0.2.10"}},"private_net":[{"ip":"10.0.0.3"}],"labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/role":"control-plane","lok8s.dev/group":"cp"}}]' ;;
      "load-balancer list") echo '[]' ;;
      "network list")       echo '[]' ;;
      "server-type list")   return 0 ;;
      "context active")     printf '%s' "${HCLOUD_CONTEXT:-}"; [[ -n "${HCLOUD_CONTEXT:-}" ]] ;;
      "server rebuild")     [[ "${HCLOUD_REBUILD_FAIL:-0}" == 1 ]] && return 1; return 0 ;;
      *) return 0 ;;
    esac
  }
  export -f hcloud

  curl() {
    printf '%s\n' "$*" >> "${CURL_LOG}"
    local args="$*"
    case "${args}" in
      *"/server"*)
        [[ "${ROBOT_UNREACHABLE:-0}" == 1 ]] && return 1
        echo '[{"server":{"server_name":"worker-0","server_number":12345,"server_ip":"203.0.113.10"}}]' ;;
      *"/key"*)
        echo "[{\"key\":{\"fingerprint\":\"${ROBOT_REG_FP:-de:ad:be:ef}\"}}]" ;;
      *"/boot/"*"/rescue"*) [[ "${ROBOT_RESCUE_FAIL:-0}" == 1 ]] && return 1; return 0 ;;
      *"/reset/"*)          return 0 ;;
      *) return 0 ;;
    esac
  }
  export -f curl

  ssh() {
    printf '%s\n' "$*" >> "${SSH_LOG}"
    [[ "${SSH_UNREACHABLE:-0}" == 1 ]] && return 1
    local last="${!#}"
    case "${last}" in
      *installimage*) [[ "${SSH_RESCUE:-1}" == 1 ]] && return 0 || return 1 ;;
      *) return 0 ;;
    esac
  }
  export -f ssh

  ssh-keygen() { printf '256 MD5:%s test (ED25519)\n' "${STUB_FP:-aa:bb:cc:dd}"; }
  export -f ssh-keygen
}

# A minimal 2-node descriptor: 1 cloud CP + 1 bare-metal worker.
_write_descriptor() {
  CFGDIR="${BATS_TEST_TMPDIR}/clusters/test.example"
  mkdir -p "${CFGDIR}/cloud-init" "${CFGDIR}/pub"
  printf 'ssh-ed25519 AAAAtest test\n' > "${CFGDIR}/pub/id.pub"
  : > "${BATS_TEST_TMPDIR}/id_key"
  DESC="${CFGDIR}/hetzner.json"
  WORK="${BATS_TEST_TMPDIR}/work"
  cat > "${DESC}" <<JSON
{
  "cluster_name": "test-cluster",
  "sshUser": "root",
  "sshPrivateKey": "${BATS_TEST_TMPDIR}/id_key",
  "sshPublicKey": "${CFGDIR}/pub/id.pub",
  "cloudInit": { "path": "cloud-init", "modules": "node", "sshPubPath": "${CFGDIR}/pub" },
  "server": [
    { "name": "cp-0", "type": "cx43", "image": "ubuntu-24.04", "#cloud.d": "node",
      "label": "lok8s.dev/cluster=test-cluster,lok8s.dev/role=control-plane" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10",
      "#internal-ip": "10.0.1.10", "#cloud.d": "node:worker",
      "#labels": "lok8s.dev/cluster=test-cluster,lok8s.dev/role=worker" }
  ]
}
JSON
}

# ── provider::rebuild ────────────────────────────────────

@test "provider::rebuild reimages the cloud CP and rescues the bare-metal worker" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success

  # Cloud CP → hcloud server rebuild with the descriptor's image.
  run cat "${HCLOUD_LOG}"
  assert_output --partial "server rebuild cp-0 --image ubuntu-24.04 --user-data-from-file"
  # The bare-metal worker is NOT rebuilt via hcloud.
  refute_output --partial "rebuild worker-0"

  # Bare-metal worker → Robot rescue (with the fingerprint) + hardware reset.
  run cat "${CURL_LOG}"
  assert_output --partial "/boot/12345/rescue"
  assert_output --partial "os=linux"
  assert_output --partial "authorized_key=de:ad:be:ef"
  assert_output --partial "/reset/12345"
  assert_output --partial "type=hw"
}

@test "provider::rebuild sources the image + cloud-init from the descriptor" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # Change the CP image → the rebuild command must follow the descriptor.
  yq -i -o json '.server[0].image = "debian-12"' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  run cat "${HCLOUD_LOG}"
  assert_output --partial "server rebuild cp-0 --image debian-12"

  # A real cloud-init file was generated and handed to hcloud.
  [ -f "${WORK}/rebuild-cp-0.cloud-init.yaml" ]
  run cat "${WORK}/rebuild-cp-0.cloud-init.yaml"
  assert_output --partial "#cloud-config"
}

@test "provider::rebuild derives the rescue fingerprint from the descriptor public key" {
  unset PROVIDER_ROBOT_RESCUE_FP
  export STUB_FP="11:22:33:44"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  run cat "${CURL_LOG}"
  assert_output --partial "authorized_key=11:22:33:44"
}

@test "provider::rebuild acts ONLY on descriptor nodes (no wide label sweep)" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  # Exactly one rebuild issued (cp-0), targeting the named server.
  run grep -c 'server rebuild' "${HCLOUD_LOG}"
  assert_output "1"
}

@test "provider::rebuild never runs a disk wipe" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  # No destructive wipe reached any of the fakes.
  run cat "${HCLOUD_LOG}" "${CURL_LOG}" "${SSH_LOG}"
  refute_output --partial "blkdiscard"
  refute_output --partial "wipefs"
  refute_output --partial "wipe-devices"
}

@test "provider::rebuild source carries no disk-wipe command" {
  run grep -nE 'blkdiscard|wipefs|dd +if=' "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
  assert_failure
}

@test "provider::rebuild is idempotent-safe (re-runs succeed)" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
}

@test "provider::rebuild returns non-zero when a cloud rebuild fails" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  export HCLOUD_REBUILD_FAIL=1
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
}

@test "provider::rebuild returns non-zero when Robot rescue fails" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  export ROBOT_RESCUE_FAIL=1
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
}

@test "provider::rebuild fails a bare-metal node without Robot creds" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  unset HROBOT_USER HROBOT_PASSWORD
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "needs Robot creds"
}

@test "provider::rebuild fails on a missing config file" {
  run provider::rebuild "${BATS_TEST_TMPDIR}/nope.json" "${WORK}"
  assert_failure
  assert_output --partial "config not found"
}

# ── provider::doctor ─────────────────────────────────────

@test "provider::doctor warns (never dies) when credentials are missing" {
  unset HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD
  export HCLOUD_CONTEXT=""
  run provider::doctor "${DESC}"
  assert_success
  assert_output --partial "no hcloud credentials"
  assert_output --partial "Robot creds unset"
  assert_output --partial "summary"
}

@test "provider::doctor reports all green when creds work + node in rescue" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef" SSH_RESCUE=1
  run provider::doctor "${DESC}"
  assert_success
  assert_output --partial $'ok\thcloud API reachable'
  assert_output --partial $'ok\tRobot API reachable'
  assert_output --partial "rescue SSH key registered in Robot"
  assert_output --partial "control-plane node(s) resolved"
  assert_output --partial "worker worker-0 resolvable in Robot"
  assert_output --partial "worker worker-0 is in rescue"
  assert_output --partial "cloud node cp-0 present"
}

@test "provider::doctor warns when the rescue key is not registered in Robot" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="00:11:22:33"
  run provider::doctor "${DESC}"
  assert_success
  assert_output --partial "NOT registered in Robot"
}

@test "provider::doctor advises when a worker is installed, not in rescue" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef" SSH_RESCUE=0
  run provider::doctor "${DESC}"
  assert_success
  assert_output --partial "installed, not in rescue"
  assert_output --partial "set HROBOT_USER/HROBOT_PASSWORD to reset it automatically"
}

@test "provider::doctor is strictly read-only (no destructive command)" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef"
  run provider::doctor "${DESC}"
  assert_success
  # No rebuild/reset/rescue activation, ever.
  refute_output --partial "server rebuild"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "rebuild"
  refute_output --partial "delete"
  run cat "${CURL_LOG}"
  refute_output --partial "/reset/"
  refute_output --partial "/rescue"
  run cat "${SSH_LOG}"
  refute_output --partial "blkdiscard"
  refute_output --partial "installimage -a"
}

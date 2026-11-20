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
  # `import` is stubbed above, so anything the provider imports must be sourced
  # here explicitly. utils/template supplies template::descriptor_json, which
  # replaced the envsubst/yq BINARY calls the provider used to make — a shell
  # function needs loading where a binary did not.
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  # The Robot calls write their config with http::curl_config.
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  # Recording fakes' log files.
  export HCLOUD_LOG="${BATS_TEST_TMPDIR}/hcloud.log"
  export CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
  export SSH_LOG="${BATS_TEST_TMPDIR}/ssh.log"
  : > "${HCLOUD_LOG}"; : > "${CURL_LOG}"; : > "${SSH_LOG}"

  # Robot API base + creds (overridable defaults consumed by the provider).
  export HROBOT_API="https://robot.test"
  export HROBOT_USER="ruser" HROBOT_PASSWORD="rpass"
  export HCLOUD_TOKEN="tok"
  # The domain lo exports for the owner mark (provider::write_config).
  export PROVIDER_DOMAIN="test.example"

  # Barrier: fail fast in tests (no real waiting).
  export PROVIDER_REBUILD_WAIT_TRIES=3 PROVIDER_REBUILD_WAIT_INTERVAL=0

  _install_fakes
  _write_descriptor

  if ! command -v jq &>/dev/null; then skip "jq not available"; fi
  if ! command -v yq &>/dev/null; then skip "yq not available"; fi

  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
  # The owner mark of test-cluster (sha256 of "<domain>\n<cluster_name>\n",
  # 32 hex chars): the fakes give it to the parts of this cluster.
  MARK="$(printf '%s\n%s\n' "${PROVIDER_DOMAIN}" test-cluster | sha256sum | cut -c1-32)"
  OWN=",lok8s.dev/owner=${MARK}"
  export MARK OWN
}

teardown() { teardown_tmpdir; }

# ── recording fakes ──────────────────────────────────────
_install_fakes() {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server list")
        echo '[{"id":10,"name":"cp-0","status":"running","public_net":{"ipv4":{"ip":"192.0.2.10"}},"private_net":[{"ip":"10.0.0.3"}],"labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/role":"control-plane","lok8s.dev/group":"cp","lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      # The rebuild reads the server by its id right before the reimage.
      "server describe")
        if [[ -n "${SERVER_DESCRIBE:-}" ]]; then
          echo "${SERVER_DESCRIBE}"
        else
          echo '{"id":10,"name":"cp-0","labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/owner":"'"${MARK}"'"}}'
        fi ;;
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
    # Simulate a Robot HTTP error: `curl --fail` exits non-zero on 4xx/5xx, while
    # a curl WITHOUT --fail exits 0 and hands back an error body (the pre-fix
    # footgun). Honouring -f here proves the code actually passes --fail.
    if [[ -n "${ROBOT_HTTP_ERROR:-}" ]]; then
      [[ " ${args} " == *" -f "* ]] && return 22
      echo '{"error":{"status":500,"code":"INTERNAL_ERROR"}}'; return 0
    fi
    case "${args}" in
      *"/server"*)
        [[ "${ROBOT_UNREACHABLE:-0}" == 1 ]] && return 1
        if [[ -n "${ROBOT_SERVER_JSON:-}" ]]; then
          echo "${ROBOT_SERVER_JSON}"
        else
          echo '[{"server":{"server_name":"worker-0","server_number":12345,"server_ip":"203.0.113.10"}}]'
        fi ;;
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

  # -y derives the public key of a private key, the way the real ssh-keygen
  # prints it ("<type> <base64> <comment>"); a key with a passphrase fails.
  ssh-keygen() {
    if [[ " $* " == *" -y "* ]]; then
      printf '%s\n' "$*" >> "${SSH_LOG}"
      [[ -z "${STUB_KEYGEN_Y_FAIL:-}" ]] || { echo "Load key \"${!#}\": incorrect passphrase supplied to decrypt private key" >&2; return 255; }
      printf '%s\n' "${STUB_DERIVED_PUB:-ssh-ed25519 AAAAtest root@host}"
      return 0
    fi
    printf '256 MD5:%s test (ED25519)\n' "${STUB_FP:-aa:bb:cc:dd}"
  }
  export -f ssh-keygen
}

# A minimal 2-node descriptor: 1 cloud CP + 1 bare-metal worker.
_write_descriptor() {
  CFGDIR="${BATS_TEST_TMPDIR}/clusters/test.example"
  mkdir -p "${CFGDIR}/cloud-init" "${CFGDIR}/pub"
  printf 'ssh-ed25519 AAAAtest test\n' > "${CFGDIR}/pub/id.pub"
  # A private key that only its owner can access, as ssh requires.
  : > "${BATS_TEST_TMPDIR}/id_key"
  chmod 600 "${BATS_TEST_TMPDIR}/id_key"
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
  # By the checked id of cp-0 (10), never by its name.
  assert_output --partial "server rebuild 10 --image=ubuntu-24.04 --user-data-from-file="
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
  assert_output --partial "server rebuild 10 --image=debian-12 "

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

@test "provider::rebuild is safe to re-run (retry re-drives the plan; no first-run-only state)" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # A recovery must be safe to RETRY. rebuild holds no first-run-only state, so a
  # second invocation re-issues the SAME destructive plan and still succeeds.
  # (This is retry-safety, NOT true idempotence: the fakes are stateless, so a
  # genuine "already in the target state → no-op" cannot be asserted here.)
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  # The retry actually re-issued the cloud reimage (it did not skip / error on
  # stale state) — the accumulating log holds one 'server rebuild 10' per run.
  run grep -c 'server rebuild 10 ' "${HCLOUD_LOG}"
  assert_output "2"
}

@test "provider::rebuild honors CLOUD_DRY_RUN: prints the plan, reimages NOTHING" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  export CLOUD_DRY_RUN=1 CLOUD_DRY_RUN_PATH="${BATS_TEST_TMPDIR}/dry-run"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success

  # The full plan is logged (what WOULD happen): the cloud reimage command, the
  # bare-metal rescue+reset, and an explicit dry-run marker.
  assert_output --partial "hcloud server rebuild 10 --image=ubuntu-24.04 "
  assert_output --partial "dry-run"
  assert_output --partial "would activate Robot rescue on robot#12345"
  assert_output --partial "would hardware-reset robot#12345"

  # … but NO destructive fake ran. The cloud VM was NOT reimaged (dry-run::cmd
  # printed instead of invoking hcloud), so the hcloud log has no 'server rebuild'.
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  # The Robot worker was NOT rescued/reset — no POST reached the curl fake.
  run cat "${CURL_LOG}"
  refute_output --partial "/boot/12345/rescue"
  refute_output --partial "/reset/12345"
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

@test "provider::rebuild resolves a Robot name collision BY IP and never touches the other box" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # Two Robot servers share the free-text server_name "worker-0" but have
  # different IPs. Only 203.0.113.10 matches the descriptor #external-ip.
  export ROBOT_SERVER_JSON='[
    {"server":{"server_name":"worker-0","server_number":99999,"server_ip":"198.51.100.99"}},
    {"server":{"server_name":"worker-0","server_number":12345,"server_ip":"203.0.113.10"}}
  ]'
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success

  run cat "${CURL_LOG}"
  # The IP-matching box (robot#12345) is the ONLY one rescued + reset.
  assert_output --partial "/boot/12345/rescue"
  assert_output --partial "/reset/12345"
  # The name-colliding, non-matching box (robot#99999) is NEVER touched.
  refute_output --partial "/boot/99999"
  refute_output --partial "/reset/99999"
}

@test "provider::rebuild atomic preflight: an unresolvable node blocks ALL destructive calls" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # worker-0's #external-ip (203.0.113.10) matches no Robot server → it can't be
  # resolved. Preflight must abort BEFORE reimaging the (valid) cloud CP.
  export ROBOT_SERVER_JSON='[{"server":{"server_name":"other","server_number":55555,"server_ip":"198.51.100.1"}}]'
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure

  # Node 1 (cp-0) is NEVER reimaged — no partial recovery.
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  # Node 2 (worker-0) is NEVER rescued/reset either.
  run cat "${CURL_LOG}"
  refute_output --partial "/reset/"
  refute_output --partial "/rescue"
}

@test "provider::rebuild aborts (no destructive call) when the Robot API errors" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # A --fail curl exits non-zero on HTTP error → the IP resolution fails →
  # preflight aborts. Without --fail the error would slip through as success.
  export ROBOT_HTTP_ERROR=1
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure

  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  run cat "${CURL_LOG}"
  refute_output --partial "/reset/"
  refute_output --partial "/rescue"
}

# A readonly TLS log or trace variable stops each Robot call before curl
# runs (utils/http.sh, http::tls_log_vars_unset). The IP lookup sends its
# stderr to /dev/null, so the rebuild names the variable before the first
# Robot call, and does not blame the lookup.
_rebuild_readonly() {
  readonly SSLKEYLOGFILE
  provider::rebuild "${DESC}" "${WORK}"
}

@test "provider::rebuild names a readonly TLS log variable and makes no Robot call" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  run _rebuild_readonly
  assert_failure
  assert_output --partial "SSLKEYLOGFILE is readonly, so lo cannot clear it for curl: the request does not run"
  refute_output --partial "did not resolve to exactly one Robot server"
  [ ! -s "${CURL_LOG}" ]
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

@test "provider::rebuild rejects a hostile server name (shell/regex metachars) before acting" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # A name with shell metacharacters must be refused by the name guard.
  yq -i -o json '.server[0].name = "cp-0; reboot"' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "refusing suspicious server name"

  # Nothing destructive was issued.
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  run cat "${CURL_LOG}"
  refute_output --partial "/reset/"
  refute_output --partial "/rescue"
}

@test "provider::rebuild fails a rebuild target with no resolvable IP (barrier can't verify it)" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # Drop worker-0's #external-ip → it resolves to no inventory IP. A node
  # scheduled for rebuild MUST have a resolvable IP, so this aborts non-zero
  # instead of silently dropping it from the barrier's wait set.
  yq -i -o json 'del(.server[1]["#external-ip"])' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "no resolvable public IP"

  # Atomic: the cloud CP is not reimaged either.
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
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

@test "provider::doctor distinguishes an errored Robot API from a missing key" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef"
  # The Robot API returns an HTTP error for every call. --fail makes doctor treat
  # it as unreachable — it must NOT report the rescue key as "not registered".
  export ROBOT_HTTP_ERROR=1
  run provider::doctor "${DESC}"
  assert_success
  assert_output --partial "Robot creds set but API not reachable"
  assert_output --partial "cannot verify rescue key"
  refute_output --partial "NOT registered in Robot"
}

# The doctor's Robot call sends its stderr to /dev/null. A readonly TLS
# log or trace variable stops that call, so the doctor names the variable
# and does not blame the Robot creds.
_doctor_readonly() {
  readonly SSLKEYLOGFILE
  provider::doctor "${DESC}"
}

@test "provider::doctor names a readonly TLS log variable, not the Robot creds" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef"
  run _doctor_readonly
  assert_success
  assert_output --partial $'warn\tSSLKEYLOGFILE is readonly, so lo cannot clear it for curl: no call to the Robot API'
  refute_output --partial "verify HROBOT_USER/HROBOT_PASSWORD"
  refute_output --partial $'ok\tRobot API reachable'
  assert_output --partial "cannot verify rescue key"
  [ ! -s "${CURL_LOG}" ]
}

_doctor_two_readonly() {
  readonly SSLKEYLOGFILE SSLTRACE
  provider::doctor "${DESC}"
}

# Two readonly names: one line that names both, in the plural.
@test "provider::doctor names two readonly TLS log variables in one line" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef"
  run _doctor_two_readonly
  assert_success
  assert_output --partial $'warn\tSSLKEYLOGFILE and SSLTRACE are readonly, so lo cannot clear them for curl: no call to the Robot API'
  refute_output --partial "verify HROBOT_USER/HROBOT_PASSWORD"
  [ ! -s "${CURL_LOG}" ]
}

_doctor_empty_list() {
  _HTTP_TLS_LOG_VARS=()
  provider::doctor "${DESC}"
}

# An empty list: the probe of the readonly names cannot check a name, so
# the doctor names the list and makes no Robot call, and does not blame the
# Robot credentials.
@test "provider::doctor names an empty TLS list, not the Robot creds" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef"
  run _doctor_empty_list
  assert_success
  assert_output --partial $'warn\tthe list of TLS log and trace variables (_HTTP_TLS_LOG_VARS) is empty or not set: no call to the Robot API'
  refute_output --partial "verify HROBOT_USER/HROBOT_PASSWORD"
  refute_output --partial $'ok\tRobot API reachable'
  [ ! -s "${CURL_LOG}" ]
}

# A failed provider::output is no "no control-plane node": the doctor goes
# on and shows the first error line of the output as the reason.
@test "provider::doctor shows why provider::output failed, and goes on" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" ROBOT_REG_FP="de:ad:be:ef" SSH_RESCUE=1
  unset PROVIDER_DOMAIN
  run provider::doctor "${DESC}"
  assert_success
  assert_output --partial $'warn\tinventory: provider::output failed, so no node is known: hetzner: the domain of the cluster is not known (PROVIDER_DOMAIN is empty).'
  refute_output --partial "no control-plane nodes resolved"
  assert_output --partial "worker worker-0 resolvable in Robot"
  assert_output --partial $'summary\t'
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
  # And NO Robot POST — every doctor Robot call is a GET (no -d / --data).
  refute_output --partial "--data"
  refute_output --partial "-d os"
  refute_output --partial "-d type"
  run cat "${SSH_LOG}"
  refute_output --partial "blkdiscard"
  refute_output --partial "installimage -a"
}

# ── a failed create stops the provision with Hetzner's reason ────────────────
# `hcloud server create` answered "error during placement
# (resource_unavailable)", the exit status was dropped, and the run walked on
# into the KubeOne inventory, which then blamed name drift.

@test "hetzner::create stops on a failed create, names the resource, and never runs the lookup" {
  export CLOUD_HETZNER_JSON='{"cluster_name":"test-cluster","server":[{"name":"cp-0","type":"cx33","location":"fsn1"}]}'
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server create") echo "hcloud: error during placement (resource_unavailable, a6c75d4a)" >&2; return 1 ;;
      "server list") echo '[]' ;;
    esac
    return 0
  }
  export -f hcloud
  run hetzner::create 'server'
  assert_failure
  assert_output --partial "could not create server 'cp-0'"
  assert_output --partial "resource_unavailable"
  run grep -c "^server create" "${HCLOUD_LOG}"
  assert_output "1"
  run grep -c "^server list" "${HCLOUD_LOG}"
  assert_output "0"
}

@test "hetzner::create runs the lookup after a create that succeeded" {
  export CLOUD_HETZNER_JSON='{"cluster_name":"test-cluster","server":[{"name":"cp-0","type":"cx33","location":"fsn1"}]}'
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server list") echo '[{"name":"cp-0","id":42,"labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
    esac
    return 0
  }
  export -f hcloud
  run hetzner::create 'server'
  assert_success
  run grep -c "^server create" "${HCLOUD_LOG}"
  assert_output "1"
  run grep -c "^server list" "${HCLOUD_LOG}"
  assert_output "1"
}

# ── boolean descriptor keys ──────────────────────────────────────
# A boolean value is a bare hcloud switch. `without-ipv6: true` used to become
# `--without-ipv6 true`, which hcloud read as a stray argument; it is the one
# token `--without-ipv6` now, and `false` stays explicit (`--without-ipv6=false`).
@test "hetzner::create passes a boolean key as one token, never a stray value" {
  export CLOUD_HETZNER_JSON='{"cluster_name":"test-cluster","server":[{"name":"w-1","type":"cx23","image":"ubuntu-24.04","without-ipv6":true,"label":"lok8s.dev/cluster=t"}]}'
  run hetzner::create 'server'
  assert_success
  run grep -E '^server create' "${HCLOUD_LOG}"
  assert_success
  assert_output --partial ' --without-ipv6 '
  refute_output --partial '--without-ipv6 true'
  refute_output --regexp 'without-ipv6 +(true|"")'
  # Adjacency: the next flag follows the switch directly — no placeholder
  # reached hcloud and no pair shifted (a swallowed --label would show here).
  assert_output --partial '--without-ipv6 --label=lok8s.dev/cluster=t'
  assert_output --partial '--type=cx23'
}

@test "hetzner::create keeps a false boolean explicit (--key=false) and other values verbatim" {
  export CLOUD_HETZNER_JSON='{"cluster_name":"test-cluster","server":[{"name":"w-2","type":"cx23","image":"ubuntu-24.04","without-ipv6":false}]}'
  run hetzner::create 'server'
  assert_success
  run grep -E '^server create' "${HCLOUD_LOG}"
  assert_output --partial '--without-ipv6=false'
  assert_output --partial '--image=ubuntu-24.04'
}

# ── an in-place recreate over existing parts ─────────────────────────────────

# _load_descriptor <json>: CLOUD_HETZNER_JSON as provider::provision builds it:
# template::descriptor_json on a hetzner.json, then the lookup that writes the
# ids of the existing resources into it.
_load_descriptor() {
  local file="${BATS_TEST_TMPDIR}/lookup/hetzner.json"
  mkdir -p "${file%/*}"
  printf '%s\n' "${1}" > "${file}"
  CLOUD_HETZNER_JSON="$(template::descriptor_json "${file}" | jq -c '.cluster_name //= "test-cluster"')"
  hetzner::lookup >/dev/null
}

# _lookup_hcloud: hcloud with existing resources (the `<resource> list -o json`
# shape the lookup reads: an array of objects with id and name).
_lookup_hcloud() {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "ssh-key list") echo '[{"id":7,"name":"primary","public_key":"ssh-ed25519 AAAA","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      "network list") echo '[{"id":9,"name":"kubernetes","ip_range":"10.0.0.0/16","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      "server list")  echo '[{"id":42,"name":"cp-0","status":"running","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      "volume list")  echo '[]' ;;
      "image list")   echo '[{"id":12345678,"type":"snapshot","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      *) return 0 ;;
    esac
  }
  export -f hcloud
}

# Finding 1: every number was read as an index into another resource list, so
# `"size": 100` on a volume reached hcloud as `--size null`.
@test "hetzner::create passes a number on a non-reference key verbatim and dereferences a reference key" {
  _lookup_hcloud
  _load_descriptor '{
    "ssh-key": [ { "name": "primary", "public-key-from-file": "/dev/null" } ],
    "network": [ { "name": "kubernetes", "ip-range": "10.0.0.0/16" } ],
    "server": [
      { "name": "cp-0", "type": "cx43", "image": "ubuntu-24.04", "ssh-key": [0], "network": 0 },
      { "name": "cp-1", "type": "cx43", "image": 12345678, "ssh-key": [0], "network": 0 }
    ],
    "volume": [ { "name": "data-0", "size": 100, "server": 0, "label": "lok8s.dev/cluster=t" } ]
  }'
  : > "${HCLOUD_LOG}"
  run hetzner::create 'server'
  assert_success
  run hetzner::create 'volume'
  assert_success

  # A snapshot image id is a literal; ssh-key and network are references.
  run grep -E '^server create' "${HCLOUD_LOG}"
  assert_output "server create --name=cp-1 --type=cx43 --image=12345678 --ssh-key=7 --network=9 --label=lok8s.dev/owner=${MARK}"
  # The size is a literal; the server is a reference (cp-0 has the id 42).
  run grep -E '^volume create' "${HCLOUD_LOG}"
  assert_output "volume create --name=data-0 --size=100 --server=42 --label=lok8s.dev/cluster=t --label=lok8s.dev/owner=${MARK}"
}

@test "hetzner::create gives a bare-metal hook a literal number verbatim and a reference as its id" {
  _lookup_hcloud
  _load_descriptor '{
    "ssh-key": [ { "name": "primary", "public-key-from-file": "/dev/null" } ],
    "server": [ { "name": "metal-0", "#cloud.root": "true", "#external-ip": "203.0.113.10",
                  "#ssh-port": 2222, "ssh-key": [0] } ]
  }'
  export FIELDS_LOG="${BATS_TEST_TMPDIR}/fields.log"
  hetzner::create::hook() { printf '%s\n' "${fields[@]}" > "${FIELDS_LOG}"; }
  run hetzner::create 'server'
  assert_success
  run cat "${FIELDS_LOG}"
  assert_output --partial $'--#ssh-port\n2222\n'
  assert_output --partial $'--ssh-key\n7'
  refute_output --partial 'null'
}

# Finding 2: `hcloud server rebuild` keeps only the creation-time SSH keys. A
# VM that lok8s did not create came back with no key lok8s can use.
_descriptor_pubkey() {
  printf 'ssh-ed25519 AAAAC3NzaDescriptorKey lok8s "test": x\n' > "${CFGDIR}/descriptor.pub"
  yq -i -o json ".sshPublicKey = \"${CFGDIR}/descriptor.pub\"" "${DESC}"
  # sshPrivateKey is the private half of this key
  export STUB_DERIVED_PUB="ssh-ed25519 AAAAC3NzaDescriptorKey operator@laptop"
}

@test "provider::rebuild authorizes the descriptor sshPublicKey in the cloud VM user-data" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  _descriptor_pubkey
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success

  # Root access: a top-level list, the key without its comment.
  run yq -r '.ssh_authorized_keys | join("|")' "${WORK}/rebuild-cp-0.cloud-init.yaml"
  assert_success
  assert_output 'ssh-ed25519 AAAAC3NzaDescriptorKey'
  run cat "${HCLOUD_LOG}"
  assert_output --partial "server rebuild 10 --image=ubuntu-24.04 --user-data-from-file=${WORK}/rebuild-cp-0.cloud-init.yaml"
}

@test "provider::rebuild gives a dedicated access user the descriptor sshPublicKey" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  _descriptor_pubkey
  yq -i -o json '.cloudInit.user = "lok8s"' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  run yq -r '.users[0].name + "=" + (.users[0]["ssh-authorized-keys"] | join("|"))' "${WORK}/rebuild-cp-0.cloud-init.yaml"
  assert_output --partial 'lok8s='
  assert_output --partial 'ssh-ed25519 AAAAC3NzaDescriptorKey'
  run yq -r 'has("ssh_authorized_keys")' "${WORK}/rebuild-cp-0.cloud-init.yaml"
  assert_output 'false'
}

@test "provider::rebuild stops before any reimage when sshPublicKey cannot be read" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  yq -i -o json ".sshPublicKey = \"${CFGDIR}/missing.pub\"" "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "rebuild: cannot read sshPublicKey '${CFGDIR}/missing.pub'"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  run cat "${CURL_LOG}"
  refute_output --partial "/rescue"
  refute_output --partial "/reset/"
}

@test "provider::rebuild stops before any reimage when sshPublicKey holds no public key" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  printf 'not a key\n' > "${CFGDIR}/bad.pub"
  yq -i -o json ".sshPublicKey = \"${CFGDIR}/bad.pub\"" "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "holds no OpenSSH public key"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

# Finding 3: a cloudInit.path that does not exist fell back to the framework
# default cloud-init without a message.
@test "provider::provision stops when cloudInit.path does not exist, before any hcloud call" {
  yq -i -o json '.cloudInit.path = "no-such-dir"' "${DESC}"
  run provider::provision "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: cloudInit.path 'no-such-dir' does not exist: ${CFGDIR}/no-such-dir."
  run cat "${HCLOUD_LOG}"
  assert_output ""
}

@test "provider::rebuild stops when cloudInit.path is not a directory, before any reimage" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  : > "${CFGDIR}/a-file"
  yq -i -o json '.cloudInit.path = "a-file"' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: cloudInit.path 'a-file' is not a directory: ${CFGDIR}/a-file."
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

@test "hetzner::_export_cloudinit_env uses an existing cloudInit.path, and the default without one" {
  export PROVIDER_CONFIG_FILE="${DESC}"
  hetzner::_export_cloudinit_env "${DESC}"
  [ "${CLOUD_PATH}" = "${CFGDIR}/cloud-init" ]
  yq -i -o json 'del(.cloudInit.path)' "${DESC}"
  hetzner::_export_cloudinit_env "${DESC}"
  [ "${CLOUD_PATH}" = "${PATH_LOK8S}/providers/hetzner/cloud-init" ]
}

# Finding 4 (issue #296): an adopted load balancer kept its old targets and
# services. add-target answered "already", so a target stayed private after
# the descriptor dropped use-private-ip.
_lb_descriptor() {
  _load_descriptor '{
    "server": [ { "name": "cp-0", "type": "cx43", "image": "ubuntu-24.04" } ],
    "load-balancer": [ { "name": "api-lb", "type": "lb11", "location": "fsn1",
      "label": "lok8s.dev/cluster=t",
      "#targets": [ '"${1}"' ],
      "#services": [ { "protocol": "tcp", "listen-port": "6443", "destination-port": "'"${2:-6443}"'" } ] } ]
  }'
  : > "${HCLOUD_LOG}"
}

# _lb_hcloud: the LB api-lb exists. `describe -o json` prints LIVE_LB (the API
# schema of hcloud-go: targets[].type label_selector|server|ip, use_private_ip,
# services[].listen_port/destination_port). LB_FAIL=<verb> fails that verb.
# LB_DESCRIBE_STDERR=<text>: the load-balancer describe also writes this line
# to stderr, and succeeds. LB_ADD_ALREADY=<error>: the first add-target is
# done, but its answer is lost (it fails); every later add-target fails with
# this error and changes nothing. LB_REREAD_FAIL=1 (with LB_ADD_ALREADY): the
# first describe after the first such error fails with a rate limit; every
# other describe succeeds.
_lb_hcloud() {
  export LB_ADOPTED='[{"id":5,"name":"api-lb","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]'
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "load-balancer create")   : > "${BATS_TEST_TMPDIR}/lb-created" ;;
      "load-balancer list")
        if [[ -f "${BATS_TEST_TMPDIR}/lb-created" ]]; then
          echo '[{"id":6,"name":"api-lb","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]'
        else
          echo "${LB_LIST:-${LB_ADOPTED}}"
        fi ;;
      "load-balancer describe")
        [[ -z "${LB_DESCRIBE_STDERR:-}" ]] || printf '%s\n' "${LB_DESCRIBE_STDERR}" >&2
        if [[ -f "${BATS_TEST_TMPDIR}/lb-reread-fail" ]]; then
          rm -f "${BATS_TEST_TMPDIR}/lb-reread-fail"
          echo "hcloud: limit of requests reached (rate_limit_exceeded)" >&2
          return 1
        fi
        if [[ -f "${BATS_TEST_TMPDIR}/lb-created" ]]; then
          echo '{"id":6,"name":"api-lb","labels":{"lok8s.dev/owner":"'"${MARK}"'"},"targets":[],"services":[]}'
          return 0
        fi
        [[ -n "${LIVE_LB:-}" ]] || { echo "hcloud: load balancer not found: ${3}" >&2; return 1; }
        if [[ -n "${LIVE_LB_AFTER:-}" && -f "${BATS_TEST_TMPDIR}/lb-changed" ]]; then echo "${LIVE_LB_AFTER}"; else echo "${LIVE_LB}"; fi ;;
      "load-balancer add-target")
        # LB_ADD_FAILS=<n>: the first n add-target calls fail.
        local n; n=$(grep -c '^load-balancer add-target' "${HCLOUD_LOG}")
        (( n > ${LB_ADD_FAILS:-0} )) || { echo "hcloud: target not found (server_error)" >&2; return 1; }
        [[ "${LB_FAIL:-}" != "add-target" ]] || { echo "hcloud: add-target failed (server_error)" >&2; return 1; }
        if [[ -n "${LB_ADD_ALREADY:-}" ]]; then
          (( n > 1 )) || { : > "${BATS_TEST_TMPDIR}/lb-changed"; echo "hcloud: timeout awaiting response headers" >&2; return 1; }
          if [[ -n "${LB_REREAD_FAIL:-}" && ! -f "${BATS_TEST_TMPDIR}/lb-reread-failed" ]]; then
            : > "${BATS_TEST_TMPDIR}/lb-reread-fail"
            : > "${BATS_TEST_TMPDIR}/lb-reread-failed"
          fi
          printf '%s\n' "${LB_ADD_ALREADY}" >&2
          return 1
        fi
        : > "${BATS_TEST_TMPDIR}/lb-changed" ;;
      "server list")            echo '[{"id":42,"name":"cp-0","status":"running","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      "server describe")        echo '{"id":42,"name":"cp-0","labels":{"lok8s.dev/owner":"'"${MARK}"'"}}' ;;
      "load-balancer "*)
        [[ "${LB_FAIL:-}" != "$2" ]] || { echo "hcloud: ${2} failed (server_error)" >&2; return 1; } ;;
    esac
    return 0
  }
  export -f hcloud
}

# _live_lb <targets> [<private_net>]: the LB as `describe -o json` prints it;
# private_net defaults to one network (LB_NO_NET=1: none).
_live_lb() {
  local net='[{"network":9,"ip":"10.0.0.2"}]'
  [[ -z "${LB_NO_NET:-}" ]] || net='[]'
  LIVE_LB='{"id":5,"name":"api-lb","labels":{"lok8s.dev/owner":"'"${MARK}"'"},"private_net":'"${net}"',
    "targets":['"${1}"'],
    "services":[{"protocol":"tcp","listen_port":6443,"destination_port":6443,"proxyprotocol":false,
                 "health_check":{"protocol":"tcp","port":6443,"interval":15,"timeout":10,"retries":3}}]}'
  export LIVE_LB
}

@test "hetzner::load-balancer replaces an adopted target whose use-private-ip differs from the descriptor" {
  _lb_hcloud
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/cluster=t,lok8s.dev/role=control-plane'"${OWN}"'"},
             "use_private_ip":true,"targets":[{"type":"server","server":{"id":42},"use_private_ip":true}]}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/cluster=t,lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_success
  assert_output --partial "replace"
  assert_output --partial " label-selector: lok8s.dev/cluster=t,lok8s.dev/role=control-plane${OWN} (use-private-ip true -> false)"

  # Remove first, then add without --use-private-ip, by the LB id. The service matches.
  run grep -E '^load-balancer (remove-target|add-target|add-service|update-service)' "${HCLOUD_LOG}"
  assert_output "load-balancer remove-target 5 --label-selector lok8s.dev/cluster=t,lok8s.dev/role=control-plane${OWN}
load-balancer add-target 5 --label-selector lok8s.dev/cluster=t,lok8s.dev/role=control-plane${OWN}"
}

@test "hetzner::load-balancer replaces a server target by id and adds use-private-ip" {
  _lb_hcloud
  _live_lb '{"type":"server","server":{"id":42},"use_private_ip":false}'
  _lb_descriptor '{ "type": "server", "value": "0", "use-private-ip": true }'
  run hetzner::load-balancer
  assert_success
  run grep -E '^load-balancer (remove-target|add-target)' "${HCLOUD_LOG}"
  assert_output "load-balancer remove-target 5 --server 42
load-balancer add-target 5 --server 42 --use-private-ip"
}

@test "hetzner::load-balancer keeps an adopted target and service that match the descriptor" {
  _lb_hcloud
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_success
  run grep -cE '^load-balancer (remove-target|add-target|add-service|update-service|delete-service)' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "hetzner::load-balancer updates an adopted service whose destination port differs, in place" {
  _lb_hcloud
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }' 16443
  run hetzner::load-balancer
  assert_success
  run grep -E '^load-balancer (add-service|update-service|delete-service)' "${HCLOUD_LOG}"
  assert_output "load-balancer update-service 5 --listen-port 6443 --protocol tcp --destination-port 16443"
}

@test "hetzner::load-balancer fails the step when it cannot remove a stale target, and adds nothing" {
  _lb_hcloud
  export LB_FAIL=remove-target
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":true}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "lb api-lb: could not remove target label-selector 'lok8s.dev/role=control-plane${OWN}' to set use-private-ip to false: hcloud: remove-target failed (server_error)."
  assert_output --partial "hetzner: the load balancer does not match the descriptor"
  run grep -c '^load-balancer add-target' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "hetzner::load-balancer adds every target and service to a new load balancer" {
  _lb_hcloud
  export LB_LIST='[]'
  unset LIVE_LB
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane", "use-private-ip": true }'
  run hetzner::load-balancer
  assert_success
  # A new LB cannot be described yet: no warning.
  refute_output --partial "could not read the load balancer"
  run grep -E '^load-balancer (create|remove-target|add-target|add-service)' "${HCLOUD_LOG}"
  # The create sets the owner mark; the hook acts on the new LB by its id (6).
  assert_output "load-balancer create --name=api-lb --type=lb11 --location=fsn1 --label=lok8s.dev/cluster=t --label=lok8s.dev/owner=${MARK}
load-balancer add-target 6 --label-selector lok8s.dev/role=control-plane${OWN} --use-private-ip
load-balancer add-service 6 --protocol tcp --listen-port 6443 --destination-port 6443"
}

# Finding 5: a static cloud worker's Node lost lok8s.dev/role, and the Pods
# that select lok8s.dev/role=worker stayed Pending. A bare-metal Node still
# does not get it: consumers keep bare metal out with that selector.
@test "provider::output keeps lok8s.dev/role on a cloud VM Node and drops it on a bare-metal Node" {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server list") echo '[{"id":61,"name":"base-fsn1-abc","status":"running",
        "public_net":{"ipv4":{"ip":"192.0.2.20"}},"private_net":[{"network":9,"ip":"10.0.0.20"}],
        "labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/role":"worker","lok8s.dev/group":"base","tier.kubehz.cloud/pro":"true","lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      *"list") echo '[]' ;;
    esac
    return 0
  }
  export -f hcloud
  yq -i -o json '.server[1]["#labels"] = "lok8s.dev/cluster=test-cluster,lok8s.dev/role=worker,role.kubehz.cloud/platform=true"' "${DESC}"
  run provider::output "${DESC}"
  assert_success
  local out="${output}"
  run jq -c '.nodes[] | select(.name == "base-fsn1-abc") | [.role, .labels]' <<<"${out}"
  assert_output '["worker",{"lok8s.dev/role":"worker","tier.kubehz.cloud/pro":"true"}]'
  run jq -c '.nodes[] | select(.name == "worker-0") | [.role, .labels]' <<<"${out}"
  assert_output '["worker",{"role.kubehz.cloud/platform":"true"}]'
}

# ── review round 1 (#297) ────────────────────────────────────────────────────

# The remove/add swap takes the LB's only target (the API endpoint) offline
# until the add is done: the add is retried, and a failure is loud.
@test "hetzner::load-balancer retries the add after a remove, then fails the step with a clear error" {
  _lb_hcloud
  export LB_FAIL=add-target HETZNER_LB_ADD_TRIES=3 HETZNER_LB_ADD_DELAY=0
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":true}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "before: label_selector lok8s.dev/role=control-plane${OWN} use-private-ip=true"
  assert_output --partial "lb api-lb: removed target label-selector 'lok8s.dev/role=control-plane${OWN}', but could not add it again with use-private-ip false in 3 attempts: hcloud: add-target failed (server_error). The load balancer does not route to this target now. Run the provision again, or add the target with this command: hcloud load-balancer add-target api-lb --label-selector lok8s.dev/role=control-plane${OWN}"
  assert_output --partial "hetzner: the load balancer does not match the descriptor"
  run grep -c '^load-balancer remove-target' "${HCLOUD_LOG}"
  assert_output "1"
  run grep -c '^load-balancer add-target' "${HCLOUD_LOG}"
  assert_output "3"
}

@test "hetzner::load-balancer retries a failed add after a remove and prints the targets before and after" {
  _lb_hcloud
  export LB_ADD_FAILS=1 HETZNER_LB_ADD_TRIES=3 HETZNER_LB_ADD_DELAY=0
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":true}'
  export LIVE_LB_AFTER='{"id":5,"name":"api-lb","labels":{"lok8s.dev/owner":"'"${MARK}"'"},"targets":[{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false}]}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_success
  assert_output --partial "before: label_selector lok8s.dev/role=control-plane${OWN} use-private-ip=true"
  assert_output --partial "after:  label_selector lok8s.dev/role=control-plane${OWN} use-private-ip=false"
  run grep -c '^load-balancer add-target' "${HCLOUD_LOG}"
  assert_output "2"
}

@test "hetzner::load-balancer keeps a target when the load balancer has no network for use-private-ip true" {
  _lb_hcloud
  export LB_NO_NET=1
  _live_lb '{"type":"server","server":{"id":42},"use_private_ip":false}'
  _lb_descriptor '{ "type": "server", "value": "0", "use-private-ip": true }'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "lb api-lb: kept target server '42' with use-private-ip false. The load balancer is on no private network, so the target cannot get use-private-ip true."
  run grep -cE '^load-balancer (remove-target|add-target)' "${HCLOUD_LOG}"
  assert_output "0"
}

# lok8s changes a load balancer only after it read it by id and checked its
# owner mark: one that it cannot read gets no change, and the step fails.
@test "hetzner::load-balancer changes nothing on an adopted load balancer that it cannot read" {
  _lb_hcloud
  unset LIVE_LB
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "hetzner: did not change load-balancer 5: could not read it right before the change ('hcloud load-balancer describe 5': hcloud: load balancer not found: 5)."
  assert_output --partial "lb api-lb: did not change the targets and services of the load balancer"
  run grep -cE '^load-balancer (add|remove)-(target|service)' "${HCLOUD_LOG}"
  assert_output "0"
}

# ── issue #305: the provider items of the #297 review ────────────────────────

# _lb_replace_fixture: an adopted target with use-private-ip true, and a
# descriptor without it: the hook removes the target and adds it again.
# LIVE_LB_AFTER (the read after the first add) is set by the test.
_lb_replace_fixture() {
  _lb_hcloud
  export HETZNER_LB_ADD_TRIES=3 HETZNER_LB_ADD_DELAY=0
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":true}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
}

# _lb_after <targets>: LIVE_LB_AFTER, the load balancer with these targets.
_lb_after() {
  export LIVE_LB_AFTER='{"id":5,"name":"api-lb","labels":{"lok8s.dev/owner":"'"${MARK}"'"},"private_net":[{"network":9,"ip":"10.0.0.2"}],"targets":['"${1}"']}'
}

# _argv_of <command line>: ARGV, the words of the command line as the shell
# reads them.
_argv_of() {
  eval "ARGV=(${1})"
}

# hcloud prints an API error as "hcloud: <message> (<code>)", or with a
# correlation id after the code (hcloud-go Error.Error). The code of a
# target that the load balancer has already is target_already_defined.
@test "hetzner::load-balancer counts a re-add that answers target_already_defined as done when a new read shows the target" {
  local answer
  for answer in \
      'hcloud: The Load Balancer target you are trying to define is already defined (target_already_defined)' \
      'hcloud: The Load Balancer target you are trying to define is already defined (target_already_defined, a6c75d4a)'; do
    rm -f "${BATS_TEST_TMPDIR}/lb-changed"
    : > "${CLOUD_LOG_FILE}"
    _lb_replace_fixture
    export LB_ADD_ALREADY="${answer}"
    _lb_after '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false}'
    run hetzner::load-balancer
    assert_success
    refute_output --partial "could not add it again"
    # The first add was done (its answer was lost); the second answered "already".
    run grep -c '^load-balancer add-target 5 ' "${HCLOUD_LOG}"
    assert_output "2"
    run grep -c 'add-target attempt 2/3: the target is defined already, and a new read shows it with use-private-ip false' "${CLOUD_LOG_FILE}"
    assert_output "1"
  done
}

@test "hetzner::load-balancer fails a re-add that answers target_already_defined when a new read does not show the target" {
  _lb_replace_fixture
  export LB_ADD_ALREADY='hcloud: The Load Balancer target you are trying to define is already defined (target_already_defined)'
  _lb_after ''
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "lb api-lb: removed target label-selector 'lok8s.dev/role=control-plane${OWN}', but could not add it again with use-private-ip false in 3 attempts: ${LB_ADD_ALREADY}."
  assert_output --partial "hetzner: the load balancer does not match the descriptor"
  run grep -c '^load-balancer add-target 5 ' "${HCLOUD_LOG}"
  assert_output "3"
}

# A new read that shows the target with the other use-private-ip stops the
# re-add at once: each later attempt gets the same answer. The message names
# the value that the read shows, and the load balancer routes to the target.
@test "hetzner::load-balancer stops a re-add that answers target_already_defined at once when a new read shows the target with another use-private-ip" {
  _lb_replace_fixture
  export LB_ADD_ALREADY='hcloud: The Load Balancer target you are trying to define is already defined (target_already_defined)'
  _lb_after '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":true}'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "lb api-lb: removed target label-selector 'lok8s.dev/role=control-plane${OWN}' to set use-private-ip false, but the add answered that the target is defined already, and a new read shows it with use-private-ip true. Another run or person changed the load balancer at the same time. The load balancer routes to this target with use-private-ip true now. Run the provision again."
  refute_output --partial "could not add it again"
  refute_output --partial "does not route to this target now"
  assert_output --partial "hetzner: the load balancer does not match the descriptor"
  # The first add was done (its answer was lost); the second answered "already": no third.
  run grep -c '^load-balancer add-target 5 ' "${HCLOUD_LOG}"
  assert_output "2"
}

# A failed new read after target_already_defined stops the re-add at once:
# the caller reports it once, and no later attempt that succeeds leaves the
# message of the read behind as a false error.
@test "hetzner::load-balancer stops a re-add at once when the new read after target_already_defined fails" {
  _lb_replace_fixture
  export LB_ADD_ALREADY='hcloud: The Load Balancer target you are trying to define is already defined (target_already_defined)' LB_REREAD_FAIL=1
  _lb_after '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false}'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "hetzner: did not change load-balancer 5: could not read it right before the change ('hcloud load-balancer describe 5': hcloud: limit of requests reached (rate_limit_exceeded))."
  assert_output --partial "lb api-lb: removed target label-selector 'lok8s.dev/role=control-plane${OWN}', and did not add it again: the load balancer or the server changed (the message above)."
  local out="${output}"
  run grep -c 'could not read it right before the change' <<<"${out}"
  assert_output "1"
  run grep -c '^load-balancer add-target 5 ' "${HCLOUD_LOG}"
  assert_output "2"
}

# Only the code target_already_defined: another error with "already" in it
# is not an answer that this target is on the load balancer.
@test "hetzner::load-balancer fails a re-add whose error has 'already' but another code, also when a new read shows the target" {
  _lb_replace_fixture
  export LB_ADD_ALREADY='hcloud: the load balancer is already locked by another action (locked)'
  _lb_after '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false}'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "but could not add it again with use-private-ip false in 3 attempts: ${LB_ADD_ALREADY}."
  run grep -c '^load-balancer add-target 5 ' "${HCLOUD_LOG}"
  assert_output "3"
}

# A successful describe can write a line to stderr (a warning of hcloud). It
# does not go into the JSON that lok8s reads, and lok8s does not print it.
@test "hetzner::load-balancer reads a describe that also writes to stderr, and does not print that line" {
  _lb_hcloud
  export LB_DESCRIBE_STDERR='canary-7f3e1b: a line on the stderr of hcloud'
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  # A TMPDIR of its own: the temp file of each describe is removed after it.
  export TMPDIR="${BATS_TEST_TMPDIR}/describe-tmp"
  mkdir -p "${TMPDIR}"
  run hetzner::load-balancer
  assert_success
  refute_output --partial "canary-7f3e1b"
  run find "${TMPDIR}" -mindepth 1
  assert_output ""
  # The live read gave the target: kept, no write.
  run grep -cE '^load-balancer (remove-target|add-target|add-service|update-service)' "${HCLOUD_LOG}"
  assert_output "0"
  run grep -c '^load-balancer describe 5 -o json' "${HCLOUD_LOG}"
  refute_output "0"
}

# A describe that fails: the error gives the reason of hcloud, the last line
# that it wrote to stderr (a 401, a 429 or a 404), and no earlier line.
@test "hetzner::load-balancer gives the last stderr line of a describe that fails in its error" {
  _lb_hcloud
  unset LIVE_LB
  export LB_DESCRIBE_STDERR='canary-7f3e1b: a line on the stderr of hcloud'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  # A TMPDIR of its own: the temp file of the describe is removed after it.
  export TMPDIR="${BATS_TEST_TMPDIR}/describe-tmp"
  mkdir -p "${TMPDIR}"
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "hetzner: did not change load-balancer 5: could not read it right before the change ('hcloud load-balancer describe 5': hcloud: load balancer not found: 5)."
  refute_output --partial "canary-7f3e1b"
  run find "${TMPDIR}" -mindepth 1
  assert_output ""
}

# Only a describe that fails gives its stderr: one that succeeds with a line
# on stderr, but answers no load balancer object, gets the error without it.
@test "hetzner::load-balancer gives no stderr line in the error of a describe that succeeds with an answer it cannot use" {
  _lb_hcloud
  export LIVE_LB='[]' LB_DESCRIBE_STDERR='canary-7f3e1b: a line on the stderr of hcloud'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "hetzner: did not change load-balancer 5: could not read it right before the change ('hcloud load-balancer describe 5')."
  refute_output --partial "canary-7f3e1b"
}

# Hetzner label selectors can have a space ("env in (a, b)"). The command in
# the message gives each argument as one shell word. A word can hold \'
# (printf %q), so the command ends the message without quotes around it.
@test "hetzner::load-balancer quotes a selector with a space or a quote in the re-add command, and the command reads back to the same arguments" {
  local sel re hint
  for sel in 'lok8s.dev/role in (control-plane, worker)' "lok8s.dev/role in (it's)"; do
    : > "${HCLOUD_LOG}"
    _lb_hcloud
    export LB_FAIL=add-target HETZNER_LB_ADD_TRIES=1 HETZNER_LB_ADD_DELAY=0
    _live_lb '{"type":"label_selector","label_selector":{"selector":"'"${sel}${OWN}"'"},"use_private_ip":true}'
    _lb_descriptor '{ "type": "label-selector", "value": "'"${sel}"'" }'
    run hetzner::load-balancer
    assert_failure
    re="or add the target with this command: ([^"$'\n'"]*)"
    [[ "${output}" =~ ${re} ]]
    hint="${BASH_REMATCH[1]}"
    [[ "${hint}" != *"${sel}"* ]]
    _argv_of "${hint}"
    [ "${#ARGV[@]}" -eq 6 ]
    [ "${ARGV[*]:0:5}" = "hcloud load-balancer add-target api-lb --label-selector" ]
    [ "${ARGV[5]}" = "${sel}${OWN}" ]
  done
}

@test "hetzner::load-balancer quotes a selector with a space in the remove command of the audit" {
  local sel='env in (a, b)'
  _lb_hcloud
  export LB_FAIL=remove-target
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane'"${OWN}"'"},"use_private_ip":false},
            {"type":"label_selector","label_selector":{"selector":"'"${sel}"'"},"use_private_ip":false}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_failure
  local re="Run this command, then run the provision again: ([^"$'\n'"]*)"
  [[ "${output}" =~ ${re} ]]
  [[ "${BASH_REMATCH[1]}" != *"${sel}"* ]]
  _argv_of "${BASH_REMATCH[1]}"
  [ "${#ARGV[@]}" -eq 6 ]
  [ "${ARGV[*]:0:5}" = "hcloud load-balancer remove-target api-lb --label-selector" ]
  [ "${ARGV[5]}" = "${sel}" ]
}

# The readiness barrier connects with sshPrivateKey after the reimage: a
# sshPublicKey of another pair must stop the rebuild before it.
@test "provider::rebuild stops before any reimage when sshPublicKey is not the key of sshPrivateKey" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  _descriptor_pubkey
  export STUB_DERIVED_PUB="ssh-ed25519 AAAAC3NzaOtherKey operator@laptop"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "rebuild: sshPublicKey '${CFGDIR}/descriptor.pub' is not the public key of sshPrivateKey '${BATS_TEST_TMPDIR}/id_key' (ssh-keygen -y gives 'ssh-ed25519 AAAAC3NzaOtherKey')."
  run grep -c -- "-y -P  -f ${BATS_TEST_TMPDIR}/id_key" "${SSH_LOG}"
  assert_output "1"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  run cat "${CURL_LOG}"
  refute_output --partial "/rescue"
}

@test "provider::rebuild warns and goes on when ssh-keygen cannot read sshPrivateKey" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef" STUB_KEYGEN_Y_FAIL=1
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  assert_output --partial "[warn] rebuild: cannot check that sshPublicKey '${CFGDIR}/pub/id.pub' is the public key of sshPrivateKey '${BATS_TEST_TMPDIR}/id_key': ssh-keygen -y failed (Load key"
  run grep -c 'server rebuild 10 ' "${HCLOUD_LOG}"
  assert_output "1"
}

@test "provider::rebuild stops before any reimage when sshPrivateKey does not exist" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  rm -f "${BATS_TEST_TMPDIR}/id_key"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "rebuild: sshPrivateKey '${BATS_TEST_TMPDIR}/id_key' does not exist"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

# ssh does not use a private key that group or other users can access ("bad
# permissions"): the `ssh -i` of the barrier goes on without it, so the
# barrier cannot reach the reimaged nodes. The rebuild stops before it.
@test "provider::rebuild stops before any reimage when group or other users can access sshPrivateKey" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  local key="${BATS_TEST_TMPDIR}/id_key" m
  for m in 644 640 604; do
    chmod "${m}" "${key}"
    : > "${HCLOUD_LOG}"; : > "${CURL_LOG}"; : > "${SSH_LOG}"
    run provider::rebuild "${DESC}" "${WORK}"
    assert_failure
    assert_output --partial "rebuild: sshPrivateKey '${key}' (${key}) has mode ${m}: group or other users can access it. ssh does not use a private key with these permissions, so the readiness check cannot connect after the reimage, and nothing was reimaged. Make the key private: chmod 600 ${key}"
    run cat "${HCLOUD_LOG}"
    refute_output --partial "server rebuild"
    run cat "${CURL_LOG}"
    refute_output --partial "/rescue"
    run grep -c -- ' -y ' "${SSH_LOG}"
    assert_output "0"
  done
}

@test "provider::rebuild checks the mode of sshPrivateKey also without sshPublicKey" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  yq -i -o json 'del(.sshPublicKey)' "${DESC}"
  chmod 644 "${BATS_TEST_TMPDIR}/id_key"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "rebuild: sshPrivateKey '${BATS_TEST_TMPDIR}/id_key' (${BATS_TEST_TMPDIR}/id_key) has mode 644"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

@test "provider::rebuild gives the chmod command of a key path with a space as one shell word" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  local key="${BATS_TEST_TMPDIR}/my keys/id key"
  mkdir -p "${key%/*}"
  : > "${key}"
  chmod 644 "${key}"
  KEY="${key}" yq -i -o json '.sshPrivateKey = strenv(KEY)' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  local re="Make the key private: chmod 600 ([^"$'\n'"]*)"
  [[ "${output}" =~ ${re} ]]
  _argv_of "${BASH_REMATCH[1]}"
  [ "${#ARGV[@]}" -eq 1 ]
  [ "${ARGV[0]}" = "${key}" ]
}

@test "provider::rebuild accepts a sshPrivateKey that only its owner can access" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  local m
  for m in 600 400; do
    chmod "${m}" "${BATS_TEST_TMPDIR}/id_key"
    : > "${HCLOUD_LOG}"
    run provider::rebuild "${DESC}" "${WORK}"
    assert_success
    refute_output --partial "has mode"
    run grep -c 'server rebuild 10 ' "${HCLOUD_LOG}"
    assert_output "1"
  done
}

# ssh checks the mode only of a key that the running user owns (OpenSSH
# sshkey_perm_ok: st_uid == getuid()). It uses a key of another owner, for
# example a root-owned 0440 Secret mount that a non-root runner reads. A
# test cannot make a file of another owner without root: a stat stand-in
# shows the key with the owner STAT_OWNER and its real mode.
@test "provider::rebuild accepts a sshPrivateKey of another owner whose mode lets group or other users access it" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  local key="${BATS_TEST_TMPDIR}/id_key" m
  export STAT_KEY="${key}" STAT_OWNER=$(( UID + 1 ))
  stat() {
    local out
    out="$(command stat "$@")" || return
    if [[ "${!#}" == "${STAT_KEY}" && " $* " == *" %u %a "* ]]; then
      out="${STAT_OWNER} ${out#* }"
    fi
    printf '%s\n' "${out}"
  }
  export -f stat
  for m in 644 440; do
    chmod "${m}" "${key}"
    : > "${HCLOUD_LOG}"
    run provider::rebuild "${DESC}" "${WORK}"
    assert_success
    refute_output --partial "has mode"
    run grep -c 'server rebuild 10 ' "${HCLOUD_LOG}"
    assert_output "1"
  done
}

# ssh opens the file that a link points to and checks the mode of that file,
# not the mode of the link (a link has mode 777).
@test "provider::rebuild accepts a sshPrivateKey link to a key with mode 600 that the running user owns" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  local link="${BATS_TEST_TMPDIR}/id_link"
  chmod 600 "${BATS_TEST_TMPDIR}/id_key"
  ln -s "${BATS_TEST_TMPDIR}/id_key" "${link}"
  LINK="${link}" yq -i -o json '.sshPrivateKey = strenv(LINK)' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  refute_output --partial "has mode"
  run grep -c 'server rebuild 10 ' "${HCLOUD_LOG}"
  assert_output "1"
}

@test "provider::rebuild stops before any reimage for a sshPrivateKey link to a key with mode 644 that the running user owns" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  local link="${BATS_TEST_TMPDIR}/id_link"
  chmod 644 "${BATS_TEST_TMPDIR}/id_key"
  ln -s "${BATS_TEST_TMPDIR}/id_key" "${link}"
  LINK="${link}" yq -i -o json '.sshPrivateKey = strenv(LINK)' "${DESC}"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "rebuild: sshPrivateKey '${link}' (${link}) has mode 644: group or other users can access it."
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

# The rebuild reimages only a server with the owner mark of this cluster: a
# server with the descriptor name and the cluster label but no mark can be
# another cluster's (hetzner_owner_test.bats covers the provision side).
@test "provider::rebuild never reimages a cloud server without the owner mark" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  # With the mark: the preflight asks for it, and cp-0 is rebuilt.
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  run grep -c -- "^server list -o json -l lok8s.dev/cluster=test-cluster,lok8s.dev/owner=${MARK}$" "${HCLOUD_LOG}"
  assert_output "1"
  : > "${HCLOUD_LOG}"; : > "${CURL_LOG}"
  # Without the mark: no reimage, no reset.
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server list")
        echo '[{"id":61,"name":"cp-0","status":"running","public_net":{"ipv4":{"ip":"192.0.2.10"}},"private_net":[{"ip":"10.0.0.3"}],"labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/role":"control-plane"}}]' ;;
      *"list") echo '[]' ;;
    esac
    return 0
  }
  export -f hcloud
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hcloud server add-label 61 lok8s.dev/owner=${MARK}"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  run cat "${CURL_LOG}"
  refute_output --partial "/reset/"
}

# The preflight id: the one server with the name, the cluster label and the
# owner mark of this cluster (not only the name and the cluster label).
@test "hetzner::_cloud_server_id takes only the one server with the name, the cluster label and the owner mark" {
  hcloud() { printf '%s\n' "$*" >> "${HCLOUD_LOG}"; printf '%s\n' "${SERVERS}"; }
  local own="\"lok8s.dev/owner\":\"${MARK}\""
  SERVERS='[{"id":61,"name":"cp-0","labels":{"lok8s.dev/cluster":"test-cluster"}}]'
  run hetzner::_cloud_server_id cp-0 lok8s.dev/cluster=test-cluster "${MARK}" test-cluster
  assert_failure
  assert_output ""
  SERVERS='[{"id":62,"name":"cp-0","labels":{"lok8s.dev/cluster":"other",'"${own}"'}}]'
  run hetzner::_cloud_server_id cp-0 lok8s.dev/cluster=test-cluster "${MARK}" test-cluster
  assert_failure
  SERVERS='[{"id":61,"name":"cp-0","labels":{"lok8s.dev/cluster":"test-cluster"}},
    {"id":63,"name":"cp-0","labels":{"lok8s.dev/cluster":"test-cluster",'"${own}"'}}]'
  run hetzner::_cloud_server_id cp-0 lok8s.dev/cluster=test-cluster "${MARK}" test-cluster
  assert_success
  assert_output "63"
}

# A failed list is no answer, also when hcloud printed a list before it
# failed, and also in a shell without pipefail.
_cloud_server_id_no_pipefail() {
  set +o pipefail
  hetzner::_cloud_server_id "$@"
}

@test "hetzner::_cloud_server_id: a failed server list gives no id, also when it printed a list" {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    echo '[{"id":42,"name":"cp-0","labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/owner":"'"${MARK}"'"}}]'
    return 1
  }
  run _cloud_server_id_no_pipefail cp-0 lok8s.dev/cluster=test-cluster "${MARK}" test-cluster
  assert_output ""
  assert_failure
}

# The rebuild acts on the server id that the preflight checked, never on a
# name: hcloud reads an all-digit name as an id, and a name can move to
# another server between the preflight and the reimage.
_rebuild_ids_hcloud() {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    local own='{"lok8s.dev/cluster":"test-cluster","lok8s.dev/role":"control-plane","lok8s.dev/owner":"'"${MARK}"'"}'
    case "$1 $2" in
      "server list")
        echo '[{"id":42,"name":"900","status":"running","public_net":{"ipv4":{"ip":"192.0.2.42"}},"private_net":[{"ip":"10.0.0.42"}],"labels":'"${own}"'},
               {"id":900,"name":"foreign","status":"running","public_net":{"ipv4":{"ip":"198.51.100.9"}},"labels":{"lok8s.dev/cluster":"test-cluster"}}]' ;;
      "server describe")
        case "$3" in
          42)
            if [[ -n "${DESCRIBE_42:-}" ]]; then
              echo "${DESCRIBE_42}"
            else
              echo '{"id":42,"name":"900","labels":'"${own}"'}'
            fi ;;
          900) echo '{"id":900,"name":"foreign","labels":{"lok8s.dev/cluster":"test-cluster"}}' ;;
          *)   echo "hcloud: server not found: $3" >&2; return 1 ;;
        esac ;;
      *"list") echo '[]' ;;
    esac
    return 0
  }
  export -f hcloud
  yq -i -o json '.server[0].name = "900"' "${DESC}"
}

@test "provider::rebuild reimages the checked id 42 of a server named 900, never the server with the id 900" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  _rebuild_ids_hcloud
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success
  run grep -E '^server (rebuild|describe) ' "${HCLOUD_LOG}"
  assert_output --regexp '^server describe 42 -o json'
  assert_output --partial "server rebuild 42 --image=ubuntu-24.04 "
  refute_output --regexp 'server (rebuild|describe) 900( |$)'
}

@test "provider::rebuild refuses a server that was renamed between the preflight and the reimage" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  _rebuild_ids_hcloud
  export DESCRIBE_42='{"id":42,"name":"900-renamed","labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/owner":"'"${MARK}"'"}}'
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: did not change server 42: its name is 900-renamed now, not 900."
  assert_output --partial "rebuild: node '900' (id 42) was not reimaged"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

@test "provider::rebuild refuses a server that lost the owner mark between the preflight and the reimage" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  _rebuild_ids_hcloud
  export DESCRIBE_42='{"id":42,"name":"900","labels":{"lok8s.dev/cluster":"test-cluster"}}'
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: did not change server 42: it does not have the owner mark of this cluster now"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
}

@test "provider::rebuild refuses a snapshot image without the owner mark before any reimage" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  yq -i -o json '.server[0].image = 4711' "${DESC}"
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server list")
        echo '[{"id":10,"name":"cp-0","status":"running","public_net":{"ipv4":{"ip":"192.0.2.10"}},"labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/owner":"'"${MARK}"'"}}]' ;;
      "image list") echo '[{"id":4711,"type":"snapshot","labels":{}}]' ;;
      *"list") echo '[]' ;;
    esac
    return 0
  }
  export -f hcloud
  run provider::rebuild "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "image 4711 (a snapshot) does not have the owner mark of this cluster"
  run cat "${HCLOUD_LOG}"
  refute_output --partial "server rebuild"
  run cat "${CURL_LOG}"
  refute_output --partial "/rescue"
}

@test "provider::rebuild refuses a signed or zero-padded id of a foreign snapshot (+4711, 04711) before any reimage" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    local own='{"lok8s.dev/cluster":"test-cluster","lok8s.dev/owner":"'"${MARK}"'"}'
    case "$1 $2" in
      "server list")
        echo '[{"id":10,"name":"cp-0","status":"running","public_net":{"ipv4":{"ip":"192.0.2.10"}},"labels":'"${own}"'}]' ;;
      "server describe") echo '{"id":10,"name":"cp-0","labels":'"${own}"'}' ;;
      "image list") echo '[{"id":4711,"type":"snapshot","labels":{}}]' ;;
      *"list") echo '[]' ;;
    esac
    return 0
  }
  export -f hcloud
  local v out rc
  for v in "+4711" "04711"; do
    : > "${HCLOUD_LOG}"
    yq -i -o json ".server[0].image = \"${v}\"" "${DESC}"
    run provider::rebuild "${DESC}" "${WORK}"
    out="${output}" rc="${status}"
    # hcloud reads both as the image id 4711 (strconv.ParseInt).
    run grep -E '^server rebuild ' "${HCLOUD_LOG}"
    assert_output ""
    output="${out}" status="${rc}"
    assert_failure
    assert_output --partial "image: '${v}' is an image id for hcloud (it has a sign or a leading zero). Write the id as plain digits, for example 4711."
    assert_output --partial "rebuild: nothing was reimaged."
  done
}

@test "provider::rebuild refuses an image with a control character or a space at an end, and never reimages from the id 4711" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    local own='{"lok8s.dev/cluster":"test-cluster","lok8s.dev/owner":"'"${MARK}"'"}'
    case "$1 $2" in
      "server list")
        echo '[{"id":10,"name":"cp-0","status":"running","public_net":{"ipv4":{"ip":"192.0.2.10"}},"labels":'"${own}"'}]' ;;
      "server describe") echo '{"id":10,"name":"cp-0","labels":'"${own}"'}' ;;
      "image list") echo '[{"id":4711,"type":"snapshot","labels":{}}]' ;;
      *"list") echo '[]' ;;
    esac
    return 0
  }
  export -f hcloud
  local v out rc
  for v in '"4711\t"' '"4711 "' '"\t4711"' '"47\t11"'; do
    : > "${HCLOUD_LOG}"
    jq ".server[0].image = ${v}" "${DESC}" > "${DESC}.new" && mv "${DESC}.new" "${DESC}"
    run provider::rebuild "${DESC}" "${WORK}"
    out="${output}" rc="${status}"
    # A tab-separated plan cut the tab: the reimage got the id 4711.
    run grep -E '^server rebuild ' "${HCLOUD_LOG}"
    assert_output ""
    output="${out}" status="${rc}"
    assert_failure
    assert_output --partial "image: ${v} has a"
    assert_output --partial "rebuild: nothing was reimaged."
  done
}

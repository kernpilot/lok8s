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
  # function needs loading where a binary did not (AUDIT.md r329).
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
  # stale state) — the accumulating log holds one 'server rebuild cp-0' per run.
  run grep -c 'server rebuild cp-0' "${HCLOUD_LOG}"
  assert_output "2"
}

@test "provider::rebuild honors CLOUD_DRY_RUN: prints the plan, reimages NOTHING" {
  export PROVIDER_ROBOT_RESCUE_FP="de:ad:be:ef"
  export CLOUD_DRY_RUN=1 CLOUD_DRY_RUN_PATH="${BATS_TEST_TMPDIR}/dry-run"
  run provider::rebuild "${DESC}" "${WORK}"
  assert_success

  # The full plan is logged (what WOULD happen): the cloud reimage command, the
  # bare-metal rescue+reset, and an explicit dry-run marker.
  assert_output --partial "hcloud server rebuild cp-0 --image ubuntu-24.04"
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

# ── B248: a failed create stops the provision with Hetzner's reason ────────────
# crowd hour72 (2026-09-24): `hcloud server create` answered "error during
# placement (resource_unavailable)", the exit status was dropped, and the run
# walked on into the KubeOne inventory, which then blamed name drift.

@test "hetzner::create stops on a failed create, names the resource, and never runs the lookup" {
  export CLOUD_HETZNER_JSON='{"server":[{"name":"cp-0","type":"cx33","location":"fsn1"}]}'
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
  export CLOUD_HETZNER_JSON='{"server":[{"name":"cp-0","type":"cx33","location":"fsn1"}]}'
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "server list") echo '[{"name":"cp-0","id":42}]' ;;
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

# ── boolean descriptor keys (B263, 2026-09-30) ───────────────────
# A boolean value is a bare hcloud switch. `without-ipv6: true` used to become
# `--without-ipv6 true`, which hcloud read as a stray argument; it is one
# `--without-ipv6=true` token now, and `false` stays explicit.
@test "hetzner::create passes a boolean key as one --key=value token, never a stray value" {
  export CLOUD_HETZNER_JSON='{"server":[{"name":"w-1","type":"cx23","image":"ubuntu-24.04","without-ipv6":true,"label":"lok8s.dev/cluster=t"}]}'
  run hetzner::create 'server'
  assert_success
  run grep -E '^server create' "${HCLOUD_LOG}"
  assert_success
  assert_output --partial '--without-ipv6=true'
  refute_output --partial '--without-ipv6 true'
  refute_output --regexp 'without-ipv6=true +(true|"")'
  # Adjacency: the next flag follows the switch directly — no placeholder
  # reached hcloud and no pair shifted (a swallowed --label would show here).
  assert_output --partial '--without-ipv6=true --label lok8s.dev/cluster=t'
  assert_output --partial '--type cx23'
}

@test "hetzner::create keeps a false boolean explicit (--key=false) and other values verbatim" {
  export CLOUD_HETZNER_JSON='{"server":[{"name":"w-2","type":"cx23","image":"ubuntu-24.04","without-ipv6":false}]}'
  run hetzner::create 'server'
  assert_success
  run grep -E '^server create' "${HCLOUD_LOG}"
  assert_output --partial '--without-ipv6=false'
  assert_output --partial '--image ubuntu-24.04'
}

# ── the kubehz.in.net in-place recreate (2026-10-23/24) ──────────────────────

# _load_descriptor <json>: CLOUD_HETZNER_JSON as provider::provision builds it:
# template::descriptor_json on a hetzner.json, then the lookup that writes the
# ids of the existing resources into it.
_load_descriptor() {
  local file="${BATS_TEST_TMPDIR}/lookup/hetzner.json"
  mkdir -p "${file%/*}"
  printf '%s\n' "${1}" > "${file}"
  CLOUD_HETZNER_JSON="$(template::descriptor_json "${file}")"
  hetzner::lookup >/dev/null
}

# _lookup_hcloud: hcloud with existing resources (the `<resource> list -o json`
# shape the lookup reads: an array of objects with id and name).
_lookup_hcloud() {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "ssh-key list") echo '[{"id":7,"name":"primary","public_key":"ssh-ed25519 AAAA"}]' ;;
      "network list") echo '[{"id":9,"name":"kubernetes","ip_range":"10.0.0.0/16"}]' ;;
      "server list")  echo '[{"id":42,"name":"cp-0","status":"running"}]' ;;
      "volume list")  echo '[]' ;;
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
  assert_output 'server create --name cp-1 --type cx43 --image 12345678 --ssh-key 7 --network 9'
  # The size is a literal; the server is a reference (cp-0 has the id 42).
  run grep -E '^volume create' "${HCLOUD_LOG}"
  assert_output 'volume create --name data-0 --size 100 --server 42 --label lok8s.dev/cluster=t'
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
  assert_output --partial "server rebuild cp-0 --image ubuntu-24.04 --user-data-from-file ${WORK}/rebuild-cp-0.cloud-init.yaml"
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
_lb_hcloud() {
  export LB_ADOPTED='[{"id":5,"name":"api-lb"}]'
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    case "$1 $2" in
      "load-balancer list")     echo "${LB_LIST:-${LB_ADOPTED}}" ;;
      "load-balancer describe") [[ -n "${LIVE_LB:-}" ]] || return 1; echo "${LIVE_LB}" ;;
      "server list")            echo '[{"id":42,"name":"cp-0","status":"running"}]' ;;
      "load-balancer "*)
        [[ "${LB_FAIL:-}" != "$2" ]] || { echo "hcloud: ${2} failed (server_error)" >&2; return 1; } ;;
    esac
    return 0
  }
  export -f hcloud
}

_live_lb() {
  LIVE_LB='{"id":5,"name":"api-lb",
    "targets":['"${1}"'],
    "services":[{"protocol":"tcp","listen_port":6443,"destination_port":6443,"proxyprotocol":false,
                 "health_check":{"protocol":"tcp","port":6443,"interval":15,"timeout":10,"retries":3}}]}'
  export LIVE_LB
}

@test "hetzner::load-balancer replaces an adopted target whose use-private-ip differs from the descriptor" {
  _lb_hcloud
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/cluster=t,lok8s.dev/role=control-plane"},
             "use_private_ip":true,"targets":[{"type":"server","server":{"id":42},"use_private_ip":true}]}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/cluster=t,lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_success
  assert_output --partial "replace"
  assert_output --partial " label-selector: lok8s.dev/cluster=t,lok8s.dev/role=control-plane (use-private-ip true -> false)"

  # Remove first, then add without --use-private-ip. The service matches.
  run grep -E '^load-balancer (remove-target|add-target|add-service|update-service)' "${HCLOUD_LOG}"
  assert_output "load-balancer remove-target api-lb --label-selector lok8s.dev/cluster=t,lok8s.dev/role=control-plane
load-balancer add-target api-lb --label-selector lok8s.dev/cluster=t,lok8s.dev/role=control-plane"
}

@test "hetzner::load-balancer replaces a server target by id and adds use-private-ip" {
  _lb_hcloud
  _live_lb '{"type":"server","server":{"id":42},"use_private_ip":false}'
  _lb_descriptor '{ "type": "server", "value": "0", "use-private-ip": true }'
  run hetzner::load-balancer
  assert_success
  run grep -E '^load-balancer (remove-target|add-target)' "${HCLOUD_LOG}"
  assert_output "load-balancer remove-target api-lb --server 42
load-balancer add-target api-lb --server 42 --use-private-ip"
}

@test "hetzner::load-balancer keeps an adopted target and service that match the descriptor" {
  _lb_hcloud
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane"},"use_private_ip":false}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_success
  run grep -cE '^load-balancer (remove-target|add-target|add-service|update-service|delete-service)' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "hetzner::load-balancer updates an adopted service whose destination port differs, in place" {
  _lb_hcloud
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane"},"use_private_ip":false}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }' 16443
  run hetzner::load-balancer
  assert_success
  run grep -E '^load-balancer (add-service|update-service|delete-service)' "${HCLOUD_LOG}"
  assert_output "load-balancer update-service api-lb --listen-port 6443 --protocol tcp --destination-port 16443"
}

@test "hetzner::load-balancer fails the step when it cannot remove a stale target, and adds nothing" {
  _lb_hcloud
  export LB_FAIL=remove-target
  _live_lb '{"type":"label_selector","label_selector":{"selector":"lok8s.dev/role=control-plane"},"use_private_ip":true}'
  _lb_descriptor '{ "type": "label-selector", "value": "lok8s.dev/role=control-plane" }'
  run hetzner::load-balancer
  assert_failure
  assert_output --partial "lb api-lb: could not remove target label-selector 'lok8s.dev/role=control-plane' to set use-private-ip to false: hcloud: remove-target failed (server_error)."
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
  run grep -E '^load-balancer (create|remove-target|add-target|add-service)' "${HCLOUD_LOG}"
  assert_output "load-balancer create --name api-lb --type lb11 --location fsn1 --label lok8s.dev/cluster=t
load-balancer add-target api-lb --label-selector lok8s.dev/role=control-plane --use-private-ip
load-balancer add-service api-lb --protocol tcp --listen-port 6443 --destination-port 6443"
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
        "labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/role":"worker","lok8s.dev/group":"base","tier.kubehz.cloud/pro":"true"}}]' ;;
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

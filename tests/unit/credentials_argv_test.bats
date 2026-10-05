#!/usr/bin/env bats
# credentials_argv_test.bats — no credential reaches a command line.
#
# Every local user can read the argv of a process (ps, /proc/<pid>/cmdline),
# and audit tools store it. Each site that sends a credential runs here with
# sentinel values and spy executables for curl, jq, kubectl, yq and hcloud
# (tests/lib/argv_sentinels.bash). Each test checks two things:
#   - no sentinel is in any recorded argv (assert_argv_clean);
#   - the credential still reached the request: the curl config, the body
#     or the env file, which the spies read the way the real tools do
#     (tests/lib/curl_capture.sh). REQ_LOG holds what they read.
# The agent CronJob script has the same checks in
# kubehz_agent_secret_test.bats and kubehz_heartbeat_test.bats.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  # argsh `:args` stub (the kubehz_claim_test.bats pattern): `--name value`
  # pairs become variables.
  :args() {
    shift
    while (( $# )); do
      case "$1" in
        --*)
          local _n="${1#--}"
          _n="${_n//-/_}"
          shift
          printf -v "${_n}" '%s' "${1:-}"
          ;;
      esac
      shift || true
    done
  }
  export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  source "${_PROJECT_ROOT}/tests/lib/argv_sentinels.bash"

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export REQ_LOG="${BATS_TEST_TMPDIR}/req.log"
  : > "${REQ_LOG}"
  mkdir -p "${PATH_CLUSTERS}/d.example"

  # One sentinel for each credential. The Robot password holds a quote
  # and a backslash, so the tests also cover the config quoting.
  export HCLOUD_TOKEN="hcloud-SENTINEL-1a2b"
  export KUBEHZ_TOKEN="khzt_SENTINEL_3c4d"
  export HROBOT_USER="robot-user-5e6f"
  export HROBOT_PASSWORD='robot-pw-SENTINEL-7g8h"\x'
  export KKP_TOKEN="kkp-SENTINEL-9i0j"
  export AWS_ACCESS_KEY_ID="AKIASENTINEL1K2L"
  export AWS_SECRET_ACCESS_KEY="aws-secret-SENTINEL/3m4n"
  export AGENT_TOKEN_IN_CLUSTER="khz_agt_SENTINEL_5o6p"
  ACCESS_TOKEN="access-SENTINEL-7q8r"
  SENTINELS=(hcloud-SENTINEL-1a2b khzt_SENTINEL_3c4d robot-pw-SENTINEL-7g8h kkp-SENTINEL-9i0j
    AKIASENTINEL1K2L aws-secret-SENTINEL khz_agt_SENTINEL_5o6p access-SENTINEL-7q8r)

  export -f curl_respond kubectl_respond hcloud_respond
  argv_spy curl jq kubectl yq hcloud
}

teardown() {
  teardown_tmpdir
}

# curl_respond: the answers of the kubehz api, Hetzner Cloud, Robot and
# KKP. Each request goes to REQ_LOG as one line: the method, the URL, the
# user, the headers and the body (a line break as \n).
curl_respond() {
  ARGV_LOG="" curl_capture "$@"
  local headers="${CURL_HEADERS%$'\n'}"
  printf '%s %s | user=%s | headers=%s | body=%s\n' "${CURL_METHOD:-GET}" "${CURL_URL}" \
    "${CURL_USER}" "${headers//$'\n'/; }" "${CURL_BODY//$'\n'/\\n}" >> "${REQ_LOG}"
  local out='{"ok":true}' code=200
  case "${CURL_URL}" in
    */server) out='[{"server":{"server_name":"worker-0","server_number":12345,"server_ip":"203.0.113.10"}}]' ;;
    */key) out='[{"key":{"fingerprint":"de:ad:be:ef"}}]' ;;
    */api/clusters/register)
      out='{"id":"cl-1","registered":true,"claimed":true,"claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-d.example"}}' ;;
    */v1/ssh_keys\?name=*) out='{"ssh_keys":[{"id":7}]}' ;;
    */api/credentials) out='{"data":{"validation":{"writable":true}}}' ;;
    */api/clusters\?perPage=500)
      out='{"ok":true,"data":[{"id":"cl-1","domain":"d.example","createdAt":"2026-01-01T00:00:00Z","status":"active","connected":true}]}' ;;
    */api/clusters) out='{"id":"cl-1"}'; code=201 ;;
    */kubeconfig) out='apiVersion: v1' ;;
    */agent-token) out='{"rotated":true,"clusterId":"cl-1"}' ;;
    */assessment) out='{"data":{}}' ;;
    */api/clusters/cl-1) out='{"data":{"status":"Running"}}' ;;
  esac
  printf '%s' "${out}"
  [[ " $* " != *'%{http_code}'* ]] || printf '\n%s' "${code}"
  return 0
}

# kubectl_respond: the env file of --from-env-file=/dev/stdin goes to
# REQ_LOG; the agent Secret of the cluster answers AGENT_TOKEN_IN_CLUSTER.
kubectl_respond() {
  local env
  case "$*" in
    *"--from-env-file=/dev/stdin"*)
      env=$(cat)
      printf 'env-file: %s\n' "${env//$'\n'/;}" >> "${REQ_LOG}"
      printf 'kind: Secret\n'
      ;;
    "apply "*) cat > /dev/null ;;
    *"get secret kubehz-agent"*) printf '%s' "${AGENT_TOKEN_IN_CLUSTER}" | base64 | tr -d '\n' ;;
  esac
  return 0
}

# hcloud_respond: the hcloud CLI reads HCLOUD_TOKEN from the environment.
hcloud_respond() {
  case "$1 $2" in
    "server list") echo '[{"name":"cp-0","status":"running","public_net":{"ipv4":{"ip":"192.0.2.10"}},"private_net":[{"ip":"10.0.0.3"}],"labels":{"lok8s.dev/cluster":"test-cluster","lok8s.dev/role":"control-plane","lok8s.dev/group":"cp"}}]' ;;
    "load-balancer list" | "network list") echo '[]' ;;
  esac
  return 0
}

# req_count <fixed text>: how many requests in REQ_LOG hold the text.
req_count() { grep -cF -- "${1}" "${REQ_LOG}" || true; }

# _cluster_yaml: a self-hosted registered cluster spec for d.example.
_cluster_yaml() {
  cat > "${PATH_CLUSTERS}/d.example/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: d
spec:
  cluster:
    domain: d.example
  kubehz:
    hosting: self
    access: registered
    apiUrl: https://api.kubehz.example
YAML
  echo "${PATH_CLUSTERS}/d.example/cluster.lok8s.yaml"
}

# ── Hetzner Robot: the password can boot a server into rescue ──

@test "Robot: lookup, rescue, reset and doctor send the user and password in a curl config" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" CLOUD_LOG_FILE="${BATS_TEST_TMPDIR}/hetzner.log"
  export HROBOT_API="https://robot.test"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  ssh() { return 0; }
  ssh-keygen() { printf '256 MD5:de:ad:be:ef test (ED25519)\n'; }
  export -f ssh ssh-keygen
  local cfg="${PATH_CLUSTERS}/d.example"
  mkdir -p "${cfg}/cloud-init" "${cfg}/pub"
  printf 'ssh-ed25519 AAAAtest test\n' > "${cfg}/pub/id.pub"
  : > "${BATS_TEST_TMPDIR}/id_key"
  cat > "${cfg}/hetzner.json" <<JSON
{
  "cluster_name": "test-cluster", "sshUser": "root",
  "sshPrivateKey": "${BATS_TEST_TMPDIR}/id_key", "sshPublicKey": "${cfg}/pub/id.pub",
  "cloudInit": { "path": "cloud-init", "modules": "node", "sshPubPath": "${cfg}/pub" },
  "server": [
    { "name": "cp-0", "type": "cx43", "image": "ubuntu-24.04", "#cloud.d": "node",
      "label": "lok8s.dev/cluster=test-cluster,lok8s.dev/role=control-plane" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10",
      "#internal-ip": "10.0.1.10", "#cloud.d": "node:worker",
      "#labels": "lok8s.dev/cluster=test-cluster,lok8s.dev/role=worker" }
  ]
}
JSON
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"

  run hetzner::_robot_server_number_by_ip 203.0.113.10
  assert_success
  assert_output 12345
  run hetzner::_robot_rescue 12345 de:ad:be:ef
  assert_success
  run hetzner::_robot_reset 12345
  assert_success
  PROVIDER_ROBOT_RESCUE_FP=de:ad:be:ef run provider::doctor "${cfg}/hetzner.json"
  assert_output --partial $'ok\tRobot API reachable'
  assert_output --partial "rescue SSH key registered in Robot"

  assert_argv_clean
  # Every Robot request carried the user and the password: /server (lookup,
  # doctor), /boot/…/rescue, /reset/…, /key.
  local n
  n=$(req_count "https://robot.test/")
  (( n >= 5 ))
  [ "$(req_count "user=${HROBOT_USER}:${HROBOT_PASSWORD} ")" = "${n}" ]
  [ "$(req_count "https://robot.test/key")" -ge 1 ]
  [ "$(req_count "https://robot.test/reset/12345")" = 1 ]
}

@test "Robot: the kubeone worker naming sends the user and password in a curl config" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
  local manifest="${BATS_TEST_TMPDIR}/kubeone.yaml"
  cat > "${manifest}" <<'YAML'
staticWorkers:
  hosts:
    - hostname: worker-0
      publicAddress: 203.0.113.10
YAML
  run kubeone::_name_robot_workers "${manifest}"
  assert_success

  assert_argv_clean
  [ "$(req_count "robot-ws.your-server.de")" = 2 ]
  [ "$(req_count "user=${HROBOT_USER}:${HROBOT_PASSWORD} ")" = 2 ]
  [ "$(req_count "/server/12345 | user=")" = 1 ]
}

# ── kubehz: the api bearer and the Hetzner Cloud token ──

@test "kubehz: the claim key, the direct claim and the token connect send no credential on argv" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  kubehz::get_ssh_fingerprint() { echo "lo:d.example"; }
  export HCLOUD_API_BASE="https://hc.example"

  run kubehz::ensure_claim_key d.example https://api.kubehz.example
  assert_success
  LOK8S_KUBEHZ_CONNECT_TOKEN=true run kubehz::direct_claim d.example "$(_cluster_yaml)" https://api.kubehz.example
  assert_success
  assert_output --partial "provisioning is enabled"

  assert_argv_clean
  # The three Hetzner Cloud calls of the claim key: list, delete, upload.
  [ "$(req_count "https://hc.example/")" = 3 ]
  [ "$(req_count "Authorization: Bearer ${HCLOUD_TOKEN} |")" = 3 ]
  [ "$(req_count "POST https://api.kubehz.example/api/clusters/register | user= | headers=Content-Type: application/json; Authorization: Bearer ${KUBEHZ_TOKEN} |")" = 1 ]
  [ "$(req_count "POST https://api.kubehz.example/api/credentials | user= | headers=Content-Type: application/json; Authorization: Bearer ${KUBEHZ_TOKEN} |")" = 1 ]
  [ "$(req_count "\"value\": \"${HCLOUD_TOKEN}\"")" = 1 ]
}

@test "kubehz: deregister, status, assess, re-enroll and the cluster lookup send no credential on argv" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  local yaml
  yaml=$(_cluster_yaml)
  export domain="d.example" DOMAIN_NAME="d.example"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.example" HCLOUD_API_BASE="https://hc.example"

  run kubehz::resolve_cluster_id d.example https://api.kubehz.example
  assert_success
  assert_output cl-1
  run kubehz::status
  assert_success
  run kubehz::assess
  assert_success
  run kubehz::re-enroll
  assert_success
  run kubehz::deregister_cluster d.example "${yaml}"
  assert_success

  assert_argv_clean
  [ "$(req_count "api.kubehz.example")" -ge 7 ]
  [ "$(req_count "api.kubehz.example")" = "$(req_count "Authorization: Bearer ${KUBEHZ_TOKEN} |")" ]
  [ "$(req_count "POST https://api.kubehz.example/api/clusters/cl-1/agent-token")" = 1 ]
  [ "$(req_count "DELETE https://api.kubehz.example/api/clusters/cl-1")" = 1 ]
  [ "$(req_count "https://hc.example/")" = 2 ]
  [ "$(req_count "https://hc.example/")" = "$(req_count "Authorization: Bearer ${HCLOUD_TOKEN} |")" ]
}

@test "kubehz token: the cache file and the exec credential keep the access token off jq's argv" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  local file="${BATS_TEST_TMPDIR}/cache/t.json"
  run kubehz::token_cache_write "${file}" "${ACCESS_TOKEN}" 1700000000
  assert_success
  run cat "${file}"
  assert_output "{\"access_token\":\"${ACCESS_TOKEN}\",\"expires_at\":1700000000}"
  run kubehz::token_emit exec-credential "${ACCESS_TOKEN}" 1700000000
  assert_success
  assert_output "{\"apiVersion\":\"client.authentication.k8s.io/v1\",\"kind\":\"ExecCredential\",\"status\":{\"token\":\"${ACCESS_TOKEN}\",\"expirationTimestamp\":\"2023-11-14T22:13:20Z\"}}"
  assert_argv_clean
}

@test "kubehz hosted: create, wait, kubeconfig and destroy send the bearer in a curl config" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local yaml
  yaml=$(_cluster_yaml)
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.example"

  run kubehz::provision_hosted d.example "${yaml}"
  assert_success
  run kubehz::destroy_hosted d.example "${yaml}"
  assert_success

  assert_argv_clean
  [ "$(req_count "POST https://api.kubehz.example/api/clusters | user= | headers=Content-Type: application/json; Authorization: Bearer ${KUBEHZ_TOKEN}")" = 1 ]
  [ "$(req_count "GET https://api.kubehz.example/api/clusters/cl-1 ")" -ge 1 ]
  [ "$(req_count "GET https://api.kubehz.example/api/clusters/cl-1/kubeconfig")" = 1 ]
  [ "$(req_count "DELETE https://api.kubehz.example/api/clusters/cl-1")" = 1 ]
  [ "$(req_count "api.kubehz.example")" = "$(req_count "Authorization: Bearer ${KUBEHZ_TOKEN} |")" ]
}

@test "kubehz shared: space_api sends the bearer in a curl config" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.example"
  run kubehz::space_api POST /api/spaces '{"name":"acme"}'
  assert_success
  assert_argv_clean
  [ "$(req_count "POST https://api.kubehz.example/api/spaces | user= | headers=Content-Type: application/json; Authorization: Bearer ${KUBEHZ_TOKEN} | body={\"name\":\"acme\"}")" = 1 ]
}

# ── drivers: KKP and CAPI ──

@test "KKP: the cloud spec and the api call keep the tokens off argv" {
  export KKP_API_URL="https://kkp.example"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/api"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"
  local hetzner aws
  hetzner=$(_build_cloud_spec hetzner "" "")
  aws=$(_build_cloud_spec aws "" "")
  run kkp::api POST /api/v2/projects/p/clusters "{\"cloud\": ${hetzner}}"
  assert_success
  run kkp::api POST /api/v2/projects/p/clusters "{\"cloud\": ${aws}}"
  assert_success

  assert_argv_clean
  [ "$(req_count "headers=Content-Type: application/json; Accept: application/json; Authorization: Bearer ${KKP_TOKEN} |")" = 2 ]
  [ "$(req_count "\"token\": \"${HCLOUD_TOKEN}\"")" = 1 ]
  [ "$(req_count "\"secretAccessKey\": \"${AWS_SECRET_ACCESS_KEY}\"")" = 1 ]
}

@test "CAPI: the credentials Secret gets its values from an env file on stdin" {
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/generate"
  local yaml="${PATH_CLUSTERS}/d.example/cluster.lok8s.yaml"
  printf 'kind: CAPI\nmetadata:\n  name: d\n' > "${yaml}"
  run capi::ensure_credentials "${yaml}" hetzner "${BATS_TEST_TMPDIR}/kc"
  assert_success
  AWS_REGION=eu-central-1 run capi::ensure_credentials "${yaml}" aws "${BATS_TEST_TMPDIR}/kc"
  assert_success

  assert_argv_clean
  [ "$(req_count "env-file: hcloud-token=${HCLOUD_TOKEN};robot-user=${HROBOT_USER};robot-password=${HROBOT_PASSWORD}")" = 1 ]
  [ "$(req_count "env-file: access-key-id=${AWS_ACCESS_KEY_ID};secret-access-key=${AWS_SECRET_ACCESS_KEY};region=eu-central-1")" = 1 ]

  # A value with a line break would start another key: refused, the Go
  # twin's text, kubectl never runs.
  : > "${ARGV_LOG}"
  HCLOUD_TOKEN=$'tok\nrobot-password=x' run capi::ensure_credentials "${yaml}" hetzner "${BATS_TEST_TMPDIR}/kc"
  assert_failure
  assert_output --partial "environment variable HCLOUD_TOKEN must not contain a newline"
  run grep -c '^kubectl' "${ARGV_LOG}"
  assert_output 0
}

#!/usr/bin/env bats
# curl_user_config_test.bats — each curl call of the bash tree that carries
# a credential reads no config file of the user and gets no TLS log or
# trace control.
#
# Unless -q is its FIRST argument, curl reads one config file of the user:
# $CURL_HOME/.curlrc, else $XDG_CONFIG_HOME/curlrc, else $HOME/.curlrc. A
# trace-ascii line there logs each header (the bearer, the Basic
# credential) and each answer, an output line writes the answer to another
# file. SSLKEYLOGFILE and the GnuTLS and NSS debug variables in the
# environment make curl or its TLS library write the session keys or a
# trace. So each call starts with `curl -q` and runs in a subshell after
# http::tls_log_vars_unset (utils/http.sh).
#
# Two kinds of check:
#   - the source gate (tests/lib/curl_gate.bash): every call with a
#     credential in the bash tree passes -q first, and every one outside the
#     agent CronJob runs after http::tls_log_vars_unset;
#   - for each caller family, the real curl against a local server
#     (tests/lib/http_stub.bash, 127.0.0.1 only): with a hostile config file
#     in each place curl looks, no trace or output file appears and the
#     result is the same as without one; with the six TLS log and trace
#     variables set, curl gets none of them and writes no key log, and a
#     readonly one stops the call before curl runs.
# The agent CronJob script has its config file case in
# kubehz_heartbeat_test.bats: it runs in its own image, without utils/http.sh.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  # argsh `:args` stub: `--name value` pairs become variables.
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
  source "${_PROJECT_ROOT}/tests/lib/curl_gate.bash"
  source "${_PROJECT_ROOT}/tests/lib/http_stub.bash"

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}/d.example" "${BATS_TEST_TMPDIR}/home"
  # No config file, proxy or TLS variable of the host reaches a test.
  export HOME="${BATS_TEST_TMPDIR}/home"
  unset CURL_HOME XDG_CONFIG_HOME CURL_CA_BUNDLE SSL_CERT_FILE SSL_CERT_DIR \
    http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY no_proxy NO_PROXY \
    SSLKEYLOGFILE GNUTLS_KEYLOGFILE GNUTLS_DEBUG_LEVEL SSLTRACE SSLDEBUG SSLDEBUGFILE

  # One value for each credential.
  local hex
  hex=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  export KUBEHZ_TOKEN="khzt_rc_${hex}" HCLOUD_TOKEN="hcloud-rc-${hex}" KKP_TOKEN="kkp-rc-${hex}"
  export KUBEHZ_AGENT_BEARER="agent-rc-${hex}"
  export HROBOT_USER="robot-user" HROBOT_PASSWORD="robot-pw-${hex}"
  export KUBEHZ_AGENT_CLIENT_ID="client-rc" KUBEHZ_AGENT_CLIENT_SECRET="client-secret-${hex}"
  export AGENT_TOKEN_IN_CLUSTER="khz_agt_rc_${hex}"
  export STUB_BEARER="${KUBEHZ_TOKEN} ${HCLOUD_TOKEN} ${KKP_TOKEN} ${KUBEHZ_AGENT_BEARER}"
  SECRETS=("${KUBEHZ_TOKEN}" "${HCLOUD_TOKEN}" "${KKP_TOKEN}" "${KUBEHZ_AGENT_BEARER}"
    "${HROBOT_PASSWORD}" "${KUBEHZ_AGENT_CLIENT_SECRET}" "${AGENT_TOKEN_IN_CLUSTER}")
  TLS_NAMES=(SSLKEYLOGFILE GNUTLS_KEYLOGFILE GNUTLS_DEBUG_LEVEL SSLTRACE SSLDEBUG SSLDEBUGFILE)
}

teardown() {
  http_stub_stop
  teardown_tmpdir
}

# ── the source gate ──────────────────────────────────────

# The detection itself: the list of the calls with a credential that the
# gate finds in the tree. Each caller family must be in it, and the count is
# the number of such calls today (a new call raises it: then add the call
# to a family case below). A broken pattern would list fewer calls and let
# the two gates below pass on nothing.
@test "curl gate: the detection finds each curl call with a credential in the bash tree" {
  local -a files=()
  mapfile -t files < <(curl_gate_files)
  run curl_gate list "${files[@]}"
  assert_success
  local f
  for f in libs/kubehz/main libs/kubehz/hosted libs/kubehz/agent libs/kubehz/shared \
    drivers/kubeone/config drivers/kkp/api providers/hetzner/main libs/kubehz/manifests/agent/cronjob.yaml; do
    assert_output --partial "/.lok8s/${f}:"
  done
  [ "$(grep -c "/.lok8s/libs/kubehz/main:" <<<"${output}")" -eq 14 ]
  [ "$(grep -c "/.lok8s/libs/kubehz/hosted:" <<<"${output}")" -eq 4 ]
  [ "$(grep -c "/.lok8s/drivers/kubeone/config:" <<<"${output}")" -eq 2 ]
  [ "$(grep -c "/.lok8s/libs/kubehz/manifests/agent/cronjob.yaml:" <<<"${output}")" -eq 4 ]
  [ "${#lines[@]}" -eq 28 ]
}

@test "curl gate: each curl call with a credential in the bash tree passes -q first" {
  local -a files=()
  mapfile -t files < <(curl_gate_files)
  run curl_gate q "${files[@]}"
  assert_success
  assert_output ""
}

# The agent CronJob runs its script in its own image, without utils/http.sh.
# Its environment is the manifest's (the pod spec and the agent ConfigMap),
# not a user's shell, so the TLS gate leaves the manifests out.
@test "curl gate: each curl call with a credential outside the manifests runs after http::tls_log_vars_unset" {
  local -a files=()
  mapfile -t files < <(curl_gate_files | grep -v '/manifests/')
  (( ${#files[@]} >= 15 ))
  run curl_gate tls "${files[@]}"
  assert_success
  assert_output ""
}

# The anti-vacuity half: each canary breaks the rule in one way, and the
# gate names its file and line. The compliant forms (and the forms that are
# not a call with a credential) give no line.
@test "curl gate: each canary breaks the rule it shows, and the compliant forms pass" {
  local bad="${BATS_TEST_TMPDIR}/bad.sh" good="${BATS_TEST_TMPDIR}/good.sh"
  cat > "${bad}" <<'SH'
curl -K - https://h.example/a
x=$(printf '%s' "${c}" | curl -sS --config - https://h.example/b)
curl -sS -H "Authorization: Bearer ${T}" https://h.example/c
curl -sS -u "${U}:${P}" https://h.example/d
http::curl_config header "Authorization: Bearer ${T}" |
  curl -sS https://h.example/e
printf '%s' "${body}" | curl -sS --data-binary @- https://h.example/f
raw=$(printf '%s\n' "${b}" |
  curl -sSL -X POST "https://h.example/g" \
    -K <(http::curl_config header "Authorization: Bearer ${T}") \
    --data-binary @-) || rc=$?
if out=$(http::curl_config user "u:p" | command curl -K - https://h.example/h); then :; fi
( true && curl -q -K - https://h.example/i )
SH
  cat > "${good}" <<'SH'
# curl -K - https://h.example/n1
echo "  curl -X POST https://h.example/n2 -H 'Authorization: Bearer <token>' \\"
curl -sS https://h.example/n3
local -a curl_args=(
  --config -
)
error "a value for the curl option holds a control character"
x=$(http::curl_config header "Authorization: Bearer ${T}" | (
  http::tls_log_vars_unset &&
    curl -q -K - -fsSL \
      https://h.example/n4
) ) || x=""
( umask 077 && http::tls_log_vars_unset && curl -q -sS -u "${U}:${P}" https://h.example/n5 )
SH
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${bad}:1: a curl call with a credential: -q is not its first argument
${bad}:2: a curl call with a credential: -q is not its first argument
${bad}:3: a curl call with a credential: -q is not its first argument
${bad}:4: a curl call with a credential: -q is not its first argument
${bad}:6: a curl call with a credential: -q is not its first argument
${bad}:7: a curl call with a credential: -q is not its first argument
${bad}:9: a curl call with a credential: -q is not its first argument
${bad}:12: a curl call with a credential: -q is not its first argument"
  run curl_gate tls "${bad}"
  assert_failure 1
  [ "${#lines[@]}" -eq 9 ]
  assert_line "${bad}:13: a curl call with a credential does not run after http::tls_log_vars_unset &&"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  run curl_gate tls "${good}"
  assert_success
  assert_output ""
  run curl_gate list "${good}"
  assert_output "${good}:10
${good}:13"
  run curl_gate x "${good}"
  assert_failure 2
}

# ── the family cases: helpers ────────────────────────────

_need_real_curl() {
  command -v python3 >/dev/null || skip "no python3"
  [[ -n "$(type -P curl)" ]] || skip "no curl"
  [[ -n "$(command -v openssl)" ]] || skip "no openssl: the https stub needs a self-signed certificate"
}

# _routes: the answers of the stub for every family (http_stub.py, the
# route table). The claim key label is the first 32 hex characters of the
# SHA-256 of d.example.
_routes() {
  export STUB_ROUTES="${BATS_TEST_TMPDIR}/routes.json"
  cat > "${STUB_ROUTES}" <<'JSON'
{
  "POST /api/clusters/register": {"code": 201, "body": {"id": "cl-1", "registered": true, "claimed": true,
    "claimKey": {"publicKey": "ssh-ed25519 AAAA k", "fingerprint": "aa:bb", "name": "kubehz-claim-d.example"}}},
  "POST /api/credentials": {"code": 200, "body": {"data": {"validation": {"writable": true}}}},
  "GET /api/clusters?perPage=500": {"code": 200, "body": {"ok": true, "data": [{"id": "cl-1", "domain": "d.example",
    "createdAt": "2026-01-01T00:00:00Z", "status": "active", "connected": true}]}},
  "GET /api/clusters/cl-1/assessment": {"code": 200, "body": {"data": {}}},
  "POST /api/clusters/cl-1/agent-token": {"code": 200, "body": {"rotated": true, "clusterId": "cl-1"}},
  "DELETE /api/clusters/cl-1": {"code": 200, "body": {"ok": true}},
  "POST /api/clusters": {"code": 201, "body": {"id": "cl-1"}},
  "GET /api/clusters/cl-1": {"code": 200, "body": {"data": {"status": "Running"}}},
  "GET /api/clusters/cl-1/kubeconfig": {"code": 200, "body": "apiVersion: v1\nkind: Config\n"},
  "GET /v1/ssh_keys?name=kubehz-claim-d.example": {"code": 200, "body": {"ssh_keys": [{"id": 7,
    "labels": {"lok8s.dev/kubehz-claim": "4832b62ed035fb411ffefd346ab61e77"}}]}},
  "DELETE /v1/ssh_keys/7": {"code": 204},
  "POST /v1/ssh_keys": {"code": 201, "body": {"ssh_key": {"id": 8}}},
  "POST /oauth/v2/token": {"code": 200, "body": {"access_token": "at-stub", "token_type": "Bearer", "expires_in": 3600}},
  "GET /server": {"code": 200, "body": [{"server": {"server_name": "worker-0", "server_number": 12345,
    "server_ip": "203.0.113.10"}}]},
  "POST /server/12345": {"code": 200, "body": {"server": {"server_number": 12345, "server_name": "worker-0"}}},
  "GET /api/v2/dc": {"code": 200, "body": [{"metadata": {"name": "fsn1"}}]},
  "POST /api/v2/projects": {"code": 201, "body": {"id": "p-1"}}
}
JSON
}

# _https_stub: the https stub with a certificate for 127.0.0.1. API is its
# base URL, CONTROL_URL the URL of the controls.
_https_stub() {
  _routes
  stub_cert
  export STUB_BASIC="${STUB_BASIC:-}"
  http_stub_start "${STUB_CERT}" "${STUB_KEY}"
  export CURL_CA_BUNDLE="${STUB_CERT}" API="https://127.0.0.1:${STUB_PORT}"
  CONTROL_URL="${API}/control"
}

# _tunnel_stub: the stub as an http proxy on 127.0.0.1 for a URL with a
# fixed host (the Robot API in the kubeone driver): curl sends CONNECT to
# the stub, which speaks TLS in the tunnel with a certificate for that host.
# curl does not resolve the host itself, so no request leaves 127.0.0.1.
_tunnel_stub() {
  _routes
  stub_cert DNS:robot-ws.your-server.de
  export STUB_TUNNEL_CERT="${STUB_CERT}" STUB_TUNNEL_KEY="${STUB_KEY}" STUB_BASIC="${STUB_BASIC:-}"
  http_stub_start
  export CURL_CA_BUNDLE="${STUB_CERT}" https_proxy="http://127.0.0.1:${STUB_PORT}"
  CONTROL_URL="https://robot-ws.your-server.de/control"
}

# _step <label> <command...>: run the command and print its status.
_step() {
  local label="${1}" rc=0
  shift
  "$@" || rc=$?
  printf '== %s: rc=%s\n' "${label}" "${rc}"
}

# _no_secret <file...>: none of SECRETS is in the files.
_no_secret() {
  local s
  for s in "${SECRETS[@]}"; do
    if grep -qF -e "${s}" "$@"; then
      echo "a credential is in $*" >&2
      return 1
    fi
  done
}

# _curlrc_case <flow> <steps>: run <flow> once without a config file of the
# user (the reference) and once with a hostile config file in each place
# curl looks: CURL_HOME, XDG_CONFIG_HOME, HOME (curlrc_dir: a trace-ascii
# and an output line into the test dir). The checks of the config file come
# first: a run that fails because the file took its answer must still show
# the file as the cause. Each run must write no trace and no output file,
# print the same stdout and stderr and end with the same status as the
# reference, and show no credential. The reference must have <steps> steps,
# each with rc 0. The control in each place: curl without -q reads the file
# there, so the case can fail. curl older than 7.73 does not look in
# XDG_CONFIG_HOME: that control is left out, with a note. Sets CONTROLS.
_curlrc_case() {
  local flow="${1}" steps="${2}" var o e rc ref_rc=0
  local ref="${BATS_TEST_TMPDIR}/ref"
  mkdir -p "${BATS_TEST_TMPDIR}/curlrc-none"
  ( unset CURL_HOME XDG_CONFIG_HOME && export HOME="${BATS_TEST_TMPDIR}/curlrc-none" && "${flow}" ) \
    > "${ref}.out" 2> "${ref}.err" || ref_rc=$?
  [ "${ref_rc}" -eq 0 ]
  [ "$(grep -c '^== .*: rc=0$' "${ref}.out")" -eq "${steps}" ]
  [ "$(grep -c '^== ' "${ref}.out")" -eq "${steps}" ]
  _no_secret "${ref}.out" "${ref}.err"
  CONTROLS=0
  for var in CURL_HOME XDG_CONFIG_HOME HOME; do
    echo "# config file in ${var}"
    curlrc_dir "${var}"
    o="${RC_DIR}.stdout" e="${RC_DIR}.stderr" rc=0
    ( curlrc_env "${var}" && "${flow}" ) > "${o}" 2> "${e}" || rc=$?
    [ ! -e "${RC_TRACE}" ]
    [ ! -e "${RC_OUT}" ]
    diff "${ref}.out" "${o}"
    diff "${ref}.err" "${e}"
    [ "${rc}" -eq "${ref_rc}" ]
    _no_secret "${o}" "${e}"
    if [[ "${var}" == XDG_CONFIG_HOME ]] && ! curl_reads_xdg; then
      echo "# no control for XDG_CONFIG_HOME: this curl is older than 7.73 and does not look there" >&3
    else
      curlrc_control "${var}" "${CONTROL_URL}"
      CONTROLS=$((CONTROLS + 1))
    fi
  done
}

# _tls_case <flow> <calls> <one>: the TLS log and trace controls. The key
# log control first (keylog_control): this curl writes a key log for a
# request to the same server when SSLKEYLOGFILE reaches it. A curl that
# writes none leaves only the record check below, which needs no key log.
# A control request that does not get to the stub fails the case. Then
# <flow> runs with the six variables set and a spy for curl that records
# its environment and runs the real curl: <calls> records, none with one of
# the six names, each with the control variable (an empty record would pass
# otherwise), no key log, and the caller keeps all six. Then <one> (a single
# call) runs with a readonly SSLKEYLOGFILE: bash says that it cannot unset
# it, the call fails, curl does not run and no request reaches the stub.
_tls_case() {
  local flow="${1}" calls="${2}" one="${3}" kc=0 rc=0 before name f
  local o="${BATS_TEST_TMPDIR}/tls.stdout" e="${BATS_TEST_TMPDIR}/tls.stderr"
  keylog_control "${CONTROL_URL}" || kc=$?
  # 2: the control request did not get to the stub. That is a fault, not a skip.
  [ "${kc}" -ne 2 ]
  if (( kc != 0 )); then
    echo "# this curl writes no TLS key log (its TLS backend ignores SSLKEYLOGFILE): the record check alone" >&3
  fi
  export CURL_ENV_DIR="${BATS_TEST_TMPDIR}/curl-env"
  mkdir -p "${CURL_ENV_DIR}"
  curl() {
    command env > "$(command mktemp "${CURL_ENV_DIR}/env.XXXXXX")"
    command curl "$@"
  }
  export -f curl
  export SSLKEYLOGFILE="${BATS_TEST_TMPDIR}/tls.keys" GNUTLS_KEYLOGFILE="${BATS_TEST_TMPDIR}/gnutls.keys" \
    GNUTLS_DEBUG_LEVEL=9 SSLTRACE=99 SSLDEBUG=99 SSLDEBUGFILE="${BATS_TEST_TMPDIR}/nss.trace" TLS_ENV_CONTROL=on
  "${flow}" > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -eq 0 ]
  [ "$(grep -c '^== .*: rc=[^0]' "${o}")" -eq 0 ]
  [ ! -e "${SSLKEYLOGFILE}" ]
  [ ! -e "${GNUTLS_KEYLOGFILE}" ]
  [ ! -e "${SSLDEBUGFILE}" ]
  _no_secret "${o}" "${e}"
  [ "$(find "${CURL_ENV_DIR}" -type f | wc -l)" -eq "${calls}" ]
  for f in "${CURL_ENV_DIR}"/env.*; do
    grep -qx 'TLS_ENV_CONTROL=on' "${f}"
    for name in "${TLS_NAMES[@]}"; do
      [ "$(grep -c "^${name}=" "${f}")" -eq 0 ]
    done
  done
  # The caller keeps each one, with its value, as an export.
  for name in "${TLS_NAMES[@]}"; do
    [[ "$(declare -p "${name}")" == "declare -x ${name}="* ]]
  done
  [ "${SSLKEYLOGFILE}|${GNUTLS_KEYLOGFILE}|${GNUTLS_DEBUG_LEVEL}|${SSLTRACE}|${SSLDEBUG}|${SSLDEBUGFILE}" = \
    "${BATS_TEST_TMPDIR}/tls.keys|${BATS_TEST_TMPDIR}/gnutls.keys|9|99|99|${BATS_TEST_TMPDIR}/nss.trace" ]
  find "${CURL_ENV_DIR}" -type f -name 'env.*' -delete
  before=$(wc -l < "${STUB_LOG}")
  rc=0
  ( readonly SSLKEYLOGFILE && "${one}" ) > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -ne 0 ]
  grep -qF 'unset: SSLKEYLOGFILE: cannot unset: readonly variable' "${e}"
  [ "$(find "${CURL_ENV_DIR}" -type f | wc -l)" -eq 0 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq "${before}" ]
  [ ! -e "${SSLKEYLOGFILE}" ]
}

# ── kubehz api (libs/kubehz/main, the KUBEHZ_TOKEN bearer) ──

_kubehz_api_setup() {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  kubehz::get_ssh_fingerprint() { echo "lo:d.example"; }
  kubectl() {
    case "$*" in
      *"get secret kubehz-agent"*) printf '%s' "${AGENT_TOKEN_IN_CLUSTER}" | base64 | tr -d '\n' ;;
    esac
  }
  export -f kubectl
  _https_stub
  YAML="${PATH_CLUSTERS}/d.example/cluster.lok8s.yaml"
  cat > "${YAML}" <<YAML
kind: Lo
metadata:
  name: d
spec:
  cluster:
    domain: d.example
  kubehz:
    hosting: self
    access: registered
    apiUrl: ${API}
YAML
  export domain="d.example" DOMAIN_NAME="d.example" LOK8S_KUBEHZ_API_URL="${API}"
}

# The register calls (bearer and anonymous), the direct claim with the
# token connect, status, assess, re-enroll and deregister: every call of
# the family. HCLOUD_TOKEN only for the token connect, so deregister leaves
# the claim key alone (the Hetzner Cloud family).
_kubehz_api_flow() {
  printf '%s' '{"domain":"d.example"}' | _step "register (bearer)" kubehz::register_post d.example "${API}" "lo next" bearer
  printf '%s' '{"domain":"d.example"}' | _step "register" kubehz::register_post d.example "${API}" "lo next"
  ( export HCLOUD_TOKEN LOK8S_KUBEHZ_CONNECT_TOKEN=true &&
    _step "direct claim" kubehz::direct_claim d.example "${YAML}" "${API}" )
  ( unset HCLOUD_TOKEN &&
    _step status kubehz::status &&
    _step assess kubehz::assess &&
    _step re-enroll kubehz::re-enroll &&
    _step deregister kubehz::deregister_cluster d.example "${YAML}" )
}

_kubehz_api_one() {
  printf '%s' '{"domain":"d.example"}' | kubehz::register_post d.example "${API}" "lo next" bearer
}

@test "kubehz api: curl reads no config file of the user" {
  _need_real_curl
  _kubehz_api_setup
  _curlrc_case _kubehz_api_flow 7
  # Each run: 3 registers (2 with the bearer), the token connect, 4 lists,
  # the assessment, the agent token and the delete.
  [ "$(grep -c '^POST /api/clusters/register bearer$' "${STUB_LOG}")" -eq 8 ]
  [ "$(grep -c '^POST /api/clusters/register none$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/credentials bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /api/clusters?perPage=500 bearer$' "${STUB_LOG}")" -eq 16 ]
  [ "$(grep -c '^GET /api/clusters/cl-1/assessment bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/clusters/cl-1/agent-token bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^DELETE /api/clusters/cl-1 bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${CONTROLS}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 11 + CONTROLS)) ]
}

@test "kubehz api: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kubehz_api_setup
  _tls_case _kubehz_api_flow 11 _kubehz_api_one
}

# ── Hetzner Cloud (libs/kubehz/main, the HCLOUD_TOKEN bearer) ──

# The claim key (the anonymous register, the key lookup, its delete and the
# upload) and the claim key cleanup of deregister (lookup, delete).
_hcloud_flow() {
  _step "claim key" kubehz::ensure_claim_key d.example "${API}"
  _step deregister kubehz::deregister_cluster d.example "${YAML}"
}

# The hcloud lookup of deregister alone: the api part refuses the plain
# http URL first (a dead end), and the claim key cleanup still runs.
_hcloud_one() {
  LOK8S_KUBEHZ_API_URL="http://127.0.0.1:9" kubehz::deregister_cluster d.example "${YAML}"
}

@test "hetzner cloud: curl reads no config file of the user" {
  _need_real_curl
  _kubehz_api_setup
  export HCLOUD_API_BASE="${API}"
  _curlrc_case _hcloud_flow 2
  [ "$(grep -c '^GET /v1/ssh_keys?name=kubehz-claim-d.example bearer$' "${STUB_LOG}")" -eq 8 ]
  [ "$(grep -c '^DELETE /v1/ssh_keys/7 bearer$' "${STUB_LOG}")" -eq 8 ]
  [ "$(grep -c '^POST /v1/ssh_keys bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${CONTROLS}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 8 + CONTROLS)) ]
}

@test "hetzner cloud: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kubehz_api_setup
  export HCLOUD_API_BASE="${API}"
  _tls_case _hcloud_flow 8 _hcloud_one
}

# ── the agent key token (libs/kubehz/main, client_id:secret) ──

_token_flow() {
  local KUBEHZ_TOKEN_ACCESS="" KUBEHZ_TOKEN_EXPIRES=""
  _step token kubehz::token_access "${API}/oauth/v2/token" openid "" 1
  [[ "${KUBEHZ_TOKEN_ACCESS}" == at-stub ]] && echo "access token: at-stub"
  (( KUBEHZ_TOKEN_EXPIRES > 0 )) && echo "expires: set"
}

_token_one() {
  kubehz::token_access "${API}/oauth/v2/token" openid "" 1
}

@test "kubehz token: curl reads no config file of the user" {
  _need_real_curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export STUB_BASIC="${KUBEHZ_AGENT_CLIENT_ID}:${KUBEHZ_AGENT_CLIENT_SECRET}"
  _https_stub
  _curlrc_case _token_flow 1
  grep -qx 'access token: at-stub' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^POST /oauth/v2/token basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 + CONTROLS)) ]
}

@test "kubehz token: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export STUB_BASIC="${KUBEHZ_AGENT_CLIENT_ID}:${KUBEHZ_AGENT_CLIENT_SECRET}"
  _https_stub
  _tls_case _token_flow 1 _token_one
}

# ── the agent api (libs/kubehz/agent, the access token) ──

_agent_flow() {
  _step "agent api" kubehz::agent_api list GET /api/spaces/sp-a
  printf '%s|%s\n' "${KUBEHZ_AGENT_STATUS}" "${KUBEHZ_AGENT_BODY}"
}

_agent_one() {
  kubehz::agent_api list GET /api/spaces/sp-b
}

_agent_setup() {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/agent"
  _https_stub
  export KUBEHZ_AGENT_BASE="${API}"
}

@test "kubehz agent: curl reads no config file of the user" {
  _need_real_curl
  _agent_setup
  _curlrc_case _agent_flow 1
  grep -qx '200|{"ok":true,"data":{"id":"sp-a"}}' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^GET /api/spaces/sp-a bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 + CONTROLS)) ]
}

@test "kubehz agent: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _agent_setup
  _tls_case _agent_flow 1 _agent_one
}

# ── hosted (libs/kubehz/hosted, the KUBEHZ_TOKEN bearer) ──

# The create, the wait, the kubeconfig and the destroy (with the lookup).
_hosted_flow() {
  _step provision kubehz::provision_hosted d.example "${YAML}"
  cat "${PATH_BASE}/.kubeconfig/d.example.yaml"
  _step destroy kubehz::destroy_hosted d.example "${YAML}"
}

_hosted_one() {
  kubehz::provision_hosted d.example "${YAML}"
}

_hosted_setup() {
  _kubehz_api_setup
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
}

@test "kubehz hosted: curl reads no config file of the user" {
  _need_real_curl
  _hosted_setup
  _curlrc_case _hosted_flow 2
  grep -qx 'kind: Config' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^POST /api/clusters bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /api/clusters/cl-1 bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /api/clusters/cl-1/kubeconfig bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^DELETE /api/clusters/cl-1 bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 5 + CONTROLS)) ]
}

@test "kubehz hosted: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _hosted_setup
  _tls_case _hosted_flow 5 _hosted_one
}

# ── kubeone Robot naming (drivers/kubeone/config, user and password) ──

# The driver calls the Robot API at its fixed host, so the stub is the
# proxy (_tunnel_stub): each call is a CONNECT and the request in it.
_kubeone_setup() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
  export STUB_BASIC="${HROBOT_USER}:${HROBOT_PASSWORD}"
  _tunnel_stub
  MANIFEST="${BATS_TEST_TMPDIR}/kubeone.yaml"
  cat > "${MANIFEST}" <<'YAML'
staticWorkers:
  hosts:
    - hostname: worker-0
      publicAddress: 203.0.113.10
YAML
}

_kubeone_flow() {
  _step "name robot workers" kubeone::_name_robot_workers "${MANIFEST}"
}

_kubeone_one() {
  kubeone::_name_robot_workers "${MANIFEST}"
}

@test "kubeone robot: curl reads no config file of the user" {
  _need_real_curl
  _kubeone_setup
  _curlrc_case _kubeone_flow 1
  [ "$(grep -c '^CONNECT robot-ws.your-server.de:443 none$' "${STUB_LOG}")" -eq $((8 + CONTROLS)) ]
  [ "$(grep -c '^GET /server basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /server/12345 basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${CONTROLS}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 4 + 2 * CONTROLS)) ]
}

@test "kubeone robot: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kubeone_setup
  _tls_case _kubeone_flow 2 _kubeone_one
}

# ── hetzner provider Robot calls (providers/hetzner/main, user and password) ──

_provider_setup() {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" CLOUD_LOG_FILE="${BATS_TEST_TMPDIR}/hetzner.log"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
  export STUB_BASIC="${HROBOT_USER}:${HROBOT_PASSWORD}"
  _https_stub
  export HROBOT_API="${API}"
}

_provider_flow() {
  _step "robot lookup" hetzner::_robot_server_number_by_ip 203.0.113.10
}

# The wrapper itself: the lookup sends its stderr to /dev/null, which would
# hide the line of bash.
_provider_one() {
  hetzner::_robot_curl -f -sS "${HROBOT_API}/server"
}

@test "hetzner provider robot: curl reads no config file of the user" {
  _need_real_curl
  _provider_setup
  _curlrc_case _provider_flow 1
  grep -qx 12345 "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^GET /server basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 + CONTROLS)) ]
}

@test "hetzner provider robot: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _provider_setup
  _tls_case _provider_flow 1 _provider_one
}

# ── KKP api (drivers/kkp/api, the KKP_TOKEN bearer and the body) ──

_kkp_setup() {
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/api"
  _https_stub
  export KKP_API_URL="${API}"
}

_kkp_flow() {
  _step "kkp get" kkp::api GET /api/v2/dc
  _step "kkp post" kkp::api POST /api/v2/projects '{"name":"p"}'
}

_kkp_one() {
  kkp::api GET /api/v2/dc
}

@test "kkp api: curl reads no config file of the user" {
  _need_real_curl
  _kkp_setup
  _curlrc_case _kkp_flow 2
  grep -qxF '[{"metadata":{"name":"fsn1"}}]' "${BATS_TEST_TMPDIR}/ref.out"
  grep -qxF '{"id":"p-1"}' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^GET /api/v2/dc bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/v2/projects bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 2 + CONTROLS)) ]
}

@test "kkp api: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kkp_setup
  _tls_case _kkp_flow 2 _kkp_one
}

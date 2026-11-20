#!/usr/bin/env bats
# kubehz_credential_table_test.bats: each kubehz entry point refuses a
# credential with a control character before its first request.
#
# A file with CRLF line ends can leave a CR at the end of a token. In a curl
# config, a CR or a line break ends the header line, and the rest becomes a
# curl option. So each entry point checks each credential before its first
# curl call (http::credential_ok, kubehz::token_has_control), or, for the
# claim nonce, before its kubectl call (the shape check).
#
# Each row of CR_TABLE is one case: a CR at the end of one credential. The
# columns, divided by "|":
#   case   the name of the case in _case (the entry point and its setup)
#   site   the function that holds the check, and the check
#          (http::credential_ok <name> or kubehz::token_has_control), or "-"
#   want   a text of the refusal (the name of the variable where the text
#          has one)
#   rc     the status of the entry point
#   calls  none: no curl call and no kubectl call. not:<text>: no call to a
#          URL that holds <text>, and no call carries the credential
#          (the entry point makes other calls before the check, which is
#          correct: those calls do not carry it)
# A second case pins that the sites of the table are all the sites of the
# kubehz libs, so a new check of a credential needs a row here.

CR_TABLE=(
  "status|kubehz::status http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "assess|kubehz::assess http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "re-enroll|kubehz::re-enroll http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "resolve-cluster-id|kubehz::resolve_cluster_id http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "deregister|kubehz::deregister_cluster http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "deregister-hcloud|kubehz::deregister_cluster http::credential_ok HCLOUD_TOKEN|environment variable HCLOUD_TOKEN must not contain a control character|1|not:https://api.hetzner.cloud/"
  "claim-key|kubehz::ensure_claim_key http::credential_ok HCLOUD_TOKEN|environment variable HCLOUD_TOKEN must not contain a control character|1|none"
  "direct-claim|kubehz::direct_claim http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "direct-claim-hcloud|kubehz::direct_claim http::credential_ok HCLOUD_TOKEN|environment variable HCLOUD_TOKEN must not contain a control character|0|not:/api/credentials"
  "wait-for-cluster|kubehz::wait_for_cluster http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "provision-hosted|kubehz::provision_hosted http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "destroy-hosted|kubehz::destroy_hosted http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "space-api|kubehz::space_api_call http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "node-preflight|node::api_access http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "node-space-preflight|node::api_access http::credential_ok KUBEHZ_TOKEN|environment variable KUBEHZ_TOKEN must not contain a control character|1|none"
  "agent-token|kubehz::agent_session kubehz::token_has_control|kubehz space list: the access token holds a control character|1|none"
  "agent-key|kubehz::token_access kubehz::token_has_control|the agent key holds a control character: check KUBEHZ_AGENT_CLIENT_ID and the client secret|1|none"
  "token|kubehz::token_access kubehz::token_has_control|the agent key holds a control character: check KUBEHZ_AGENT_CLIENT_ID and the client secret|1|none"
  "claim|-|invalid claim nonce|1|none"
)

# The checks of the kubehz libs that are no credential check of an entry
# point, and why. Each one is a site as _lib_sites prints it.
SITES_NOT_IN_TABLE=(
  "kubehz::token_has_control [[:cntrl:]]|the check itself: the table has a row for each call of it"
  "handover::ssh_path_unsafe [[:cntrl:]]|a check of a path for an ssh -o option, not of a credential"
)

# _lib_sites <file>...: one line for each check in the files, with the
# function that holds it: "<function> http::credential_ok <name>" for each
# name of a call (a name can be in quotes), "<function>
# kubehz::token_has_control" for each call, and "<function> [[:cntrl:]]" for
# each other check of the class. A line that starts a function sets the
# function; the rest of that line is read too. A comment line is no call. A
# call with a name that it cannot read (a $variable, a line that goes on with
# a backslash, no name) gives "UNREAD <file>:<line>: <text>". The finder does
# not strip a comment or a string at the end of a line: a match there only
# adds a site, so the gate fails on it and hides none.
_lib_sites() {
  LC_ALL=C awk -v q="'" '
    function site(s) { print f " " s }
    function unread() { print "UNREAD " FILENAME ":" FNR ": " $0 }
    BEGIN {
      plain = "^[A-Za-z_][A-Za-z0-9_]*"
      dq = "^\"[A-Za-z_][A-Za-z0-9_]*\""
      sq = "^" q "[A-Za-z_][A-Za-z0-9_]*" q
    }
    FNR == 1 { f = "(top level)" }
    /^[[:space:]]*#/ { next }
    {
      s = $0
      if (match(s, /^[A-Za-z_][A-Za-z0-9_:.-]*[[:space:]]*\(\)/)) {
        f = substr(s, 1, RLENGTH)
        sub(/[[:space:]]*\(\)$/, "", f)
        s = substr(s, RLENGTH + 1)
      }
      t = s
      while (match(t, /kubehz::token_has_control/)) {
        site("kubehz::token_has_control")
        t = substr(t, RSTART + RLENGTH)
      }
      t = s
      while (match(t, /\[:cntrl:\]/)) {
        site("[[:cntrl:]]")
        t = substr(t, RSTART + RLENGTH)
      }
      t = s
      while (match(t, /http::credential_ok/)) {
        t = substr(t, RSTART + RLENGTH)
        r = t
        n = 0
        while (1) {
          sub(/^[[:space:]]+/, "", r)
          if (r == "" || r ~ /^(\|\||&&|;|\||\)|#)/) break
          if (match(r, plain) || match(r, dq) || match(r, sq)) {
            name = substr(r, 1, RLENGTH)
            r = substr(r, RLENGTH + 1)
            gsub(/"/, "", name)
            gsub(q, "", name)
            if (r != "" && r !~ /^([[:space:]]|;|\||&|\))/) { n = -1; break }
            site("http::credential_ok " name)
            n++
            continue
          }
          n = -1
          break
        }
        if (n <= 0) unread()
      }
    }' "$@"
}

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/node"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/agent"
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"

  # The argsh :args builtin is not there when a lib is sourced in bats (the
  # stub of kubehz_node_test.bats): each --name value pair sets the
  # variable, and a --flag before another flag or the end sets 1.
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

  # One spec of the domain. The yq stub gives its kubehz block: an HTTPS
  # api, access registered, and the hosting of SPEC_HOSTING (self when it is
  # not set).
  SPEC="${PATH_CLUSTERS}/acme.example.org/cluster.lok8s.yaml"
  mkdir -p "${PATH_CLUSTERS}/acme.example.org"
  touch "${SPEC}"
  export DOMAIN_NAME="acme.example.org"
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"')   echo "${SPEC_HOSTING:-self}" ;;
      '.spec.kubehz.apiUrl // ""')        echo "https://api.example.test" ;;
      '.spec.kubehz.access')              echo "registered" ;;
      '.kind // ""')                      echo "Lo" ;;
      '.spec.cluster.domain // ""')       echo "acme.example.org" ;;
      '.spec.kubernetes.version // ""')   echo "1.33.0" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Each curl call goes into CALL_LOG: its method and URL, then its headers,
  # user, body and config, as curl reads them (tests/lib/curl_capture.sh).
  # The register of a cluster gets an answer (a claimed cluster), so the
  # hcloud connect of kubehz::direct_claim comes after it. Each other call
  # fails as curl does when it cannot connect (exit 7).
  export CALL_LOG="${BATS_TEST_TMPDIR}/calls.log"
  curl() {
    curl_capture "$@"
    {
      printf 'call %s %s\n' "${CURL_METHOD:-GET}" "${CURL_URL}"
      printf '%s\n' "${CURL_HEADERS}" "${CURL_USER}" "${CURL_BODY}" "${CURL_CONFIG}"
    } >> "${CALL_LOG}"
    case "${CURL_URL}" in
      */api/clusters/register) printf '%s\n200' '{"id":"cl-9","claimed":true}' ;;
      *) return 7 ;;
    esac
  }
  export KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  kubectl() {
    printf 'kubectl %s\n' "$*" >> "${KUBECTL_LOG}"
    return 1
  }
  export -f curl curl_capture curl_unquote argv_log kubectl

  sleep() { :; }
  export -f sleep

  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  export XDG_CACHE_HOME="${BATS_TEST_TMPDIR}/cache"
  mkdir -p "${TMPDIR}"
  unset KUBEHZ_TOKEN HCLOUD_TOKEN HCLOUD_API_BASE KUBEHZ_API_URL LOK8S_KUBEHZ_API_URL \
    LOK8S_KUBEHZ_CONNECT_TOKEN KUBEHZ_AGENT_CLIENT_ID KUBEHZ_AGENT_CLIENT_SECRET \
    KUBEHZ_AGENT_TOKEN_URL KUBEHZ_AGENT_SCOPE SPEC_HOSTING

  # The credential under test ends in a CR. Every value that a case refuses
  # holds the text cr-test-credential, so a check can find it in a call or
  # in the output. The other credentials have no control character.
  BAD=$'cr-test-credential\r'
  GOOD="khz-good-token"
  NONCE_BAD=$'khzn_cr-test-credential-0123456789abcdef\r'
}

teardown() {
  teardown_tmpdir
}

# _case <case>: the setup of one row, then its entry point. Runs under
# `run`, so each export stays in that case.
_case() {
  local api="https://api.example.test"
  case "$1" in
    status)
      export KUBEHZ_TOKEN="${BAD}"
      kubehz::status ;;
    assess)
      export KUBEHZ_TOKEN="${BAD}"
      kubehz::assess ;;
    re-enroll)
      export KUBEHZ_TOKEN="${BAD}"
      kubehz::re-enroll ;;
    resolve-cluster-id)
      export KUBEHZ_TOKEN="${BAD}"
      kubehz::resolve_cluster_id acme.example.org "${api}" ;;
    deregister)
      export KUBEHZ_TOKEN="${BAD}" LOK8S_KUBEHZ_API_URL="${api}"
      kubehz::deregister_cluster acme.example.org "${SPEC}" ;;
    deregister-hcloud)
      # The registry read fails (curl exit 7), and the removal of the claim
      # key comes after it.
      export KUBEHZ_TOKEN="${GOOD}" HCLOUD_TOKEN="${BAD}" LOK8S_KUBEHZ_API_URL="${api}"
      kubehz::deregister_cluster acme.example.org "${SPEC}" ;;
    claim-key)
      export HCLOUD_TOKEN="${BAD}"
      kubehz::ensure_claim_key acme.example.org "${api}" ;;
    direct-claim)
      export KUBEHZ_TOKEN="${BAD}"
      kubehz::direct_claim acme.example.org "${SPEC}" "${api}" ;;
    direct-claim-hcloud)
      # The register succeeds. The hcloud connect after it is optional, so
      # the refusal is a warning and the status stays 0.
      export KUBEHZ_TOKEN="${GOOD}" HCLOUD_TOKEN="${BAD}" LOK8S_KUBEHZ_CONNECT_TOKEN=true
      kubehz::direct_claim acme.example.org "${SPEC}" "${api}" ;;
    wait-for-cluster)
      export KUBEHZ_TOKEN="${BAD}"
      kubehz::wait_for_cluster "${api}" cl-1 1 ;;
    provision-hosted)
      export KUBEHZ_TOKEN="${BAD}" LOK8S_KUBEHZ_API_URL="${api}"
      kubehz::provision_hosted acme.example.org "${SPEC}" ;;
    destroy-hosted)
      export KUBEHZ_TOKEN="${BAD}" LOK8S_KUBEHZ_API_URL="${api}"
      kubehz::destroy_hosted acme.example.org "${SPEC}" ;;
    space-api)
      export KUBEHZ_TOKEN="${BAD}" LOK8S_KUBEHZ_API_URL="${api}"
      kubehz::space_api GET /api/spaces ;;
    node-preflight)
      export SPEC_HOSTING=hosted KUBEHZ_TOKEN="${BAD}"
      node::preflight acme.example.org "list nodes" ;;
    node-space-preflight)
      export SPEC_HOSTING=shared KUBEHZ_TOKEN="${BAD}"
      node::space_preflight acme.example.org "cordon a node" ;;
    agent-token)
      export KUBEHZ_API_URL="${api}" KUBEHZ_TOKEN="${BAD}"
      kubehz::agent_session "space list" ;;
    agent-key | token)
      export KUBEHZ_API_URL="${api}" KUBEHZ_AGENT_CLIENT_ID="kubehz-agent-ak-1a2b3c4d" \
        KUBEHZ_AGENT_CLIENT_SECRET="${BAD}" \
        KUBEHZ_AGENT_TOKEN_URL="https://id.example.test/oauth/v2/token" KUBEHZ_AGENT_SCOPE="openid"
      if [[ "$1" == token ]]; then
        kubehz::token
      else
        kubehz::agent_session "space list"
      fi ;;
    claim)
      kubehz::claim --nonce "${NONCE_BAD}" ;;
    *)
      echo "no case $1" >&2
      return 99 ;;
  esac
}

@test "each kubehz entry point refuses a credential with a CR at its end before a request" {
  local row name site want rc calls before
  local -a bad=()
  for row in "${CR_TABLE[@]}"; do
    IFS='|' read -r name site want rc calls <<<"${row}"
    rm -f "${CALL_LOG}" "${KUBECTL_LOG}"
    before=${#bad[@]}
    run _case "${name}"
    if [[ "${status}" != "${rc}" ]]; then
      bad+=("${name}: status ${status}, want ${rc}")
    fi
    if [[ "${output}" != *"${want}"* ]]; then
      bad+=("${name}: the output does not hold: ${want}")
    fi
    if [[ "${output}" == *cr-test-credential* ]]; then
      bad+=("${name}: the output holds the credential")
    fi
    case "${calls}" in
      none)
        if [[ -e "${CALL_LOG}" || -e "${KUBECTL_LOG}" ]]; then
          bad+=("${name}: a request before the refusal: $(cat "${CALL_LOG}" "${KUBECTL_LOG}" 2>/dev/null | grep -E '^(call|kubectl) ' || true)")
        fi ;;
      not:?*)
        if cat "${CALL_LOG}" "${KUBECTL_LOG}" 2>/dev/null | grep -qF cr-test-credential; then
          bad+=("${name}: a request carries the credential")
        fi
        if [[ -e "${CALL_LOG}" ]] && grep '^call ' "${CALL_LOG}" | grep -qF -- "${calls#not:}"; then
          bad+=("${name}: a request to ${calls#not:}")
        fi ;;
      *) bad+=("${name}: no calls rule ${calls}") ;;
    esac
    if (( ${#bad[@]} > before )); then
      printf 'case %s, output:\n%s\n' "${name}" "${output}"
    fi
  done
  if (( ${#bad[@]} )); then
    printf '%s\n' "${bad[@]}"
    return 1
  fi
}

# The sites of the table must be the sites of the libs: each check in the
# kubehz libs (_lib_sites), with the function that holds it. A new check of
# a credential then fails here until the table has a row for it. Two rows
# can go through one site (two entry points, one check), so the table side
# counts each site once. The lib side counts each check: a second check of
# one name in one function fails, and so does a check that goes.
@test "the table has a row for each credential check of the kubehz libs" {
  local row name site want rc calls f want_sites got_sites unread
  local -a sites=() libs=()
  for row in "${CR_TABLE[@]}"; do
    IFS='|' read -r name site want rc calls <<<"${row}"
    [[ "${site}" == - ]] || sites+=("${site}")
  done
  for row in "${SITES_NOT_IN_TABLE[@]}"; do
    sites+=("${row%%|*}")
  done
  want_sites=$(printf '%s\n' "${sites[@]}" | LC_ALL=C sort -u)
  for f in "${_PROJECT_ROOT}/.lok8s/libs/kubehz"/*; do
    [[ ! -f "${f}" ]] || libs+=("${f}")
  done
  (( ${#libs[@]} >= 5 ))
  got_sites=$(_lib_sites "${libs[@]}")
  unread=$(grep '^UNREAD ' <<<"${got_sites}" || true)
  if [[ -n "${unread}" ]]; then
    printf 'a check that the gate cannot read:\n%s\n' "${unread}"
    return 1
  fi
  got_sites=$(LC_ALL=C sort <<<"${got_sites}")
  [ -n "${got_sites}" ]
  if [[ "${got_sites}" != "${want_sites}" ]]; then
    diff <(printf '%s\n' "${want_sites}") <(printf '%s\n' "${got_sites}") || true
    return 1
  fi
}

# Each shape of a check that _lib_sites must find, and each shape that it
# must refuse to read.
@test "the site finder reads each shape of a check" {
  local fx="${BATS_TEST_TMPDIR}/fixture-lib" got
  cat > "${fx}" <<'FIXTURE'
# http::credential_ok IN_A_COMMENT
fx::def() { http::credential_ok DEF_LINE || return 1; }
fx::names() {
  http::credential_ok FIRST SECOND_2 "QUOTED" 'SINGLE' || return 1
  if ! http::credential_ok AFTER_NOT; then return 1; fi
  http::require_https "${u}" x && http::credential_ok AFTER_AND && kubehz::token_has_control "${v}"
  [[ "${w}" == *[[:cntrl:]]* ]] && return 1
  http::credential_ok "${dynamic}" || return 1
  http::credential_ok ONE \
    TWO
  http::credential_ok || return 1
  http::credential_ok NAME$suffix || return 1
}
FIXTURE
  got=$(_lib_sites "${fx}" | sed "s|${fx}|fx|" | LC_ALL=C sort)
  assert_equal "${got}" "$(LC_ALL=C sort <<'WANT'
fx::def http::credential_ok DEF_LINE
fx::names http::credential_ok FIRST
fx::names http::credential_ok SECOND_2
fx::names http::credential_ok QUOTED
fx::names http::credential_ok SINGLE
fx::names http::credential_ok AFTER_NOT
fx::names http::credential_ok AFTER_AND
fx::names kubehz::token_has_control
fx::names [[:cntrl:]]
fx::names http::credential_ok ONE
UNREAD fx:8:   http::credential_ok "${dynamic}" || return 1
UNREAD fx:9:   http::credential_ok ONE \
UNREAD fx:11:   http::credential_ok || return 1
UNREAD fx:12:   http::credential_ok NAME$suffix || return 1
WANT
)"
}

# `lo kubehz claim --nonce -` reads one line from stdin and removes the
# white space at its start and at its end. So the CR of a CRLF line goes
# before the shape check, and the claim goes on to its kubectl call.
@test "claim: --nonce - drops the CR at the end of a CRLF line before the shape check" {
  rm -f "${KUBECTL_LOG}"
  run kubehz::claim --nonce - <<<"${NONCE_BAD}"
  assert_failure
  refute_output --partial "invalid claim nonce"
  assert_output --partial "configmap/kubehz-agent-state not found"
  grep -q '^kubectl -n kubehz-system get configmap kubehz-agent-state' "${KUBECTL_LOG}"
}

#!/usr/bin/env bats
# kubehz_agent_test.bats — `lo kubehz space …` and `lo kubehz cluster …`, the
# agent tools (bash twin of internal/kubehz/agent.go).
#
# Pins with the Go tests' values: the text and json bytes, every refusal
# message, the hints, the id and number checks that run before any request,
# and the bearer containment (the access token reaches curl in a config on
# stdin, never on argv). hack/parity-kubehz.sh diffs the two implementations
# against an https stub on top of this.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  # argsh `:args` stub: positionals in the order the args array names them,
  # `--name value` pairs, -o, and the one boolean --force/-f.
  :args() {
    shift
    local -a _pos=()
    local _i _p=0 _n
    for (( _i = 0; _i < ${#args[@]}; _i += 2 )); do
      [[ -z "${args[_i]}" || "${args[_i]}" == *"|"* ]] || _pos+=("${args[_i]}")
    done
    while (( $# )); do
      case "$1" in
        -o) shift; printf -v output '%s' "${1:-}" ;;
        --force|-f) force=1 ;;
        --*) _n="${1#--}"; _n="${_n//-/_}"; shift; printf -v "${_n}" '%s' "${1:-}" ;;
        *) printf -v "${_pos[_p]}" '%s' "$1"; _p=$(( _p + 1 )) ;;
      esac
      shift || true
    done
  }
  export -f :args

  export XDG_CACHE_HOME="${BATS_TEST_TMPDIR}/cache"
  export KUBEHZ_API_URL="https://api.kubehz.example/"
  export KUBEHZ_AGENT_CLIENT_ID="kubehz-agent-ak-1a2b3c4d"
  export KUBEHZ_AGENT_CLIENT_SECRET="s3cr3t-value"
  export KUBEHZ_AGENT_TOKEN_URL="https://id.kubehz.example/oauth/v2/token"
  export KUBEHZ_AGENT_SCOPE="openid"
  unset KUBEHZ_TOKEN
  export CURL_ARGS="${BATS_TEST_TMPDIR}/curl.args"
  export CURL_STDIN="${BATS_TEST_TMPDIR}/curl.stdin"
  export ROUTES="${BATS_TEST_TMPDIR}/routes"
  : > "${ROUTES}"

  # curl stub: records argv (one line per call, the -w newline as \n) and
  # stdin. The token endpoint mints jwt-agent; an api call answers the
  # ROUTES line "<METHOD> <path> <code> <body>" (path without the query),
  # else 404. STUB_DOWN=1: no answer (exit 7).
  curl() {
    local all="$*"
    printf '%s\n' "${all//$'\n'/\\n}" >> "${CURL_ARGS}"
    cat >> "${CURL_STDIN}"
    [[ -z "${STUB_DOWN:-}" ]] || return 7
    local url="${*: -1}" method=GET prev="" a path line
    for a in "$@"; do
      [[ "${prev}" != "-X" ]] || method="${a}"
      prev="${a}"
    done
    if [[ "${url}" == *"/oauth/v2/token" ]]; then
      printf '%s\n%s' '{"access_token":"jwt-agent","expires_in":43199}' 200
      return 0
    fi
    # STUB_HOOK runs before an api answer (a file that appears, a link
    # that moves during the download).
    [[ -z "${STUB_HOOK:-}" ]] || eval "${STUB_HOOK}"
    path="${url#https://api.kubehz.example}"
    path="${path%%\?*}"
    while IFS= read -r line; do
      [[ "${line}" == "${method} ${path} "* ]] || continue
      line="${line#"${method} ${path} "}"
      printf '%s\n%s' "${line#* }" "${line%% *}"
      return 0
    done < "${ROUTES}"
    printf '%s\n%s' '{"data":{"code":"UNMOCKED"}}' 404
  }
  export -f curl

  date() {
    if [[ "$*" == "-u +%s" ]]; then echo 1700000000; else command date "$@"; fi
  }
  export -f date

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/agent"
}

teardown() {
  teardown_tmpdir
}

# route <method> <path> <code> <body>
route() { printf '%s %s %s %s\n' "$1" "$2" "$3" "$4" >> "${ROUTES}"; }
# api_calls: how many api requests curl saw (the grant does not count).
api_calls() {
  [[ -f "${CURL_ARGS}" ]] || { echo 0; return; }
  grep -c 'api.kubehz.example' "${CURL_ARGS}" || true
}
# quiet: run with stderr dropped, so ${output} is stdout alone.
quiet() { "$@" 2>/dev/null; }

SPACE='{"id":"sp-1a2b3c4d","tenantId":"t-1","name":"Acme Prod","slug":"acme","status":"Active","maxNodes":2,"maxNamespaces":1,"maxObjectKiB":256,"leaseExpiresAt":"2026-10-04T14:00:00.000Z","createdAt":"2026-10-04T12:00:00.000Z","namespaces":["acme"],"nodeCount":1}'
ODD='{"id":"sp-9z9z9z9z","name":"Ev\u001b[2Jil \\ na\u2028me\u0085\u009b\u202e\u2066 \u00fc","slug":"yes","status":"Pending","maxNodes":1,"maxNamespaces":3,"maxObjectKiB":null,"leaseExpiresAt":null,"createdAt":"2026-10-04T12:30:00.000Z","namespaces":["yes",7],"nodeCount":0}'
DETAIL='{"id":"sp-1a2b3c4d","name":"Acme Prod","slug":"acme","status":"Active","maxNodes":2,"maxNamespaces":1,"maxObjectKiB":256,"leaseExpiresAt":"2026-10-04T14:00:00.000Z","createdAt":"2026-10-04T12:00:00.000Z","namespaces":[{"name":"acme","createdAt":"x"}],"nodes":[{"name":"worker-1","lane":"metal","status":"Ready"}],"usage":{"nodes":1,"nodesReady":1},"endpoint":"https://acme.k8s.kubehz.example"}'
CLUSTER='{"id":"cl-1a2b3c4d","domain":"agent.example.org","hosting":"hosted","status":"Running","region":"fsn1","kubernetesVersion":"v1.34.1","controlPlaneReplicas":1,"apiEndpoint":"https://203.0.113.7:6443","health":"healthy","leaseExpiresAt":null,"createdAt":"2026-10-04T12:00:00.000Z","workers":[]}'

@test "space list: the minted bearer, the canonical path and the Go twin's table" {
  route GET /api/spaces 200 '{"ok":true,"data":['"${SPACE}"','"${ODD}"',"junk"],"meta":{"pagination":{"total":2}}}'
  run quiet kubehz::agent_list space text
  assert_success
  assert_output "ID           SLUG  NAME              STATUS   NODES  LEASE ENDS
sp-1a2b3c4d  acme  Acme Prod         Active   1/2    2026-10-04T14:00:00.000Z
sp-9z9z9z9z  yes   Ev[2Jil \\ name ü  Pending  0/1    -"
  # The trailing slash of KUBEHZ_API_URL is dropped (a double slash is not
  # canonical), and the page is the api's maximum.
  run grep -c 'https://api.kubehz.example/api/spaces?perPage=500$' "${CURL_ARGS}"
  assert_output 1
}

@test "space list -o json: the Go twin's bytes, every control character dropped" {
  route GET /api/spaces 200 '{"ok":true,"data":['"${ODD}"']}'
  run quiet kubehz::agent_list space json
  assert_success
  assert_output '{
  "spaces": [
    {
      "id": "sp-9z9z9z9z",
      "name": "Ev[2Jil \\ name ü",
      "slug": "yes",
      "status": "Pending",
      "maxNodes": 1,
      "maxNamespaces": 3,
      "maxObjectKiB": null,
      "nodeCount": 0,
      "namespaces": [
        "yes"
      ],
      "leaseExpiresAt": null,
      "createdAt": "2026-10-04T12:30:00.000Z"
    }
  ]
}'
}

@test "cluster list: a short page gets the warning, the table the Go twin's columns" {
  route GET /api/clusters 200 '{"ok":true,"data":['"${CLUSTER}"'],"meta":{"pagination":{"total":612}}}'
  run kubehz::agent_list cluster text
  assert_success
  assert_line --index 0 '[warn] kubehz cluster list: the list shows 1 of 612 clusters'
  assert_line --index 1 'ID           DOMAIN             HOSTING  STATUS   VERSION  LEASE ENDS'
  assert_line --index 2 'cl-1a2b3c4d  agent.example.org  hosted   Running  v1.34.1  -'
}

@test "space list: an answer without a list is refused" {
  route GET /api/spaces 200 '{"ok":true,"data":'"${SPACE}"'}'
  run kubehz::agent_list space text
  assert_failure
  assert_output '[error] kubehz space list: the api answered without a list of spaces'
}

@test "space get: the detail projection, text and json" {
  route GET /api/spaces/sp-1a2b3c4d 200 '{"ok":true,"data":'"${DETAIL}"'}'
  run kubehz::space::get sp-1a2b3c4d
  assert_success
  assert_output "ID:          sp-1a2b3c4d
Name:        Acme Prod
Slug:        acme
Status:      Active
Nodes:       1 of 2
Namespaces:  acme (limit 1)
Object cap:  256 KiB
Endpoint:    https://acme.k8s.kubehz.example
Lease ends:  2026-10-04T14:00:00.000Z
Created:     2026-10-04T12:00:00.000Z"
  run kubehz::space::get sp-1a2b3c4d -o json
  assert_success
  assert_output --partial '"nodeCount": 1,'
  assert_output --partial '"createdAt": "2026-10-04T12:00:00.000Z",
  "endpoint": "https://acme.k8s.kubehz.example",
  "nodes": [
    {
      "name": "worker-1",
      "status": "Ready"
    }
  ]
}'
}

@test "cluster get: the Go twin's text" {
  route GET /api/clusters/cl-1a2b3c4d 200 '{"ok":true,"data":'"${CLUSTER}"'}'
  run kubehz::cluster::get cl-1a2b3c4d
  assert_success
  assert_output "ID:          cl-1a2b3c4d
Domain:      agent.example.org
Hosting:     hosted
Status:      Running
Health:      healthy
Region:      fsn1
Version:     v1.34.1
Apiservers:  1
Endpoint:    https://203.0.113.7:6443
Lease ends:  -
Created:     2026-10-04T12:00:00.000Z"
}

@test "an -o other than text, json or yaml is the Go twin's parse error" {
  run kubehz::space::get sp-1a2b3c4d -o xml
  assert_failure
  assert_output 'Error: invalid --output "xml": text, json or yaml

  Run "lo -h" for more information.'
  [ "$(api_calls)" -eq 0 ]
}

@test "space create: only the numbers given are sent, normalized" {
  route POST /api/spaces 201 '{"ok":true,"data":'"${SPACE}"'}'
  run kubehz::space::create --name 'Acme Prod' --slug acme
  assert_success
  assert_line --index 0 'ID:          sp-1a2b3c4d'
  refute_output --partial 'Endpoint:'
  run grep -c -- '--data-binary {"name":"Acme Prod","slug":"acme"} ' "${CURL_ARGS}"
  assert_output 1
  : > "${CURL_ARGS}"
  run kubehz::space::create --name Acme --slug acme --nodes 2 --namespaces 008 --object-cap-kib 256 --region fsn1 --lease-hours 0720
  assert_success
  run grep -c -- '--data-binary {"name":"Acme","slug":"acme","maxNodes":2,"maxNamespaces":8,"maxObjectKiB":256,"region":"fsn1","leaseHours":720} ' "${CURL_ARGS}"
  assert_output 1
}

@test "space create: a bad number is refused before any request" {
  local flag value
  for flag in "nodes 0" "nodes -1" "namespaces 2.5" "object-cap-kib 99999999999999999999" "nodes +5"; do
    value="${flag#* }"
    flag="${flag%% *}"
    run kubehz::space::create --name Acme --slug acme "--${flag}" "${value}"
    assert_failure
    assert_output "[error] kubehz space create acme: --${flag} ${value} is not valid
  Use a whole number of 1 or more, or leave the flag out for the platform default."
  done
  run kubehz::space::create --name Acme --slug acme --lease-hours 721
  assert_failure
  assert_output --partial '[error] kubehz space create acme: --lease-hours 721 is not valid'
  [ ! -e "${CURL_ARGS}" ]
}

@test "lease: the hours are sent as a number and checked against 1 to 720" {
  route PATCH /api/spaces/sp-1a2b3c4d/lease 200 '{"ok":true,"data":{"id":"sp-1a2b3c4d","leaseExpiresAt":"2026-10-05T12:00:00.000Z"}}'
  run kubehz::space::lease sp-1a2b3c4d --hours 024
  assert_success
  assert_output 'space sp-1a2b3c4d: the lease ends 2026-10-05T12:00:00.000Z'
  run grep -c -- '-X PATCH .*--data-binary {"hours":24} ' "${CURL_ARGS}"
  assert_output 1
  run kubehz::space::lease sp-1a2b3c4d --hours 024 -o json
  assert_output '{
  "id": "sp-1a2b3c4d",
  "leaseExpiresAt": "2026-10-05T12:00:00.000Z"
}'
  : > "${CURL_ARGS}"
  local hours
  for hours in 0 721 x 1e3 99999999999999999999; do
    run kubehz::cluster::lease cl-1a2b3c4d --hours "${hours}"
    assert_failure
    assert_output "[error] kubehz cluster lease cl-1a2b3c4d: --hours ${hours} is not valid
  Use a whole number of hours from 1 to 720."
  done
  [ "$(api_calls)" -eq 0 ]
}

@test "delete: the Go twin's text and json" {
  route DELETE /api/spaces/sp-1a2b3c4d 200 '{"ok":true,"data":{"id":"sp-1a2b3c4d","status":"Deleting"}}'
  run kubehz::space::delete sp-1a2b3c4d
  assert_success
  assert_output 'space sp-1a2b3c4d: Deleting'
  run kubehz::space::delete sp-1a2b3c4d -o yaml
  assert_success
  assert_output 'id: sp-1a2b3c4d
status: Deleting'
}

@test "an id outside the api's shape reaches no request, not even the grant" {
  run kubehz::space::get ../clusters
  assert_failure
  assert_output '[error] kubehz space get: ../clusters is not a space id
  A space id is sp- and 1 to 64 letters, digits or dashes. List them: lo kubehz space list'
  run kubehz::space::delete 'sp-1/../../x'
  assert_output --partial '[error] kubehz space delete: sp-1/../../x is not a space id'
  run kubehz::cluster::get sp-1a2b3c4d
  assert_output --partial '[error] kubehz cluster get: sp-1a2b3c4d is not a cluster id
  A cluster id is cl- and'
  run kubehz::cluster::lease "cl-$(printf 'a%.0s' {1..65})" --hours 1
  assert_output --partial 'is not a cluster id'
  run kubehz::space::kubeconfig 'sp-a?b' --file kc
  assert_output --partial '[error] kubehz space kubeconfig: sp-a?b is not a space id'
  run kubehz::space::get $'sp-\e[2Jx'
  assert_output --partial '[error] kubehz space get: sp-[2Jx is not a space id'
  [ ! -e "${CURL_ARGS}" ]
}

@test "refusals: the status, the code, the api's message and one next step" {
  local body='{"statusCode":401,"data":{"code":"UNAUTHORIZED","message":"Invalid or expired agent key","help":"api help"}}'
  route GET /api/spaces/sp-1a2b3c4d 401 "${body}"
  run kubehz::space::get sp-1a2b3c4d
  assert_failure
  assert_output '[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 401 UNAUTHORIZED): Invalid or expired agent key
  The api did not accept the agent key. A revoked or expired key needs a new key from a tenant owner.'

  unset KUBEHZ_AGENT_CLIENT_ID KUBEHZ_AGENT_CLIENT_SECRET
  export KUBEHZ_TOKEN=khzt_x
  run kubehz::space::get sp-1a2b3c4d
  assert_line --index 1 '  The api did not accept KUBEHZ_TOKEN. Mint a new token in the kubehz dashboard.'
}

@test "refusals: lo's hint replaces the api's help where lo knows more" {
  local code msg want
  while IFS='|' read -r code msg want; do
    : > "${ROUTES}"
    route GET /api/spaces/sp-1a2b3c4d 409 '{"data":{"code":"'"${code}"'","message":"'"${msg}"'","help":"api help"}}'
    run kubehz::space::get sp-1a2b3c4d
    assert_failure
    assert_output "[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 409 ${code}): ${msg}
  ${want}"
  done <<'EOF'
TOKEN_SCOPE_MISSING|This endpoint requires the 'clusters:write' scope|The agent key can read but not write. Use an agent key with the role editor or admin.
AGENT_KEY_OUT_OF_SCOPE|tenant-wide|api help
AGENT_KEY_SPEND_CAP|This agent key spent 1200 of its 1000 cents this month|Delete what the agent key created, or ask a tenant owner for a key with a higher spend cap. The count starts again on the first day of the month (UTC).
SPACE_LIMITS_ABOVE_FREE|too big|api help
SPACE_LIMITS_ABOVE_SHARED|too big|Lower --nodes, --namespaces or --object-cap-kib. A value you leave out takes the platform default.
NO_SHARD_AVAILABLE|full|This is a platform capacity limit, not an account limit. Try again later, or name another region with --region.
KUBECONFIG_NOT_READY|not ready|api help
EOF
  : > "${ROUTES}"
  route GET /api/spaces/sp-1a2b3c4d 404 '{"data":{"code":"NOT_FOUND","message":"Space not found","help":"api help"}}'
  run kubehz::space::get sp-1a2b3c4d
  assert_line --index 1 '  No space sp-1a2b3c4d exists, or the credential cannot reach it. List what it reaches: lo kubehz space list'
}

@test "refusals: TOKEN_SCOPE_MISSING with KUBEHZ_TOKEN prints the api's help" {
  # A KUBEHZ_TOKEN can hold clusters:write without read: lo cannot say
  # which scope is missing, the api's help does.
  route GET /api/spaces/sp-1a2b3c4d 403 '{"data":{"code":"TOKEN_SCOPE_MISSING","message":"This endpoint requires the '"'read'"' scope","help":"Mint a token carrying it."}}'
  KUBEHZ_TOKEN=khzt_x KUBEHZ_AGENT_CLIENT_ID="" KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::space::get sp-1a2b3c4d
  assert_failure
  assert_output "[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 403 TOKEN_SCOPE_MISSING): This endpoint requires the 'read' scope
  Mint a token carrying it."
}

@test "refusals: server strings are cleaned, an odd code is not repeated, an empty body has no reason" {
  route GET /api/spaces/sp-1a2b3c4d 400 '{"data":{"code":"BAD_REQUEST","message":"a\u001b[2Jb \\e[31m\nc\td\u009b\u202e\u2069","help":"\u001b]0;t\u0007c\n\u0085"}}'
  run kubehz::space::get sp-1a2b3c4d
  assert_output '[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 400 BAD_REQUEST): a[2Jb \e[31mcd
  ]0;tc'
  : > "${ROUTES}"
  route GET /api/spaces/sp-1a2b3c4d 418 '{"data":{"code":"x; rm","message":"m"}}'
  run kubehz::space::get sp-1a2b3c4d
  assert_output '[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 418): m'
  : > "${ROUTES}"
  route GET /api/spaces/sp-1a2b3c4d 502 ''
  run kubehz::space::get sp-1a2b3c4d
  assert_output '[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 502): no reason given'
  # A message or help that is not a string counts as absent.
  : > "${ROUTES}"
  route GET /api/spaces/sp-1a2b3c4d 400 '{"data":{"code":"BAD_REQUEST","message":5,"help":{"a":1}}}'
  run kubehz::space::get sp-1a2b3c4d
  assert_output '[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 400 BAD_REQUEST): no reason given'
  : > "${ROUTES}"
  route GET /api/spaces/sp-1a2b3c4d 400 '{"data":{"message":["x"]},"message":"top"}'
  run kubehz::space::get sp-1a2b3c4d
  assert_output '[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 400): top'
}

@test "a 404 on a list names no id: the api's help is the next step" {
  route GET /api/spaces 404 '{"data":{"code":"NOT_FOUND","message":"Not found","help":"Check KUBEHZ_API_URL."}}'
  run kubehz::agent_list space text
  assert_failure
  assert_output '[error] kubehz space list: the api refused the request (HTTP 404 NOT_FOUND): Not found
  Check KUBEHZ_API_URL.'
}

@test "a 2xx without a record is refused" {
  local body
  for body in '<html>gateway</html>' '{"ok":true,"data":[]}' '{"id":"sp-1a2b3c4d"}'; do
    : > "${ROUTES}"
    route GET /api/spaces/sp-1a2b3c4d 200 "${body}"
    run kubehz::space::get sp-1a2b3c4d
    assert_failure
    assert_output '[error] kubehz space get sp-1a2b3c4d: the api answered without a space record'
  done
}

@test "session refusals print the Go twin's messages and reach no api" {
  KUBEHZ_API_URL="" run kubehz::agent_list space text
  assert_output '[error] kubehz space list: KUBEHZ_API_URL is not set
  Set it to the kubehz api, for example: export KUBEHZ_API_URL=https://api.kubehz.cloud'
  KUBEHZ_API_URL="http://api.kubehz.example" run kubehz::agent_list space text
  assert_output '[error] KUBEHZ_API_URL must use HTTPS: http://api.kubehz.example
[error] Plain HTTP is not allowed for security reasons'
  KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::agent_list space text
  assert_output '[error] kubehz space list: the agent key needs KUBEHZ_AGENT_CLIENT_SECRET
  The answer that created the key holds the four values: clientId, clientSecret, tokenEndpoint and tokenScope.'
  KUBEHZ_AGENT_TOKEN_URL="" KUBEHZ_AGENT_SCOPE="" run kubehz::agent_list space text
  assert_line --index 0 '[error] kubehz space list: the agent key needs KUBEHZ_AGENT_TOKEN_URL and KUBEHZ_AGENT_SCOPE'
  KUBEHZ_AGENT_CLIENT_ID="" KUBEHZ_AGENT_TOKEN_URL="" KUBEHZ_AGENT_SCOPE="" run kubehz::agent_list space text
  assert_line --index 0 '[error] kubehz space list: the agent key needs KUBEHZ_AGENT_CLIENT_ID, KUBEHZ_AGENT_TOKEN_URL and KUBEHZ_AGENT_SCOPE'
  KUBEHZ_AGENT_CLIENT_ID="" KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::agent_list space text
  assert_output '[error] kubehz space list: no credential for the kubehz api
  Set KUBEHZ_AGENT_CLIENT_ID and KUBEHZ_AGENT_CLIENT_SECRET (an agent key), or KUBEHZ_TOKEN.'
  KUBEHZ_AGENT_CLIENT_ID="" KUBEHZ_AGENT_CLIENT_SECRET="" KUBEHZ_TOKEN=$'a\nb' run kubehz::agent_list space text
  assert_output '[error] kubehz space list: the access token holds a control character'
  [ "$(api_calls)" -eq 0 ]
}

@test "the bearer reaches curl on stdin only, over https only, and no redirect is followed" {
  route GET /api/spaces 200 '{"ok":true,"data":[]}'
  run kubehz::agent_list space text
  assert_success
  run grep -c 'header = "Authorization: Bearer jwt-agent"' "${CURL_STDIN}"
  assert_output 1
  run grep 'api.kubehz.example' "${CURL_ARGS}"
  refute_output --partial 'jwt-agent'
  refute_output --partial 's3cr3t-value'
  refute_output --partial ' -L'
  assert_output --regexp '^-q -sS --proto =https '
  assert_output --partial '--max-time 60 -K - -X GET -H Accept: application/json'
}

@test "KUBEHZ_TOKEN is the bearer without a key; the key wins when both are set" {
  route GET /api/spaces 200 '{"ok":true,"data":[]}'
  KUBEHZ_TOKEN=khzt_tenant KUBEHZ_AGENT_CLIENT_ID="" KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::agent_list space json
  assert_success
  assert_output '{
  "spaces": []
}'
  run grep -c 'Bearer khzt_tenant' "${CURL_STDIN}"
  assert_output 1
  run grep -c 'oauth/v2/token' "${CURL_ARGS}"
  assert_output 0
  : > "${CURL_STDIN}"
  KUBEHZ_TOKEN=khzt_tenant run kubehz::agent_list space text
  assert_output 'ID  SLUG  NAME  STATUS  NODES  LEASE ENDS'
  run grep -c 'Bearer jwt-agent' "${CURL_STDIN}"
  assert_output 1
}

@test "no answer names the api and the next step" {
  STUB_DOWN=1 KUBEHZ_TOKEN=khzt_x KUBEHZ_AGENT_CLIENT_ID="" KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::space::get sp-1a2b3c4d
  assert_failure
  assert_output '[error] kubehz space get sp-1a2b3c4d: the api at https://api.kubehz.example did not answer
  Check KUBEHZ_API_URL and the network. Then try again.'
}

@test "kubeconfig: the body goes to a 0600 file, stdout carries the path only" {
  route GET /api/spaces/sp-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local file="${BATS_TEST_TMPDIR}/out/kc.yaml"
  mkdir -p "${BATS_TEST_TMPDIR}/out"
  run kubehz::space::kubeconfig sp-1a2b3c4d --file "${file}"
  assert_success
  assert_output "${file}"
  run cat "${file}"
  assert_output 'apiVersion: v1'
  run stat -c '%a' "${file}"
  assert_output 600
  run ls -A "${BATS_TEST_TMPDIR}/out"
  assert_output 'kc.yaml'
  run kubehz::space::kubeconfig sp-1a2b3c4d --file "${file}" -o json --force
  assert_output '{
  "id": "sp-1a2b3c4d",
  "file": "'"${file}"'"
}'
}

@test "kubeconfig: a missing directory, an empty answer and a refusal write nothing" {
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${BATS_TEST_TMPDIR}/no/kc.yaml"
  assert_failure
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write ${BATS_TEST_TMPDIR}/no/kc.yaml
  Name a file in a directory that exists and that you can write to."
  : > "${ROUTES}"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 ' '
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_output '[error] kubehz cluster kubeconfig cl-1a2b3c4d: the api answered without a kubeconfig'
  : > "${ROUTES}"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 409 '{"data":{"code":"KUBECONFIG_NOT_READY","message":"not ready","help":"Poll, then retry."}}'
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_output '[error] kubehz cluster kubeconfig cl-1a2b3c4d: the api refused the request (HTTP 409 KUBECONFIG_NOT_READY): not ready
  Poll, then retry.'
  [ ! -e "${BATS_TEST_TMPDIR}/kc.yaml" ]
}

@test "kubeconfig: the path rules run before any request, as the Go twin's kubeconfigTarget" {
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d="${BATS_TEST_TMPDIR}/p"
  mkdir -p "${d}/dir" "${d}/kube"
  ln -s "${d}/dir" "${d}/to-dir"
  echo contexts > "${d}/config"
  echo contexts > "${d}/kube/real"
  ln -s kube/real "${d}/to-file"
  ln -s "${d}/gone" "${d}/to-nothing"

  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/dir" --force
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/dir is a directory
  Name a file, not a directory."
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/to-dir" --force
  assert_output --partial "${d}/to-dir is a directory"
  [ -L "${d}/to-dir" ]
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/config"
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/config exists
  Pass --force to replace it, or name a new file."
  run cat "${d}/config"
  assert_output contexts
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/to-file"
  assert_output --partial "${d}/to-file exists"
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/to-nothing"
  assert_output --partial "${d}/to-nothing exists"
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/to-nothing" -f
  assert_output --partial "cannot write ${d}/to-nothing"
  # A refused path costs no grant and no download.
  [ ! -e "${CURL_ARGS}" ]

  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/config" --force
  assert_success
  run cat "${d}/config"
  assert_output 'apiVersion: v1'
  # --force writes through a link: the target changes, the link stays.
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/to-file" -f
  assert_success
  assert_output "${d}/to-file"
  [ -L "${d}/to-file" ]
  run cat "${d}/kube/real"
  assert_output 'apiVersion: v1'
  run ls -A "${d}/kube"
  assert_output 'real'
}

@test "kubeconfig: a link is written through only when we own the link and its target" {
  [[ "$(id -u)" != 0 ]] || skip "root owns /etc/passwd: no foreign target to point at"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d="${BATS_TEST_TMPDIR}/own" want
  mkdir -p "${d}"
  ln -s /etc/passwd "${d}/link"
  want="[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/link is a link, and the link or its target belongs to another user
  lo writes through a link only when you own the link and its target. Name another file."
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/link" --force
  assert_failure
  assert_output "${want}"
  # As root (id stubbed), the target passes and the link (ours) does not.
  id() { if [[ "${1:-}" == -u ]]; then echo 0; else command id "$@"; fi; }
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/link" --force
  assert_failure
  assert_output "${want}"
  unset -f id
  [ ! -e "${CURL_ARGS}" ]
}

@test "kubeconfig: the path rules run again just before the write" {
  [[ "$(id -u)" != 0 ]] || skip "root owns /etc/passwd: no foreign target to point at"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d="${BATS_TEST_TMPDIR}/again"
  mkdir -p "${d}"
  echo contexts > "${d}/real"
  ln -s real "${d}/link"
  # The link moves to another user's file during the download.
  STUB_HOOK="ln -sfn /etc/passwd '${d}/link'" run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/link" --force
  assert_failure
  assert_output --partial 'is a link, and the link or its target belongs to another user'
  run cat "${d}/real"
  assert_output contexts
}

@test "kubeconfig: the write step on its own never replaces a file without --force" {
  # The race window after the last check is too short to hit through the
  # command: ln, not mv, keeps a file that appeared there.
  local d="${BATS_TEST_TMPDIR}/write"
  mkdir -p "${d}"
  echo planted > "${d}/kc.yaml"
  KUBEHZ_AGENT_BODY='new'
  run kubehz::agent_write "${d}/kc.yaml" 0
  assert_failure
  run cat "${d}/kc.yaml"
  assert_output planted
  run kubehz::agent_write "${d}/kc.yaml" 1
  assert_success
  run cat "${d}/kc.yaml"
  assert_output new
  run kubehz::agent_write "${d}/fresh.yaml" 0
  assert_success
  run stat -c '%a' "${d}/fresh.yaml"
  assert_output 600
  run ls -A "${d}"
  assert_output $'fresh.yaml\nkc.yaml'
}

@test "kubeconfig: without --force, a file that appears during the download stays" {
  route GET /api/spaces/sp-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d="${BATS_TEST_TMPDIR}/race"
  mkdir -p "${d}"
  STUB_HOOK="echo planted > '${d}/kc.yaml'" run kubehz::space::kubeconfig sp-1a2b3c4d --file "${d}/kc.yaml"
  assert_failure
  assert_output "[error] kubehz space kubeconfig sp-1a2b3c4d: ${d}/kc.yaml exists
  Pass --force to replace it, or name a new file."
  run cat "${d}/kc.yaml"
  assert_output planted
  run ls -A "${d}"
  assert_output 'kc.yaml'
}

@test "the bearer reaches curl's config with its quotes and backslashes escaped" {
  route GET /api/spaces 200 '{"ok":true,"data":[]}'
  KUBEHZ_TOKEN='a"b\c' KUBEHZ_AGENT_CLIENT_ID="" KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::agent_list space text
  assert_success
  run cat "${CURL_STDIN}"
  assert_output 'header = "Authorization: Bearer a\"b\\c"'
}

@test "the usage arrays carry the markers the tiers read, and kubeconfig none" {
  run declare -f kubehz::space
  assert_output --partial "'list@readonly'"
  assert_output --partial "'create'"
  assert_output --partial "'delete@destructive'"
  assert_output --partial "'lease@destructive'"
  run declare -f kubehz::cluster
  assert_output --partial "'lease@destructive'"
  assert_output --partial "'kubeconfig'"
}

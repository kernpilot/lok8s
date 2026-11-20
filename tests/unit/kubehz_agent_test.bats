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
# foreign_file: a regular file that another user (root) owns. The argsh
# container bind-mounts an /etc/passwd of the test user, so the first
# candidate is not always one.
foreign_file() {
  local f
  for f in /etc/passwd "$(realpath "$(command -v bash)")" "$(realpath /bin/sh)"; do
    [[ -f "${f}" && "$(stat -c '%u' -- "${f}")" == 0 ]] && { echo "${f}"; return 0; }
  done
  return 1
}

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

# clean drops each character that shown_name shows as '?' (the def
# unshown, utils/verbose.sh), but keeps a backslash. For each case of the
# shared table (internal/ui/testdata/shown.json), the input without each
# character that the case's result shows as '?' is what clean gives. The
# Go twin is TestCleanTextDropsEachUnshownCharacter.
@test "clean: drops each character that shown_name shows as '?', keeps a backslash" {
  CASES="${_PROJECT_ROOT}/internal/ui/testdata/shown.json"
  run jq -r "${KUBEHZ_AGENT_JQ}"' .[]
    | ([(.in | explode), (.want | explode)] | transpose
       | map(select(.[1] != 63 or .[0] == 63 or .[0] == 92) | .[0]) | implode) as $want
    | (.in | clean) as $got | select($got != $want)
    | "\(.case): \($got | @json), want \($want | @json)"' "${CASES}"
  assert_success
  assert_output ""
  # The table drops something, and clean keeps a backslash: else the test
  # proves nothing.
  run jq -r "${KUBEHZ_AGENT_JQ}"' [.[] | select((.in | clean) != .in)] | length' "${CASES}"
  assert_success
  (( output >= 15 ))
  run jq -r "${KUBEHZ_AGENT_JQ}"' "a\\b" | clean' -n
  assert_output 'a\b'
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

# error() prints a typed value as it is, as the Go twin does: a backslash
# in a slug, a number flag, an id or a file path is one backslash in the
# refusal, and no ESC byte reaches the output.
@test "refusals: a typed backslash in a slug, a number, an id or a file path prints as typed" {
  run kubehz::space::create --name Acme --slug 'a\e[2J' --nodes '1\n'
  assert_failure
  assert_output '[error] kubehz space create a\e[2J: --nodes 1\n is not valid
  Use a whole number of 1 or more, or leave the flag out for the platform default.'
  run kubehz::cluster::lease cl-1a2b3c4d --hours '1\c'
  assert_failure
  assert_output '[error] kubehz cluster lease cl-1a2b3c4d: --hours 1\c is not valid
  Use a whole number of hours from 1 to 720.'
  run kubehz::space::lease 'sp-\e[2J' --hours 1
  assert_failure
  assert_output --partial '[error] kubehz space lease: sp-\e[2J is not a space id'
  local d="${BATS_TEST_TMPDIR}/p\e[2J"
  mkdir -p "${d}"
  echo contexts > "${d}/config"
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/config"
  assert_failure
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/config exists
  Pass --force to replace it, or name a new file."
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}" --force
  assert_failure
  assert_output --partial "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d} is a directory"
  [[ "${output}" != *$'\033'* ]]
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

@test "yaml: a string that a YAML 1.1 reader reads as a bool or a number prints in double quotes" {
  route DELETE /api/spaces/sp-1a2b3c4d 200 '{"ok":true,"data":{"id":"sp-1a2b3c4d","status":"No"}}'
  run kubehz::space::delete sp-1a2b3c4d -o yaml
  assert_success
  assert_output 'id: sp-1a2b3c4d
status: "No"'
  route GET /api/spaces/sp-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  mkdir -p "${BATS_TEST_TMPDIR}/out"
  cd "${BATS_TEST_TMPDIR}/out"
  run kubehz::space::kubeconfig sp-1a2b3c4d --file 0777 -o yaml
  assert_success
  assert_output 'id: sp-1a2b3c4d
file: "0777"'
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
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write ${BATS_TEST_TMPDIR}/no/kc.yaml: ${BATS_TEST_TMPDIR}/no does not exist or cannot be read
  Name a file in a directory that exists and that you can write to."
  [ ! -e "${CURL_ARGS}" ]
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

# The hint of a link whose path is not safe (Go: linkNotSafe).
LINK_HINT='  lo writes through a link only to a file of yours. Each link and each directory on the way must belong to root or to you,
  and must not be writable by its group or by others unless it has the sticky bit. Name another file.'
LINK_HOPS='  Point the link at the file, or name the file itself.'
NOT_REGULAR='  lo replaces only a regular file. Name another file.'
UNWRITABLE='  Name a file in a directory that exists and that you can write to.'
UNMAPPED='  The uid of this user is not mapped in the namespace, so lo cannot check who owns the path.
  Run it where the uid of this user is mapped (unshare --map-current-user), or name a file that is not a link.'

# _link_dir <name>: a fresh dir (mode 0700) under the test dir, with each
# link resolved, as the walk names it.
_link_dir() {
  local d
  d="$(cd -P -- "${BATS_TEST_TMPDIR}" && pwd -P)/${1}"
  mkdir -m 700 -- "${d}"
  printf '%s\n' "${d}"
}

# _link_chain <dir> <n> <last>: <dir>/l1 -> l2 -> ... -> l<n> -> <last>.
_link_chain() {
  local i dest
  for (( i = 1; i <= ${2}; i++ )); do
    dest="l$(( i + 1 ))"
    (( i < ${2} )) || dest="${3}"
    ln -s "${dest}" "${1}/l${i}"
  done
}

# A stat stand-in: with STAT_UID_AS=<path>=<uid>, that path shows the owner
# <uid> (the first field), each other path its real owner (Go: ownerAs).
_stat_uid_as() {
  stat() {
    local out
    out=$(command stat "$@") || return
    if [[ -n "${STAT_UID_AS:-}" && "${*: -1}" == "${STAT_UID_AS%=*}" ]]; then
      out="${STAT_UID_AS##*=}${out#"${out%% *}"}"
    fi
    printf '%s\n' "${out}"
  }
}

@test "kubeconfig: a link is written through only to a file of ours" {
  [[ "$(id -u)" != 0 ]] || skip "the test needs a user other than root"
  local d want foreign
  foreign=$(foreign_file) || skip "no file of root to point at"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  d=$(_link_dir own)
  ln -s "${foreign}" "${d}/link"
  want="[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/link is a link, and its path is not safe: $(realpath -- "${foreign}") belongs to another user (uid 0)
${LINK_HINT}"
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/link" --force
  assert_failure
  assert_output "${want}"
  # As root (id stubbed), the parts of ours on the way belong to another user.
  id() { if [[ "${1:-}" == -u ]]; then echo 0; else command id "$@"; fi; }
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/link" --force
  assert_failure
  assert_output --partial "${d}/link is a link, and its path is not safe: "
  assert_output --partial " belongs to another user (uid $(command id -u))"
  unset -f id
  [ ! -e "${CURL_ARGS}" ]
}

@test "kubeconfig: the path rules run again just before the write" {
  [[ "$(id -u)" != 0 ]] || skip "the test needs a user other than root"
  local d foreign
  foreign=$(foreign_file) || skip "no file of root to point at"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  d=$(_link_dir again)
  echo contexts > "${d}/real"
  ln -s real "${d}/link"
  # The link moves to another user's file during the download.
  STUB_HOOK="ln -sfn '${foreign}' '${d}/link'" run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/link" --force
  assert_failure
  assert_output --partial "is a link, and its path is not safe: $(realpath -- "${foreign}") belongs to another user (uid 0)"
  run cat "${d}/real"
  assert_output contexts
}

# The Go twin is TestAgentKubeconfigLinkChain: each case that passes. No
# verdict here depends on the caller, so root runs them too.
@test "kubeconfig --force: a clean link chain passes, and the file lands at the final target" {
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  _stat_uid_as
  local d kind land file alias hops as
  # case/the file that gets the kubeconfig
  for kind in chain3/kube/real dirlink8/kube/real chain8/real sticky/st/real rootlink/real relative/real \
    stalepwd/real pwdopen/wd/real pwdowner/wd/real pwdhops/wd/real dotdot/kube/real argdotdot/kube/real; do
    d=$(_link_dir "${kind%%/*}")
    land="${d}/${kind#*/}" file="${d}/l1"
    case "${kind%%/*}" in
      chain3)
        mkdir -m 755 "${d}/a"
        mkdir -m 700 "${d}/b" "${d}/kube"
        ln -s a/l2 "${d}/l1"
        ln -s "${d}/b/l3" "${d}/a/l2"
        ln -s ../kube/real "${d}/b/l3" ;;
      dirlink8)
        # 8 links with a link to a directory among them.
        mkdir -m 700 "${d}/kube"
        ln -s kube "${d}/sub"
        _link_chain "${d}" 7 sub/real ;;
      chain8) _link_chain "${d}" 8 real ;;
      sticky)
        mkdir -m 1777 "${d}/st"
        ln -s st/l2 "${d}/l1"
        ln -s real "${d}/st/l2" ;;
      rootlink|relative) _link_chain "${d}" 2 real ;;
      stalepwd)
        # A relative path starts at the working directory, not at $PWD.
        _link_chain "${d}" 1 real
        mkdir -m 700 "${d}/other"
        echo contexts > "${d}/other/real"
        ln -s real "${d}/other/l1" ;;
      pwdopen|pwdowner|pwdhops)
        # PWD names the working directory wd through the link alias: in an
        # open directory, of another user (STAT_UID_AS), or as one link more
        # than the limit. The walk starts at wd, the physical path.
        mkdir -m 700 "${d}/wd"
        alias="${d}/alias" hops=1
        if [[ "${kind%%/*}" == pwdopen ]]; then
          mkdir -m 777 "${d}/open"
          alias="${d}/open/alias"
        fi
        [[ "${kind%%/*}" != pwdhops ]] || hops=8
        ln -s "${d}/wd" "${alias}"
        _link_chain "${d}/wd" "${hops}" real ;;
      dotdot|argdotdot)
        # ".." goes up from where the link dl points.
        mkdir -m 700 "${d}/kube" "${d}/kube/sub"
        ln -s kube/sub "${d}/dl"
        if [[ "${kind%%/*}" == dotdot ]]; then
          _link_chain "${d}" 1 dl/../real
        else
          ln -s real "${d}/kube/l1"
          file="${d}/dl/../l1"
        fi ;;
    esac
    echo contexts > "${land}"
    case "${kind%%/*}" in
      relative)
        cd "${d}"
        file=l1
        run kubehz::cluster::kubeconfig cl-1a2b3c4d --file l1 --force ;;
      stalepwd)
        cd "${d}"
        file=l1
        PWD="${d}/other" run kubehz::cluster::kubeconfig cl-1a2b3c4d --file l1 --force ;;
      pwdopen|pwdowner|pwdhops)
        cd -L "${alias}"
        [[ "${PWD}" == "${alias}" ]] || fail "${kind}: PWD is ${PWD}, not the alias"
        file=l1 as=""
        [[ "${kind%%/*}" != pwdowner ]] || as="${alias}=4242"
        STAT_UID_AS="${as}" run kubehz::cluster::kubeconfig cl-1a2b3c4d --file l1 --force ;;
      rootlink) STAT_UID_AS="${d}/l2=0" run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${file}" --force ;;
      *) run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${file}" --force ;;
    esac
    assert_success
    run cat "${land}"
    assert_output 'apiVersion: v1'
    if [[ "${kind%%/*}" == stalepwd ]]; then
      run cat "${d}/other/real"
      assert_output contexts
    fi
    [ -L "${file}" ] || fail "${kind}: the first link was replaced"
    run ls -A "${land%/*}"
    refute_output --partial .kubeconfig-
  done
}

# PWD=/proc/self/cwd names the working directory through /proc: the walk
# starts at the physical path, so the path is not in /proc. The Go twin is
# in TestAgentKubeconfigLinkChain.
@test "kubeconfig --force: with PWD=/proc/self/cwd, the walk starts at the physical working directory" {
  [[ -e /proc/self/cwd ]] || skip "this system has no /proc"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d
  d=$(_link_dir procpwd)
  mkdir -m 700 "${d}/wd"
  echo contexts > "${d}/wd/real"
  _link_chain "${d}/wd" 1 real
  cd "${d}/wd"
  cd -L /proc/self/cwd
  [[ "${PWD}" == /proc/self/cwd ]] || fail "PWD is ${PWD}, not /proc/self/cwd"
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file l1 --force
  assert_success
  run cat "${d}/wd/real"
  assert_output 'apiVersion: v1'
  [ -L "${d}/wd/l1" ]
}

# The write keeps the bytes of a directory name: for a name that ends in a
# newline, or holds one, the temporary file is made in that directory, not
# in the one without the newline, and the write lands at the checked
# target. A mktemp stand-in records the template of each kubeconfig
# temporary file (NUL-separated: the names hold newlines). The Go twin is
# TestKubeconfigTempDir and the newline case of TestAgentKubeconfigLinkChain.
@test "kubeconfig: a directory name that ends in a newline, or holds one, keeps its bytes in the write" {
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d sub i=0 log="${BATS_TEST_TMPDIR}/mktemp.log" want="${BATS_TEST_TMPDIR}/mktemp.want"
  mktemp() {
    [[ "${*: -1}" != */.kubeconfig-XXXXXX ]] || printf '%s\0' "${*: -1}" >> "${log}"
    command mktemp "$@"
  }
  for sub in $'safe\n' $'sa\nfe'; do
    (( ++i ))
    d=$(_link_dir "newline${i}")
    # The name without the newline is a directory too: it stays empty.
    mkdir -m 700 "${d}/${sub}" "${d}/safe"
    echo contexts > "${d}/${sub}/real"
    ln -s "${sub}/real" "${d}/l1"
    printf '%s\0' "${d}/${sub}/.kubeconfig-XXXXXX" > "${want}"
    # Through a link, with --force: the walk checks the target.
    : > "${log}"
    run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/l1" --force
    assert_success
    run cat "${d}/${sub}/real"
    assert_output 'apiVersion: v1'
    cmp -s "${log}" "${want}" || fail "${sub@Q}, through a link: the temporary file is not in the checked directory"
    # A new file, without --force: the hard link publishes it there.
    : > "${log}"
    run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/${sub}/new.yaml"
    assert_success
    run cat "${d}/${sub}/new.yaml"
    assert_output 'apiVersion: v1'
    cmp -s "${log}" "${want}" || fail "${sub@Q}, a new file: the temporary file is not in the checked directory"
    run ls -A "${d}/safe"
    assert_output ''
    run ls -A "${d}/${sub}"
    assert_output $'new.yaml\nreal'
  done
  unset -f mktemp
}

# A plain or a new path with alias/.. in it: the temporary file goes to the
# directory that the kernel resolves (one up from where alias points), the
# directory of the mv or the ln, not to ro, the lexical parent, which
# cannot be written here. The Go twin is
# TestAgentKubeconfigTempFileInThePhysicalParent.
@test "kubeconfig: for alias/../config, the write happens in the directory that the kernel resolves" {
  [[ "$(id -u)" != 0 ]] || skip "the test needs a user other than root: root writes to a directory with mode 0500"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d force
  for force in 0 1; do
    d=$(_link_dir "up${force}")
    mkdir -m 700 "${d}/kube" "${d}/kube/sub" "${d}/ro"
    ln -s "${d}/kube/sub" "${d}/ro/alias"
    chmod 500 "${d}/ro"
    if (( force )); then
      echo contexts > "${d}/kube/config"
      run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/ro/alias/../config" --force
    else
      run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/ro/alias/../config"
    fi
    chmod 700 "${d}/ro"
    assert_success
    run cat "${d}/kube/config"
    assert_output 'apiVersion: v1'
    run ls -A "${d}/kube"
    assert_output $'config\nsub'
    run ls -A "${d}/ro"
    assert_output alias
  done
}

# The Go twin is TestAgentKubeconfigLinkChain: each case that is refused.
# No verdict here depends on the caller, so root runs them too (a target of
# root is the next test).
@test "kubeconfig --force: the link walk refuses a long chain, a loop, a mode or an owner on the way, and writes nothing" {
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  _stat_uid_as
  local d kind want file as
  for kind in chain9 dirlink9 loop midmode above targetdir linkowner dirowner targetowner; do
    d=$(_link_dir "${kind}")
    file="${d}/l1" as=""
    echo contexts > "${d}/real"
    case "${kind}" in
      chain9)
        _link_chain "${d}" 9 real
        want="${d}/l9 is link 9 on the way, and lo follows at most 8 (a loop, or a chain that is too long)
${LINK_HOPS}" ;;
      dirlink9)
        # 9 links with a link to a directory among them: that link counts.
        mkdir -m 700 "${d}/kube"
        ln -s kube "${d}/sub"
        _link_chain "${d}" 8 sub/real
        want="${d}/sub is link 9 on the way, and lo follows at most 8 (a loop, or a chain that is too long)
${LINK_HOPS}" ;;
      loop)
        ln -s b "${d}/a"
        ln -s a "${d}/b"
        file="${d}/a"
        want="${d}/a is link 9 on the way, and lo follows at most 8 (a loop, or a chain that is too long)
${LINK_HOPS}" ;;
      midmode)
        mkdir -m 775 "${d}/mid"
        ln -s mid/l2 "${d}/l1"
        ln -s ../real "${d}/mid/l2"
        want="${d}/mid is writable by its group and has no sticky bit
${LINK_HINT}" ;;
      above)
        mkdir -m 777 "${d}/open"
        mkdir -m 700 "${d}/open/sub"
        ln -s open/sub/l2 "${d}/l1"
        ln -s ../../real "${d}/open/sub/l2"
        want="${d}/open is writable by others and has no sticky bit
${LINK_HINT}" ;;
      targetdir)
        mkdir -m 777 "${d}/open"
        echo contexts > "${d}/open/real"
        ln -s open/real "${d}/l1"
        want="${d}/open is writable by others and has no sticky bit
${LINK_HINT}" ;;
      linkowner)
        _link_chain "${d}" 2 real
        as="${d}/l2=4242"
        want="${d}/l2 belongs to another user (uid 4242)
${LINK_HINT}" ;;
      dirowner)
        mkdir -m 755 "${d}/mid"
        ln -s mid/l2 "${d}/l1"
        ln -s ../real "${d}/mid/l2"
        as="${d}/mid=4242"
        want="${d}/mid belongs to another user (uid 4242)
${LINK_HINT}" ;;
      targetowner)
        _link_chain "${d}" 1 real
        as="${d}/real=4242"
        want="${d}/real belongs to another user (uid 4242)
${LINK_HINT}" ;;
    esac
    STAT_UID_AS="${as}" run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${file}" --force
    assert_failure
    assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${file} is a link, and its path is not safe: ${want}"
    run cat "${d}/real"
    assert_output contexts
  done
  [ ! -e "${CURL_ARGS}" ]
}

# The Go twin is the case "a target of root is refused" of
# TestAgentKubeconfigLinkChain.
@test "kubeconfig --force: a target of root is refused: the target must be a file of yours" {
  [[ "$(id -u)" != 0 ]] || skip "the test needs a user other than root: a file of root is a file of yours for root"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  _stat_uid_as
  local d
  d=$(_link_dir targetroot)
  echo contexts > "${d}/real"
  _link_chain "${d}" 1 real
  STAT_UID_AS="${d}/real=0" run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${d}/l1" --force
  assert_failure
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/l1 is a link, and its path is not safe: ${d}/real belongs to another user (uid 0)
${LINK_HINT}"
  run cat "${d}/real"
  assert_output contexts
  [ ! -e "${CURL_ARGS}" ]
}

# Without a mapping for this user (unshare --user alone), this user and
# every other unmapped owner show the overflow uid, so "ours" proves
# nothing: the first part below / on the way is refused with the hint for
# an unmapped user. The Go twin is TestAgentKubeconfigLinkForAnUnmappedUser.
# Skipped where no user namespace can be made.
@test "kubeconfig --force: a caller without a uid mapping trusts no part of its own uid on the way" {
  unshare --user true 2>/dev/null || skip "no user namespace here"
  local d ovf top
  ovf=$(cat /proc/sys/kernel/overflowuid 2>/dev/null) || ovf=65534
  d=$(_link_dir unmapped)
  echo contexts > "${d}/real"
  _link_chain "${d}" 1 real
  top="/$(cut -d / -f 2 <<< "${d}")"
  run unshare --user bash -c '
    import() { :; }
    source "${1}/.lok8s/utils/verbose.sh"
    source "${1}/.lok8s/libs/kubehz/agent"
    rc=0
    kubehz::agent_link_target "cluster kubeconfig cl-1a2b3c4d" "${2}" || rc=$?
    echo "rc: ${rc}"' _ "${_PROJECT_ROOT}" "${d}/l1"
  assert_success
  assert_line "rc: 1"
  assert_output --partial "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/l1 is a link, and its path is not safe: ${top} cannot be checked (uid ${ovf})
  The uid of this user is not mapped in the namespace, so lo cannot check who owns the path.
  Run it where the uid of this user is mapped (unshare --map-current-user), or name a file that is not a link."
  run cat "${d}/real"
  assert_output contexts
}

# The Go twin is TestAgentKubeconfigLinkChain: the kernel's path rules, as
# the target of a link and as the argument, and the regular-file rule, for
# the target of a link and for a plain path. No verdict here depends on the
# caller, so root runs them too.
@test "kubeconfig --force: real/, real/., missing/../real, file/../real, a FIFO and a link to one are refused before any request" {
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d kind want file
  for kind in slash slashdot missing regular fifo plainfifo argslash argslashdot argmissing argregular; do
    d=$(_link_dir "${kind}")
    file="${d}/l1"
    echo contexts > "${d}/real"
    echo contexts > "${d}/regular"
    case "${kind}" in
      slash)
        _link_chain "${d}" 1 real/
        want="cannot write ${file}: ${d}/real is not a directory" ;;
      slashdot)
        _link_chain "${d}" 1 real/.
        want="cannot write ${file}: ${d}/real is not a directory" ;;
      missing)
        _link_chain "${d}" 1 missing/../real
        want="cannot write ${file}: ${d}/missing does not exist or cannot be read" ;;
      regular)
        _link_chain "${d}" 1 regular/../real
        want="cannot write ${file}: ${d}/regular is not a directory" ;;
      fifo)
        mkfifo -m 600 "${d}/fifo"
        _link_chain "${d}" 1 fifo
        want="${file} is a link, and its target ${d}/fifo is not a regular file
${NOT_REGULAR}" ;;
      plainfifo)
        mkfifo -m 600 "${d}/fifo"
        file="${d}/fifo"
        want="${file} is not a regular file
${NOT_REGULAR}" ;;
      argslash|argslashdot)
        file="${d}/real/"
        [[ "${kind}" == argslash ]] || file="${d}/real/."
        want="cannot write ${file}: ${d}/real is not a directory" ;;
      argmissing)
        file="${d}/missing/../real"
        want="cannot write ${file}: ${d}/missing does not exist or cannot be read" ;;
      argregular)
        file="${d}/regular/../real"
        want="cannot write ${file}: ${d}/regular is not a directory" ;;
    esac
    [[ "${kind}" == *fifo ]] || want+=$'\n'"${UNWRITABLE}"
    run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${file}" --force
    assert_failure
    assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${want}"
    run cat "${d}/real"
    assert_output contexts
    [[ "${kind}" != *fifo ]] || [ -p "${d}/fifo" ] || fail "${kind}: the FIFO was replaced"
  done
  [ ! -e "${CURL_ARGS}" ]
}

# _in_removed_wd <dir>: the link walk for "l1" in <dir>, after <dir> is
# removed. Called through run, so the cd stays in the subshell.
_in_removed_wd() {
  cd -- "${1}" && rmdir -- "${1}" && kubehz::agent_link_target "cluster kubeconfig cl-1a2b3c4d" l1
}

# Two refusals that kubehz::agent_target does not reach, so the test calls
# the walk itself: a link to a directory (the -d check refuses it first),
# and a relative path when the working directory was removed (the path then
# does not exist, so it is not a link). The Go twin is
# TestAgentKubeconfigLinkTargetDirect.
@test "the link walk on its own: a directory target is not a regular file, and a removed working directory is refused" {
  local d
  d=$(_link_dir direct)
  mkdir -m 700 "${d}/sub" "${d}/wd"
  _link_chain "${d}" 1 sub
  run kubehz::agent_link_target "cluster kubeconfig cl-1a2b3c4d" "${d}/l1"
  assert_failure
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/l1 is a link, and its target ${d}/sub is not a regular file
${NOT_REGULAR}"
  run _in_removed_wd "${d}/wd"
  assert_failure
  assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write l1: the working directory cannot be read
  Run lo in a directory that exists, or name the file with its full path."
  [ ! -e "${d}/wd" ]
}

# Without a uid mapping, a stat stand-in shows each directory as root (and
# a path in STAT_UID_AS its uid), so the walk reaches the owner checks of
# the link and of the target: a link or a target of our uid proves
# nothing there. The Go twin is
# TestAgentKubeconfigLinkOwnersForAnUnmappedUser.
@test "kubeconfig --force: without a uid mapping, a link or a target of our uid cannot be checked" {
  unshare --user true 2>/dev/null || skip "no user namespace here"
  local d ovf as
  ovf=$(cat /proc/sys/kernel/overflowuid 2>/dev/null) || ovf=65534
  d=$(_link_dir owners)
  echo contexts > "${d}/real"
  _link_chain "${d}" 1 real
  # STAT_UID_AS|the part that cannot be checked
  for as in "|l1" "${d}/l1=0|real"; do
    STAT_UID_AS="${as%|*}" run unshare --user bash -c '
      import() { :; }
      source "${1}/.lok8s/utils/verbose.sh"
      source "${1}/.lok8s/libs/kubehz/agent"
      stat() {
        local out
        out=$(command stat "$@") || return
        if [[ -n "${STAT_UID_AS:-}" && "${*: -1}" == "${STAT_UID_AS%=*}" ]]; then
          out="${STAT_UID_AS##*=}${out#"${out%% *}"}"
        elif [[ -d "${*: -1}" && ! -L "${*: -1}" ]]; then
          out="0${out#"${out%% *}"}"
        fi
        printf "%s\n" "${out}"
      }
      rc=0
      kubehz::agent_link_target "cluster kubeconfig cl-1a2b3c4d" "${2}" || rc=$?
      echo "rc: ${rc}"' _ "${_PROJECT_ROOT}" "${d}/l1"
    assert_success
    assert_line "rc: 1"
    assert_output --partial "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${d}/l1 is a link, and its path is not safe: ${d}/${as#*|} cannot be checked (uid ${ovf})
${UNMAPPED}"
  done
  run cat "${d}/real"
  assert_output contexts
}

@test "kubeconfig: a path in /proc or /dev, or a link there, is refused before any request" {
  [[ -d /proc/self/fd ]] || skip "this system has no /proc"
  route GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent 200 'apiVersion: v1'
  local d="${BATS_TEST_TMPDIR}/virt" up f i slashes
  mkdir -p "${d}"
  echo stdout > "${d}/out.txt"
  # Bats uses fd 3. /proc/self/fd/8 names out.txt in every process that
  # inherits fd 8, realpath too.
  exec 8>>"${d}/out.txt"
  ln -s /proc/self/fd/8 "${d}/link"
  ln -s /proc/self/fd "${d}/fds"
  ln -s fds "${d}/hop"
  up="${d}"
  slashes="${d//[^\/]/}"
  for (( i = 0; i < ${#slashes}; i++ )); do up+="/.."; done
  for f in /proc/self/fd/8 /dev/fd/8 /dev "${up}/proc/self/fd/8" "${d}/link" "${d}/hop/8"; do
    run kubehz::cluster::kubeconfig cl-1a2b3c4d --file "${f}" --force
    assert_failure
    assert_output "[error] kubehz cluster kubeconfig cl-1a2b3c4d: ${f} is in /proc or /dev, or a link in its path points there
  lo does not write to /proc or /dev. Name a file in another directory."
  done
  run kubehz::cluster::kubeconfig cl-1a2b3c4d --file /proc/self/fd/8
  assert_failure
  assert_output --partial "/proc/self/fd/8 is in /proc or /dev"
  exec 8>&-
  run cat "${d}/out.txt"
  assert_output stdout
  [ ! -e "${CURL_ARGS}" ]
}

@test "the /proc and /dev walk: only those components count, a link by where it points" {
  local d="${BATS_TEST_TMPDIR}/walk" f
  mkdir -p "${d}/proc"
  ln -s /proc "${d}/p"
  ln -s /dev/shm "${d}/shm"
  ln -s proc "${d}/here"
  ln -s loop "${d}/loop"
  mkdir -p "${d}/real"
  ln -s /proc "${d}/real/x"
  ln -s real "${d}/sub"
  cd "${d}"
  for f in /proc /proc/ /proc/1/x //proc/x /./dev/x /dev /dev/null /tmp/../dev/x \
    p p/kc.yaml shm/kc.yaml proc/../p/x sub/x/1; do
    kubehz::agent_in_proc_or_dev "${f}" || fail "${f}: want in /proc or /dev"
  done
  for f in /procfs/x /devx /device/x /tmp/proc kc.yaml proc/kc.yaml here/kc.yaml loop/kc.yaml; do
    ! kubehz::agent_in_proc_or_dev "${f}" || fail "${f}: want not in /proc or /dev"
  done
  # A relative path starts at the working directory, not at $PWD.
  PWD="${BATS_TEST_TMPDIR}" kubehz::agent_in_proc_or_dev p/kc.yaml || fail "a stale PWD: p/kc.yaml is in /proc"
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

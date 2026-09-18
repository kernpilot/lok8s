#!/usr/bin/env bats
# kubehz_shared_test.bats — unit tests for hosting: shared (kubehz Spaces)
#
# The curl mock is keyed on "<METHOD> <path>" and answers with the api's
# enveloped shapes; every response carries the trailing status line the
# -w $'\n%{http_code}' contract in kubehz::space_api expects.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  mkdir -p "${BATS_TEST_TMPDIR}/clusters/acme.example.org"
  touch "${BATS_TEST_TMPDIR}/clusters/acme.example.org/cluster.lok8s.yaml"

  export LOK8S_KUBEHZ_API_URL="https://api.example.test"
  export KUBEHZ_TOKEN="khz_test_token"

  # A no-op sleep keeps the wait loops instant.
  sleep() { :; }
  export -f sleep
}

teardown() {
  teardown_tmpdir
}

# yq mock for a spec with an explicit space block + two declared machines.
# The limits block is the space: nodes 3, namespaces 2, object cap 128 KiB.
yq_space_spec() {
  yq() {
    case "$2" in
      '.spec.kubehz.space.slug // ""')   echo "acme" ;;
      '.spec.kubehz.space.name // ""')   echo "Acme Prod" ;;
      '.spec.kubehz.space.region // ""') echo "" ;;
      '.spec.kubehz.space.plan // ""')   echo "" ;;
      '.spec.kubehz.space.limits.nodes | type')        echo '!!int' ;;
      '.spec.kubehz.space.limits.namespaces | type')   echo '!!int' ;;
      '.spec.kubehz.space.limits.objectCapKiB | type') echo '!!int' ;;
      '.spec.kubehz.space.limits.nodes')        echo "3" ;;
      '.spec.kubehz.space.limits.namespaces')   echo "2" ;;
      '.spec.kubehz.space.limits.objectCapKiB') echo "128" ;;
      '.spec.kubehz.space.nodes[]?')     printf 'worker-1\nworker-2\n' ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

# yq mock with NO space block at all — every default must hold.
yq_space_defaults() {
  yq() {
    case "$2" in
      '.spec.kubehz.space.nodes[]?') : ;;
      *'| type') echo '!!null' ;;
      *'// ""'*) echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

# yq mock with ONE limits field set to a value the caller names.
# Usage: yq_space_field <field> <type> <value>
yq_space_field() {
  export _SPACE_FIELD="$1" _SPACE_TYPE="$2" _SPACE_VALUE="$3"
  yq() {
    case "$2" in
      ".spec.kubehz.space.limits.${_SPACE_FIELD} | type") echo "${_SPACE_TYPE}" ;;
      ".spec.kubehz.space.limits.${_SPACE_FIELD}")        echo "${_SPACE_VALUE}" ;;
      '.spec.kubehz.space.nodes[]?') : ;;
      *'| type') echo '!!null' ;;
      *'// ""'*) echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

# ── validate_config: the hosting enum ────────────────────

@test "validate_config: accepts hosting=shared with apiUrl and kind Kubehz" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Kubehz" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/clusters/acme.example.org/cluster.lok8s.yaml"
  LOK8S_KUBEHZ_HOSTING="shared" LOK8S_KUBEHZ_ACCESS="none"
  LOK8S_KUBEHZ_API_URL="https://api.example.test"

  run kubehz::validate_config
  assert_success
}

@test "validate_config: rejects hosting=shared without apiUrl" {
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/clusters/acme.example.org/cluster.lok8s.yaml"
  LOK8S_KUBEHZ_HOSTING="shared" LOK8S_KUBEHZ_ACCESS="none" LOK8S_KUBEHZ_API_URL=""

  run kubehz::validate_config
  assert_failure
  assert_output --partial "apiUrl is required when hosting: shared"
}

@test "validate_config: rejects hosting=shared with a non-Kubehz kind" {
  yq() {
    case "$2" in
      '.kind // ""') echo "KubeOne" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/clusters/acme.example.org/cluster.lok8s.yaml"
  LOK8S_KUBEHZ_HOSTING="shared" LOK8S_KUBEHZ_ACCESS="none"
  LOK8S_KUBEHZ_API_URL="https://api.example.test"

  run kubehz::validate_config
  assert_failure
  assert_output --partial "requires kind: Kubehz"
}

@test "validate_config: rejects kind Kubehz without hosting=shared" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Kubehz" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/clusters/acme.example.org/cluster.lok8s.yaml"
  LOK8S_KUBEHZ_HOSTING="self" LOK8S_KUBEHZ_ACCESS="none" LOK8S_KUBEHZ_API_URL=""

  run kubehz::validate_config
  assert_failure
  assert_output --partial "kind: Kubehz requires spec.kubehz.hosting: shared"
}

@test "validate_config: still rejects an unknown hosting value" {
  LOK8S_KUBEHZ_HOSTING="communal" LOK8S_KUBEHZ_ACCESS="none" LOK8S_KUBEHZ_API_URL=""

  run kubehz::validate_config
  assert_failure
  assert_output --partial "invalid spec.kubehz.hosting"
}

# ── space_config: defaults ───────────────────────────────

@test "space_config: slug defaults to the domain's first label, name to slug" {
  yq_space_defaults

  kubehz::space_config "acme.example.org" "/dev/null"

  [ "${LOK8S_SPACE_SLUG}" = "acme" ]
  [ "${LOK8S_SPACE_NAME}" = "acme" ]
  [ "${#LOK8S_SPACE_NODES[@]}" -eq 0 ]
  [ "${LOK8S_SPACE_MAX_NODES}" -eq 2 ]
  [ "${LOK8S_SPACE_MAX_NAMESPACES}" -eq 1 ]
  [ "${LOK8S_SPACE_MAX_OBJECT_KIB}" -eq 256 ]
}

# ── space_limits: the three numbers and their bounds ─────

@test "space_limits: reads the three numbers the limits block sets" {
  yq_space_spec

  kubehz::space_limits "/dev/null"

  [ "${LOK8S_SPACE_MAX_NODES}" -eq 3 ]
  [ "${LOK8S_SPACE_MAX_NAMESPACES}" -eq 2 ]
  [ "${LOK8S_SPACE_MAX_OBJECT_KIB}" -eq 128 ]
}

@test "space_limits: refuses a node count out of range" {
  yq_space_field nodes '!!int' 6

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: 6 (expected a whole number from 1 to 5)"
}

@test "space_limits: refuses zero nodes" {
  yq_space_field nodes '!!int' 0

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: 0"
}

# A fraction and a boolean are scalars: the refusal names the value the spec
# carries, the same as a word does. Only a list and a map have no value to
# name. The Go twin pins the same three.
@test "space_limits: names the value of a fraction" {
  yq_space_field nodes '!!float' 2.5

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: 2.5 (expected a whole number from 1 to 5)"
}

@test "space_limits: names the value of a boolean" {
  yq_space_field nodes '!!bool' true

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: true (expected a whole number from 1 to 5)"
}

@test "space_limits: a map under a limit has no value to name" {
  yq_space_field nodes '!!map' ''

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: expected a whole number from 1 to 5"
}

@test "space_limits: refuses a namespace count out of range" {
  yq_space_field namespaces '!!int' 4

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.namespaces: 4 (expected a whole number from 1 to 3)"
}

@test "space_limits: refuses an object cap below 64 KiB" {
  yq_space_field objectCapKiB '!!int' 63

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.objectCapKiB: 63 (expected a whole number from 64 to 512)"
}

@test "space_limits: refuses an object cap above 512 KiB" {
  yq_space_field objectCapKiB '!!int' 513

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.objectCapKiB: 513"
}

@test "space_limits: refuses a retired plan" {
  yq() {
    case "$2" in
      '.spec.kubehz.space.plan // ""') echo "shared-s" ;;
      *'| type') echo '!!null' ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "spec.kubehz.space.plan is not valid: space plans are retired"
  assert_output --partial "spec.kubehz.space.limits.objectCapKiB instead."
}

@test "space_limits: a list under limits.nodes names both fields" {
  yq_space_field nodes '!!seq' ''

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "spec.kubehz.space.limits.nodes is a list: it is the node ceiling"
  assert_output --partial "The machine names stay under spec.kubehz.space.nodes."
}

@test "space_config: the machine-name list under space.nodes is untouched" {
  yq_space_spec

  kubehz::space_config "acme.example.org" "/dev/null"

  [ "${#LOK8S_SPACE_NODES[@]}" -eq 2 ]
  [ "${LOK8S_SPACE_NODES[0]}" = "worker-1" ]
}

# ── provision: create → wait → mint per node ─────────────

@test "provision_shared: creates the space, waits Active, mints one ticket per declared node" {
  yq_space_spec
  curl() {
    local method="GET" url=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) shift 2 ;;
        *) shift ;;
      esac
    done
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        printf '{"ok":true,"data":[]}\n200' ;;
      "POST /api/spaces")
        printf '{"ok":true,"data":{"id":"sp-123","slug":"acme","status":"Pending"}}\n201' ;;
      "GET /api/spaces/sp-123")
        printf '{"ok":true,"data":{"id":"sp-123","status":"Active"}}\n200' ;;
      "POST /api/spaces/sp-123/join-token")
        printf '{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"w","expiresAt":"2026-08-07T20:00:00Z"}}\n201' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "Space 'acme' is Active (id: sp-123)"
  assert_output --partial "worker-1"
  assert_output --partial "worker-2"
  # Two nodes declared → the plaintext ticket appears exactly twice.
  [ "$(grep -c "a1b2c3.d4e5f6g7h8i9j0k1" <<<"${output}")" -eq 2 ]
}

@test "space_ensure: the create body carries the three numbers" {
  yq_space_spec
  CURL_BODY="${BATS_TEST_TMPDIR}/body"
  export CURL_BODY
  curl() {
    local method="GET" url="" body=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) body="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        printf '{"ok":true,"data":[]}\n200' ;;
      "POST /api/spaces")
        printf '%s' "${body}" > "${CURL_BODY}"
        printf '{"ok":true,"data":{"id":"sp-123"}}\n201' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  kubehz::space_config "acme.example.org" "/dev/null"
  run kubehz::space_ensure "acme.example.org" "/dev/null"
  assert_success
  run jq -c '[.maxNodes, .maxNamespaces, .maxObjectKiB]' "${CURL_BODY}"
  assert_output '[3,2,128]'
}

@test "provision_shared: adopts an existing space instead of re-creating" {
  yq_space_defaults
  CURL_POSTS="${BATS_TEST_TMPDIR}/posts"
  export CURL_POSTS
  curl() {
    local method="GET" url=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) shift 2 ;;
        *) shift ;;
      esac
    done
    [[ "${method}" == "POST" ]] && echo "${url}" >> "${CURL_POSTS}"
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        printf '{"ok":true,"data":[{"id":"sp-777","slug":"acme","status":"Active"}]}\n200' ;;
      "GET /api/spaces/sp-777")
        printf '{"ok":true,"data":{"id":"sp-777","status":"Active"}}\n200' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "Space 'acme' is Active (id: sp-777)"
  # Adoption must be read-only: no POST may have fired.
  [ ! -f "${CURL_POSTS}" ]
}

@test "provision_shared: renders the capacity rejection on NO_SHARD_AVAILABLE" {
  yq_space_defaults
  curl() {
    local method="GET" url=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) shift 2 ;;
        *) shift ;;
      esac
    done
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        printf '{"ok":true,"data":[]}\n200' ;;
      "POST /api/spaces")
        printf '{"ok":false,"data":{"code":"NO_SHARD_AVAILABLE","message":"no capacity"}}\n409' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "no shared control plane has room"
  assert_output --partial "hosting: self"
}

# One curl mock for the limit refusals: the create answers the code, the
# status and the message the caller names. jq builds the body, so a message
# may carry quotes, backslashes, control characters and newlines.
# Usage: curl_space_refusal <code> <status> [message]
curl_space_refusal() {
  _REFUSE_BODY=$(jq -nc --arg c "$1" --arg m "${3:-}" \
    '{ok: false, data: {code: $c, message: $m}}')
  export _REFUSE_BODY _REFUSE_STATUS="$2"
  curl() {
    local method="GET" url=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) shift 2 ;;
        *) shift ;;
      esac
    done
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        printf '{"ok":true,"data":[]}\n200' ;;
      "POST /api/spaces")
        printf '%s\n%s' "${_REFUSE_BODY}" "${_REFUSE_STATUS}" ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl
}

# The free ceiling is three numbers, so the refusal names three on each side:
# what the spec asks for, and what a free account allows.
@test "provision_shared: the free refusal names all three numbers" {
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_FREE 403 refused

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): they are above the free allowance"
  assert_output --partial "A free account gets 1 space with 2 nodes, 1 namespace and a 256 KiB object cap."
  # The ceiling keys on a payment method, so that is the next action.
  assert_output --partial "Add a payment method to the account to raise them."
  assert_output --partial "To keep the free allowance, decrease the values in spec.kubehz.space.limits."
}

# The shared ceiling moves with the account, so the api's message carries the
# real numbers and lo prints it instead of a fixed maximum.
@test "provision_shared: the shared refusal prints the api message" {
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_SHARED 400 \
    "This account allows 5 nodes, 3 namespaces and an object cap of 512 KiB for one space: maxNodes is 9, the limit is 5"

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): This account allows 5 nodes, 3 namespaces and an object cap of 512 KiB for one space: maxNodes is 9, the limit is 5"
  assert_output --partial "plane: set spec.kubehz.hosting to hosted."
}

@test "provision_shared: the shared refusal falls back without an api message" {
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_SHARED 400 ""

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): they are above the maximum of a shared control plane"
  assert_output --partial "plane: set spec.kubehz.hosting to hosted."
}

# A refusal message is a SERVER string. The api could hide an escape sequence
# in it and redraw this terminal, so both implementations scrub it, clip it to
# 256 characters and (here only, because error() prints through `echo -e`)
# escape the backslashes last.
#
# The fixture and the expected bytes are the GOLDEN PAIR the Go twin owns
# (internal/kubehz/testdata/golden/, written by `go test ./internal/kubehz/
# -run TestProvisionSharedScrubsTheSharedCeilingMessage -update`). Both
# implementations are fed the same message and asserted against the same
# bytes, so a drift on either side turns one of the two suites red. The parity
# harness cannot carry this case: spec.kubehz.apiUrl must be HTTPS, so no
# plain-http stub can answer either implementation.
@test "provision_shared: the shared refusal is scrubbed, clipped and escaped" {
  local golden_dir="${_PROJECT_ROOT}/internal/kubehz/testdata/golden"
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_SHARED 400 \
    "$(cat "${golden_dir}/space-above-shared-message.txt")"

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  # Byte for byte what the Go twin renders from the same message.
  assert_output "$(cat "${golden_dir}/space-above-shared.txt")"
  # No control character reaches the terminal, and the six characters the api
  # sent stay six characters.
  refute_output --partial "$(printf '\033')"
  refute_output --partial "$(printf '\007')"
  assert_output --partial 'literal:\033[2J'
}

@test "provision_shared: a lost create race adopts via re-lookup" {
  yq_space_defaults
  CURL_STATE="${BATS_TEST_TMPDIR}/race"
  export CURL_STATE
  curl() {
    local method="GET" url=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) shift 2 ;;
        *) shift ;;
      esac
    done
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        # First list: empty. After the racing 409: the winner's row.
        if [[ -f "${CURL_STATE}" ]]; then
          printf '{"ok":true,"data":[{"id":"sp-race","slug":"acme","status":"Active"}]}\n200'
        else
          printf '{"ok":true,"data":[]}\n200'
        fi ;;
      "POST /api/spaces")
        touch "${CURL_STATE}"
        printf '{"ok":false,"data":{"code":"CONFLICT","message":"slug exists"}}\n409' ;;
      "GET /api/spaces/sp-race")
        printf '{"ok":true,"data":{"id":"sp-race","status":"Active"}}\n200' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "Space 'acme' is Active (id: sp-race)"
}

@test "provision_shared: refuses to run without KUBEHZ_TOKEN" {
  yq_space_defaults
  unset KUBEHZ_TOKEN

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "KUBEHZ_TOKEN is required"
}

# ── destroy ──────────────────────────────────────────────

@test "destroy_shared: deletes the space found by slug" {
  yq_space_defaults
  curl() {
    local method="GET" url=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) shift 2 ;;
        *) shift ;;
      esac
    done
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        printf '{"ok":true,"data":[{"id":"sp-9","slug":"acme","status":"Active"}]}\n200' ;;
      "DELETE /api/spaces/sp-9")
        printf '{"ok":true}\n200' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::destroy_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "Space 'acme' removed (id: sp-9)"
}

@test "destroy_shared: absent space is a clean no-op" {
  yq_space_defaults
  curl() {
    printf '{"ok":true,"data":[]}\n200'
  }
  export -f curl

  run kubehz::destroy_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "nothing to destroy"
}

# ── status ───────────────────────────────────────────────

@test "space_status: renders phase, the three numbers and the node table" {
  yq_space_defaults
  curl() {
    local method="GET" url=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        https://*) url="$1"; shift ;;
        -d) shift 2 ;;
        *) shift ;;
      esac
    done
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        printf '{"ok":true,"data":[{"id":"sp-5","slug":"acme","status":"Active","maxNodes":2,"maxNamespaces":1,"maxObjectKiB":256}]}\n200' ;;
      "GET /api/spaces/sp-5/nodes")
        # The REAL route shape (kubehz-api nodes.get.ts): an OBJECT with
        # `nodes` (each {name,…} — not nodeName) and `usage` — the old
        # bare-array mock is exactly why the parsing bug survived its tests.
        printf '{"ok":true,"data":{"nodes":[{"name":"worker-1","status":"Ready","lane":"hcloud"}],"usage":{"nodes":1,"maxNodes":2}}}\n200' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::space_status "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "Phase:   Active"
  assert_output --partial "Limits:  nodes 2, namespaces 1, object cap 256 KiB"
  assert_output --partial "worker-1  Ready  hcloud"
}

# ── docs ↔ code: the hosting enum must not drift ─────────
# The public docs state the accepted values in three places (the guide's yaml
# example, its axis table, and the specs reference). A value the code accepts
# but the docs omit is an undiscoverable feature; a value the docs promise but
# the code rejects is a broken promise. `shared` was missing from the specs
# reference until P4.4 — exactly this drift, caught by reading rather than by
# a test. Now it is a test.

@test "docs: the documented hosting enum matches what validate_config accepts" {
  local guide="${_PROJECT_ROOT}/docs/guide/kubehz.md"
  local specs="${_PROJECT_ROOT}/docs/reference/specs.md"
  [ -f "${guide}" ] && [ -f "${specs}" ]

  # The values the CODE accepts, read from the case arm itself.
  local accepted
  accepted=$(grep -oE '^[[:space:]]+self\|hosted\|shared\)' "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main" | tr -d ' )')
  [ "${accepted}" = "self|hosted|shared" ]

  # Each documented surface must name every accepted value.
  local value
  for value in self hosted shared; do
    grep -q "hosting: self | hosted | shared" "${specs}" \
      || { echo "specs.md yaml block does not list the full hosting enum" >&2; return 1; }
    grep -qE "\`${value}\`" "${guide}" \
      || { echo "guide does not document hosting value: ${value}" >&2; return 1; }
    grep -qE "\`${value}\`" "${specs}" \
      || { echo "specs reference does not document hosting value: ${value}" >&2; return 1; }
  done
}

@test "docs: the Kubehz driver kind is documented wherever the driver list is" {
  local specs="${_PROJECT_ROOT}/docs/reference/specs.md"
  # The kind exists as a driver on disk...
  [ -f "${_PROJECT_ROOT}/.lok8s/drivers/kubehz/main" ]
  # ...so both driver lists in the reference must name it, or a user reading
  # the spec cannot discover the one kind that hosting: shared requires.
  grep -q 'kind: Lo | KubeOne | Capi | Kkp | Kubehz' "${specs}"
  grep -qE '\| `kind` \| yes \|.*`Kubehz`' "${specs}"
}

# ── the auth contract (review round 1, F4) ───────────────

@test "space_api: every call carries the bearer token — a driver that drops auth must fail HERE" {
  # The other mocks in this file ignore -H entirely, so a regression that
  # stops sending Authorization would stay green everywhere else. This one
  # test pins the header contract for the whole client.
  curl() {
    local auth_seen=""
    while (( $# )); do
      case "$1" in
        # EXACT bearer required (round 2): any-non-empty let a wrong variable
        # (e.g. HCLOUD_TOKEN) pass. The sentinel is pinned by the test env,
        # and the empty-token leg cannot match because ?* demands substance.
        -H) [[ "$2" == "Authorization: Bearer khz_test_token" ]] && auth_seen=1; shift 2 ;;
        *) shift ;;
      esac
    done
    if [[ -z "${auth_seen}" ]]; then
      printf '{"error":"no bearer"}\n401'
      return 0
    fi
    printf '{"ok":true,"data":{"id":"sp-auth"}}\n200'
  }
  export -f curl

  run kubehz::space_api GET "/api/spaces/sp-auth"
  [ "$status" -eq 0 ]
  # And the negative leg: an empty token env must not silently send
  # "Bearer " — the api answers 401 and space_api reports non-2xx (rc 2).
  local saved="${KUBEHZ_TOKEN}"
  KUBEHZ_TOKEN=""
  run kubehz::space_api GET "/api/spaces/sp-auth"
  [ "$status" -ne 0 ]
  KUBEHZ_TOKEN="${saved}"
}

# ── mutation pins for the round-1/round-2 guards ─────────
# Each of these fails if its guard is deleted (review round 2: "revert every
# hunk except the grep/auth-test and the suite stays green" — no longer true).

@test "validate_config: rejects access: registered with hosting: shared (a Space has no agent)" {
  export LOK8S_KUBEHZ_HOSTING="shared" LOK8S_KUBEHZ_ACCESS="registered" \
         LOK8S_KUBEHZ_API_URL="https://api.example.test" LOK8S_KUBEHZ_KIND="Kubehz"
  run kubehz::validate_config
  [ "$status" -ne 0 ]
  [[ "$output" == *"access: none"* ]]
}

@test "space_wait_active: fails fast on 401 instead of looping to timeout" {
  curl() { printf '{"error":"unauthorized"}\n401'; }
  export -f curl
  local start end
  start=$SECONDS
  run kubehz::space_wait_active "sp-x" 60
  end=$SECONDS
  [ "$status" -ne 0 ]
  [[ "$output" == *"refused the token"* ]]
  # fail-FAST: well under the 60s window (one poll, no 5s-loop grind)
  (( end - start < 15 ))
}

@test "space_wait_active: fails fast on 404 — the space vanished" {
  curl() { printf '{"error":"gone"}\n404'; }
  export -f curl
  run kubehz::space_wait_active "sp-x" 60
  [ "$status" -ne 0 ]
  [[ "$output" == *"vanished"* ]]
}

@test "space_config: a yq parse failure refuses, never defaults the slug" {
  # A broken yaml must not fall back to the domain label — destroy_shared
  # would target the WRONG space (round 1 F2).
  local broken="${BATS_TEST_TMPDIR}/broken.yaml"
  printf '{{ not yaml' > "${broken}"
  run kubehz::space_config "acme.example" "${broken}"
  [ "$status" -ne 0 ]
}

@test "read_config guard: the driver refuses a broken cluster.lok8s.yaml with the READ error, not a hosting misdiagnosis" {
  local dom="brokenspec.example"
  mkdir -p "${PATH_CLUSTERS}/${dom}"
  printf '{{ not yaml' > "${PATH_CLUSTERS}/${dom}/cluster.lok8s.yaml"
  # source WITHOUT error suppression — a failed source made this test pass
  # on exit 127 ('command not found'), exercising nothing (BW01 caught it).
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubehz/main"
  run driver::ensure_shared_config "${dom}"
  [ "$status" -ne 0 ]
  # The guard's whole point: the error must NOT be the empty-var misdiagnosis.
  # (Round 2 self-catch: the first version of this assert embedded a newline
  # that never matches anything — vacuously true, mutation survived.)
  [[ "$output" != *"invalid spec.kubehz.hosting:"* ]]
  rm -rf "${PATH_CLUSTERS}/${dom}"
}

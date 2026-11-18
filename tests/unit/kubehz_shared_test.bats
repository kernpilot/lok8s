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
  # The ticket writer checks the node name with node::assert_node_name;
  # libs/kubehz/main imports node in lo, and import is a no-op here.
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/node"

  # A mint writes its ticket file below the test's own tree, never into the
  # host's temp dir.
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"

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
  # No limits block: no number is set, so none is sent.
  [ -z "${LOK8S_SPACE_MAX_NODES}" ]
  [ -z "${LOK8S_SPACE_MAX_NAMESPACES}" ]
  [ -z "${LOK8S_SPACE_MAX_OBJECT_KIB}" ]
}

# ── space_limits: the three numbers and their bounds ─────

@test "space_limits: reads the three numbers the limits block sets" {
  yq_space_spec

  kubehz::space_limits "/dev/null"

  [ "${LOK8S_SPACE_MAX_NODES}" -eq 3 ]
  [ "${LOK8S_SPACE_MAX_NAMESPACES}" -eq 2 ]
  [ "${LOK8S_SPACE_MAX_OBJECT_KIB}" -eq 128 ]
}

# Only the SHAPE is local: a whole number, 1 or more. Zero and a negative
# number can never be a ceiling, so they die here.
@test "space_limits: refuses zero nodes" {
  yq_space_field nodes '!!int' 0

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: 0 (expected a whole number of 1 or more)"
}

@test "space_limits: refuses a negative node count" {
  yq_space_field nodes '!!int' -1

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: -1 (expected a whole number of 1 or more)"
}

@test "space_limits: refuses a zero object cap" {
  yq_space_field objectCapKiB '!!int' 0

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.objectCapKiB: 0 (expected a whole number of 1 or more)"
}

# A fraction and a boolean are scalars: the refusal names the value the spec
# carries, the same as a word does. Only a list and a map have no value to
# name. The Go twin pins the same three.
@test "space_limits: names the value of a fraction" {
  yq_space_field nodes '!!float' 2.5

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: 2.5 (expected a whole number of 1 or more)"
}

@test "space_limits: names the value of a boolean" {
  yq_space_field nodes '!!bool' true

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: true (expected a whole number of 1 or more)"
}

@test "space_limits: a map under a limit has no value to name" {
  yq_space_field nodes '!!map' ''

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: expected a whole number of 1 or more"
}

# The contract is digits only: no sign, no spaces. The Go twin follows this
# tree, not strconv's leniency.
@test "space_limits: refuses a leading plus" {
  yq_space_field nodes '!!str' '+5'

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: +5 (expected a whole number of 1 or more)"
}

@test "space_limits: refuses a padded value" {
  yq_space_field nodes '!!str' ' 5 '

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes:  5  (expected a whole number of 1 or more)"
}

# Past the strconv range the Go twin parses. `(( ))` would wrap silently here.
@test "space_limits: refuses a value past the integer range" {
  yq_space_field nodes '!!int' 99999999999999999999

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "invalid spec.kubehz.space.limits.nodes: 99999999999999999999 (expected a whole number of 1 or more)"
}

# A leading zero is NOT octal. `(( 008 ))` would print `value too great for
# base`; both trees read eight and print eight.
@test "space_limits: a leading zero is a decimal, not an octal" {
  yq_space_field nodes '!!int' 008

  run kubehz::space_limits "/dev/null"
  assert_success
  refute_output --partial "value too great for base"

  yq_space_field nodes '!!int' 008
  kubehz::space_limits "/dev/null"
  [ "${LOK8S_SPACE_MAX_NODES}" = "8" ]
}

# There is NO local upper bound any more. What an account may ask for differs
# per account and extra resources are bought, so a number the platform will
# refuse still leaves this CLI: the api answers with the account's real
# ceiling. All three were refused locally before.
@test "space_limits: accepts a node count the platform will refuse" {
  yq_space_field nodes '!!int' 6

  run kubehz::space_limits "/dev/null"
  assert_success
}

@test "space_limits: accepts a namespace count the platform will refuse" {
  yq_space_field namespaces '!!int' 4

  run kubehz::space_limits "/dev/null"
  assert_success
}

@test "space_limits: accepts an object cap the platform will refuse" {
  yq_space_field objectCapKiB '!!int' 513

  run kubehz::space_limits "/dev/null"
  assert_success
}

# A spec that names no number leaves every one of them EMPTY: lo holds no
# default of its own, and an empty value is left out of the request.
@test "space_limits: a spec with no limits block sets no number" {
  yq_space_defaults

  kubehz::space_limits "/dev/null"

  [ -z "${LOK8S_SPACE_MAX_NODES}" ]
  [ -z "${LOK8S_SPACE_MAX_NAMESPACES}" ]
  [ -z "${LOK8S_SPACE_MAX_OBJECT_KIB}" ]
}

# ANY plan node is a plan. yq's `//` reads `false` and `""` as absent, so a
# check built on it would let those two specs through unrefused. The Go twin
# pins the same set.
yq_space_plan_type() {
  export _PLAN_TYPE="$1"
  yq() {
    case "$2" in
      '.spec.kubehz.space.plan | type') echo "${_PLAN_TYPE}" ;;
      *'| type') echo '!!null' ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

@test "space_limits: refuses a retired plan" {
  local t
  for t in '!!str' '!!bool' '!!int' '!!map'; do
    yq_space_plan_type "${t}"

    run kubehz::space_limits "/dev/null"
    assert_failure
    assert_output --partial "spec.kubehz.space.plan is not valid: space plans are retired"
    assert_output --partial "spec.kubehz.space.limits.objectCapKiB instead."
  done
}

@test "space_limits: an explicit null plan is not a plan" {
  yq_space_plan_type '!!null'

  run kubehz::space_limits "/dev/null"
  assert_success
}

@test "space_limits: a list under limits.nodes names both fields" {
  yq_space_field nodes '!!seq' ''

  run kubehz::space_limits "/dev/null"
  assert_failure
  assert_output --partial "spec.kubehz.space.limits.nodes is a list: it is the node ceiling"
  assert_output --partial "Set spec.kubehz.space.limits.nodes to a whole number of 1 or more."
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
  # Two nodes declared → two ticket files; stdout repeats neither ticket.
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  refute_output --partial "${ticket}"
  [ "$(grep -c ', mode 0600)$' <<<"${output}")" -eq 2 ]
  local node files
  for node in worker-1 worker-2; do
    files=("${TMPDIR}"/kubehz-join-*/"kubehz-join-${node}.token")
    [ "${#files[@]}" -eq 1 ]
    [ -f "${files[0]}" ]
  done
}

# A curl mock that saves the create body to ${CURL_BODY}.
curl_capture_create() {
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
}

@test "space_ensure: the create body carries the three numbers" {
  yq_space_spec
  curl_capture_create

  kubehz::space_config "acme.example.org" "/dev/null"
  run kubehz::space_ensure "acme.example.org" "/dev/null"
  assert_success
  # Byte for byte what the Go twin sends (TestProvisionSharedCreatesWaitsMints).
  run cat "${CURL_BODY}"
  assert_output '{"name":"Acme Prod","slug":"acme","maxNodes":3,"maxNamespaces":2,"maxObjectKiB":128}'
}

# A limit the spec does not name is LEFT OUT of the create body. lo holds no
# default of its own, so the api applies the platform's — and a value the
# account may later have more of is never pinned by this CLI. The Go twin
# sends the same bytes (TestProvisionSharedOmitsTheLimitsTheSpecLeavesOut).
@test "space_ensure: a limit the spec leaves out is not in the create body" {
  yq() {
    case "$2" in
      '.spec.kubehz.space.slug // ""')   echo "acme" ;;
      '.spec.kubehz.space.name // ""')   echo "Acme Prod" ;;
      '.spec.kubehz.space.limits.namespaces | type') echo '!!int' ;;
      '.spec.kubehz.space.limits.namespaces')        echo "2" ;;
      '.spec.kubehz.space.nodes[]?') : ;;
      *'| type') echo '!!null' ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  curl_capture_create

  kubehz::space_config "acme.example.org" "/dev/null"
  run kubehz::space_ensure "acme.example.org" "/dev/null"
  assert_success
  run cat "${CURL_BODY}"
  assert_output '{"name":"Acme Prod","slug":"acme","maxNamespaces":2}'
}

@test "space_ensure: a spec with no limits block sends none of the three" {
  yq_space_defaults
  curl_capture_create

  kubehz::space_config "acme.example.org" "/dev/null"
  run kubehz::space_ensure "acme.example.org" "/dev/null"
  assert_success
  run cat "${CURL_BODY}"
  assert_output '{"name":"acme","slug":"acme"}'
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
# Usage: curl_space_refusal <code> <status> [message] [help]
curl_space_refusal() {
  _REFUSE_BODY=$(jq -nc --arg c "$1" --arg m "${3:-}" --arg h "${4:-}" \
    '{ok: false, data: {code: $c, message: $m, help: $h}}')
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

# The ACCOUNT's ceiling differs per account and extra resources are bought, so
# the api's own message is the only place the real numbers exist. lo prints it,
# adds the values it sent, and names the next step.
@test "provision_shared: the free refusal prints the api message and help" {
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_FREE 403 \
    "A free account allows 2 nodes, 1 namespace and an object cap of 256 KiB for one space: maxNodes is 3, the limit is 2" \
    "Add a payment method to the account, or lower the values."

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): A free account allows 2 nodes, 1 namespace and an object cap of 256 KiB for one space: maxNodes is 3, the limit is 2"
  assert_output --partial "  Add a payment method to the account, or lower the values."
  assert_output --partial "Decrease the values in spec.kubehz.space.limits, or raise the ceiling"
  assert_output --partial "of the account in the kubehz dashboard."
}

@test "provision_shared: the shared refusal prints the api message" {
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_SHARED 400 \
    "This account allows 5 nodes, 3 namespaces and an object cap of 512 KiB for one space: maxNodes is 9, the limit is 5" \
    "Lower the values, or create a hosted control plane for a larger cluster."

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): This account allows 5 nodes, 3 namespaces and an object cap of 512 KiB for one space: maxNodes is 9, the limit is 5"
  assert_output --partial "  Lower the values, or create a hosted control plane for a larger cluster."
  assert_output --partial "plane: set spec.kubehz.hosting to hosted."
}

# An api that sends the code alone still gets a usable line. The fallback
# states NO number: lo does not know the account's ceiling.
@test "provision_shared: the free refusal falls back without an api message" {
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_FREE 403 "" ""

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): they are above what this account allows"
  # The spec names numbers, so the next step is to lower them.
  assert_output --partial "Decrease the values in spec.kubehz.space.limits, or raise the ceiling"
  assert_output --partial "of the account in the kubehz dashboard."
}

@test "provision_shared: the shared refusal falls back without an api message" {
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_SHARED 400 "" ""

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): they are above what this account may set on a shared control plane"
  assert_output --partial "plane: set spec.kubehz.hosting to hosted."
}

@test "provision_shared: the retired-plan refusal prints the api message" {
  yq_space_spec
  curl_space_refusal SPACE_PLAN_RETIRED 400 \
    "Space plans are retired. The request sends planId, which the platform no longer accepts." \
    "Send maxNodes, maxNamespaces and maxObjectKiB instead."

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the request: Space plans are retired. The request sends planId, which the platform no longer accepts."
  assert_output --partial "  Send maxNodes, maxNamespaces and maxObjectKiB instead."
  assert_output --partial "A space uses spec.kubehz.space.limits: nodes, namespaces and objectCapKiB."
}

# A spec that names no number still gets a readable refusal.
@test "provision_shared: the refusal says so when the spec sets no number" {
  yq_space_defaults
  curl_space_refusal SPACE_LIMITS_ABOVE_FREE 403 "" ""

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  assert_output --partial "kubehz refused the space limits (the spec sets none): they are above what this account allows"
  # Nothing to decrease: the next step is to SET the values.
  assert_output --partial "Set the values in spec.kubehz.space.limits, or raise the ceiling"
  refute_output --partial "Decrease the values"
}

# A refusal message and its help are SERVER strings. The api could hide an
# escape sequence in either and redraw this terminal, so both implementations
# scrub them and clip them to 256 characters. The message additionally gets
# the backslash escape error() needs, because error() prints through
# `echo -e`; the help does NOT, because a plain echo would then double every
# backslash on screen.
#
# The fixtures and the expected bytes are the GOLDEN SET the Go twin owns
# (internal/kubehz/testdata/golden/, written by `go test ./internal/kubehz/
# -run TestProvisionSharedScrubsTheSharedCeilingMessage -update`). Both
# implementations are fed the same strings and asserted against the same
# bytes, so a drift on either side turns one of the two suites red. The parity
# harness cannot carry this case: spec.kubehz.apiUrl must be HTTPS, so no
# plain-http stub can answer either implementation.
@test "provision_shared: the shared refusal is scrubbed, clipped and escaped" {
  local golden_dir="${_PROJECT_ROOT}/internal/kubehz/testdata/golden"
  yq_space_spec
  curl_space_refusal SPACE_LIMITS_ABOVE_SHARED 400 \
    "$(cat "${golden_dir}/space-above-shared-message.txt")" \
    "$(cat "${golden_dir}/space-above-shared-help.txt")"

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  # Byte for byte what the Go twin renders from the same two strings.
  assert_output "$(cat "${golden_dir}/space-above-shared.txt")"
  # No control character reaches the terminal.
  refute_output --partial "$(printf '\033')"
  refute_output --partial "$(printf '\007')"
  # The inputs also hold a C1 CSI, a line separator and a bidi override.
  refute_output --partial $'\xc2\x9b'
  refute_output --partial $'\xe2\x80\xa8'
  refute_output --partial $'\xe2\x80\xae'
  # The characters the api sent stay exactly as many characters, on BOTH
  # print paths.
  assert_output --partial 'literal:\033[2J'
  assert_output --partial 'literal:\t and \\ stay'
}

# scrub drops each character that shown_name shows as '?' (the def unshown,
# utils/verbose.sh), but keeps a tab, a newline and a backslash. For each case
# of the shared table (internal/ui/testdata/shown.json), the input without each
# other character that the case's result shows as '?' is what scrub gives. The
# Go twin is TestScrubDropsEachUnshownCharacterButTabAndNewline.
@test "scrub: drops each character that shown_name shows as '?', keeps a tab, a newline and a backslash" {
  CASES="${_PROJECT_ROOT}/internal/ui/testdata/shown.json"
  run jq -r "${KUBEHZ_SCRUB_JQ}"' .[]
    | ([(.in | explode), (.want | explode)] | transpose
       | map(select(.[1] != 63 or .[0] == 63 or .[0] == 92 or .[0] == 9 or .[0] == 10) | .[0]) | implode) as $want
    | (.in | scrub) as $got | select($got != $want)
    | "\(.case): \($got | @json), want \($want | @json)"' "${CASES}"
  assert_success
  assert_output ""
  # The table drops something: else the test proves nothing.
  run jq -r "${KUBEHZ_SCRUB_JQ}"' [.[] | select((.in | scrub) != .in)] | length' "${CASES}"
  assert_success
  (( output >= 15 ))
}

# kubehz::scrub works on code points, also in the C locale (tr -d worked on
# bytes): the bytes of a C1 CSI (U+009B), a line separator (U+2028), a bidi
# override (U+202E) and an ESC go; a tab and a newline stay.
@test "kubehz::scrub: U+009B, U+2028, U+202E and ESC go in the C locale, a tab and a newline stay" {
  export LC_ALL=C
  local in want
  in=$(printf 'a\xc2\x9bb\xe2\x80\xa8c\xe2\x80\xaed\te\nf\x1b[2Jg')
  want=$(printf 'abcd\te\nf[2Jg')
  run kubehz::scrub "${in}"
  assert_success
  assert_output "${want}"
}

# space_api_error is the FIRST thing an api can print through: `lo provision`
# lists the spaces before it does anything else. A message carrying an escape
# sequence would clear the caller's terminal, so both halves are scrubbed and
# bounded, exactly as the refusal path does. Same golden set, same bytes as
# TestSpaceAPIErrorScrubsAndClipsBothHalves.
@test "space_api_error: both halves are scrubbed and clipped" {
  local golden_dir="${_PROJECT_ROOT}/internal/kubehz/testdata/golden"
  yq_space_spec
  _ERR_BODY=$(jq -nc \
    --arg m "$(cat "${golden_dir}/space-above-shared-message.txt")" \
    --arg h "$(cat "${golden_dir}/space-above-shared-help.txt")" \
    '{ok: false, message: $m, help: $h}')
  export _ERR_BODY
  curl() {
    printf '%s\n500' "${_ERR_BODY}"
  }
  export -f curl

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_failure
  # Byte for byte what the Go twin renders from the same two strings.
  assert_output "$(cat "${golden_dir}/space-api-error.txt")"
  refute_output --partial "$(printf '\033')"
  refute_output --partial "$(printf '\007')"
  # The inputs also hold a C1 CSI, a line separator and a bidi override.
  refute_output --partial $'\xc2\x9b'
  refute_output --partial $'\xe2\x80\xa8'
  refute_output --partial $'\xe2\x80\xae'
  assert_output --partial 'literal:\033[2J'
  assert_output --partial 'literal:\t and \\ stay'
}

# The expiry of a join ticket is a server string too: scrubbed, then clipped
# at 256 characters (the Go twin's
# TestSpaceMintJoinScrubsServerStringsAndRefusesAnOddTicket).
@test "space_mint_join: the expiry is scrubbed and clipped" {
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _MINT_BODY='{"ok":true,"data":{"token":"'"${ticket}"'","expiresAt":"2026\u001b[2J\u009b\u2028\u202e-08-07"}}'
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl

  run kubehz::space_mint_join sp-123 worker-1
  assert_success
  assert_output --partial "valid until 2026[2J-08-07, single use"
  refute_output --partial $'\033'
  refute_output --partial $'\xc2\x9b'
  refute_output --partial $'\xe2\x80\xa8'
  refute_output --partial $'\xe2\x80\xae'

  local long
  long=$(printf 'y%.0s' {1..300})
  _MINT_BODY=$(jq -nc --arg t "${ticket}" --arg e "${long}" '{ok: true, data: {token: $t, expiresAt: $e}}')
  run kubehz::space_mint_join sp-123 worker-1
  assert_success
  assert_output --partial "valid until ${long:0:256}…, single use"
}

# A mint writes the ticket to a private file and names the file on stdout,
# as the Go twin does without a script
# (TestSpaceMintJoinWithoutScriptWritesTheTicketPrivately): a fresh 0700
# directory below TMPDIR, the file 0600, the ticket and a newline in it.
# A name planted in the fresh directory: the next test.
@test "space_mint_join: the ticket goes to a private file, not to stdout" {
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _MINT_BODY=$(jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}')
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl

  run kubehz::space_mint_join sp-123 worker-1
  assert_success
  refute_output --partial "${ticket}"
  local files
  # The writer resolves each symlink of TMPDIR and prints that path.
  files=("$(_real "${TMPDIR}")"/kubehz-join-*/kubehz-join-worker-1.token)
  [ "${#files[@]}" -eq 1 ]
  [ -f "${files[0]}" ]
  [ ! -L "${files[0]}" ]
  assert_output --partial "  Node 'worker-1' — join ticket (valid until 2026-08-07T20:00:00Z, single use):
    (written to ${files[0]}, mode 0600)
  On the machine, follow the node-join guide for your platform
  (kubehz docs: Spaces → Joining nodes). The ticket is bound to this
  node name and expires quickly — mint a fresh one with:
    lo kubehz join worker-1"
  [ "$(cat "${files[0]}")" = "${ticket}" ]
  [ "$(wc -c < "${files[0]}")" -eq $(( ${#ticket} + 1 )) ]
  [ "$(stat -c %a "${files[0]}")" = 600 ]
  [ "$(stat -c %a "$(dirname "${files[0]}")")" = 700 ]
  [ "$(dirname "${files[0]}")" != "${TMPDIR}" ]
}

# The path with each symlink resolved, as the writer resolves TMPDIR.
_real() {
  (cd -P -- "${1}" && pwd -P)
}

# A mktemp stand-in: it records its template in MKTEMP_ARG and returns
# MKTEMP_DIR, which the test prepared (made by MKTEMP_PREP when set).
_stub_mktemp() {
  MKTEMP_ARG="${BATS_TEST_TMPDIR}/mktemp.arg"
  MKTEMP_DIR="${BATS_TEST_TMPDIR}/priv"
  export MKTEMP_ARG MKTEMP_DIR
  mktemp() {
    printf '%s\n' "${*: -1}" > "${MKTEMP_ARG}"
    printf '%s\n' "${MKTEMP_DIR}"
  }
  export -f mktemp
}

# A name planted at the very path of the ticket (a symlink to a victim, in
# the directory mktemp gave) is never written through: noclobber refuses
# it, the live-ticket note follows, the victim keeps its bytes, and the
# directory goes. As the Go twin's
# TestSpaceMintJoinNeverWritesThroughAPlantedName.
@test "space_mint_join: a name planted at the ticket path is never written through" {
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _MINT_BODY=$(jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}')
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  _stub_mktemp
  mkdir -m 700 "${MKTEMP_DIR}"
  printf 'untouched\n' > "${BATS_TEST_TMPDIR}/victim"
  ln -s "${BATS_TEST_TMPDIR}/victim" "${MKTEMP_DIR}/kubehz-join-worker-1.token"

  run kubehz::space_mint_join sp-123 worker-1
  assert_failure
  assert_output --partial "could not write the join ticket for 'worker-1'"
  assert_output --partial "The ticket was minted before the write failed"
  refute_output --partial "${ticket}"
  [ "$(cat "${BATS_TEST_TMPDIR}/victim")" = untouched ]
  [ ! -e "${MKTEMP_DIR}" ]
  [ ! -L "${MKTEMP_DIR}" ]
  [ "$(cat "${MKTEMP_ARG}")" = "$(_real "${TMPDIR}")/kubehz-join-XXXXXXXXXX" ]
}

# A write that fails in the fresh directory (one the user cannot write)
# leaves nothing: the directory goes, and the live-ticket note follows.
@test "space_mint_join: a failed write in the fresh directory removes it" {
  [[ "$(id -u)" != 0 ]] || skip "root writes into a 0500 directory"
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _MINT_BODY=$(jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}')
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  _stub_mktemp
  mkdir -m 500 "${MKTEMP_DIR}"

  run kubehz::space_mint_join sp-123 worker-1
  assert_failure
  assert_output --partial "could not write the join ticket for 'worker-1'"
  assert_output --partial "The ticket was minted before the write failed"
  refute_output --partial "${ticket}"
  [ ! -e "${MKTEMP_DIR}" ]
}

# TMPDIR=/ (and //) is the root directory, not the working directory, as
# in the Go twin; a trailing slash goes, and each symlink is resolved (the
# writer checks the resolved path and makes the directory there).
@test "space_write_join_ticket: TMPDIR=/ is the root directory" {
  local ticket pair tmpdir want real_t
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _stub_mktemp
  mkdir -m 700 "${BATS_TEST_TMPDIR}/t"
  ln -s "${BATS_TEST_TMPDIR}/t" "${BATS_TEST_TMPDIR}/link"
  real_t=$(_real "${BATS_TEST_TMPDIR}/t")
  # TMPDIR|the template's directory
  for pair in "/|" "//|" "${BATS_TEST_TMPDIR}/t/|${real_t}" "${BATS_TEST_TMPDIR}/link/|${real_t}"; do
    tmpdir="${pair%%|*}"
    want="${pair#*|}"
    rm -rf -- "${MKTEMP_DIR}"
    mkdir -m 700 "${MKTEMP_DIR}"
    TMPDIR="${tmpdir}" run kubehz::space_write_join_ticket worker-1 "${ticket}"
    assert_success
    assert_output "${MKTEMP_DIR}/kubehz-join-worker-1.token"
    [ "$(cat "${MKTEMP_ARG}")" = "${want}/kubehz-join-XXXXXXXXXX" ]
  done
}

# The writer checks the node name itself (the name is part of the path),
# not only space_mint_join before the mint.
@test "space_write_join_ticket: a path-shaped node name is refused before any directory" {
  local ticket dirs
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  run kubehz::space_write_join_ticket ../escaped "${ticket}"
  assert_failure 2
  assert_output --partial "is not a node name the platform accepts"
  refute_output --partial "${ticket}"
  dirs=("${TMPDIR}"/kubehz-join-*)
  [ ! -e "${dirs[0]}" ]
}

# The write fails after the mint: the live-ticket note, nothing on stdout.
# A path-shaped node name is refused before the mint (no request, so no
# live ticket) and before any directory is made.
@test "space_mint_join: a failed ticket write prints the live-ticket note" {
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _MINT_BODY=$(jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}')
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl

  TMPDIR="${BATS_TEST_TMPDIR}/does-not-exist" run kubehz::space_mint_join sp-123 worker-1
  assert_failure
  assert_output --partial "could not write the join ticket for 'worker-1'"
  assert_output --partial "The ticket was minted before the write failed. It stays valid until 2026-08-07T20:00:00Z,"
  assert_output --partial "lo kubehz join worker-1"
  refute_output --partial "${ticket}"

  CURL_CALLS="${BATS_TEST_TMPDIR}/curl.calls"
  export CURL_CALLS
  curl() {
    echo called >> "${CURL_CALLS}"
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  run kubehz::space_mint_join sp-123 ../escaped
  assert_failure
  assert_output --partial "is not a node name the platform accepts"
  refute_output --partial "${ticket}"
  [ ! -e "${CURL_CALLS}" ]
  local dirs=("${TMPDIR}"/kubehz-join-*)
  [ ! -e "${dirs[0]}" ]
}

# A ticket outside the kubeadm bootstrap-token shape is refused before
# anything is printed or written, as by the Go twin's joinTokenRe: an
# uppercase letter, a non-ASCII letter, a short secret, an escape, a second
# line, a separator other than the dot, and a trailing newline, a NUL or a
# CR (the token is read as JSON text, so $(...) strips and drops nothing).
# The shape check spells its characters out, so the locale does not widen
# it (the next test).
@test "space_mint_join: a ticket outside the bootstrap-token shape is refused" {
  local good bad
  good=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  for bad in "${good^^}" "${good/a1/é1}" "${good%k1}" "${good}"$'\e[2J' "${good}"$'\n'"${good}" "${good/./-}"; do
    _MINT_BODY=$(jq -nc --arg t "${bad}" '{ok: true, data: {token: $t, expiresAt: "soon"}}')
    export _MINT_BODY
    run kubehz::space_mint_join sp-123 worker-1
    assert_failure
    assert_output --partial "kubehz API returned a join ticket for 'worker-1' in a shape this CLI does not recognise; nothing was written"
    refute_output --partial "join ticket (valid until"
    refute_output --partial "${bad}"
  done
  local sfx
  for sfx in '\n' '\u0000' '\r'; do
    _MINT_BODY=$(jq -nc --arg t "${good}" "{ok: true, data: {token: (\$t + \"${sfx}\"), expiresAt: \"soon\"}}")
    export _MINT_BODY
    run kubehz::space_mint_join sp-123 worker-1
    assert_failure
    assert_output --partial "kubehz API returned a join ticket for 'worker-1' in a shape this CLI does not recognise; nothing was written"
    refute_output --partial "join ticket (valid until"
    refute_output --partial "${good}"
  done
  local dirs=("${TMPDIR}"/kubehz-join-*)
  [ ! -e "${dirs[0]}" ]
}

# In a UTF-8 locale where a bash range [a-z] matches e with an accent
# (en_US.UTF-8 with glibc does), the spelled-out class still refuses a
# non-ASCII letter. Skipped where no such locale exists.
@test "space_mint_join: the shape check does not widen with the locale" {
  local good loc wide="" bad e_acute=$'\xc3\xa9' sharp_s=$'\xc3\x9f'
  good=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  for loc in $(locale -a 2>/dev/null); do
    [[ "${loc,,}" == *utf*8* ]] || continue
    if LC_ALL="${loc}" bash -c '[[ $1 =~ ^[a-z]$ ]]' _ "${e_acute}" 2>/dev/null; then
      wide="${loc}"
      break
    fi
  done
  [[ -n "${wide}" ]] || skip "no locale here widens [a-z]"
  export LC_ALL="${wide}"
  # The range widens in this shell too, so the check below is the real one.
  [[ "${e_acute}" =~ ^[a-z]$ ]]
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  for bad in "${good/a1/${e_acute}1}" "${good/a1/${sharp_s}1}"; do
    _MINT_BODY=$(jq -nc --arg t "${bad}" '{ok: true, data: {token: $t, expiresAt: "soon"}}')
    export _MINT_BODY
    run kubehz::space_mint_join sp-123 worker-1
    assert_failure
    assert_output --partial "in a shape this CLI does not recognise; nothing was written"
  done
}

# A FIFO planted at the very path of the ticket, or a link to one, gets no
# byte of the ticket: noclobber lets a FIFO through, so the writer makes
# the file empty first and checks it is a regular file of ours, not a link,
# before the write. The test holds the FIFO open for reading and writing:
# no open by the writer can block, and the FIFO keeps what it gets for the
# read after the mint. The live-ticket note follows, and the directory
# goes. As the Go twin's TestSpaceMintJoinNeverWritesIntoAPlantedFIFO.
@test "space_mint_join: a FIFO at the ticket path, or a link to one, gets no byte of the ticket" {
  local ticket kind fifo got ffd
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _MINT_BODY=$(jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}')
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  _stub_mktemp
  for kind in fifo link; do
    rm -rf -- "${MKTEMP_DIR}"
    mkdir -m 700 "${MKTEMP_DIR}"
    fifo="${MKTEMP_DIR}/kubehz-join-worker-1.token"
    if [[ "${kind}" == link ]]; then
      fifo="${BATS_TEST_TMPDIR}/fifo"
      rm -f -- "${fifo}"
      ln -s "${fifo}" "${MKTEMP_DIR}/kubehz-join-worker-1.token"
    fi
    mkfifo -m 600 "${fifo}"
    exec {ffd}<>"${fifo}"
    run kubehz::space_mint_join sp-123 worker-1
    got=""
    read -r -t 1 -u "${ffd}" got || true
    exec {ffd}>&-
    assert_failure
    assert_output --partial "could not write the join ticket for 'worker-1'"
    assert_output --partial "The ticket was minted before the write failed"
    refute_output --partial "${ticket}"
    [ -z "${got}" ]
    [ ! -e "${MKTEMP_DIR}" ]
  done
}

# A TMPDIR whose path a foreign owner could change is refused before any
# directory is made: a part others can write without the sticky bit (the
# TMPDIR itself or a parent), and a link to such a directory (the resolved
# path counts). A part with the sticky bit passes, as /tmp does. As the Go
# twin's TestSpaceMintJoinRefusesATMPDIROthersCanChange.
@test "space_write_join_ticket: a TMPDIR others can change is refused" {
  local ticket pair tmpdir bad dirs
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  mkdir "${BATS_TEST_TMPDIR}/gw" "${BATS_TEST_TMPDIR}/ow" "${BATS_TEST_TMPDIR}/wp" "${BATS_TEST_TMPDIR}/lw" "${BATS_TEST_TMPDIR}/st"
  mkdir "${BATS_TEST_TMPDIR}/wp/sub"
  chmod 770 "${BATS_TEST_TMPDIR}/gw" "${BATS_TEST_TMPDIR}/lw"
  chmod 707 "${BATS_TEST_TMPDIR}/ow"
  chmod 777 "${BATS_TEST_TMPDIR}/wp"
  chmod 700 "${BATS_TEST_TMPDIR}/wp/sub"
  chmod 1777 "${BATS_TEST_TMPDIR}/st"
  ln -s "${BATS_TEST_TMPDIR}/lw" "${BATS_TEST_TMPDIR}/link"
  # TMPDIR|the part that fails
  for pair in "gw|gw" "ow|ow" "wp/sub|wp" "link|lw"; do
    tmpdir="${BATS_TEST_TMPDIR}/${pair%%|*}"
    bad=$(_real "${BATS_TEST_TMPDIR}/${pair#*|}")
    TMPDIR="${tmpdir}" run kubehz::space_write_join_ticket worker-1 "${ticket}"
    assert_failure 1
    assert_output --partial "the temp dir for the join ticket is not safe: ${bad} is writable by others and has no sticky bit"
    assert_output --partial "Set TMPDIR to a directory of your own that only you can write (mode 0700)."
    refute_output --partial "${ticket}"
    dirs=("${tmpdir}"/kubehz-join-*)
    [ ! -e "${dirs[0]}" ]
  done
  TMPDIR="${BATS_TEST_TMPDIR}/st" run kubehz::space_write_join_ticket worker-1 "${ticket}"
  assert_success
  [ "$(cat "${output}")" = "${ticket}" ]
}

# A part of another user is refused, also when its owner is the overflow
# uid: a user namespace that maps only this user shows the parts of root
# with that owner, and only "/" may have it (the next test). The first part
# above the test's TMPDIR that is not ours (often /tmp, sticky or not) is
# refused, and so is /usr. Skipped where no user namespace can be made.
@test "space_write_join_ticket: a TMPDIR part of another user is refused" {
  unshare --user --map-current-user true 2>/dev/null || skip "no user namespace here"
  local ticket ovf part foreign="" tmpdir want
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  printf '%s' "${ticket}" > "${BATS_TEST_TMPDIR}/ticket"
  ovf=$(cat /proc/sys/kernel/overflowuid 2>/dev/null) || ovf=65534
  part=$(_real "${TMPDIR}")
  while :; do
    if [[ "$(stat -c %u -- "${part}")" != "${EUID}" ]]; then
      foreign="${part}"
      break
    fi
    [[ "${part}" != / ]] || break
    part="${part%/*}"
    part="${part:-/}"
  done
  [[ -n "${foreign}" && "${foreign}" != / ]] || skip "every part above TMPDIR but / is ours"
  # TMPDIR|the part that fails
  for tmpdir in "${TMPDIR}|${foreign}" "/usr|/usr"; do
    want="${tmpdir#*|}"
    run unshare --user --map-current-user bash -c '
      import() { :; }
      source "${1}/.lok8s/utils/verbose.sh"
      source "${1}/.lok8s/libs/kubehz/shared"
      source "${1}/.lok8s/libs/kubehz/node"
      TMPDIR="${3}" kubehz::space_write_join_ticket worker-1 "$(cat "${2}")"' _ "${_PROJECT_ROOT}" "${BATS_TEST_TMPDIR}/ticket" "${tmpdir%%|*}"
    assert_failure 1
    assert_output --partial "the temp dir for the join ticket is not safe: ${want} belongs to another user (uid ${ovf})"
    refute_output --partial "${ticket}"
  done
  local dirs=("${TMPDIR}"/kubehz-join-*)
  [ ! -e "${dirs[0]}" ]
}

# "/" alone may show the overflow uid: in a user namespace that maps only
# this user, the host's "/" (root's) shows that owner. A tmpfs on /tmp
# (ours inside the namespace) gives a TMPDIR whose only foreign part is
# "/", and the ticket is written there, 0600 in a 0700 directory. Skipped
# where no user namespace with a tmpfs mount can be made.
@test "space_write_join_ticket: in a user namespace, a / of the overflow uid passes" {
  unshare --user --map-current-user --keep-caps --mount sh -c 'mount -t tmpfs none /tmp' 2>/dev/null \
    || skip "no user namespace with a tmpfs mount here"
  local ticket ovf
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  printf '%s' "${ticket}" > "${BATS_TEST_TMPDIR}/ticket"
  ovf=$(cat /proc/sys/kernel/overflowuid 2>/dev/null) || ovf=65534
  # The sources and the ticket are read before the tmpfs hides /tmp.
  run unshare --user --map-current-user --keep-caps --mount bash -c '
    import() { :; }
    source "${1}/.lok8s/utils/verbose.sh"
    source "${1}/.lok8s/libs/kubehz/shared"
    source "${1}/.lok8s/libs/kubehz/node"
    t=$(cat "${2}")
    echo "owner of /: $(stat -c %u /)"
    mount -t tmpfs -o mode=1777 none /tmp || exit 9
    mkdir -m 700 /tmp/t
    p=$(TMPDIR=/tmp/t kubehz::space_write_join_ticket worker-1 "${t}") || exit 3
    echo "path: ${p}"
    echo "modes: $(stat -c %a "${p%/*}") $(stat -c %a "${p}")"
    if [ "$(cat "${p}")" = "${t}" ]; then echo "content: the ticket"; fi' _ "${_PROJECT_ROOT}" "${BATS_TEST_TMPDIR}/ticket"
  assert_success
  assert_output --partial "owner of /: ${ovf}"
  assert_output --regexp "path: /tmp/t/kubehz-join-[A-Za-z0-9]+/kubehz-join-worker-1\.token"
  assert_output --partial "modes: 700 600"
  assert_output --partial "content: the ticket"
  refute_output --partial "not safe"
  refute_output --partial "${ticket}"
}

# The overflow uid comes from /proc/sys/kernel/overflowuid, else 65534. A
# file bound over it says 4242: the "/" of the namespace (it shows the real
# overflow uid) is then another user's and is refused. An empty file gives
# 65534, and "/" passes where the real overflow uid is 65534. Skipped where
# no user namespace with mounts can be made.
@test "space_join_tmp_dir: the overflow uid comes from the kernel file, else 65534" {
  unshare --user --map-current-user --keep-caps --mount sh -c 'mount -t tmpfs none /tmp' 2>/dev/null \
    || skip "no user namespace with a tmpfs mount here"
  printf '4242\n' > "${BATS_TEST_TMPDIR}/num"
  : > "${BATS_TEST_TMPDIR}/empty"
  local f
  for f in num empty; do
    run unshare --user --map-current-user --keep-caps --mount bash -c '
      import() { :; }
      source "${1}/.lok8s/utils/verbose.sh"
      source "${1}/.lok8s/libs/kubehz/shared"
      mount --bind "${2}" /proc/sys/kernel/overflowuid || exit 9
      echo "owner of /: $(stat -c %u /)"
      mount -t tmpfs -o mode=1777 none /tmp || exit 9
      mkdir -m 700 /tmp/t
      TMPDIR=/tmp/t kubehz::space_join_tmp_dir' _ "${_PROJECT_ROOT}" "${BATS_TEST_TMPDIR}/${f}"
    if [[ "${f}" == num ]]; then
      assert_failure 1
      assert_output --partial "the temp dir for the join ticket is not safe: / belongs to another user (uid "
    elif [[ "${output}" == *"owner of /: 65534"* ]]; then
      assert_success
      assert_line "/tmp/t"
    fi
  done
}

# The modes do not follow the umask: under 0400 (which would give a 0300
# directory) and 0077, the directory is 0700 and the file 0600, and the
# ticket is in it. As the Go twin's TestSpaceMintJoinSetsTheModesUnderAnyUmask.
@test "space_mint_join: the modes do not follow the umask" {
  local ticket mask tmpd files
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  _MINT_BODY=$(jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}')
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  _masked_mint() {
    umask "${1}"
    kubehz::space_mint_join sp-123 worker-1
  }
  for mask in 0400 0077; do
    tmpd="${BATS_TEST_TMPDIR}/u${mask}"
    mkdir -m 700 "${tmpd}"
    TMPDIR="${tmpd}" run _masked_mint "${mask}"
    assert_success
    files=("${tmpd}"/kubehz-join-*/kubehz-join-worker-1.token)
    [ "${#files[@]}" -eq 1 ]
    [ "$(stat -c %a "${files[0]}")" = 600 ]
    [ "$(stat -c %a "$(dirname "${files[0]}")")" = 700 ]
    [ "$(cat "${files[0]}")" = "${ticket}" ]
  done
}

# A default ACL on TMPDIR wins over the umask: a file made in it gets the
# group's bits from the ACL (0660 here), not 0600. The writer sets the mode
# of the empty file before the write, as the Go twin does through the
# handle, so the ticket file is 0600 in a 0700 directory all the same.
# Skipped where no ACL can be set.
@test "space_write_join_ticket: a default ACL on TMPDIR does not widen the modes" {
  local ticket tmpd files
  command -v setfacl >/dev/null || skip "no setfacl here"
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  tmpd="${BATS_TEST_TMPDIR}/acl"
  mkdir -m 700 "${tmpd}"
  setfacl -d -m u::rwx,g::rwx,o::-,m::rwx "${tmpd}" 2>/dev/null || skip "no ACL on this file system"
  # The ACL is live: a plain file made in it is 0660 under umask 077.
  ( umask 077 && : > "${tmpd}/probe" )
  [ "$(stat -c %a "${tmpd}/probe")" = 660 ]
  rm -- "${tmpd}/probe"
  TMPDIR="${tmpd}" run kubehz::space_write_join_ticket worker-1 "${ticket}"
  assert_success
  files=("${tmpd}"/kubehz-join-*/kubehz-join-worker-1.token)
  [ "${#files[@]}" -eq 1 ]
  [ "${output}" = "$(_real "${tmpd}")/${files[0]#"${tmpd}/"}" ]
  [ "$(stat -c %a "${files[0]}")" = 600 ]
  [ "$(stat -c %a "$(dirname "${files[0]}")")" = 700 ]
  [ "$(cat "${files[0]}")" = "${ticket}" ]
}

# Under set -x (the trace on stdout: BASH_XTRACEFD=1) the trace holds no
# ticket, no response and no API token, also when a RETURN trap under
# set -T turns tracing on again inside (the first guard line). Lines before
# and after the mint prove that the trace was on. The response comes from
# a file, so no exported variable of the test holds the ticket.
@test "space_mint_join: a trace holds no ticket and no API token" {
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  MINT_FILE="${BATS_TEST_TMPDIR}/mint.json"
  jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}' > "${MINT_FILE}"
  export MINT_FILE
  curl() {
    printf '%s\n201' "$(cat "${MINT_FILE}")"
  }
  export -f curl
  _traced_mint() {
    BASH_XTRACEFD=1
    set -x
    : trace-on
    kubehz::space_mint_join sp-123 worker-1
    : trace-after
  }
  _traced_mint_t() {
    BASH_XTRACEFD=1
    set -T
    trap 'set -x' RETURN
    set -x
    : trace-on
    kubehz::space_mint_join sp-123 worker-1
    trap - RETURN
    : trace-after
  }
  local fn
  for fn in _traced_mint _traced_mint_t; do
    run "${fn}"
    assert_success
    assert_output --partial "+ : trace-on"
    assert_output --partial "+ : trace-after"
    assert_output --partial "(written to "
    refute_output --partial "${ticket}"
    refute_output --partial "${ticket#*.}"
    refute_output --partial "${KUBEHZ_TOKEN}"
  done
}

# Each api call keeps the bearer out of a trace too, not only the mint:
# the bearer goes to curl in a config on stdin, through a function argument
# that a trace would print.
@test "space_api: a trace holds no API token" {
  curl() {
    cat > /dev/null
    printf '%s\n200' '{"ok":true,"data":[]}'
  }
  export -f curl
  _traced_api() {
    BASH_XTRACEFD=1
    set -x
    : trace-on
    kubehz::space_api GET /api/spaces
    : trace-after
  }
  run _traced_api
  assert_success
  assert_output --partial "+ : trace-on"
  assert_output --partial "+ : trace-after"
  refute_output --partial "${KUBEHZ_TOKEN}"
}

# Under set -a no child gets the ticket or the response: not during the
# write (a mktemp stand-in records its environment), and not after it.
# The response global was exported before (as an earlier export would);
# the writer takes the export off, and the mint unsets it at the end.
@test "space_mint_join: under set -a no child gets the ticket" {
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  MINT_FILE="${BATS_TEST_TMPDIR}/mint.json"
  jq -nc --arg t "${ticket}" '{ok: true, data: {token: $t, expiresAt: "2026-08-07T20:00:00Z"}}' > "${MINT_FILE}"
  ENV_DIR="${BATS_TEST_TMPDIR}"
  export MINT_FILE ENV_DIR
  curl() {
    printf '%s\n201' "$(cat "${MINT_FILE}")"
  }
  mktemp() {
    env > "${ENV_DIR}/during.env"
    command mktemp "$@"
  }
  export -f curl mktemp
  _exported_mint() {
    export KUBEHZ_SPACE_API_BODY=""
    set -a
    kubehz::space_mint_join sp-123 worker-1 || return
    env > "${ENV_DIR}/after.env"
    printf '%s\n' "${KUBEHZ_SPACE_API_BODY-<unset>}" > "${ENV_DIR}/body.after"
  }
  run _exported_mint
  assert_success
  assert_output --partial "(written to "
  [ -s "${ENV_DIR}/during.env" ]
  [ -s "${ENV_DIR}/after.env" ]
  # Not `! grep`: bash ignores errexit for a negated command, so that line
  # could never fail the test.
  [ "$(grep -c -F -- "${ticket#*.}" "${ENV_DIR}/during.env")" -eq 0 ]
  [ "$(grep -c -F -- "${ticket#*.}" "${ENV_DIR}/after.env")" -eq 0 ]
  [ "$(cat "${ENV_DIR}/body.after")" = "<unset>" ]
}

# The writer turns allexport off itself, also when it is called on its
# own: under set -a no child of the write (a mktemp stand-in records its
# environment) gets the ticket. The ticket reaches the writer as an
# argument only, never in an exported variable of the test.
@test "space_write_join_ticket: under set -a no child gets the ticket" {
  local ticket
  ticket=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  printf '%s' "${ticket}" > "${BATS_TEST_TMPDIR}/ticket"
  ENV_DIR="${BATS_TEST_TMPDIR}"
  export ENV_DIR
  mktemp() {
    env > "${ENV_DIR}/during.env"
    command mktemp "$@"
  }
  export -f mktemp
  _exported_write() {
    local t
    t=$(cat "${ENV_DIR}/ticket")
    set -a
    kubehz::space_write_join_ticket worker-1 "${t}"
  }
  run _exported_write
  assert_success
  [ -s "${ENV_DIR}/during.env" ]
  [ "$(grep -c -F -- "${ticket#*.}" "${ENV_DIR}/during.env")" -eq 0 ]
  [ "$(cat "${output}")" = "${ticket}" ]
}

# nocasematch would let an uppercase ticket through the shape check: the
# check turns it off and sets it back after. As the Go twin's joinTokenRe.
@test "space_mint_join: nocasematch does not widen the shape check" {
  local good bad
  good=$(printf '%s.%s' a1b2c3 d4e5f6g7h8i9j0k1)
  bad="${good^^}"
  _MINT_BODY=$(jq -nc --arg t "${bad}" '{ok: true, data: {token: $t, expiresAt: "soon"}}')
  export _MINT_BODY
  curl() {
    printf '%s\n201' "${_MINT_BODY}"
  }
  export -f curl
  _ncm_mint() {
    local rc=0
    shopt -s nocasematch
    kubehz::space_mint_join sp-123 worker-1 || rc=$?
    shopt -q nocasematch && echo "nocasematch is on again"
    return "${rc}"
  }
  run _ncm_mint
  assert_failure
  assert_output --partial "kubehz API returned a join ticket for 'worker-1' in a shape this CLI does not recognise; nothing was written"
  assert_output --partial "nocasematch is on again"
  refute_output --partial "${bad}"
  local dirs=("${TMPDIR}"/kubehz-join-*)
  [ ! -e "${dirs[0]}" ]
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
  # The bearer reaches curl in a config on stdin (-K -), never on argv:
  # curl_capture reads it the way curl does.
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  curl() {
    local auth_seen=""
    [[ " $* " != *"khz_test_token"* ]] || { printf '{"error":"bearer on argv"}\n400'; return 0; }
    curl_capture "$@"
    # EXACT bearer required (round 2): any-non-empty let a wrong variable
    # (e.g. HCLOUD_TOKEN) pass. The sentinel is pinned by the test env, and
    # the empty-token leg cannot match because ?* demands substance.
    [[ $'\n'"${CURL_HEADERS}" == *$'\n'"Authorization: Bearer khz_test_token"$'\n'* ]] && auth_seen=1
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

@test "space_api: a curl failure is rc 1 with the unreachable line; space_api_call keeps the curl code and prints none" {
  bats_require_minimum_version 1.5.0
  curl() { echo "curl: (7) stub failure" >&2; return 7; }
  export -f curl

  run --separate-stderr kubehz::space_api GET "/api/spaces"
  [ "$status" -eq 1 ]
  [[ "${stderr}" == "curl: (7) stub failure"$'\n'"[error] kubehz API unreachable (GET /api/spaces)" ]]

  # The caller that tells "before the send" from "after it" (node::maintain)
  # reads the curl code and renders the failure itself.
  local rc=0
  kubehz::space_api_call POST "/api/spaces" '{}' 2>"${BATS_TEST_TMPDIR}/call.err" || rc=$?
  [ "${rc}" -eq 1 ]
  [ "${KUBEHZ_SPACE_API_CURL_RC}" = "7" ]
  [[ "$(cat "${BATS_TEST_TMPDIR}/call.err")" == "curl: (7) stub failure" ]]

  # The token check refuses before curl runs: no curl code, and space_api
  # adds no unreachable line to the check's own.
  KUBEHZ_TOKEN=$'khz_test_token\r' run --separate-stderr kubehz::space_api GET "/api/spaces"
  [ "$status" -eq 1 ]
  [[ "${stderr}" == *"must not contain a control character"* ]]
  [[ "${stderr}" != *"unreachable"* ]]
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

@test "provision_shared: writes the space kubeconfig the api serves and names the context (D30)" {
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
      "GET /api/spaces/sp-123/kubeconfig")
        printf 'apiVersion: v1\nkind: Config\ncurrent-context: kubehz-acme\n200' ;;
      "POST /api/spaces/sp-123/join-token")
        printf '{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"w","expiresAt":"2026-08-07T20:00:00Z"}}\n201' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "kubectl --context kubehz-acme -n acme get pods"
  assert_output --partial "Kubeconfig: ${PATH_BASE}/.kubeconfig/acme.example.org.yaml"
  run cat "${PATH_BASE}/.kubeconfig/acme.example.org.yaml"
  assert_output --partial "current-context: kubehz-acme"
  run stat -c '%a' "${PATH_BASE}/.kubeconfig/acme.example.org.yaml"
  assert_output "600"
}

@test "provision_shared: keeps the older access lines when the api has no kubeconfig for the space" {
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
      "GET /api/spaces/sp-123/kubeconfig")
        printf '{"ok":false,"error":{"code":"SPACE_KUBECONFIG_NOT_READY"}}\n503' ;;
      "POST /api/spaces/sp-123/join-token")
        printf '{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"w","expiresAt":"2026-08-07T20:00:00Z"}}\n201' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::provision_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "Access: sign in with your kubehz account (OIDC)"
  [ ! -e "${PATH_BASE}/.kubeconfig/acme.example.org.yaml" ]
}

@test "destroy_shared: removes the kubeconfig provision wrote (D30)" {
  yq_space_spec
  mkdir -p "${PATH_BASE}/.kubeconfig"
  printf 'apiVersion: v1\n' > "${PATH_BASE}/.kubeconfig/acme.example.org.yaml"
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
        printf '{"ok":true,"data":[{"id":"sp-123","slug":"acme","status":"Active"}]}\n200' ;;
      "DELETE /api/spaces/sp-123")
        printf '{"ok":true}\n200' ;;
      *)
        printf '{"ok":false}\n500' ;;
    esac
  }
  export -f curl

  run kubehz::destroy_shared "acme.example.org" "/dev/null"
  assert_success
  assert_output --partial "Space 'acme' removed (id: sp-123)"
  [ ! -e "${PATH_BASE}/.kubeconfig/acme.example.org.yaml" ]
}

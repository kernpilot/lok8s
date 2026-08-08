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

# yq mock for a spec with an explicit space block + two declared nodes.
yq_space_spec() {
  yq() {
    case "$2" in
      '.spec.kubehz.space.slug // ""')   echo "acme" ;;
      '.spec.kubehz.space.name // ""')   echo "Acme Prod" ;;
      '.spec.kubehz.space.plan // ""')   echo "shared-s" ;;
      '.spec.kubehz.space.region // ""') echo "" ;;
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
      '.kind') echo "Kubehz" ;;
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
      '.kind') echo "KubeOne" ;;
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
      '.kind') echo "Kubehz" ;;
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

@test "space_status: renders phase, plan and the node table" {
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
        printf '{"ok":true,"data":[{"id":"sp-5","slug":"acme","status":"Active","planId":"shared-free"}]}\n200' ;;
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
  assert_output --partial "Plan:    shared-free"
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
  accepted=$(grep -oE '^\s+self\|hosted\|shared\)' "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main" | tr -d ' )')
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

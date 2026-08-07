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
        printf '{"ok":true,"data":{"id":"sp-123","slug":"acme","phase":"Provisioning"}}\n201' ;;
      "GET /api/spaces/sp-123")
        printf '{"ok":true,"data":{"id":"sp-123","phase":"Active"}}\n200' ;;
      "POST /api/spaces/sp-123/join-token")
        printf '{"ok":true,"data":{"token":"khzj_TESTTOKEN","nodeName":"w","expiresAt":"2026-08-07T20:00:00Z"}}\n201' ;;
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
  [ "$(grep -c "khzj_TESTTOKEN" <<<"${output}")" -eq 2 ]
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
        printf '{"ok":true,"data":[{"id":"sp-777","slug":"acme","phase":"Active"}]}\n200' ;;
      "GET /api/spaces/sp-777")
        printf '{"ok":true,"data":{"id":"sp-777","phase":"Active"}}\n200' ;;
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
          printf '{"ok":true,"data":[{"id":"sp-race","slug":"acme","phase":"Active"}]}\n200'
        else
          printf '{"ok":true,"data":[]}\n200'
        fi ;;
      "POST /api/spaces")
        touch "${CURL_STATE}"
        printf '{"ok":false,"data":{"code":"CONFLICT","message":"slug exists"}}\n409' ;;
      "GET /api/spaces/sp-race")
        printf '{"ok":true,"data":{"id":"sp-race","phase":"Active"}}\n200' ;;
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
        printf '{"ok":true,"data":[{"id":"sp-9","slug":"acme","phase":"Active"}]}\n200' ;;
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
        printf '{"ok":true,"data":[{"id":"sp-5","slug":"acme","phase":"Active","planId":"shared-free"}]}\n200' ;;
      "GET /api/spaces/sp-5/nodes")
        printf '{"ok":true,"data":[{"nodeName":"worker-1","status":"Ready","lane":"hcloud"}]}\n200' ;;
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

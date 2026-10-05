#!/usr/bin/env bats
# kubehz_register_test.bats — unit tests for kubehz registration and deregistration

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # register_cluster re-asserts HTTPS via http::require_https before any network
  # call, so the helper must be available to the sourced lib.
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  # Create domain structure
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev"

  # Stub provision::resolve_spec (used by kubehz::register subcommand)
  provision::resolve_spec() {
    LOK8S_SPEC_FILE="${PATH_CLUSTERS}/$1/cluster.lok8s.yaml"
    LOK8S_SPEC_KIND="cluster"
  }
  export -f provision::resolve_spec
}

teardown() {
  teardown_tmpdir
}

# ── get_ssh_fingerprint: Lo kind uses domain ─────────────

@test "get_ssh_fingerprint: Lo kind returns lo:<domain>" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::get_ssh_fingerprint "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
  assert_success
  assert_output "lo:test.kubehz.dev"
}

# ── get_ssh_fingerprint: KubeOne reads key file ─────────

@test "get_ssh_fingerprint: KubeOne reads ssh key file" {
  yq() {
    case "$2" in
      '.kind // ""') echo "KubeOne" ;;
      '.spec.hcloud.sshPublicKeyFile // .spec.ssh.publicKeyFile // "~/.ssh/id_ed25519.pub"')
        echo "${BATS_TEST_TMPDIR}/test_key.pub" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Mock ssh-keygen — must be invoked with `-E md5` (Hetzner exposes MD5).
  ssh-keygen() {
    [[ " $* " == *" -E md5 "* ]] || { echo "ssh-keygen called without -E md5: $*" >&2; return 1; }
    echo "256 MD5:ec:ea:8f:11:f3:c6:e8:10:c1:58:40:be:24:87:a8:04 test@host (ED25519)"
  }
  export -f ssh-keygen

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::get_ssh_fingerprint "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output "MD5:ec:ea:8f:11:f3:c6:e8:10:c1:58:40:be:24:87:a8:04"
}

# ── get_ssh_fingerprint: Capi queries hcloud ─────────────

@test "get_ssh_fingerprint: Capi queries hcloud for ssh key" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Capi" ;;
      '.spec.hcloud.sshKeyName // ""') echo "my-key" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  hcloud() {
    echo '{"public_key": "ssh-ed25519 AAAA mock-capi-key"}'
  }
  export -f hcloud

  jq() {
    echo "ssh-ed25519 AAAA mock-capi-key"
  }
  export -f jq

  ssh-keygen() {
    [[ " $* " == *" -E md5 "* ]] || { echo "ssh-keygen called without -E md5: $*" >&2; return 1; }
    echo "256 MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99 test@host (ED25519)"
  }
  export -f ssh-keygen

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::get_ssh_fingerprint "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output "MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
}

# ── get_ssh_fingerprint: unknown kind fails ──────────────

@test "get_ssh_fingerprint: unknown kind returns error" {
  yq() {
    case "$2" in
      '.kind // ""') echo "UnknownKind" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::get_ssh_fingerprint "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "Cannot extract SSH fingerprint for kind=unknownkind"
}

# ── register_cluster: successful registration ────────────

@test "register_cluster: posts to /api/clusters/register and prints the claim fingerprint" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Assert the producer hits the REGISTER endpoint (not claims/verify).
  curl() {
    [[ " $* " == *" https://api.kubehz.dev/api/clusters/register "* ]] \
      || { echo "curl wrong endpoint: $*" >&2; return 1; }
    echo '{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}'
  }
  export -f curl

  jq() {
    case "$2" in
      '.id // empty') echo "cl-001" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  # The claim handshake (public fingerprint) is surfaced to the user.
  assert_output --partial "Claim it in the dashboard"
  assert_output --partial "fingerprint: lo:test.kubehz.dev"
}

# ── register_cluster: access managed notes the platform-side tier gate ───

@test "register_cluster: access managed notes the Supporter+ gate and still registers" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  curl() {
    [[ " $* " == *" https://api.kubehz.dev/api/clusters/register "* ]] \
      || { echo "curl wrong endpoint: $*" >&2; return 1; }
    echo '{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}'
  }
  export -f curl

  jq() {
    case "$2" in
      '.id // empty') echo "cl-001" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  # Managed is live but subscription-gated PLATFORM-side; registration is identical.
  export LOK8S_KUBEHZ_ACCESS="managed"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  # Honest notice: the gate lives platform-side (Supporter+), after claiming.
  assert_output --partial "Supporter+"
  # … but the cluster is still registered for read-only heartbeat visibility.
  assert_output --partial "Claim it in the dashboard"
  assert_output --partial "fingerprint: lo:test.kubehz.dev"
}

# ── regression: the non-functional managed operator stays parked ─

@test "kubehz lib ships no ghost operator image or manifest (parked, never applied)" {
  # access: managed must never point at a non-existent operator image (an apply
  # would ImagePullBackOff forever). Guard the ghost from creeping back into the
  # public repo: no such image string, and no manifests/operator/ directory.
  run grep -rnF "ghcr.io/kernpilot/kubehz-operator" "${_PROJECT_ROOT}/.lok8s/libs/kubehz"
  assert_failure
  assert [ ! -d "${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/operator" ]
  # The real heartbeat agent (access: registered) is untouched.
  assert [ -f "${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/agent/cronjob.yaml" ]
}

# ── register_cluster: HTTPS is enforced before any network call ─

@test "register_cluster: refuses a plain-HTTP apiUrl (no curl)" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # curl must NOT be reached — fail loudly if it is.
  curl() { echo "curl should not run over plain HTTP" >&2; return 99; }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="http://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "must use HTTPS"
}

# ── register_cluster: missing cluster id in response is non-fatal ─

@test "register_cluster: empty cluster id warns but returns 0" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  curl() {
    echo '{"message": "something went wrong"}'
  }
  export -f curl

  jq() {
    case "$2" in
      '.id // empty') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "returned no cluster id"
}

# ── register_cluster: curl failure is non-fatal ──────────

@test "register_cluster: API unreachable warns but returns 0" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  curl() {
    return 1
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "kubehz API request failed"
}

# ── register_cluster: fingerprint extraction failure is non-fatal ─

@test "register_cluster: fingerprint failure warns but returns 0" {
  yq() {
    case "$2" in
      '.kind // ""') echo "UnknownKind" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "Could not extract SSH fingerprint"
}

# ── deregister_cluster: resolve-then-delete-by-id ────────
#
# The REAL api contract: the list endpoint ignores ?domain= and there is NO
# DELETE /api/clusters?domain=… route — deregister must resolve the id from
# the tenant registry (client-side domain filter, oldest-first) and DELETE
# /api/clusters/<id>. These mocks pin exactly that; a query-string DELETE
# fails the test. (The previous mocks answered the nonexistent route and
# pinned the broken contract green.)

# Shared list fixture: newest-first (the api's order), TWO rows for the
# domain plus an unrelated tenant sibling. Oldest-first resolution must pick
# cl-old — the row the server binds agent identity to — never .data[0].
_deregister_list_fixture() {
  cat <<'EOF'
{"ok":true,"data":[
  {"id":"cl-other","domain":"other.example.com","status":"Running","createdAt":"2026-03-01T00:00:00Z"},
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","createdAt":"2026-01-01T00:00:00Z"}
],"meta":{"page":1,"perPage":500,"total":3}}
EOF
}

@test "deregister_cluster: resolves the id and DELETEs /api/clusters/<id> (oldest row)" {
  curl() {
    if [[ " $* " == *" DELETE "* ]]; then
      [[ "$*" != *"?domain="* ]] \
        || { echo "query-string DELETE (route does not exist): $*" >&2; return 1; }
      [[ " $* " == *" https://api.kubehz.dev/api/clusters/cl-old "* ]] \
        || { echo "DELETE wrong id/path: $*" >&2; return 1; }
      printf '{"ok":true,"data":{"deleted":true,"id":"cl-old"}}\n200'
    else
      [[ "$*" == *"/api/clusters?perPage=500"* ]] \
        || { echo "unexpected GET: $*" >&2; return 1; }
      _deregister_list_fixture
    fi
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::deregister_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "removed from the platform"
  assert_output --partial "cl-old"
}

@test "deregister_cluster: no row for the domain reports not-registered (no DELETE)" {
  curl() {
    [[ " $* " != *" DELETE "* ]] \
      || { echo "DELETE must not run without a resolved id: $*" >&2; return 1; }
    echo '{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","createdAt":"2026-03-01T00:00:00Z"}]}'
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::deregister_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "no cluster is registered for test.kubehz.dev"
}

@test "deregister_cluster: registry lookup failure returns 1 and never DELETEs" {
  curl() {
    [[ " $* " != *" DELETE "* ]] \
      || { echo "DELETE must not run when the lookup failed: $*" >&2; return 1; }
    return 1
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::deregister_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "was not removed"
}

@test "deregister_cluster: a refused DELETE reports the HTTP status and returns 1" {
  curl() {
    if [[ " $* " == *" DELETE "* ]]; then
      printf '{"ok":false,"data":{"message":"cluster not found"}}\n404'
    else
      _deregister_list_fixture
    fi
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::deregister_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "HTTP 404"
  assert_output --partial "was not removed"
}

# ── deregister_cluster: refuses plain-HTTP (no curl) ─────

@test "deregister_cluster: refuses a plain-HTTP apiUrl (no curl, returns 1)" {
  # The KUBEHZ_TOKEN bearer travels on this URL, and deregister skips
  # validate_config (reachable standalone), so it must re-assert HTTPS itself.
  # The row was not removed, so the function reports failure.
  curl() { echo "curl should not run over plain HTTP" >&2; return 99; }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="http://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::deregister_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "must use HTTPS"
}

# ── status subcommand: access none ───────────────────────

@test "status: shows not registered when access is none" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "" ;;
      '.spec.kubehz.access') echo "null" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  # Create cluster.lok8s.yaml
  touch "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  export domain="test.kubehz.dev"
  export DOMAIN_NAME="test.kubehz.dev"

  run kubehz::status
  assert_success
  assert_output --partial "not registered (access: none)"
}

# ── status subcommand: reads the ROW, not the envelope ───
#
# GET /api/clusters answers the pagination envelope {ok, data: […]} and
# ignores ?domain=. The old code read `.status` off the ENVELOPE, so every
# registered cluster printed "Status: unknown". These tests feed the real
# envelope shape and assert the ROW's fields come out.

_status_yq_mock() {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "https://api.kubehz.dev" ;;
      '.spec.kubehz.access') echo "registered" ;;
      '.spec.kubehz.connectHcloudToken // false') echo "false" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  touch "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
  export domain="test.kubehz.dev"
  export DOMAIN_NAME="test.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"
}

@test "status: prints the row's status + heartbeat from the paginated envelope" {
  _status_yq_mock
  curl() {
    [[ "$*" == *"/api/clusters?perPage=500"* ]] \
      || { echo "unexpected curl: $*" >&2; return 1; }
    # Realistic envelope, newest-first; the oldest row for the domain is the
    # agent-bound one and carries the live status.
    cat <<'EOF'
{"ok":true,"data":[
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","lastHeartbeat":"2026-08-19T10:00:00Z","connected":true,"createdAt":"2026-01-01T00:00:00Z"},
  {"id":"cl-other","domain":"other.example.com","status":"Error","createdAt":"2026-03-01T00:00:00Z"}
],"meta":{"page":1,"perPage":500,"total":3}}
200
EOF
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::status
  assert_success
  assert_output --partial "Status:  Running (id: cl-old)"
  assert_output --partial "Beat:    2026-08-19T10:00:00Z (connected)"
  # The envelope has no .status — "unknown" would mean we read the wrong level.
  refute_output --partial "Status:  unknown"
}

@test "status: no row for the domain prints not registered" {
  _status_yq_mock
  curl() {
    printf '{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","status":"Running","createdAt":"2026-03-01T00:00:00Z"}]}\n200'
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::status
  assert_success
  assert_output --partial "not registered (no cluster for test.kubehz.dev"
}

@test "status: a non-2xx list answer is reported as unknown with the HTTP status" {
  _status_yq_mock
  curl() { printf '{"ok":false}\n401'; }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::status
  assert_success
  assert_output --partial "unknown (HTTP 401"
}

# ── register subcommand: rejects access none ─────────────

@test "register subcommand: rejects when access is none" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "" ;;
      '.spec.kubehz.access') echo "null" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  touch "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  export domain="test.kubehz.dev"
  export DOMAIN_NAME="test.kubehz.dev"
  export LOK8S_SPEC_KIND="Lo"
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  run kubehz::register
  assert_failure
  assert_output --partial "access is 'none'"
}

# ── deregister subcommand: rejects access none ──────────

@test "deregister subcommand: rejects when access is none" {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "" ;;
      '.spec.kubehz.access') echo "null" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  touch "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  export domain="test.kubehz.dev"
  export DOMAIN_NAME="test.kubehz.dev"

  run kubehz::deregister
  assert_failure
  assert_output --partial "access is 'none'"
}

# ── direct_claim: authenticated register (KUBEHZ_TOKEN) ───

@test "direct_claim: registers with the bearer and reports the claimed cluster" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Must send the bearer to the REGISTER endpoint, in the curl config and
  # never on argv.
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  curl() {
    [[ " $* " == *" https://api.kubehz.dev/api/clusters/register "* ]] \
      || { echo "wrong endpoint: $*" >&2; return 1; }
    [[ " $* " != *"khzt_test"* ]] || { echo "bearer on argv: $*" >&2; return 1; }
    curl_capture "$@"
    [[ "${CURL_HEADERS}" == *"Authorization: Bearer khzt_test"* ]] \
      || { echo "missing bearer: ${CURL_HEADERS}" >&2; return 1; }
    echo '{"id":"cl-777","claimed":true}'
  }
  export -f curl

  jq() {
    case "$*" in
      *'.id // empty'*) echo "cl-777" ;;
      *'.claimed // false'*) echo "true" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="khzt_test"

  run kubehz::direct_claim "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" "https://api.kubehz.dev"
  assert_success
  assert_output --partial "registered and claimed to your account"
}

@test "direct_claim: a non-claimed response fails (falls back)" {
  yq() { case "$2" in '.kind // ""') echo "Lo" ;; '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;; *) echo "" ;; esac; }
  export -f yq
  curl() { echo '{"id":"cl-1","claimed":false}'; }
  export -f curl
  jq() { case "$*" in *'.id // empty'*) echo "cl-1" ;; *'.claimed // false'*) echo "false" ;; *) echo "" ;; esac; }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export KUBEHZ_TOKEN="khzt_test"

  run kubehz::direct_claim "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" "https://api.kubehz.dev"
  assert_failure
}

@test "direct_claim: connectHcloudToken connects the hcloud token when writable" {
  yq() { case "$2" in '.kind // ""') echo "Lo" ;; '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;; *) echo "" ;; esac; }
  export -f yq

  # Register → claimed; credentials POST → writable token. The bearer comes
  # in the curl config, never on argv.
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  curl() {
    [[ " $* " != *"khzt_test"* && " $* " != *"hc_test"* ]] || { echo "a credential on argv: $*" >&2; return 1; }
    curl_capture "$@"
    if [[ "${CURL_URL}" == *"/api/credentials"* ]]; then
      [[ "${CURL_HEADERS}" == *"Authorization: Bearer khzt_test"* ]] || { echo "cred missing bearer" >&2; return 1; }
      echo '{"data":{"stored":true,"validation":{"checked":true,"authenticated":true,"writable":true}}}'
    else
      echo '{"id":"cl-9","claimed":true}'
    fi
  }
  export -f curl
  jq() {
    case "$*" in
      *'.id // empty'*) echo "cl-9" ;;
      *'.claimed // false'*) echo "true" ;;
      *'.data.validation.writable // "unknown"'*) echo "true" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export KUBEHZ_TOKEN="khzt_test"
  export HCLOUD_TOKEN="hc_test"
  export LOK8S_KUBEHZ_CONNECT_TOKEN="true"

  run kubehz::direct_claim "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" "https://api.kubehz.dev"
  assert_success
  assert_output --partial "provisioning is enabled"
}

@test "direct_claim: connectHcloudToken warns when a read-only token is connected" {
  yq() { case "$2" in '.kind // ""') echo "Lo" ;; '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;; *) echo "" ;; esac; }
  export -f yq
  curl() {
    if [[ " $* " == *"/api/credentials"* ]]; then
      echo '{"data":{"stored":true,"validation":{"writable":false}}}'
    else
      echo '{"id":"cl-9","claimed":true}'
    fi
  }
  export -f curl
  jq() {
    case "$*" in
      *'.id // empty'*) echo "cl-9" ;;
      *'.claimed // false'*) echo "true" ;;
      *'.data.validation.writable // "unknown"'*) echo "false" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export KUBEHZ_TOKEN="khzt_test" HCLOUD_TOKEN="hc_test" LOK8S_KUBEHZ_CONNECT_TOKEN="true"

  run kubehz::direct_claim "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" "https://api.kubehz.dev"
  assert_success
  assert_output --partial "READ-ONLY"
}

# ── register_cluster: stores the announce's bind secret (B243) ───────────

@test "register_cluster: stores the announce bind secret at clusters/<domain>/.kubehz-bind (0600, no newline)" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  local secret="9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"
  curl() {
    echo '{"id":"cl-001","domain":"test.kubehz.dev","registered":true,"bindSecret":"9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"}'
  }
  export -f curl

  jq() {
    case "$2" in
      '.id // empty') echo "cl-001" ;;
      '.bindSecret // empty') echo "9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  local bind_file="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  assert [ -f "${bind_file}" ]
  run cat "${bind_file}"
  assert_output "${secret}"
  # No trailing newline: the deploy reads the value into a Secret verbatim.
  # stat, not wc -c: GNU wc pads its count with leading spaces.
  run stat -c '%s' "${bind_file}"
  assert_output "64"
  # 0600: nobody but the operator reads the bind secret.
  run stat -c '%a' "${bind_file}"
  assert_output "600"
}

@test "register_cluster: a response with no bind secret writes no .kubehz-bind" {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  curl() { echo '{"id":"cl-001","domain":"test.kubehz.dev","registered":true}'; }
  export -f curl
  jq() {
    case "$2" in
      '.id // empty') echo "cl-001" ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert [ ! -f "${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind" ]
}

# ── register: sends the stored bind secret (B289) ───────────────────────

BIND_STORED="9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"
BIND_NEXT="009c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"

# _register_sandbox <response bindSecret>: the real jq behind a wrapper that
# saves its argv, a Lo spec through the yq stub, and a curl that saves its
# argv and its stdin (the request body). Every register response hands out
# <response bindSecret>.
_register_sandbox() {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  export STUB_NEXT="${1}" STUB_CURL_ARGV="${BATS_TEST_TMPDIR}/curl.argv" STUB_CURL_BODY="${BATS_TEST_TMPDIR}/curl.body"
  export STUB_JQ_ARGV="${BATS_TEST_TMPDIR}/jq.argv"
  : > "${STUB_CURL_ARGV}"
  : > "${STUB_CURL_BODY}"
  : > "${STUB_JQ_ARGV}"
  jq() {
    echo "$*" >> "${STUB_JQ_ARGV}"
    command jq "$@"
  }
  export -f jq
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  curl() {
    echo "$*" >> "${STUB_CURL_ARGV}"
    curl_capture "$@"
    [[ "${CURL_URL}" != *"/api/clusters/register" ]] || printf '%s' "${CURL_BODY}" >> "${STUB_CURL_BODY}"
    case "$*" in
      *"/api/clusters/register"*)
        printf '{"id":"cl-001","registered":true,"claimed":true,"bindSecret":"%s","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-test.kubehz.dev"}}\n' "${STUB_NEXT}" ;;
      *"/v1/ssh_keys?name="*) printf '{"ssh_keys":[]}\n' ;;
      *) printf '{}\n' ;;
    esac
  }
  export -f curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  unset KUBEHZ_TOKEN HCLOUD_TOKEN
}

# _register_mode <legacy|bearer|claim-key>: one register call in that mode.
_register_mode() {
  case "${1}" in
    legacy) kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" ;;
    bearer) KUBEHZ_TOKEN="khzt_test" kubehz::direct_claim "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" "https://api.kubehz.dev" ;;
    claim-key) HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example" kubehz::ensure_claim_key "test.kubehz.dev" "https://api.kubehz.dev" ;;
  esac
}

# _register_base <mode>: the body each mode sends without a bind secret.
_register_base() {
  case "${1}" in
    claim-key) echo '{"domain":"test.kubehz.dev","claimKey":true}' ;;
    *) echo '{"domain":"test.kubehz.dev","fingerprint":"lo:test.kubehz.dev"}' ;;
  esac
}

@test "register: every mode sends the stored bind secret in the body, never on argv, and stores the new one (B289)" {
  local mode bind_file="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  for mode in legacy bearer claim-key; do
    _register_sandbox "${BIND_NEXT}"
    printf %s "${BIND_STORED}" > "${bind_file}"

    run _register_mode "${mode}"
    assert_success
    refute_output --partial "${BIND_STORED}"
    # `command jq`: the test's own jq calls stay out of the argv record.
    run command jq -c . "${STUB_CURL_BODY}"
    assert_output "$(_register_base "${mode}" | command jq -c --arg b "${BIND_STORED}" '. + {bindSecret: $b}')"
    # The register call refuses a redirect off https (F7).
    run grep -c -e " --proto-redir =https -X POST https://api.kubehz.dev/api/clusters/register " "${STUB_CURL_ARGV}"
    assert_output "1"
    run grep -c -e "${BIND_STORED}" "${STUB_CURL_ARGV}" "${STUB_JQ_ARGV}"
    assert_output "${STUB_CURL_ARGV}:0
${STUB_JQ_ARGV}:0"
    run cat "${bind_file}"
    assert_output "${BIND_NEXT}"
  done
}

@test "register: a missing, irregular or malformed stored bind secret adds no bindSecret (B289)" {
  local mode stored bind_file="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  for mode in legacy bearer claim-key; do
    for stored in missing directory unreadable upper short short-newline newline not-hex oversized; do
      [[ "${stored}" != unreadable || "$(id -u)" != 0 ]] || continue  # root reads a 0000 file
      _register_sandbox ""
      rm -rf "${bind_file}"
      case "${stored}" in
        missing) ;;
        unreadable) printf %s "${BIND_STORED}" > "${bind_file}"; chmod 000 "${bind_file}" ;;
        oversized) { printf %s "${BIND_STORED}"; head -c 200000 /dev/zero | tr '\0' a; } > "${bind_file}" ;;
        directory) mkdir -p "${bind_file}" ;;
        upper) printf %s "${BIND_STORED^^}" > "${bind_file}" ;;
        short) printf %s "${BIND_STORED:1}" > "${bind_file}" ;;
        short-newline) printf '%s\n' "${BIND_STORED:1}" > "${bind_file}" ;;
        newline) printf '%s\n' "${BIND_STORED}" > "${bind_file}" ;;
        not-hex) printf %s "g${BIND_STORED:1}" > "${bind_file}" ;;
      esac

      run _register_mode "${mode}"
      assert_success
      # One call, no fallback: the file never breaks the body, and no shell
      # error reaches the output.
      refute_output --partial "jq:"
      refute_output --partial "failed"
      refute_output --partial "denied"
      run command jq -c . "${STUB_CURL_BODY}"
      assert_output "$(_register_base "${mode}")"
    done
  done
  rm -rf "${bind_file}"
}

@test "register: a new bind secret replaces an unreadable stored file (0600, no temporary file left)" {
  [[ "$(id -u)" != 0 ]] || skip "root reads a 0000 file"
  local bind_file="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  _register_sandbox "${BIND_NEXT}"
  printf %s "${BIND_STORED}" > "${bind_file}"
  chmod 000 "${bind_file}"

  run _register_mode legacy
  assert_success
  refute_output --partial "could not store"
  run cat "${bind_file}"
  assert_output "${BIND_NEXT}"
  run stat -c '%a' "${bind_file}"
  assert_output "600"
  run ls -A "${PATH_CLUSTERS}/test.kubehz.dev"
  assert_output ".kubehz-bind"
}

# ── persist_bind_secret: the in-place writer (review B-1, B-2, B-4) ─────

_source_main_for_persist() {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  mkdir -p "${PATH_CLUSTERS}/test.kubehz.dev"
}

@test "persist_bind_secret: a directory in the slot is kept, nothing goes into it, nothing is left next to it" {
  _source_main_for_persist
  local slot="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  mkdir -p "${slot}"

  run kubehz::persist_bind_secret test.kubehz.dev "${BIND_NEXT}"
  assert_success
  assert_output "[warn] kubehz: could not store the bind secret: clusters/test.kubehz.dev/.kubehz-bind is a directory"
  assert [ -d "${slot}" ]
  run ls -A "${slot}"
  assert_output ""
  run ls -A "${PATH_CLUSTERS}/test.kubehz.dev"
  assert_output ".kubehz-bind"
}

@test "persist_bind_secret: a link to a directory in the slot is kept (the same as Go)" {
  _source_main_for_persist
  local slot="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind" target="${BATS_TEST_TMPDIR}/elsewhere"
  mkdir -p "${target}"
  ln -s "${target}" "${slot}"

  run kubehz::persist_bind_secret test.kubehz.dev "${BIND_NEXT}"
  assert_success
  assert_output "[warn] kubehz: could not store the bind secret: clusters/test.kubehz.dev/.kubehz-bind is a directory"
  assert [ -L "${slot}" ]
  run ls -A "${target}"
  assert_output ""
}

# A write that fails after the create (printf overridden in run's subshell
# only, so the bats helpers keep the builtin).
_persist_with_a_failing_write() {
  printf() { return 1; }
  kubehz::persist_bind_secret "$@"
}

@test "persist_bind_secret: a failing write leaves nothing behind and warns" {
  _source_main_for_persist
  local dir="${PATH_CLUSTERS}/test.kubehz.dev"
  printf %s "${BIND_STORED}" > "${dir}/.kubehz-bind"

  run _persist_with_a_failing_write test.kubehz.dev "${BIND_NEXT}"
  assert_success
  assert_output "[warn] kubehz: could not store the bind secret"
  run ls -A "${dir}"
  assert_output ""
}

@test "register_body: jq checks the value again where it uses it (a file that changes after the check)" {
  _source_main_for_persist
  local bind_file="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  printf '%s\n' "${BIND_STORED}" > "${bind_file}"
  # The shared check passes; the file then holds no valid value.
  kubehz::bind_secret_ok() { return 0; }

  # shellcheck disable=SC2016  # jq program text
  run kubehz::register_body test.kubehz.dev '{domain: $d}' --arg d test.kubehz.dev
  assert_success
  run command jq -c . <<<"${output}"
  assert_output '{"domain":"test.kubehz.dev"}'
}

# Plant a link to PLANT_TARGET in the slot right after the unlink, before the
# create: the window between rm and the noclobber create. rm is overridden in
# run's subshell only, and plants once.
_persist_with_a_planted_link() {
  rm() {
    command rm "$@"
    if [[ ! -e "${BATS_TEST_TMPDIR}/planted" ]]; then
      : > "${BATS_TEST_TMPDIR}/planted"
      ln -s "${PLANT_TARGET}" "${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
    fi
  }
  kubehz::persist_bind_secret "$@"
}

@test "persist_bind_secret: a link planted between the unlink and the create is never followed" {
  _source_main_for_persist
  local slot="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  printf %s "${BIND_STORED}" > "${slot}"
  export PLANT_TARGET="${BATS_TEST_TMPDIR}/outside"

  run _persist_with_a_planted_link test.kubehz.dev "${BIND_NEXT}"
  assert_success
  assert_output --partial "[warn] kubehz: could not store the bind secret"
  # The secret never reached the link's target, and the link is gone.
  assert [ ! -e "${BATS_TEST_TMPDIR}/outside" ]
  assert [ ! -L "${slot}" ]
}

@test "persist_bind_secret: a link to a device planted between the unlink and the create is removed, with a warning (N1)" {
  _source_main_for_persist
  local slot="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  printf %s "${BIND_STORED}" > "${slot}"
  # noclobber writes through a link to an existing device; the check after
  # the create must catch it.
  export PLANT_TARGET=/dev/null

  run _persist_with_a_planted_link test.kubehz.dev "${BIND_NEXT}"
  assert_success
  assert_output "[warn] kubehz: could not store the bind secret"
  assert [ ! -L "${slot}" ]
  assert [ ! -e "${slot}" ]
  assert [ -c /dev/null ]
}

@test "bind_secret_ok and register_body: a FIFO in the slot is no bind secret and never blocks (N3)" {
  _source_main_for_persist
  local slot="${PATH_CLUSTERS}/test.kubehz.dev/.kubehz-bind"
  mkfifo "${slot}"
  export -f kubehz::bind_secret_ok kubehz::register_body

  # A read of a FIFO waits for a writer; timeout turns a hang into status 124.
  run timeout 10 bash -c 'kubehz::bind_secret_ok "$1"' _ "${slot}"
  assert_failure 1
  # shellcheck disable=SC2016  # jq program text
  run timeout 10 bash -c 'kubehz::register_body "$1" "{domain: \$d}" --arg d "$1" | command jq -c .' _ test.kubehz.dev
  assert_success
  assert_output '{"domain":"test.kubehz.dev"}'
}

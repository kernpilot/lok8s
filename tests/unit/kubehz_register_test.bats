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
    [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
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
    [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
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
    [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
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
    [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
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
  curl() { echo '{"id":"cl-1","claimed":false}'; [[ " $* " != *'%{http_code}'* ]] || printf '\n200'; }
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
      [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
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
      [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
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

# ── register_cluster: stores the announce's bind secret ──────────────────

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
    [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
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

  curl() { echo '{"id":"cl-001","domain":"test.kubehz.dev","registered":true}'; [[ " $* " != *'%{http_code}'* ]] || printf '\n200'; }
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

# ── register: sends the stored bind secret ──────────────────────────────

BIND_STORED="9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"
BIND_NEXT="009c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"

# _register_sandbox <response bindSecret>: the real jq behind a wrapper that
# saves its argv, a Lo spec through the yq stub, and a curl that saves its
# argv and its stdin (the request body). Every register response hands out
# <response bindSecret>. STUB_FINGERPRINT and STUB_PUBLIC_KEY set the claim
# key of the answer. With STUB_ENV_DUMP set, each jq child writes its
# environment to that file.
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
    [[ -z "${STUB_ENV_DUMP:-}" ]] || env >> "${STUB_ENV_DUMP}"
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
        printf '{"id":"cl-001","registered":true,"claimed":true,"bindSecret":"%s","claimKey":{"publicKey":"%s","fingerprint":"%s","name":"kubehz-claim-test.kubehz.dev"}}\n200' "${STUB_NEXT}" "${STUB_PUBLIC_KEY:-ssh-ed25519 AAAA k}" "${STUB_FINGERPRINT:-aa:bb}" ;;
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

@test "register: every mode sends the stored bind secret in the body, never on argv, and stores the new one" {
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

@test "register: a missing, irregular or malformed stored bind secret adds no bindSecret" {
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

# ── register: 503/429 with Retry-After (#312) ───────────────────────────

# _retry_sandbox <answer>...: a curl stub for the register endpoint that
# answers from a script. An answer is <status>:<Retry-After> (an empty
# Retry-After sends no header, and the value may hold spaces); the n-th call
# gets the n-th answer, and the
# last one repeats. The stub writes the answer's headers to the -D file and
# prints the body, then the status for `-w '\n%{http_code}'`, and returns 0,
# as curl does without --fail. It refuses any --fail option the way curl
# 7.61 (RHEL 8) refuses --fail-with-body: the register must run on that
# curl. The Hetzner Cloud calls of the claim key answer as in
# _register_sandbox. sleep is a stub that records each wait. Each register
# call goes to STUB_CALLS, each wait to STUB_SLEEPS.
_retry_sandbox() {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  STUB_ANSWERS=$(IFS='|'; printf '%s' "$*")
  export STUB_ANSWERS STUB_CALLS="${BATS_TEST_TMPDIR}/register.calls" STUB_SLEEPS="${BATS_TEST_TMPDIR}/sleeps"
  : > "${STUB_CALLS}"
  : > "${STUB_SLEEPS}"
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  curl() {
    local dump="" prev="" a
    for a in "$@"; do
      [[ "${prev}" != -D ]] || dump="${a}"
      prev="${a}"
      case "${a}" in
        --fail*) echo "curl: option ${a}: is unknown" >&2; return 2 ;;
      esac
    done
    curl_capture "$@"
    case "${CURL_URL}" in
      */api/clusters/register) ;;
      *"/v1/ssh_keys?name="*) printf '{"ssh_keys":[]}\n'; return 0 ;;
      *) printf '{}\n'; return 0 ;;
    esac
    echo "${CURL_HEADERS//$'\n'/; }" >> "${STUB_CALLS}"
    local -a answers
    IFS='|' read -r -a answers <<<"${STUB_ANSWERS}"
    local n
    n=$(wc -l < "${STUB_CALLS}")
    (( n <= ${#answers[@]} )) || n=${#answers[@]}
    local status="${answers[n-1]%%:*}" ra="${answers[n-1]#*:}"
    if [[ -n "${dump}" ]]; then
      { printf 'HTTP/1.1 %s X\r\n' "${status}"; [[ -z "${ra}" ]] || printf 'Retry-After: %s\r\n' "${ra}"; printf '\r\n'; } > "${dump}"
    fi
    if (( status >= 400 )); then
      printf '{"statusCode":%s,"data":{"code":"%s"}}' "${status}" "${STUB_CODE-REGISTRATION_UNAVAILABLE}"
    else
      printf '{"id":"cl-001","registered":true,"claimed":true,"claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-test.kubehz.dev"}}'
    fi
    [[ " $* " != *'%{http_code}'* ]] || printf '\n%s' "${status}"
  }
  export -f curl
  sleep() { echo "$1" >> "${STUB_SLEEPS}"; }
  export -f sleep
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  unset KUBEHZ_TOKEN HCLOUD_TOKEN
}

_retry_calls() { wc -l < "${STUB_CALLS}" | tr -d ' '; }

@test "register (#312): a 503 with Retry-After, then 200: one retry after the wait the header asks" {
  local mode
  for mode in legacy bearer claim-key; do
    _retry_sandbox 503:30 200:
    run _register_mode "${mode}"
    assert_success
    assert_output --partial "[warn] kubehz: the platform answered HTTP 503 REGISTRATION_UNAVAILABLE. Try 2 of 3 in 30s."
    case "${mode}" in
      legacy) assert_line "kubehz: cluster 'test.kubehz.dev' registered (pending). Claim it in the dashboard:" ;;
      bearer) assert_line "kubehz: cluster 'test.kubehz.dev' registered and claimed to your account (cl-001)." ;;
      claim-key) assert_line "kubehz: cluster 'test.kubehz.dev' registered (pending). Claim key 'kubehz-claim-test.kubehz.dev' uploaded to your Hetzner Cloud account." ;;
    esac
    refute_output --partial "[error]"
    [ "$(_retry_calls)" = 2 ]
    run cat "${STUB_SLEEPS}"
    assert_output "30"
  done
  # Each try sends the bearer again (a new curl config every try).
  _retry_sandbox 503:1 200:
  KUBEHZ_TOKEN="khzt_test" run kubehz::direct_claim "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" "https://api.kubehz.dev"
  assert_success
  run grep -c "Authorization: Bearer khzt_test" "${STUB_CALLS}"
  assert_output "2"
}

@test "register (#312): still unavailable after 3 tries stops with the code and the command, no fall back" {
  # Bearer and claim key both set: a fall back would reach the claim key and
  # the legacy announce, and add register calls.
  _retry_sandbox 503:30
  export KUBEHZ_TOKEN="khzt_test" HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example"
  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure 1
  assert_output --partial "[error] kubehz: the platform did not register test.kubehz.dev (HTTP 503 REGISTRATION_UNAVAILABLE, 3 tries). Run 'lo kubehz register --domain test.kubehz.dev' when the platform is available again."
  refute_output --partial "falling back"
  refute_output --partial "continue without kubehz integration"
  [ "$(_retry_calls)" = 3 ]
  run cat "${STUB_SLEEPS}"
  assert_output $'30\n30'

  # Each flow alone stops the same way; the helper's own rc is 3.
  local mode
  for mode in legacy claim-key; do
    _retry_sandbox 503:30
    [[ "${mode}" != claim-key ]] || export HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example"
    run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" "lo provision --bootstrap --domain test.kubehz.dev"
    assert_failure 1
    assert_output --partial "(HTTP 503 REGISTRATION_UNAVAILABLE, 3 tries). Run 'lo provision --bootstrap --domain test.kubehz.dev' when"
    [ "$(_retry_calls)" = 3 ]
  done
  _retry_sandbox 503:30
  run _register_mode bearer
  assert_failure 3
}

@test "register (#312): a 429 with Retry-After is tried again" {
  _retry_sandbox 429:5 200:
  STUB_CODE=RATE_LIMITED run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "HTTP 429 RATE_LIMITED. Try 2 of 3 in 5s."
  [ "$(_retry_calls)" = 2 ]
}

@test "register (#312): a 503 without Retry-After is not tried again and falls back as before" {
  _retry_sandbox 503:
  export KUBEHZ_TOKEN="khzt_test"
  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  # The fall back warnings name the refusal (status and code).
  assert_line "[warn] kubehz direct claim failed for test.kubehz.dev (HTTP 503 REGISTRATION_UNAVAILABLE); falling back to the anonymous registration flow"
  assert_line "[warn] kubehz API request failed for test.kubehz.dev (HTTP 503 REGISTRATION_UNAVAILABLE), cluster will continue without kubehz integration"
  refute_output --partial "[error]"
  # The direct claim, then the legacy announce: one try each, no wait.
  [ "$(_retry_calls)" = 2 ]
  [ ! -s "${STUB_SLEEPS}" ]
}

@test "register (#312): a huge Retry-After waits at most 60 s; another form is no Retry-After" {
  _retry_sandbox 503:99999999999999999999
  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure 1
  run cat "${STUB_SLEEPS}"
  assert_output $'60\n60'

  # A leading zero is decimal, not octal.
  _retry_sandbox 503:09 200:
  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  run cat "${STUB_SLEEPS}"
  assert_output "9"

  local ra
  for ra in "Wed, 21 Oct 2026 07:28:00 GMT" "1.5" "-5"; do
    _retry_sandbox "503:${ra}"
    run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
    assert_success
    assert_output --partial "kubehz API request failed"
    [ "$(_retry_calls)" = 1 ]
  done
}

@test "register (#312): a code that is not a code shape is dropped from the lines" {
  _retry_sandbox 503:1
  # Valid JSON: jq decodes the escape, and the shape check drops the value.
  STUB_CODE='\u001b[2JPWNED' run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure 1
  assert_output --partial "(HTTP 503, 3 tries)"
  refute_output --partial "PWNED"
}

@test "register (#312): the register call uses no --fail option (curl 7.61 has no --fail-with-body)" {
  local mode
  for mode in legacy bearer claim-key; do
    _retry_sandbox 200:
    export ARGV_LOG="${BATS_TEST_TMPDIR}/argv.log"
    : > "${ARGV_LOG}"
    run _register_mode "${mode}"
    assert_success
    run grep -c -e "/api/clusters/register" "${ARGV_LOG}"
    assert_output "1"
    run grep -c -e "--fail" "${ARGV_LOG}"
    assert_output "0"
    unset ARGV_LOG
  done
}

@test "register (#312): a claim key refusal names its status in the fall back warning" {
  _retry_sandbox 400:
  export HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example"
  STUB_CODE=BAD_REQUEST run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_line "[warn] kubehz claim-key registration failed for test.kubehz.dev (HTTP 400 BAD_REQUEST); falling back to the legacy fingerprint announce"
  assert_line "[warn] kubehz API request failed for test.kubehz.dev (HTTP 400 BAD_REQUEST), cluster will continue without kubehz integration"
}

@test "register (#312): a transport failure names no status" {
  _retry_sandbox 200:
  curl() { echo "curl: (7) Failed to connect" >&2; return 7; }
  export -f curl
  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_line "curl: (7) Failed to connect"
  assert_line "[warn] kubehz API request failed for test.kubehz.dev, cluster will continue without kubehz integration"
}

@test "register (#312): Ctrl-C during the wait leaves no temporary file" {
  _retry_sandbox 503:30
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  # The wait gets SIGINT, the way Ctrl-C reaches the foreground group.
  sleep() { kill -INT "${BASHPID}"; }
  export -f sleep
  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "$(_retry_calls)" = 1 ]
  run ls -A "${TMPDIR}"
  assert_output ""
}

# ── claim key ownership (a shared Hetzner project) ───────
# register replaces, and deregister deletes, only a claim key with the label
# lok8s.dev/kubehz-claim of the domain; any other key of that name stays, and
# lo says why. The Go twins: internal/kubehz/register_claimkey_test.go.

# The label value of test.kubehz.dev:
# printf %s test.kubehz.dev | sha256sum | cut -c1-32
_CLAIM_MARK=8c1fa30c717e3eab81f42edf29128ca3
_CLAIM_LEGACY_REGISTER="[warn] kubehz: lo does not replace the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: it has no label lok8s.dev/kubehz-claim, so a person or an older lo made it. If an older lo made it for test.kubehz.dev, delete it (Hetzner Console -> Security -> SSH keys), then run 'lo kubehz register --domain test.kubehz.dev' again."
_CLAIM_OTHER_REGISTER="[warn] kubehz: lo does not replace the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: its label lok8s.dev/kubehz-claim does not match test.kubehz.dev, so another cluster made it."
_CLAIM_LEGACY_DEREGISTER="[warn] kubehz: lo does not delete the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: it has no label lok8s.dev/kubehz-claim, so a person or an older lo made it. If an older lo made it for test.kubehz.dev, delete it by hand (Hetzner Console -> Security -> SSH keys)."
_CLAIM_OTHER_DEREGISTER="[warn] kubehz: lo does not delete the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: its label lok8s.dev/kubehz-claim does not match test.kubehz.dev, so another cluster made it."
_CLAIM_UPLOAD="POST https://hc.example/v1/ssh_keys {\"name\":\"kubehz-claim-test.kubehz.dev\",\"public_key\":\"ssh-ed25519 AAAA k\",\"labels\":{\"lok8s.dev/kubehz-claim\":\"${_CLAIM_MARK}\"}}"
_CLAIM_GET="GET https://hc.example/v1/ssh_keys?name=kubehz-claim-test.kubehz.dev"

# _claim_keys [<labels json>]: a GET /v1/ssh_keys?name= answer with the key
# 42 (no argument: no labels field).
_claim_keys() {
  if (( $# == 0 )); then
    printf '{"ssh_keys":[{"id":42,"name":"kubehz-claim-test.kubehz.dev"}]}'
  else
    printf '{"ssh_keys":[{"id":42,"name":"kubehz-claim-test.kubehz.dev","labels":%s}]}' "${1}"
  fi
}

# _claim_key <id> [<labels json>]: one key of a GET /v1/ssh_keys?name=
# answer (an empty id leaves the id out).
_claim_key() {
  local key='"name":"kubehz-claim-test.kubehz.dev"'
  [[ -z "${1}" ]] || key="\"id\":${1},${key}"
  [[ -z "${2:-}" ]] || key="${key},\"labels\":${2}"
  printf '{%s}' "${key}"
}
_CLAIM_OWN="{\"lok8s.dev/kubehz-claim\":\"${_CLAIM_MARK}\"}"
_CLAIM_DELETE42="DELETE https://hc.example/v1/ssh_keys/42"
_CLAIM_NO_MARK="[warn] kubehz: lo needs sha256sum or shasum to mark the claim key 'kubehz-claim-test.kubehz.dev', so it does not change the key in your Hetzner Cloud project."

# _claim_no_sha256: neither sha256sum nor shasum is installed. command -v
# finds neither, and a call of either prints what bash prints for a missing
# command, so a code path that calls one anyway shows in the output.
_claim_no_sha256() {
  command() {
    if [[ "${1:-}" == -v && ( "${2:-}" == sha256sum || "${2:-}" == shasum ) ]]; then
      return 1
    fi
    builtin command "$@"
  }
  sha256sum() { echo "bash: sha256sum: command not found" >&2; return 127; }
  shasum() { echo "bash: shasum: command not found" >&2; return 127; }
}

# The full SHA-256 of test.kubehz.dev; _CLAIM_MARK is its first 32
# characters: printf %s test.kubehz.dev | sha256sum
_CLAIM_SHA256=8c1fa30c717e3eab81f42edf29128ca31f73084c4b2fbd1c2dfa6deb2b6e8205

# _claim_shasum_only: shasum is installed and sha256sum is not (macOS, for
# example). command -v does not find sha256sum, and a call of it prints what
# bash prints for a missing command. The runner can have no shasum (CI has
# none), so shasum is a stub: it prints the shasum format of _CLAIM_SHA256,
# and only for the arguments -a 256 and the exact input test.kubehz.dev (no
# newline) on stdin. Any other call fails, so a fall back that hashes
# another way gives no mark or another mark. Each call of the stub adds
# "shasum <arguments>" to CLAIM_SHASUM_CALLS: a fall back that gets the
# hash from sha256sum on another path (command sha256sum, env sha256sum,
# /usr/bin/sha256sum) gives the same mark, but adds no line.
_claim_shasum_only() {
  export CLAIM_SHASUM_CALLS="${BATS_TEST_TMPDIR}/shasum.calls"
  : > "${CLAIM_SHASUM_CALLS}"
  command() {
    if [[ "${1:-}" == -v && "${2:-}" == sha256sum ]]; then
      return 1
    fi
    builtin command "$@"
  }
  sha256sum() { echo "bash: sha256sum: command not found" >&2; return 127; }
  shasum() {
    local input
    printf 'shasum %s\n' "$*" >> "${CLAIM_SHASUM_CALLS}"
    input=$(cat; printf x)
    [[ "$*" == "-a 256" && "${input%x}" == test.kubehz.dev ]] || return 2
    printf '%s  -\n' "${_CLAIM_SHA256}"
  }
}

# _claim_sandbox <keys json>: the api mints the claim key of test.kubehz.dev
# and lists the registry; GET /v1/ssh_keys?name= answers <keys json>. Each
# Hetzner Cloud call goes to CLAIM_CALLS as "<method> <url>[ <body>]", the
# body as compact JSON. CLAIM_TICKET and CLAIM_PUBLIC_KEY set the claim key
# of the register answer, and CLAIM_PLATFORM_REFUSE=1 makes the platform
# refuse the delete of the cluster. With CLAIM_CONFIGS set, each curl call
# adds its config (the credentials) to that file. With STUB_ENV_DUMP set,
# each curl child writes its environment to that file.
_claim_sandbox() {
  export CLAIM_KEYS="${1}" CLAIM_CALLS="${BATS_TEST_TMPDIR}/hcloud.calls"
  : > "${CLAIM_CALLS}"
  source "${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  curl() {
    curl_capture "$@"
    [[ -z "${CLAIM_CONFIGS:-}" ]] || printf '%s\n' "${CURL_CONFIG}" >> "${CLAIM_CONFIGS}"
    [[ -z "${STUB_ENV_DUMP:-}" ]] || env >> "${STUB_ENV_DUMP}"
    case "${CURL_URL}" in
      */api/clusters/register)
        printf '{"id":"cl-001","registered":true,"claimKey":{"publicKey":"%s","fingerprint":"%s","name":"kubehz-claim-test.kubehz.dev"}}' \
          "${CLAIM_PUBLIC_KEY:-ssh-ed25519 AAAA k}" "${CLAIM_TICKET:-aa:bb}"
        [[ " $* " != *'%{http_code}'* ]] || printf '\n200'
        ;;
      https://hc.example/*)
        local body=""
        [[ -z "${CURL_BODY}" ]] || body=" $(jq -c . <<<"${CURL_BODY}")"
        printf '%s %s%s\n' "${CURL_METHOD:-GET}" "${CURL_URL}" "${body}" >> "${CLAIM_CALLS}"
        if [[ -z "${CURL_METHOD}" ]]; then
          # CLAIM_GET_RC: the list fails the way `curl -f` does (22 on an
          # HTTP error), with no answer.
          [[ -z "${CLAIM_GET_RC:-}" ]] || return "${CLAIM_GET_RC}"
          printf '%s' "${CLAIM_KEYS}"
        fi
        ;;
      */api/clusters\?perPage=500) _deregister_list_fixture ;;
      */api/clusters/*)
        if [[ "${CLAIM_PLATFORM_REFUSE:-0}" == 1 ]]; then
          printf '{"data":{"message":"refused by the fixture"}}\n500'
        else
          printf '{"ok":true}\n200'
        fi
        ;;
    esac
    return 0
  }
  export -f curl curl_capture
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev" KUBEHZ_TOKEN="test-token" \
    HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example"
}

@test "claim key: the mark is the first 32 hex characters of the domain's SHA-256" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  run kubehz::claim_key_mark test.kubehz.dev
  assert_success
  assert_output "${_CLAIM_MARK}"
  run kubehz::claim_key_mark "$(printf 'long.%.0s' {1..30})dev"
  assert_output --regexp '^[0-9a-f]{32}$'
}

@test "claim key: register replaces its own key, and the new key carries the label" {
  _claim_sandbox "$(_claim_keys "{\"lok8s.dev/kubehz-claim\":\"${_CLAIM_MARK}\"}")"
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  refute_output --partial "lo does not replace"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
DELETE https://hc.example/v1/ssh_keys/42
${_CLAIM_UPLOAD}"
}

@test "claim key: register with no key creates it with the label" {
  _claim_sandbox '{"ssh_keys":[]}'
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
${_CLAIM_UPLOAD}"
}

@test "claim key: register refuses a legacy or foreign key without the label" {
  local keys
  for keys in "$(_claim_keys)" "$(_claim_keys '{}')" "$(_claim_keys '{"owner":"alice"}')"; do
    _claim_sandbox "${keys}"
    run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
    assert_failure
    assert_line "${_CLAIM_LEGACY_REGISTER}"
    run cat "${CLAIM_CALLS}"
    assert_output "${_CLAIM_GET}"
  done
}

@test "claim key: register refuses the key of another cluster" {
  _claim_sandbox "$(_claim_keys '{"lok8s.dev/kubehz-claim":"0123456789abcdef0123456789abcdef"}')"
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_failure
  assert_line "${_CLAIM_OTHER_REGISTER}"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

@test "claim key: without sha256sum, shasum -a 256 gives the same mark, and register replaces its own key" {
  _claim_sandbox "$(_claim_keys "${_CLAIM_OWN}")"
  local with_sha256sum
  with_sha256sum=$(kubehz::claim_key_mark test.kubehz.dev)
  assert_equal "${with_sha256sum}" "${_CLAIM_MARK}"
  _claim_shasum_only
  run kubehz::claim_key_mark test.kubehz.dev
  assert_success
  assert_output "${with_sha256sum}"
  # The mark came from shasum: one call, with -a 256.
  run cat "${CLAIM_SHASUM_CALLS}"
  assert_output "shasum -a 256"
  # register finds its own key by this mark, deletes it, and gives the new
  # key the same mark. It calculates the mark two times: for the label of
  # the new key, and to judge the key that it found.
  : > "${CLAIM_SHASUM_CALLS}"
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  refute_output --partial "command not found"
  refute_output --partial "lo does not replace"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
${_CLAIM_DELETE42}
${_CLAIM_UPLOAD}"
  run cat "${CLAIM_SHASUM_CALLS}"
  assert_output "shasum -a 256
shasum -a 256"
}

@test "claim key: register without a SHA-256 tool sends nothing to Hetzner Cloud" {
  _claim_sandbox '{"ssh_keys":[]}'
  _claim_no_sha256
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_failure
  assert_line "${_CLAIM_NO_MARK}"
  refute_output --partial "command not found"
  run cat "${CLAIM_CALLS}"
  assert_output ""
}

@test "claim key: deregister deletes its own key" {
  _claim_sandbox "$(_claim_keys "{\"lok8s.dev/kubehz-claim\":\"${_CLAIM_MARK}\"}")"
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  refute_output --partial "lo does not delete"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
DELETE https://hc.example/v1/ssh_keys/42"
}

@test "claim key: deregister leaves a legacy or foreign key without the label" {
  local keys
  for keys in "$(_claim_keys)" "$(_claim_keys '{}')" "$(_claim_keys '{"owner":"alice"}')"; do
    _claim_sandbox "${keys}"
    run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
    assert_success
    assert_line "${_CLAIM_LEGACY_DEREGISTER}"
    run cat "${CLAIM_CALLS}"
    assert_output "${_CLAIM_GET}"
  done
}

@test "claim key: deregister leaves the key of another cluster" {
  _claim_sandbox "$(_claim_keys '{"lok8s.dev/kubehz-claim":"0123456789abcdef0123456789abcdef"}')"
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_line "${_CLAIM_OTHER_DEREGISTER}"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

@test "claim key: deregister without a SHA-256 tool deletes no key" {
  _claim_sandbox "$(_claim_keys)"
  _claim_no_sha256
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_line "${_CLAIM_NO_MARK}"
  refute_output --partial "command not found"
  refute_output --partial "no label"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

# Nothing to act on (the list fails, the answer does not parse, the key has
# no id): lo deletes nothing. register uploads: a key of that name that the
# list did not show makes the upload fail, and register falls back.

@test "claim key: register uploads when the key list fails, and deletes nothing" {
  _claim_sandbox ''
  export CLAIM_GET_RC=22
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
${_CLAIM_UPLOAD}"
}

@test "claim key: register uploads when the key list does not parse, and deletes nothing" {
  _claim_sandbox '<html>'
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
${_CLAIM_UPLOAD}"
}

@test "claim key: register uploads next to a key without an id, and deletes nothing" {
  _claim_sandbox "{\"ssh_keys\":[$(_claim_key '' "${_CLAIM_OWN}")]}"
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
${_CLAIM_UPLOAD}"
}

# Only the first key of the answer counts.

@test "claim key: register judges the first of two keys: a foreign first key stays, and so does the second" {
  _claim_sandbox "{\"ssh_keys\":[$(_claim_key 42),$(_claim_key 43 "${_CLAIM_OWN}")]}"
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_failure
  assert_line "${_CLAIM_LEGACY_REGISTER}"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

@test "claim key: register judges the first of two keys: an own first key is replaced, the second stays" {
  _claim_sandbox "{\"ssh_keys\":[$(_claim_key 42 "${_CLAIM_OWN}"),$(_claim_key 43)]}"
  run kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
${_CLAIM_DELETE42}
${_CLAIM_UPLOAD}"
}

@test "claim key: deregister deletes nothing when the key list fails" {
  _claim_sandbox ''
  export CLAIM_GET_RC=22
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

@test "claim key: deregister deletes nothing when the key list does not parse" {
  _claim_sandbox '<html>'
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

@test "claim key: deregister deletes nothing for a key without an id" {
  _claim_sandbox "{\"ssh_keys\":[$(_claim_key '' "${_CLAIM_OWN}")]}"
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

@test "claim key: deregister judges the first of two keys: a foreign first key stays, and so does the second" {
  _claim_sandbox "{\"ssh_keys\":[$(_claim_key 42),$(_claim_key 43 "${_CLAIM_OWN}")]}"
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_line "${_CLAIM_LEGACY_DEREGISTER}"
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}"
}

@test "claim key: deregister judges the first of two keys: an own first key goes, the second stays" {
  _claim_sandbox "{\"ssh_keys\":[$(_claim_key 42 "${_CLAIM_OWN}"),$(_claim_key 43)]}"
  run kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  run cat "${CLAIM_CALLS}"
  assert_output "${_CLAIM_GET}
${_CLAIM_DELETE42}"
}

# warn() prints the key name and the domain as they are: a backslash in
# either is one backslash in the warning, and no ESC byte reaches the output.
@test "claim key: a backslash in the key name or the domain prints as typed in the warning" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  run kubehz::claim_key_own '{"ssh_keys":[{"id":42}]}' 'test\e[2J.dev' replace 'kubehz-claim-test\e[2J.dev'
  assert_failure
  assert_output "[warn] kubehz: lo does not replace the SSH key 'kubehz-claim-test\e[2J.dev' in your Hetzner Cloud project: it has no label lok8s.dev/kubehz-claim, so a person or an older lo made it. If an older lo made it for test\e[2J.dev, delete it (Hetzner Console -> Security -> SSH keys), then run 'lo kubehz register --domain test\e[2J.dev' again."
  run kubehz::claim_key_own '{"ssh_keys":[{"id":42,"labels":{"lok8s.dev/kubehz-claim":"0123456789abcdef0123456789abcdef"}}]}' 'test\n.dev' delete 'k\c'
  assert_failure
  assert_output "[warn] kubehz: lo does not delete the SSH key 'k\c' in your Hetzner Cloud project: its label lok8s.dev/kubehz-claim does not match test\n.dev, so another cluster made it."
  run kubehz::claim_key_no_mark 'k\e[2J'
  assert_output "[warn] kubehz: lo needs sha256sum or shasum to mark the claim key 'k\e[2J', so it does not change the key in your Hetzner Cloud project."
  [[ "${output}" != *$'\033'* ]]
}

# ── the claim ticket in CI logs ──────────────────────────
# spinup.yml and provision.yml run `lo provision` with HCLOUD_TOKEN, and a
# public repo shows their job logs to everyone. Any signed-in kubehz user who
# has the claim ticket (the fingerprint of the claim key) can claim the
# cluster, so both workflows pipe the output through hide_claim_ticket, an
# inline function of the run script. This test runs the function of
# spinup.yml on the register output of the bash implementation. The Go twin,
# internal/kubehz/claim_ticket_workflow_test.go, runs it on the output of the
# Go implementation and checks that both workflows have the same function.

# The fixture: a valid ed25519 public key (an ssh-keygen output for these
# tests; the private key does not exist) and the MD5 fingerprint of its key
# blob, the one-time claim ticket. The Go twin has the same values in
# internal/testutil/claimticket.go.
_TICKET="04:35:55:25:a3:3d:b3:9a:bb:d9:10:20:56:b8:92:31"
_CLAIM_KEY_PUBLIC="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILb+3WQ5OfPMd1OnV0p1lHo1cEizaxwddsmWOt+c+dpL claim-ticket-fixture"
# A second ticket in capital hex digits, a ticket without the MD5 shape
# (only the exact match of the two register lines hides it), and a
# server-key fingerprint as the legacy announce prints it.
_TICKET_UPPER="AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:01"
_TICKET_OTHER="SHA256:fake+claim/ticket=without-the-md5-shape"
_SERVER_FP="12:34:56:78:9a:bc:de:f0:12:34:56:78:9a:bc:de:f0"
_FP_TAIL="   (also visible in Hetzner Console -> Security -> SSH keys)"
_TICKET_HIDDEN="<hidden: one-time claim ticket>"

# _no_ticket <where> <text>: fail when <text> holds the fixture ticket in
# any form: with colons and without, in lower and in upper case, and the
# base64 key blob, whose MD5 the ticket is. Each form is a literal of its
# own: none comes from another form or from the code under test, so the
# check cannot share a bug with the format code of the register.
_no_ticket() {
  local where="${1}" text="${2}" form
  local -a forms=(
    "04:35:55:25:a3:3d:b3:9a:bb:d9:10:20:56:b8:92:31"
    "04:35:55:25:A3:3D:B3:9A:BB:D9:10:20:56:B8:92:31"
    "04355525a33db39abbd9102056b89231"
    "04355525A33DB39ABBD9102056B89231"
    "AAAAC3NzaC1lZDI1NTE5AAAAILb+3WQ5OfPMd1OnV0p1lHo1cEizaxwddsmWOt+c+dpL"
  )
  for form in "${forms[@]}"; do
    [[ "${text}" != *"${form}"* ]] || fail "${where} holds the claim ticket (the form ${form})"
  done
}

# _no_ticket_in_files <dir>: _no_ticket on each regular file under <dir>.
_no_ticket_in_files() {
  local f
  while IFS= read -r -d '' f; do
    _no_ticket "the file ${f#"${1}/"}" "$(cat -- "${f}")"
  done < <(find "${1}" -type f -print0)
}

# _ticket_fixture: the register answers of the register sandbox carry the
# fixture key and its ticket. CI and LOK8S_NONINTERACTIVE of the runner are
# unset, so a test runs the same on a CI runner (there CI hides the ticket
# also on a terminal, and so masks a register that prints it on a pipe).
_ticket_fixture() {
  unset CI LOK8S_NONINTERACTIVE
  export STUB_FINGERPRINT="${_TICKET}" STUB_PUBLIC_KEY="${_CLAIM_KEY_PUBLIC}"
}

# _hide_claim_ticket_fn: the definition of hide_claim_ticket in spinup.yml,
# without the 10-space indent of the run block.
_hide_claim_ticket_fn() {
  awk '
    $0 == "          hide_claim_ticket() {" { on = 1 }
    on { print substr($0, 11) }
    on && $0 == "          }" { exit }' "${_PROJECT_ROOT}/.github/workflows/spinup.yml"
}

# _ticket_register <claim-key|announce> [<show_ticket>]: one register_cluster
# call, with HCLOUD_TOKEN (the claim key) or without it (the fingerprint
# announce). <show_ticket> is the argument that --print-token gives.
_ticket_register() {
  local yaml="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  if [[ "${1}" == claim-key ]]; then
    HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example" \
      kubehz::register_cluster "test.kubehz.dev" "${yaml}" "" "${2:-}"
  else
    kubehz::register_cluster "test.kubehz.dev" "${yaml}" "" "${2:-}"
  fi
}

@test "claim ticket: the workflow filter hides it in the register output, and every other line stays" {
  local fn claim announce l hidden other input want
  local -a ins=() wants=()
  fn="$(_hide_claim_ticket_fn)"
  [[ "${fn}" == "hide_claim_ticket() {"*"}" ]] || fail "no hide_claim_ticket function in spinup.yml"
  eval "${fn}"

  # The filter is for the output with the ticket in it: run captures
  # stdout, which is not a terminal, so the register runs with the
  # argument of --print-token.
  _ticket_fixture
  _register_sandbox ""
  run _ticket_register claim-key 1
  assert_success
  claim="${output}"
  [[ "$(grep -cF -- "${_TICKET}" <<<"${claim}")" == 2 ]] ||
    fail "the claim-key registration does not print the ticket on two lines: ${claim}"
  _register_sandbox ""
  run _ticket_register announce
  assert_success
  assert_line "  fingerprint: lo:test.kubehz.dev"
  announce="${output}"

  # Each input line, and the line that the filter must write for it.
  _add() {
    ins+=("${1}")
    wants+=("${2}")
  }
  # The real register lines: with LF and CRLF, with a ticket without the
  # MD5 shape, and behind a log prefix.
  while IFS= read -r l; do
    hidden="${l//"${_TICKET}"/${_TICKET_HIDDEN}}"
    other="${l//"${_TICKET}"/${_TICKET_OTHER}}"
    _add "${l}" "${hidden}"
    _add "${l}"$'\r' "${hidden}"$'\r'
    _add "${other}" "${hidden}"
    _add "${other}"$'\r' "${hidden}"$'\r'
    _add "  [info] ${l}" "  [info] ${hidden}"
  done <<<"${claim}"
  # The announce stays: its "lo:<domain>" has no MD5 shape.
  while IFS= read -r l; do
    _add "${l}" "${l}"
  done <<<"${announce}"
  _add "  fingerprint: ${_SERVER_FP}" "  fingerprint: ${_TICKET_HIDDEN}"
  _add "warning: ${_TICKET}" "warning: ${_TICKET_HIDDEN}"
  _add "two: ${_TICKET} and ${_TICKET_UPPER}." "two: ${_TICKET_HIDDEN} and ${_TICKET_HIDDEN}."
  _add "17 pairs: ${_TICKET}:01" "17 pairs: ${_TICKET_HIDDEN}"
  _add "  fingerprint: "$'\xff'"${_TICKET}${_FP_TAIL}" "  fingerprint: ${_TICKET_HIDDEN}${_FP_TAIL}"
  _add "bytes: "$'\xff'"${_TICKET}" "bytes: "$'\xff'"${_TICKET_HIDDEN}"
  # Lines like ticket lines that are not: a looser match changes them.
  while IFS= read -r l; do
    _add "${l}" "${l}"
  done <<'EOF'
  fingerprint: 11:22   (also visible in Hetzner Console -> Security -> SSH keys) and more
   fingerprint: 11:22   (also visible in Hetzner Console -> Security -> SSH keys)
x       -H 'Content-Type: application/json' -d '{"fingerprint":"11:22"}'
       -H 'Content-Type: application/json' -d '{"fingerprint":"11:22"}' and more
a line without a fingerprint
15 pairs: aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88
one digit short: aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:9
EOF
  # The last line has no newline, and holds a ticket.
  _add "last: ${_TICKET}" "last: ${_TICKET_HIDDEN}"
  input="$(printf '%s\n' "${ins[@]}")"
  want="$(printf '%s\n' "${wants[@]}")"

  # A UTF-8 locale: the function's own LC_ALL=C makes it compare bytes.
  export LC_ALL=C.UTF-8
  run hide_claim_ticket < <(printf '%s' "${input}")
  assert_success
  refute_output --partial "${_TICKET}"
  refute_output --partial "${_TICKET_UPPER}"
  refute_output --partial "${_TICKET_OTHER}"
  refute_output --partial "${_SERVER_FP}"
  assert_output "${want}"
}

# ── The claim ticket on stdout: --print-token, or a terminal ─────────────
# The claim-key registration prints the one-time claim ticket only with
# --print-token (the argument <show_ticket> 1), or when stdout is a
# terminal and the environment is not a CI run (verbose::noninteractive).
# Elsewhere the curl hint gets the placeholder, and the "fingerprint:" line
# names the key in the Hetzner Console and the command that shows a new
# ticket. The Go twin: internal/kubehz/register_ticket_test.go.

_TICKET_HIDDEN_CURL="       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"<hidden: one-time claim ticket>\"}'"
_TICKET_HIDDEN_FP="  fingerprint: <hidden: one-time claim ticket>   (in Hetzner Console -> Security -> SSH keys, key 'kubehz-claim-test.kubehz.dev'; 'lo kubehz register --domain test.kubehz.dev --print-token' registers again and shows a new ticket)"
_TICKET_SHOWN_CURL="       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"${_TICKET}\"}'"
_TICKET_SHOWN_FP="  fingerprint: ${_TICKET}${_FP_TAIL}"
# A string with the shape of an MD5 fingerprint.
_TICKET_SHAPE='[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){15}'

# _ticket_hidden: the last run ended well and has the two placeholder lines
# in its output, no form of the ticket is in its output (stdout and stderr)
# or in a file of the test, and the claim key went to Hetzner Cloud.
_ticket_hidden() {
  assert_success
  _no_ticket "the output" "${output}"
  refute_output --regexp "${_TICKET_SHAPE}"
  assert_output --partial "${_TICKET_HIDDEN_CURL}"
  assert_output --partial "${_TICKET_HIDDEN_FP}"
  _no_ticket_in_files "${BATS_TEST_TMPDIR}"
  grep -qF -- "-X POST https://hc.example/v1/ssh_keys" "${STUB_CURL_ARGV}" ||
    fail "the claim key was not uploaded"
}

# _ticket_shown: the last run ended well and has both ticket lines in its
# output, and no placeholder.
_ticket_shown() {
  assert_success
  assert_output --partial "${_TICKET_SHOWN_CURL}"
  assert_output --partial "${_TICKET_SHOWN_FP}"
  refute_output --partial "${_TICKET_HIDDEN}"
}

@test "claim ticket: a stdout that is not a terminal gets no ticket in any form, and the lines say where to read it" {
  _ticket_fixture
  _register_sandbox ""
  run _ticket_register claim-key
  _ticket_hidden
}

@test "claim ticket: --print-token shows it on a stdout that is not a terminal, also in a CI run" {
  _ticket_fixture
  _register_sandbox ""
  run _ticket_register claim-key 1
  _ticket_shown
  CI=true run _ticket_register claim-key 1
  _ticket_shown
}

# Only the argument 1 shows the ticket. Another value does not, and it is
# never evaluated as arithmetic.
@test "claim ticket: a show_ticket other than 1 shows no ticket" {
  _ticket_fixture
  _register_sandbox ""
  local v
  for v in 0 true ' 1' 'x[$(echo PWNED >&2)]'; do
    run _ticket_register claim-key "${v}"
    _ticket_hidden
    refute_output --partial "PWNED"
  done
}

# _kubehz_register_sandbox: the spec of test.kubehz.dev for kubehz::register
# (access registered, through the yq stub), and HCLOUD_TOKEN for the claim
# key.
_kubehz_register_sandbox() {
  yq() {
    case "$2" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "https://api.kubehz.dev" ;;
      '.spec.kubehz.access') echo "registered" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  touch "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
  export HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example"
}

# The argsh parse of the flag is the parity harness's (it runs the real lo):
# here :args is a stub that sets what the parse of --print-token sets.
@test "claim ticket: kubehz::register takes --print-token to the claim key" {
  _ticket_fixture
  _register_sandbox ""
  _kubehz_register_sandbox
  declare -f kubehz::register | grep -qF "'print-token|:+'" ||
    fail "kubehz::register has no print-token flag"
  :args() { domain="test.kubehz.dev" print_token=1; }
  run kubehz::register
  _ticket_shown
  :args() { domain="test.kubehz.dev"; }
  run kubehz::register
  _ticket_hidden
}

# print_token is a local of each entry point, and each one passes the flag
# down as an argument. A print_token in the environment, or in the scope of
# a caller, shows nothing.
@test "claim ticket: an ambient print_token=1 shows no ticket (register_cluster, kubehz::register)" {
  _ticket_fixture
  _register_sandbox ""
  export print_token=1
  run _ticket_register claim-key
  _ticket_hidden
  _ticket_in_a_caller() {
    local print_token=1
    _ticket_register claim-key
  }
  run _ticket_in_a_caller
  _ticket_hidden
  _kubehz_register_sandbox
  :args() { domain="test.kubehz.dev"; }
  run kubehz::register
  _ticket_hidden
}

# _dispatch_sandbox: the real provision::dispatch of libs/provision, with a
# lo driver that does nothing and a kubehz config with access registered,
# then the real register_cluster in the register sandbox, with HCLOUD_TOKEN
# (the claim key). main::up of lo and main::provision of libs/provision call
# it. :args sets what the parse of --print-token sets when ARGS_PRINT_TOKEN
# is 1 (the argsh parse is the parity harness's).
_dispatch_sandbox() {
  _register_sandbox ""
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  eval "$(sed -n '/^main::up()/,/^}/p' "${_PROJECT_ROOT}/.lok8s/lo")"
  declare -f main::up >/dev/null || fail "no main::up in lo"
  mkdir -p "${PATH_LOK8S}/drivers/lo"
  printf 'driver::provision() { :; }\n' > "${PATH_LOK8S}/drivers/lo/main"
  touch "${PATH_CLUSTERS}/test.kubehz.dev/cluster.lok8s.yaml"
  provision::resolve_spec() {
    LOK8S_SPEC_FILE="${PATH_CLUSTERS}/${1}/cluster.lok8s.yaml"
    LOK8S_SPEC_KIND="cluster"
  }
  provision::load_provider_creds() { :; }
  provision::read_kind() { echo lo; }
  provision::confirm_infra() { :; }
  kubehz::read_config() { export LOK8S_KUBEHZ_ACCESS=registered; }
  kubehz::validate_config() { :; }
  :args() {
    domain="test.kubehz.dev"
    [[ "${ARGS_PRINT_TOKEN:-0}" != 1 ]] || print_token=1
  }
  _lo_header() { :; }
  tilt::up() { :; }
  bootstrap::_flag_given() { return 1; }
  bootstrap::mode_check() { return 0; }
  bootstrap::_dry_run_exit() { return "${1}"; }
  export HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example"
}

@test "claim ticket: lo up and lo provision take --print-token through the dispatch to the register, and an ambient print_token does not" {
  _ticket_fixture
  _dispatch_sandbox
  local entry
  for entry in main::up main::provision; do
    ARGS_PRINT_TOKEN=1 run "${entry}"
    _ticket_shown
    run "${entry}"
    _ticket_hidden
    print_token=1 run "${entry}"
    _ticket_hidden
  done
  # The call of lo recover: the domain alone (next test).
  print_token=1 run provision::dispatch test.kubehz.dev
  _ticket_hidden
}

@test "claim ticket: lo recover calls the dispatch with the domain alone, so it shows no ticket" {
  run grep -nE '^[^#]*provision::dispatch( |$)' "${_PROJECT_ROOT}/.lok8s/libs/recover"
  assert_success
  assert_equal "${#lines[@]}" 1
  assert_output --regexp '^[0-9]+:  recover::_tty_or_null provision::dispatch "\$\{_RECOVER_DOMAIN\}"$'
}

# _ticket_on_a_terminal [<name>=<value>...]: ensure_claim_key in a child
# bash on a terminal (script(1)), with the given environment and without
# the CI and LOK8S_NONINTERACTIVE of the runner. script runs the command
# with $SHELL, so SHELL is this bash. The child gets the stubs that the
# sandbox exports, and sources the libs again.
_ticket_on_a_terminal() {
  env -u CI -u LOK8S_NONINTERACTIVE "$@" SHELL="${BASH}" script -qec 'source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh" && source "${_PROJECT_ROOT}/.lok8s/utils/http.sh" && source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main" && [[ -t 1 ]] && HCLOUD_TOKEN=hc_test HCLOUD_API_BASE=https://hc.example kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev' /dev/null
}

@test "claim ticket: a terminal gets the ticket" {
  command -v script >/dev/null || skip "no script(1) for a terminal"
  _ticket_fixture
  _register_sandbox ""
  export _PROJECT_ROOT
  run _ticket_on_a_terminal
  _ticket_shown
  run _ticket_on_a_terminal CI=false
  _ticket_shown
}

# A runner can give the job a pty, and a terminal recording keeps what the
# terminal shows: a CI run (CI set and not "false") or LOK8S_NONINTERACTIVE
# hides the ticket on a terminal too.
@test "claim ticket: a terminal in a CI run, or with LOK8S_NONINTERACTIVE, gets no ticket" {
  command -v script >/dev/null || skip "no script(1) for a terminal"
  _ticket_fixture
  _register_sandbox ""
  export _PROJECT_ROOT
  local e
  for e in CI=true CI=1 LOK8S_NONINTERACTIVE=1 "LOK8S_NONINTERACTIVE=1 CI=false"; do
    # shellcheck disable=SC2086 # each word is one assignment
    run _ticket_on_a_terminal ${e}
    _ticket_hidden
  done
}

# The rule of the environment. The Go twin: TestNonInteractive in
# internal/ui/interactive_test.go.
@test "verbose::noninteractive: LOK8S_NONINTERACTIVE set, or CI set and not false" {
  local c env want rc
  for c in ":1" "CI=true:0" "CI=1:0" "CI=false:1" "CI=:1" "LOK8S_NONINTERACTIVE=1:0" "LOK8S_NONINTERACTIVE=1 CI=false:0"; do
    env="${c%:*}" want="${c##*:}" rc=0
    # shellcheck disable=SC2086 # each word is one assignment
    env -u CI -u LOK8S_NONINTERACTIVE ${env} "${BASH}" -c 'source "${1}" && verbose::noninteractive' _ "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh" || rc=$?
    [[ "${rc}" == "${want}" ]] || fail "verbose::noninteractive with '${env}': rc ${rc}, want ${want}"
  done
}

# _traced_claim_key: the claim key under set -x, with the Hetzner token in
# the environment before the trace starts.
_traced_claim_key() {
  local rc=0
  export HCLOUD_TOKEN="hc_test" HCLOUD_API_BASE="https://hc.example"
  set -x
  kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev || rc=$?
  set +x
  return "${rc}"
}

# Without the guard, the trace shows the response and the fingerprint, and
# the curl config line with the Hetzner token. The guard also gives the
# trace back after the function: the caller's next line is traced.
@test "claim ticket: set -x in ensure_claim_key prints neither the ticket nor the Hetzner token" {
  _ticket_fixture
  _register_sandbox ""
  run _traced_claim_key
  assert_success
  # The trace is on before and after the function.
  assert_output --partial "kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev"
  assert_output --regexp $'\n[+]+ set \\+x'
  _no_ticket "the trace" "${output}"
  refute_output --regexp "${_TICKET_SHAPE}"
  refute_output --partial "hc_test"
  assert_line "${_TICKET_HIDDEN_FP}"
}

# ── The guard of register_cluster and deregister_cluster ─────────────────
# Both functions expand KUBEHZ_TOKEN and HCLOUD_TOKEN, and hold the platform
# answers and the key list of Hetzner Cloud (a public key and its
# fingerprint, the ticket). The two guard lines (utils/keysonly.sh) keep a
# caller's trace (set -x, and a DEBUG, RETURN or ERR trap with set -T or
# set -E) off them, and `local +x` keeps them out of the environment of a
# child.

# The tokens of these tests: a trace that holds one fails.
_TRACE_KUBEHZ_TOKEN="khzt_traceFixtureKubehzToken7f3a9c"
_TRACE_HCLOUD_TOKEN="hcloudTraceFixtureToken5b8e2d"

# _trace_sandbox: the claim sandbox with the fixture key and ticket, both
# tokens, a Lo spec through the yq stub (the direct claim of KUBEHZ_TOKEN
# reads the server fingerprint from it), and a key list as Hetzner Cloud
# answers it: the key 42 of this cluster, with its fingerprint, its public
# key and the label. Each curl call adds its config to CLAIM_CONFIGS, so a
# test can show that the tokens were in flight.
_trace_sandbox() {
  yq() {
    case "${2:-}" in
      '.kind // ""') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
  export CLAIM_TICKET="${_TICKET}" CLAIM_PUBLIC_KEY="${_CLAIM_KEY_PUBLIC}" CLAIM_CONFIGS="${BATS_TEST_TMPDIR}/curl.configs"
  : > "${CLAIM_CONFIGS}"
  _claim_sandbox "{\"ssh_keys\":[{\"id\":42,\"name\":\"kubehz-claim-test.kubehz.dev\",\"fingerprint\":\"${_TICKET}\",\"public_key\":\"${_CLAIM_KEY_PUBLIC}\",\"labels\":${_CLAIM_OWN}}]}"
  export KUBEHZ_TOKEN="${_TRACE_KUBEHZ_TOKEN}" HCLOUD_TOKEN="${_TRACE_HCLOUD_TOKEN}"
}

# _tokens_in_flight: both tokens went to curl in a config.
_tokens_in_flight() {
  grep -qF -- "Bearer ${_TRACE_KUBEHZ_TOKEN}" "${CLAIM_CONFIGS}" || fail "KUBEHZ_TOKEN was not sent"
  grep -qF -- "Bearer ${_TRACE_HCLOUD_TOKEN}" "${CLAIM_CONFIGS}" || fail "HCLOUD_TOKEN was not sent"
}

# _no_trace_secret: no file of the runs of assert_no_trace_leak holds a
# token or a form of the ticket.
_no_trace_secret() {
  local d n=0
  for d in "${BATS_TEST_TMPDIR}"/trace.*; do
    [[ -d "${d}" ]] || continue
    n=$((n + 1))
    ! grep -rqF -e "${_TRACE_KUBEHZ_TOKEN}" -e "${_TRACE_HCLOUD_TOKEN}" -- "${d}" ||
      fail "a file of the trace runs holds a token: $(grep -rlF -e "${_TRACE_KUBEHZ_TOKEN}" -e "${_TRACE_HCLOUD_TOKEN}" -- "${d}")"
    _no_ticket_in_files "${d}"
  done
  (( n > 0 )) || fail "no trace run"
}

@test "trace: register_cluster under a caller's trace or trap shows no token, no ticket and no public key" {
  _trace_sandbox
  assert_no_trace_leak "${_TRACE_KUBEHZ_TOKEN}" kubehz::register_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  _no_trace_secret
  # The values were in flight: both tokens, the upload of the public key,
  # and the key list (the own key 42 was replaced).
  _tokens_in_flight
  grep -qF -- "\"public_key\":\"${_CLAIM_KEY_PUBLIC}\"" "${CLAIM_CALLS}" || fail "the claim key was not uploaded"
  grep -qxF -- "${_CLAIM_DELETE42}" "${CLAIM_CALLS}" || fail "the own key 42 was not replaced"
}

@test "trace: deregister_cluster under a caller's trace or trap shows no token, no ticket and no public key, also when the platform refuses the delete" {
  _trace_sandbox
  export CLAIM_PLATFORM_REFUSE=1
  _deregister_accepted() { CLAIM_PLATFORM_REFUSE=0 kubehz::deregister_cluster "$@"; }
  # The sixth run: the refused delete under errexit and errtrace.
  local -a trace_fail=(kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml")
  assert_no_trace_leak "${_TRACE_KUBEHZ_TOKEN}" _deregister_accepted test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  _no_trace_secret
  _tokens_in_flight
  grep -qxF -- "${_CLAIM_DELETE42}" "${CLAIM_CALLS}" || fail "the own key 42 was not deleted"
  grep -qF -- "delete refused for test.kubehz.dev" "${BATS_TEST_TMPDIR}"/trace.*/stderr.errfail ||
    fail "the platform did not refuse the delete in the errexit run"
}

# _env_and_trap_probe <command...>: run the command with set -a, functrace
# and errtrace, a DEBUG, RETURN and ERR trap that adds the names of the
# values (declare -p of response, keys, fingerprint and public_key) to
# TRAP_LOG, and those names exported by the caller with values of its own.
# Each curl child of the claim sandbox writes its environment to
# STUB_ENV_DUMP.
_env_and_trap_probe() {
  local rc=0
  export response=caller-response keys=caller-keys fingerprint=caller-fingerprint public_key=caller-public-key
  export STUB_ENV_DUMP="${BATS_TEST_TMPDIR}/env.dump" TRAP_LOG="${BATS_TEST_TMPDIR}/trap.log"
  : > "${STUB_ENV_DUMP}"
  : > "${TRAP_LOG}"
  trap 'declare -p response keys fingerprint public_key >> "${TRAP_LOG}" 2>/dev/null' DEBUG RETURN ERR
  set -aTE
  "$@" || rc=$?
  set +aTE
  trap - DEBUG RETURN ERR
  return "${rc}"
}

# _probe_clean: the children and the traps of the last probe ran, saw the
# values of the caller, and got no form of the ticket. The environment of a
# child also holds the inputs of the claim sandbox (CLAIM_KEYS, CLAIM_TICKET
# and CLAIM_PUBLIC_KEY, exported for its stub): the check skips those lines.
_probe_clean() {
  local dump="${BATS_TEST_TMPDIR}/env.dump" log="${BATS_TEST_TMPDIR}/trap.log"
  [[ -s "${dump}" ]] || fail "no child wrote its environment"
  grep -qF "caller-response" "${log}" || fail "the traps did not run"
  _no_ticket "the environment of a child" "$(grep -vE '^(CLAIM_KEYS|CLAIM_TICKET|CLAIM_PUBLIC_KEY)=' "${dump}")"
  _no_ticket "the output of the traps" "$(cat "${log}")"
}

# register_cluster declares its own value names `local +x` before it calls
# ensure_claim_key, so a name of ensure_claim_key takes the attributes of
# that local, not of the caller's export. The direct call shows the guard of
# ensure_claim_key alone.
@test "env and traps: set -a, an exported name and a caller's DEBUG, RETURN or ERR trap get no value of register_cluster, ensure_claim_key or deregister_cluster" {
  _trace_sandbox
  run _env_and_trap_probe kubehz::register_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  _probe_clean
  grep -qF -- "\"public_key\":\"${_CLAIM_KEY_PUBLIC}\"" "${CLAIM_CALLS}" || fail "the claim key was not uploaded"
  : > "${CLAIM_CALLS}"
  run _env_and_trap_probe kubehz::ensure_claim_key test.kubehz.dev https://api.kubehz.dev
  assert_success
  _probe_clean
  grep -qF -- "\"public_key\":\"${_CLAIM_KEY_PUBLIC}\"" "${CLAIM_CALLS}" || fail "the claim key was not uploaded by ensure_claim_key"
  export CLAIM_PLATFORM_REFUSE=1
  run _env_and_trap_probe kubehz::deregister_cluster test.kubehz.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  _probe_clean
  assert_output --partial "delete refused for test.kubehz.dev"
}

# _kept <command...>: run the command under a caller's set -xTEa and a
# DEBUG, RETURN and ERR trap. Then print its return code, and whether the
# options and the traps after it are the ones before it.
_kept() {
  local rc=0 before after tb ta
  trap ':' DEBUG
  trap ':' RETURN
  trap ':' ERR
  set -xTEa
  before="$-" tb="$(trap -p DEBUG RETURN ERR)"
  "$@" >/dev/null 2>&1 || rc=$?
  after="$-" ta="$(trap -p DEBUG RETURN ERR)"
  { set +xTEa; } 2>/dev/null
  trap - DEBUG RETURN ERR
  printf 'rc %s\n' "${rc}"
  [[ "${before}" != "${after}" ]] || printf 'options kept\n'
  [[ "${tb}" != "${ta}" || -z "${ta}" ]] || printf 'traps kept\n'
}

# _kept_readonly <command...>: _kept with a readonly global response: the
# local of the function cannot be declared, so it returns 1.
_kept_readonly() {
  declare -gr response=caller-response
  _kept "$@"
}

@test "guard: a success and a failure of register_cluster and deregister_cluster give the caller its options and traps back" {
  _trace_sandbox
  local yaml="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  run _kept kubehz::register_cluster test.kubehz.dev "${yaml}"
  assert_success
  assert_line "rc 0"
  assert_line "options kept"
  assert_line "traps kept"
  run _kept_readonly kubehz::register_cluster test.kubehz.dev "${yaml}"
  assert_line "rc 1"
  assert_line "options kept"
  assert_line "traps kept"
  export CLAIM_PLATFORM_REFUSE=1
  run _kept kubehz::deregister_cluster test.kubehz.dev "${yaml}"
  assert_line "rc 1"
  assert_line "options kept"
  assert_line "traps kept"
}

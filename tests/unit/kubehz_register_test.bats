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
      '.kind') echo "Lo" ;;
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
      '.kind') echo "KubeOne" ;;
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
      '.kind') echo "Capi" ;;
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
      '.kind') echo "UnknownKind" ;;
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
      '.kind') echo "Lo" ;;
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
      '.kind') echo "Lo" ;;
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
      '.kind') echo "Lo" ;;
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
      '.kind') echo "Lo" ;;
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
      '.kind') echo "Lo" ;;
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
      '.kind') echo "UnknownKind" ;;
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
      '.kind') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Must send the bearer to the REGISTER endpoint.
  curl() {
    [[ " $* " == *" https://api.kubehz.dev/api/clusters/register "* ]] \
      || { echo "wrong endpoint: $*" >&2; return 1; }
    [[ " $* " == *"Authorization: Bearer khzt_test"* ]] \
      || { echo "missing bearer: $*" >&2; return 1; }
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
  yq() { case "$2" in '.kind') echo "Lo" ;; '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;; *) echo "" ;; esac; }
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
  yq() { case "$2" in '.kind') echo "Lo" ;; '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;; *) echo "" ;; esac; }
  export -f yq

  # Register → claimed; credentials POST → writable token.
  curl() {
    if [[ " $* " == *"/api/credentials"* ]]; then
      [[ " $* " == *"Authorization: Bearer khzt_test"* ]] || { echo "cred missing bearer" >&2; return 1; }
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
  yq() { case "$2" in '.kind') echo "Lo" ;; '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;; *) echo "" ;; esac; }
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

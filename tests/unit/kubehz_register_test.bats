#!/usr/bin/env bats
# kubehz_register_test.bats — unit tests for kubehz registration and deregistration

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  # The claim-key path gates on HCLOUD_TOKEN — the host/CI environment may
  # export one, so unset it here; claim-key tests set it explicitly.
  unset HCLOUD_TOKEN

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

# ── register_cluster: server-key fallback (no HCLOUD_TOKEN) ─

@test "register_cluster: no HCLOUD_TOKEN falls back to server key and prints the fingerprint" {
  yq() {
    case "$2" in
      '.kind') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Assert the producer hits the REGISTER endpoint (not claims/verify) and
  # capture the JSON body (-d) so we can inspect the claimable flag.
  curl() {
    [[ " $* " == *" https://api.kubehz.dev/api/clusters/register "* ]] \
      || { echo "curl wrong endpoint: $*" >&2; return 1; }
    local prev="" a
    for a in "$@"; do
      [[ "${prev}" == "-d" ]] && printf '%s' "${a}" > "${BATS_TEST_TMPDIR}/register-payload.json"
      prev="${a}"
    done
    echo '{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}'
  }
  export -f curl

  # Delegate the payload build (jq -n …) to real jq so the assertion sees the
  # actual wire body; the response extraction stays mocked.
  jq() {
    case "$*" in
      *'.id // empty'*) echo "cl-001" ;;
      *) command jq "$@" ;;
    esac
  }
  export -f jq

  # Must NOT be touched without HCLOUD_TOKEN.
  hcloud() { echo "hcloud must not run without HCLOUD_TOKEN: $*" >&2; return 99; }
  export -f hcloud

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  # Fallback keeps the old handshake: the (public) fingerprint is printed,
  # plus the pointer at the token-verification claim path.
  assert_output --partial "Claim it in the dashboard"
  assert_output --partial "fingerprint: lo:test.kubehz.dev"
  assert_output --partial "token-verification path"

  # Server-key fallback: the fingerprint may be PUBLIC, so the registration must
  # NOT be marked equality-claimable — the payload carries no claimable flag.
  run cat "${BATS_TEST_TMPDIR}/register-payload.json"
  refute_output --partial "claimable"
  assert_output --partial '"fingerprint": "lo:test.kubehz.dev"'
}

# ── register_cluster: claim-key path (HCLOUD_TOKEN present) ─

@test "register_cluster: claim-key path uploads kubehz-claim-<domain> and never prints the fingerprint" {
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
    local prev="" a
    for a in "$@"; do
      [[ "${prev}" == "-d" ]] && printf '%s' "${a}" > "${BATS_TEST_TMPDIR}/register-payload.json"
      prev="${a}"
    done
    echo '{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}'
  }
  export -f curl

  jq() {
    case "$*" in
      *'.id // empty'*) echo "cl-001" ;;
      *'.fingerprint // empty'*) echo "" ;;
      *) command jq "$@" ;;  # payload build (jq -n …) → real JSON so the test can assert the wire body
    esac
  }
  export -f jq

  # No existing key -> create; the upload MUST carry the ticket name.
  hcloud() {
    case "$1 $2" in
      "ssh-key describe") return 1 ;;
      "ssh-key create")
        [[ " $* " == *" --name kubehz-claim-test.kubehz.dev "* ]] \
          || { echo "hcloud create wrong key name: $*" >&2; return 1; }
        [[ " $* " == *"--public-key-from-file"* ]] \
          || { echo "hcloud create without public key file: $*" >&2; return 1; }
        return 0 ;;
      *) echo "unexpected hcloud call: $*" >&2; return 1 ;;
    esac
  }
  export -f hcloud

  # Generation call creates the key files; fingerprint call must use -E md5.
  ssh-keygen() {
    if [[ " $* " == *" -t ed25519 "* ]]; then
      local -a args=("$@"); local i f=""
      for ((i = 0; i < ${#args[@]}; i++)); do
        [[ "${args[i]}" == "-f" ]] && f="${args[i + 1]}"
      done
      [[ -n "${f}" ]] || { echo "ssh-keygen -t without -f: $*" >&2; return 1; }
      : > "${f}"
      : > "${f}.pub"
      return 0
    fi
    [[ " $* " == *" -E md5 "* ]] || { echo "ssh-keygen called without -E md5: $*" >&2; return 1; }
    echo "256 MD5:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00 kubehz-claim-test.kubehz.dev (ED25519)"
  }
  export -f ssh-keygen

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export HCLOUD_TOKEN="test-hcloud-token"
  # Private temp dir for the ephemeral keypair — lets us assert the discard.
  export TMPDIR="${BATS_TEST_TMPDIR}/claimtmp"
  mkdir -p "${TMPDIR}"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registered (pending)"
  assert_output --partial "Hetzner Cloud console"
  assert_output --partial "kubehz-claim-test.kubehz.dev"
  assert_output --partial "/claim"
  # The fingerprint is now the claim secret — it must NOT appear in output.
  refute_output --partial "11:22:33"
  refute_output --partial "fingerprint:"

  # The claim-key path marks the registration equality-claimable: the register
  # payload carries `claimable: true` (ONLY this path sends the flag).
  run cat "${BATS_TEST_TMPDIR}/register-payload.json"
  assert_output --partial '"claimable": true'

  # The ephemeral private key is DISCARDED (temp dir removed).
  run find "${BATS_TEST_TMPDIR}/claimtmp" -mindepth 1
  assert_output ""
}

@test "register_cluster: claim-key path reuses an existing kubehz-claim key (no rotate, no print)" {
  yq() {
    case "$2" in
      '.kind') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  curl() {
    local prev="" a
    for a in "$@"; do
      [[ "${prev}" == "-d" ]] && printf '%s' "${a}" > "${BATS_TEST_TMPDIR}/register-payload.json"
      prev="${a}"
    done
    echo '{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}'
  }
  export -f curl

  jq() {
    case "$*" in
      *'.id // empty'*) echo "cl-001" ;;
      *'.fingerprint // empty'*) echo "ee:dd:cc:bb:aa:99:88:77:66:55:44:33:22:11:00:ff" ;;
      *) command jq "$@" ;;
    esac
  }
  export -f jq

  # Existing key: describe succeeds; create/keygen must NOT run (no rotation —
  # rotating would invalidate the ticket the user already read).
  hcloud() {
    case "$1 $2" in
      "ssh-key describe")
        [[ "$3" == "kubehz-claim-test.kubehz.dev" ]] \
          || { echo "describe wrong key name: $*" >&2; return 1; }
        echo '{"name":"kubehz-claim-test.kubehz.dev","fingerprint":"ee:dd:cc:bb:aa:99:88:77:66:55:44:33:22:11:00:ff"}'
        return 0 ;;
      *) echo "unexpected hcloud call (must reuse, not create): $*" >&2; return 1 ;;
    esac
  }
  export -f hcloud

  ssh-keygen() { echo "ssh-keygen must not run when the claim key exists: $*" >&2; return 1; }
  export -f ssh-keygen

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export HCLOUD_TOKEN="test-hcloud-token"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registered (pending)"
  assert_output --partial "kubehz-claim-test.kubehz.dev"
  refute_output --partial "ee:dd:cc"
  refute_output --partial "fingerprint:"

  # Reuse is still the claim-key path → the registration is equality-claimable.
  run cat "${BATS_TEST_TMPDIR}/register-payload.json"
  assert_output --partial '"claimable": true'
}

@test "register_cluster: claim-key provisioning failure falls back to the server key" {
  yq() {
    case "$2" in
      '.kind') echo "Lo" ;;
      '.spec.cluster.domain // ""') echo "test.kubehz.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  curl() {
    local prev="" a
    for a in "$@"; do
      [[ "${prev}" == "-d" ]] && printf '%s' "${a}" > "${BATS_TEST_TMPDIR}/register-payload.json"
      prev="${a}"
    done
    echo '{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}'
  }
  export -f curl

  jq() {
    case "$*" in
      *'.id // empty'*) echo "cl-001" ;;
      *'.fingerprint // empty'*) echo "" ;;
      *) command jq "$@" ;;
    esac
  }
  export -f jq

  # hcloud is present but broken (revoked token, API down, …).
  hcloud() { return 1; }
  export -f hcloud

  ssh-keygen() {
    if [[ " $* " == *" -t ed25519 "* ]]; then
      local -a args=("$@"); local i f=""
      for ((i = 0; i < ${#args[@]}; i++)); do
        [[ "${args[i]}" == "-f" ]] && f="${args[i + 1]}"
      done
      [[ -n "${f}" ]] && { : > "${f}"; : > "${f}.pub"; }
      return 0
    fi
    echo "256 MD5:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00 x (ED25519)"
  }
  export -f ssh-keygen

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export HCLOUD_TOKEN="test-hcloud-token"

  run kubehz::register_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  # Fail-soft: registration still happens via the old server-key handshake.
  assert_output --partial "falling back to the server SSH-key fingerprint"
  assert_output --partial "fingerprint: lo:test.kubehz.dev"

  # Fell back to the server key → the registration must NOT be equality-claimable.
  run cat "${BATS_TEST_TMPDIR}/register-payload.json"
  refute_output --partial "claimable"
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

# ── deregister_cluster: calls DELETE API ─────────────────

@test "deregister_cluster: calls DELETE and succeeds" {
  local curl_called=""
  curl() {
    # Just succeed silently
    return 0
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::deregister_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
}

# ── deregister_cluster: API failure is silent ────────────

@test "deregister_cluster: API failure still succeeds (non-fatal)" {
  curl() {
    return 1
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"

  run kubehz::deregister_cluster "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
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

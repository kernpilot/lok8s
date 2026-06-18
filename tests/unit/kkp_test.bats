#!/usr/bin/env bats
# kkp_test.bats — unit tests for .lok8s/drivers/kkp/{api,main}

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/api"

  # argsh `:args` builtin — minimal stub: scan the caller's `args`
  # array for bare positional names (no `|` flags) and assign each
  # to the matching positional arg from "$@" (which the caller passes
  # after the description). This covers the simple `:args "desc" "$@"`
  # pattern in driver::kubeconfig / driver::status.
  :args() {
    # shellcheck disable=SC2034
    shift  # drop description
    local -a _pos_names=()
    local i
    for ((i=0; i<${#args[@]}; i+=2)); do
      # Positional names don't contain `|`; flag specs do (e.g. 'foo|f:+').
      [[ "${args[i]}" == *"|"* ]] && continue
      _pos_names+=("${args[i]}")
    done
    local j=0
    for name in "${_pos_names[@]}"; do
      if (( $# > 0 )); then
        printf -v "${name}" '%s' "$1"
        shift
      fi
      j=$((j+1))
    done
  }
  export -f :args

  # Copy KKP cluster spec fixture
  cp "${FIXTURES_DIR}/kkp-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml"

  # Default env vars for most tests
  export KKP_TOKEN="test-kkp-token-abc123"
  export KKP_API_URL="https://kkp.test.example.com"
}

teardown() {
  teardown_tmpdir
}

# ── kkp::validate_url ─────────────────────────────────────

@test "kkp::validate_url accepts https URLs" {
  run kkp::validate_url "https://kkp.example.com"
  assert_success
}

@test "kkp::validate_url rejects http URLs" {
  run kkp::validate_url "http://kkp.example.com"
  assert_failure
  assert_output --partial "must use HTTPS"
}

@test "kkp::validate_url rejects non-http URLs" {
  run kkp::validate_url "ftp://kkp.example.com"
  assert_failure
  assert_output --partial "must use HTTPS"
}

@test "kkp::validate_url rejects empty string" {
  run kkp::validate_url ""
  assert_failure
  assert_output --partial "must use HTTPS"
}

# ── kkp::validate_credentials ─────────────────────────────

@test "kkp::validate_credentials succeeds with KKP_TOKEN and HCLOUD_TOKEN" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.kkp.apiUrl // ""') echo "https://kkp.test.example.com" ;;
        '.spec.kkp.preset // ""') echo "" ;;
        '.spec.provider.name // ""') echo "hetzner" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  export KKP_TOKEN="test-token"
  export KKP_API_URL="https://kkp.test.example.com"
  export HCLOUD_TOKEN="test-hcloud-token"

  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml"
  assert_success
}

@test "kkp::validate_credentials fails without KKP_TOKEN" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.kkp.apiUrl // ""') echo "https://kkp.test.example.com" ;;
        '.spec.kkp.preset // ""') echo "" ;;
        '.spec.provider.name // ""') echo "hetzner" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  unset KKP_TOKEN 2>/dev/null || true
  export KKP_API_URL="https://kkp.test.example.com"
  export HCLOUD_TOKEN="test-hcloud-token"

  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "KKP_TOKEN"
}

@test "kkp::validate_credentials fails without HCLOUD_TOKEN for hetzner" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.kkp.apiUrl // ""') echo "https://kkp.test.example.com" ;;
        '.spec.kkp.preset // ""') echo "" ;;
        '.spec.provider.name // ""') echo "hetzner" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  export KKP_TOKEN="test-token"
  export KKP_API_URL="https://kkp.test.example.com"
  unset HCLOUD_TOKEN 2>/dev/null || true

  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "HCLOUD_TOKEN"
}

@test "kkp::validate_credentials skips provider check when preset is set" {
  # Create a fixture with preset set
  cat > "${BATS_TEST_TMPDIR}/kkp-preset.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Kkp
metadata:
  name: test-kkp-preset
spec:
  kubernetes:
    version: "v1.29.2"
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "test-project-123"
    datacenter: "hetzner-fsn1"
    preset: "hetzner-default"
  provider: hetzner
  workers:
    pool-1:
      replicas: 3
      flavor: cpx31
YAML

  export KKP_TOKEN="test-token"
  export KKP_API_URL="https://kkp.test.example.com"
  unset HCLOUD_TOKEN 2>/dev/null || true

  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-preset.lok8s.yaml"
  assert_success
}

@test "kkp::validate_credentials rejects http API URL" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.kkp.apiUrl // ""') echo "http://kkp.insecure.example.com" ;;
        '.spec.kkp.preset // ""') echo "" ;;
        '.spec.provider.name // ""') echo "hetzner" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  export KKP_TOKEN="test-token"
  export KKP_API_URL="http://kkp.insecure.example.com"

  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "HTTPS"
}

# ── kkp::api ──────────────────────────────────────────────

@test "kkp::api fails without KKP_TOKEN" {
  unset KKP_TOKEN 2>/dev/null || true

  run kkp::api GET "/api/v2/dc"
  assert_failure
  assert_output --partial "KKP_TOKEN is not set"
}

@test "kkp::api fails without KKP_API_URL" {
  unset KKP_API_URL 2>/dev/null || true

  run kkp::api GET "/api/v2/dc"
  assert_failure
  assert_output --partial "KKP_API_URL is not set"
}

@test "kkp::api rejects http KKP_API_URL" {
  export KKP_API_URL="http://kkp.insecure.example.com"

  run kkp::api GET "/api/v2/dc"
  assert_failure
  assert_output --partial "must use HTTPS"
}

@test "kkp::api calls curl with correct auth header" {
  curl() {
    local has_auth=false
    for arg in "$@"; do
      if [[ "${arg}" == *"Bearer test-kkp-token-abc123"* ]]; then
        has_auth=true
      fi
    done
    if [[ "${has_auth}" == "true" ]]; then
      printf '{"ok": true}\n200'
    else
      printf '{"error": "no auth"}\n401'
    fi
  }
  export -f curl

  run kkp::api GET "/api/v2/dc"
  assert_success
  assert_output --partial '"ok": true'
}

@test "kkp::api passes --cacert when KKP_CA_CERT is a readable file" {
  local ca="${BATS_TEST_TMPDIR}/ca.crt"
  : > "${ca}"
  export KKP_CA_CERT="${ca}"
  curl() {
    local saw=false prev=""
    for arg in "$@"; do
      [[ "${prev}" == "--cacert" && "${arg}" == "${KKP_CA_CERT}" ]] && saw=true
      prev="${arg}"
    done
    if [[ "${saw}" == true ]]; then printf '{"ok": true}\n200'; else printf '{"miss": true}\n400'; fi
  }
  export -f curl

  run kkp::api GET "/api/v2/dc"
  assert_success
  assert_output --partial '"ok": true'
}

@test "kkp::api fails when KKP_CA_CERT points to a missing file" {
  export KKP_CA_CERT="${BATS_TEST_TMPDIR}/does-not-exist.crt"
  curl() { printf '{"ok": true}\n200'; }   # must never be reached
  export -f curl

  run kkp::api GET "/api/v2/dc"
  assert_failure
  assert_output --partial "not a readable file"
}

@test "kkp::validate_credentials exports KKP_CA_CERT from spec.kkp.caCert" {
  unset KKP_CA_CERT 2>/dev/null || true
  local ca="${BATS_TEST_TMPDIR}/myca.crt"
  : > "${ca}"
  cat > "${BATS_TEST_TMPDIR}/kkp-ca.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1
kind: Kkp
metadata: {name: t}
spec:
  kkp:
    apiUrl: https://kkp.test.example.com
    caCert: myca.crt
  provider: {name: byo}
YAML
  # Call directly (not `run`) so the export is visible in the test shell.
  kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-ca.lok8s.yaml"
  [ "${KKP_CA_CERT:-}" = "${ca}" ]
}

@test "kkp::validate_credentials rejects a missing spec.kkp.caCert file" {
  unset KKP_CA_CERT 2>/dev/null || true
  cat > "${BATS_TEST_TMPDIR}/kkp-ca-bad.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1
kind: Kkp
metadata: {name: t}
spec:
  kkp:
    apiUrl: https://kkp.test.example.com
    caCert: /nope/missing-ca.crt
  provider: {name: byo}
YAML
  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-ca-bad.lok8s.yaml"
  assert_failure
  assert_output --partial "spec.kkp.caCert points to a missing file"
}

@test "kkp::api does not log token in debug output" {
  export DEBUG=1
  curl() { printf '{"ok": true}\n200'; }
  export -f curl

  run kkp::api GET "/api/v2/dc"
  assert_success
  refute_output --partial "test-kkp-token-abc123"
  assert_output --partial "<redacted>"
}

@test "kkp::api retries on 429 rate limit" {
  local call_count_file="${BATS_TEST_TMPDIR}/call_count"
  echo "0" > "${call_count_file}"

  curl() {
    local count
    count=$(cat "${call_count_file}")
    count=$(( count + 1 ))
    echo "${count}" > "${call_count_file}"
    if (( count < 2 )); then
      printf '{"error": "rate limited"}\n429'
    else
      printf '{"ok": true}\n200'
    fi
  }
  export -f curl

  sleep() { :; }
  export -f sleep

  export KKP_RETRY_DELAY=0

  run kkp::api GET "/api/v2/dc"
  assert_success
  assert_output --partial '"ok": true'
}

@test "kkp::api returns error on 4xx/5xx" {
  curl() { printf '{"error": "not found"}\n404'; }
  export -f curl

  run kkp::api GET "/api/v2/projects/bad/clusters/bad"
  assert_failure
  assert_output --partial "HTTP 404"
}

# ── kkp::create_cluster ───────────────────────────────────

@test "kkp::create_cluster returns cluster ID" {
  curl() { printf '{"id": "abc123cluster"}\n200'; }
  export -f curl

  run kkp::create_cluster "project-1" '{"cluster":{"name":"test"}}'
  assert_success
  assert_output "abc123cluster"
}

@test "kkp::create_cluster fails when no ID in response" {
  curl() { printf '{}\n200'; }
  export -f curl

  run kkp::create_cluster "project-1" '{"cluster":{"name":"test"}}'
  assert_failure
  assert_output --partial "no cluster ID"
}

# ── kkp::delete_cluster ───────────────────────────────────

@test "kkp::delete_cluster succeeds on 200" {
  curl() { printf '\n200'; }
  export -f curl

  run kkp::delete_cluster "project-1" "cluster-abc"
  assert_success
}

# ── kkp::get_kubeconfig ──────────────────────────────────

@test "kkp::get_kubeconfig writes kubeconfig to file" {
  curl() { printf 'apiVersion: v1\nkind: Config\nclusters: []\n200'; }
  export -f curl

  local outfile="${BATS_TEST_TMPDIR}/kubeconfig.yaml"

  run kkp::get_kubeconfig "project-1" "cluster-abc" "${outfile}"
  assert_success
  [ -f "${outfile}" ]
}

# ── kkp::create_machinedeployment ─────────────────────────

@test "kkp::create_machinedeployment returns MD ID" {
  curl() { printf '{"id": "md-pool1-xyz"}\n200'; }
  export -f curl

  run kkp::create_machinedeployment "proj-1" "cluster-1" '{"name":"pool-1"}'
  assert_success
  assert_output "md-pool1-xyz"
}

# ── kkp::core_healthy ─────────────────────────────────────
# The v2 REST API exposes no .status.phase; readiness comes from the
# /health endpoint (verified against a real KKP 2.30 install).

_ALL_UP='{"apiserver":"HealthStatusUp","etcd":"HealthStatusUp","controller":"HealthStatusUp","scheduler":"HealthStatusUp","machineController":"HealthStatusDown"}'

@test "kkp::core_healthy succeeds when core components are up" {
  run kkp::core_healthy "${_ALL_UP}"
  assert_success
}

@test "kkp::core_healthy ignores provider-dependent components" {
  # machineController is down in the fixture (bringyourown) — still healthy
  run kkp::core_healthy "${_ALL_UP}"
  assert_success
}

@test "kkp::core_healthy fails when etcd is provisioning" {
  run kkp::core_healthy '{"apiserver":"HealthStatusUp","etcd":"HealthStatusProvisioning","controller":"HealthStatusUp","scheduler":"HealthStatusUp"}'
  assert_failure
}

@test "kkp::core_healthy fails when a core component is missing" {
  run kkp::core_healthy '{"apiserver":"HealthStatusUp"}'
  assert_failure
}

@test "kkp::core_healthy accepts legacy numeric health values" {
  run kkp::core_healthy '{"apiserver":1,"etcd":1,"controller":1,"scheduler":1}'
  assert_success
}

# ── kkp::wait_ready ───────────────────────────────────────

@test "kkp::wait_ready returns when control plane is healthy" {
  curl() { printf '%s\n200' "${_ALL_UP}"; }
  export -f curl

  run kkp::wait_ready "project-1" "cluster-abc" 5
  assert_success
}

@test "kkp::wait_ready fails on timeout while provisioning" {
  curl() { printf '{"apiserver":"HealthStatusProvisioning","etcd":"HealthStatusProvisioning","controller":"HealthStatusProvisioning","scheduler":"HealthStatusProvisioning"}\n200'; }
  export -f curl

  sleep() { :; }
  export -f sleep

  export KKP_WAIT_INTERVAL=1

  run kkp::wait_ready "project-1" "cluster-abc" 1
  assert_failure
  assert_output --partial "Timed out"
}

# ── driver::status (health-derived) ───────────────────────

_setup_status_state() {
  # driver::status reads saved IDs from the domain work dir
  mkdir -p "${PATH_CLUSTERS}/test-domain/.kkp"
  echo "cluster-abc" > "${PATH_CLUSTERS}/test-domain/.kkp/cluster_id"
  echo "project-1" > "${PATH_CLUSTERS}/test-domain/.kkp/project_id"
  cp "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml" \
    "${PATH_CLUSTERS}/test-domain/cluster.lok8s.yaml"
}

@test "driver::status returns NotFound when no cluster_id file" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"
  mkdir -p "${PATH_CLUSTERS}/test-domain"
  cp "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml" \
    "${PATH_CLUSTERS}/test-domain/cluster.lok8s.yaml"

  run driver::status "test-domain"
  assert_success
  assert_output "NotFound"
}

@test "driver::status returns Running when core healthy" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"
  _setup_status_state

  curl() { printf '%s\n200' "${_ALL_UP}"; }
  export -f curl

  run driver::status "test-domain"
  assert_success
  assert_output "Running"
}

@test "driver::status returns Provisioning when core not yet healthy" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"
  _setup_status_state

  curl() { printf '{"apiserver":"HealthStatusProvisioning","etcd":"HealthStatusProvisioning","controller":"HealthStatusProvisioning","scheduler":"HealthStatusProvisioning"}\n200'; }
  export -f curl

  run driver::status "test-domain"
  assert_success
  assert_output "Provisioning"
}

@test "driver::status returns Unknown when the API errors" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"
  _setup_status_state

  curl() { printf '{"error":"boom"}\n500'; }
  export -f curl

  run driver::status "test-domain"
  assert_success
  assert_output "Unknown"
}

# ── driver::kubeconfig ────────────────────────────────────

@test "driver::kubeconfig returns metadata.name-based path" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"
  mkdir -p "${PATH_CLUSTERS}/test-domain"
  cp "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml" \
    "${PATH_CLUSTERS}/test-domain/cluster.lok8s.yaml"

  run driver::kubeconfig "test-domain"
  assert_success
  assert_output "${PATH_BASE}/.kubeconfig/test-kkp-cluster.yaml"
}

# ── kkp::wait_components ──────────────────────────────────

@test "kkp::wait_components returns when named components are up" {
  curl() { printf '{"machineController":"HealthStatusUp","operatingSystemManager":"HealthStatusUp"}\n200'; }
  export -f curl

  run kkp::wait_components "project-1" "cluster-abc" 5 machineController operatingSystemManager
  assert_success
}

@test "kkp::wait_components times out while a component is provisioning" {
  curl() { printf '{"machineController":"HealthStatusUp","operatingSystemManager":"HealthStatusProvisioning"}\n200'; }
  export -f curl

  sleep() { :; }
  export -f sleep
  export KKP_WAIT_INTERVAL=1

  run kkp::wait_components "project-1" "cluster-abc" 1 machineController operatingSystemManager
  assert_failure
  assert_output --partial "Timed out"
}

# ── machine deployment payload ────────────────────────────

@test "_build_machinedeployment_json hetzner uses REST field 'type'" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"

  run _build_machinedeployment_json "pool-1" 1 "cpx22" "ubuntu" "hetzner" 0 0
  assert_success
  assert_output --partial '"type": "cpx22"'
  refute_output --partial "serverType"
}

# ── bringyourown provider ─────────────────────────────────

@test "_build_cloud_spec byo needs no credentials" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"

  run _build_cloud_spec "byo" "${BATS_TEST_TMPDIR}/kkp-cluster.lok8s.yaml" ""
  assert_success
  assert_output --partial '"bringyourown"'
}

@test "kkp::validate_credentials succeeds for byo without HCLOUD_TOKEN" {
  cat > "${BATS_TEST_TMPDIR}/kkp-byo.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Kkp
metadata:
  name: test-kkp-byo
spec:
  kubernetes:
    version: "1.35.5"
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "test-project-123"
    datacenter: "byo-local"
  provider:
    name: byo
YAML

  export KKP_TOKEN="test-token"
  export KKP_API_URL="https://kkp.test.example.com"
  unset HCLOUD_TOKEN 2>/dev/null || true

  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-byo.lok8s.yaml"
  assert_success
}

@test "kkp::validate_credentials handles scalar provider shape" {
  # `provider: hetzner` (bare scalar) must behave like `provider.name: hetzner`
  cat > "${BATS_TEST_TMPDIR}/kkp-scalar.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Kkp
metadata:
  name: test-kkp-scalar
spec:
  kkp:
    apiUrl: "https://kkp.test.example.com"
    projectId: "test-project-123"
    datacenter: "hetzner-fsn1"
  provider: hetzner
YAML

  export KKP_TOKEN="test-token"
  export KKP_API_URL="https://kkp.test.example.com"
  unset HCLOUD_TOKEN 2>/dev/null || true

  run kkp::validate_credentials "${BATS_TEST_TMPDIR}/kkp-scalar.lok8s.yaml"
  assert_failure
  assert_output --partial "HCLOUD_TOKEN"
}

# ── HTTPS enforcement (integration-level) ─────────────────

@test "driver::ensure_credentials rejects http API URL from spec" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"

  # Create a fixture with http:// URL to test HTTPS enforcement
  cat > "${BATS_TEST_TMPDIR}/kkp-insecure.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Kkp
metadata:
  name: test-kkp-insecure
spec:
  kubernetes:
    version: "v1.29.2"
  kkp:
    apiUrl: "http://kkp.insecure.example.com"
    projectId: "test-project-123"
    datacenter: "hetzner-fsn1"
  provider: hetzner
YAML

  unset KKP_API_URL 2>/dev/null || true
  export KKP_TOKEN="test-token"

  run driver::ensure_credentials "${BATS_TEST_TMPDIR}/kkp-insecure.lok8s.yaml"
  assert_failure
  assert_output --partial "HTTPS"
}

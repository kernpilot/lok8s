#!/usr/bin/env bats
# kubehz_hosted_test.bats — unit tests for hosted provisioning API client

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # destroy_hosted's HTTPS bearer guard is UNCONDITIONAL (a security gate,
  # never probe-skipped) — the real helper must be present.
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  # argsh `:args` builtin — stub as no-op
  :args() { :; }
  export -f :args

  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
}

teardown() {
  teardown_tmpdir
}

# ── build_cluster_payload ────────────────────────────────

@test "build_cluster_payload: produces correct JSON with all fields" {
  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "test.kubehz.dev" ;;
      # BARE '.kind': kubehz::build_cluster_payload reads the WIRE field, which
      # keeps the spec's original case. Answering '.kind // ""' here would mock
      # domain::spec_driver's expression instead — a reader this code path never
      # uses — and the assertion below would be measuring the mock's fallback.
      '.kind') echo "KubeOne" ;;
      '.spec.provider // "hetzner"') echo "hetzner" ;;
      '.spec.hcloud.region // .spec.aws.region // "fsn1"') echo "nbg1" ;;
      '.spec.kubernetes.version') echo "v1.31.10" ;;
      '.spec.controlPlane.replicas // 1') echo "3" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Use real jq for payload construction
  if ! command -v jq &>/dev/null; then
    skip "jq not available"
  fi

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  run kubehz::build_cluster_payload "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  # Verify JSON fields
  echo "${output}" | jq -e '.domain == "test.kubehz.dev"'
  echo "${output}" | jq -e '.kind == "KubeOne"'
  echo "${output}" | jq -e '.provider == "hetzner"'
  echo "${output}" | jq -e '.region == "nbg1"'
  echo "${output}" | jq -e '.kubernetesVersion == "v1.31.10"'
  echo "${output}" | jq -e '.controlPlaneReplicas == 3'
}

@test "build_cluster_payload: defaults provider to hetzner and replicas to 1" {
  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "default.kubehz.dev" ;;
      # BARE '.kind': kubehz::build_cluster_payload reads the WIRE field, which
      # keeps the spec's original case. Answering '.kind // ""' here would mock
      # domain::spec_driver's expression instead — a reader this code path never
      # uses — and the assertion below would be measuring the mock's fallback.
      '.kind') echo "Capi" ;;
      '.spec.provider // "hetzner"') echo "hetzner" ;;
      '.spec.hcloud.region // .spec.aws.region // "fsn1"') echo "fsn1" ;;
      '.spec.kubernetes.version') echo "v1.30.0" ;;
      '.spec.controlPlane.replicas // 1') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  if ! command -v jq &>/dev/null; then
    skip "jq not available"
  fi

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  run kubehz::build_cluster_payload "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  echo "${output}" | jq -e '.provider == "hetzner"'
  echo "${output}" | jq -e '.controlPlaneReplicas == 1'
}

# ── wait_for_cluster ─────────────────────────────────────

@test "wait_for_cluster: returns immediately when status is Running" {
  local _call_count=0
  curl() {
    echo '{"id":"cl-001","status":"Running"}'
  }
  export -f curl

  jq() {
    echo "Running"
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export KUBEHZ_TOKEN="test-token"

  run kubehz::wait_for_cluster "https://api.kubehz.dev" "cl-001" 30
  assert_success
}

@test "wait_for_cluster: fails when status is Failed" {
  curl() {
    echo '{"id":"cl-001","status":"Failed"}'
  }
  export -f curl

  jq() {
    echo "Failed"
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export KUBEHZ_TOKEN="test-token"

  run kubehz::wait_for_cluster "https://api.kubehz.dev" "cl-001" 30
  assert_failure
  assert_output --partial "failed"
}

# ── provision_hosted ─────────────────────────────────────

@test "provision_hosted: creates cluster, waits, and downloads kubeconfig" {
  local _curl_calls=()
  curl() {
    _curl_calls+=("$*")
    case "$*" in
      *POST*api/clusters*)
        # Enveloped create (UI route) + the trailing HTTP-status line (-w).
        printf '%s\n%s\n' '{"ok":true,"data":{"id":"cl-hosted-001","status":"Creating"}}' '201'
        ;;
      *api/clusters/cl-hosted-001/kubeconfig*)
        echo "apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://hosted.kubehz.dev:6443
  name: hosted"
        ;;
      *api/clusters/cl-hosted-001*)
        echo '{"id":"cl-hosted-001","status":"Running"}'
        ;;
      *) echo '{}' ;;
    esac
  }
  export -f curl

  jq() {
    case "$*" in
      # The AT_CAPACITY guard's `jq -e` probe — return non-zero (not a capacity error).
      *AT_CAPACITY*) return 1 ;;
      *'.data.id // .id'*) echo "cl-hosted-001" ;;
      *'.id'*) echo "cl-hosted-001" ;;
      *'.status'*) echo "Running" ;;
      *-n*) echo '{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"fsn1","kubernetesVersion":"v1.31.10","controlPlaneReplicas":1}' ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "test.kubehz.dev" ;;
      # BARE '.kind': kubehz::build_cluster_payload reads the WIRE field, which
      # keeps the spec's original case. Answering '.kind // ""' here would mock
      # domain::spec_driver's expression instead — a reader this code path never
      # uses — and the assertion below would be measuring the mock's fallback.
      '.kind') echo "KubeOne" ;;
      '.spec.provider // "hetzner"') echo "hetzner" ;;
      '.spec.hcloud.region // .spec.aws.region // "fsn1"') echo "fsn1" ;;
      '.spec.kubernetes.version') echo "v1.31.10" ;;
      '.spec.controlPlane.replicas // 1') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  # Verify kubeconfig was written
  [ -f "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]
}

@test "provision_hosted: fails when API returns no cluster ID" {
  curl() {
    # A 200 whose body carries no id, plus the trailing status line.
    printf '%s\n%s\n' '{"ok":true,"data":{}}' '200'
  }
  export -f curl

  jq() {
    case "$*" in
      *AT_CAPACITY*) return 1 ;;
      *'.data.id // .id'*) echo "null" ;;
      *'.id'*) echo "null" ;;
      *-n*) echo '{"domain":"x"}' ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  yq() { echo ""; }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "did not return a cluster ID"
}

# ── provision_hosted: capacity rejection (the "max mechanism") ───────────

@test "provision_hosted: renders a friendly capacity envelope on 503 AT_CAPACITY" {
  # FAIL-HARD (not skip) if jq is absent: this test parses the envelope and is
  # worthless without it. A silent skip would ship a false-green suite. Run via
  # PATH_BIN=$PWD/.bin ./.bin/argsh test … (CI mounts the b toolchain).
  command -v jq &>/dev/null || { echo "FATAL: jq required — run with PATH_BIN=\$PWD/.bin" >&2; return 1; }

  # curl returns the enveloped 503 body + a trailing HTTP status line (the -w
  # format the real client uses), so the client can split the two.
  curl() {
    case "$*" in
      *POST*api/clusters*)
        printf '%s\n%s\n' \
          '{"ok":false,"data":{"code":"AT_CAPACITY","message":"at capacity","detail":{"tier":"dev","used":40,"limit":40,"retryAfter":3600}}}' \
          '503'
        ;;
      *) printf '%s\n%s\n' '{}' '200' ;;
    esac
  }
  export -f curl

  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "test.kubehz.dev" ;;
      '.spec.kubernetes.version') echo "v1.31.10" ;;
      '.spec.controlPlane.replicas // 1') echo "1" ;;
      '.spec.provider // "hetzner"') echo "hetzner" ;;
      '.spec.hcloud.region // .spec.aws.region // "fsn1"') echo "fsn1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "at capacity for the 'dev' plan"
  assert_output --partial "40/40"
  assert_output --partial "hosting: self"
  assert_output --partial "/api/capacity"
  # retryAfter 3600 is humanized to "~60 min" (not the raw "~3600s").
  assert_output --partial "~60 min"
  refute_output --partial "~3600s"
  # The remedy must NOT advise a non-existent spec.kubehz.plan field.
  refute_output --partial "spec.kubehz.plan"
}

@test "provision_hosted: splits status from a MULTI-LINE (pretty-printed) JSON body" {
  command -v jq &>/dev/null || { echo "FATAL: jq required — run with PATH_BIN=\$PWD/.bin" >&2; return 1; }

  # A pretty-printed 503 body — every field on its own line, then the trailing
  # %{http_code} line. Locks in the newline-split: http_code must be the LAST
  # line and the body must keep ALL its interior newlines.
  curl() {
    case "$*" in
      *POST*api/clusters*)
        printf '%s\n%s\n' \
'{
  "ok": false,
  "data": {
    "code": "AT_CAPACITY",
    "message": "at capacity",
    "detail": {
      "tier": "starter",
      "used": 20,
      "limit": 20,
      "retryAfter": 1800
    }
  }
}' \
          '503'
        ;;
      *) printf '%s\n%s\n' '{}' '200' ;;
    esac
  }
  export -f curl

  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "test.kubehz.dev" ;;
      '.spec.kubernetes.version') echo "v1.31.10" ;;
      '.spec.controlPlane.replicas // 1') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  # The multi-line body parsed correctly → the capacity envelope rendered.
  assert_output --partial "at capacity for the 'starter' plan"
  assert_output --partial "20/20"
  # retryAfter 1800 humanizes to ~30 min.
  assert_output --partial "~30 min"
}

@test "render_capacity_rejection: a sub-60s retryAfter stays in seconds (~Ns)" {
  command -v jq &>/dev/null || { echo "FATAL: jq required — run with PATH_BIN=\$PWD/.bin" >&2; return 1; }
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  run kubehz::render_capacity_rejection '{"data":{"detail":{"tier":"dev","used":3,"limit":3,"retryAfter":45}}}'
  assert_output --partial "~45s"
  refute_output --partial "min"
}

@test "render_capacity_rejection: a missing/non-numeric retryAfter emits no wait hint" {
  command -v jq &>/dev/null || { echo "FATAL: jq required — run with PATH_BIN=\$PWD/.bin" >&2; return 1; }
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  run kubehz::render_capacity_rejection '{"data":{"detail":{"tier":"dev"}}}'
  assert_output --partial "at capacity for the 'dev' plan"
  refute_output --partial "suggested wait"
  # And still never advises the phantom spec.kubehz.plan field.
  refute_output --partial "spec.kubehz.plan"
}

@test "provision_hosted: surfaces the api message on a non-capacity error status" {
  if ! command -v jq &>/dev/null; then skip "jq not available"; fi

  curl() {
    case "$*" in
      *POST*api/clusters*)
        printf '%s\n%s\n' \
          '{"ok":false,"data":{"code":"HOSTED_BACKEND_ERROR","message":"Failed to schedule the hosted control plane"}}' \
          '502'
        ;;
      *) printf '%s\n%s\n' '{}' '200' ;;
    esac
  }
  export -f curl

  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "test.kubehz.dev" ;;
      '.spec.kubernetes.version') echo "v1.31.10" ;;
      '.spec.controlPlane.replicas // 1') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "HTTP 502"
  assert_output --partial "Failed to schedule the hosted control plane"
}

# ── destroy_hosted ───────────────────────────────────────
#
# Pins the REAL contract: resolve the id from the tenant registry
# (kubehz::resolve_cluster_id — client-side domain filter, oldest-first),
# then DELETE /api/clusters/<id>. Real jq — the resolution logic under test
# IS the jq filter. A query-string DELETE, or a delete on an unresolved id,
# fails the test.

@test "destroy_hosted: resolves the id (oldest row, not .data[0]) and DELETEs by id" {
  yq() { echo ""; }
  export -f yq
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
      # Newest-first list, two rows for the domain: .data[0] is the WRONG
      # cluster (the bug this pins against).
      cat <<'EOF'
{"ok":true,"data":[
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","createdAt":"2026-01-01T00:00:00Z"}
]}
EOF
    fi
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  # Create a kubeconfig to verify cleanup
  touch "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml"

  run kubehz::destroy_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  # Verify kubeconfig was cleaned up
  [ ! -f "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]
}

@test "destroy_hosted: REFUSES when the registry lookup fails (no delete, kubeconfig kept)" {
  yq() { echo ""; }
  export -f yq
  curl() {
    [[ " $* " != *" DELETE "* ]] \
      || { echo "DELETE must not run when the lookup failed: $*" >&2; return 1; }
    return 1
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  touch "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml"

  run kubehz::destroy_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "nothing was deleted"
  # The cluster may still exist — its credentials must survive the refusal.
  [ -f "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]
}

@test "destroy_hosted: no row for the domain stays idempotent (nothing to destroy)" {
  yq() { echo ""; }
  export -f yq
  curl() {
    [[ " $* " != *" DELETE "* ]] \
      || { echo "DELETE must not run without a resolved id: $*" >&2; return 1; }
    echo '{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","createdAt":"2026-03-01T00:00:00Z"}]}'
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::destroy_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "nothing to destroy"
}

@test "destroy_hosted: a refused DELETE reports the HTTP status and returns 1" {
  yq() { echo ""; }
  export -f yq
  curl() {
    if [[ " $* " == *" DELETE "* ]]; then
      printf '{"ok":false,"data":{"message":"forbidden"}}\n403'
    else
      echo '{"ok":true,"data":[{"id":"cl-1","domain":"test.kubehz.dev","createdAt":"2026-01-01T00:00:00Z"}]}'
    fi
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::destroy_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "HTTP 403"
}

# ── KubeOne driver hosted branch ─────────────────────────

@test "KubeOne driver::provision branches to hosted when hosting=hosted" {
  # Mock kubehz functions
  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="hosted"
    LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
    export LOK8S_KUBEHZ_HOSTING LOK8S_KUBEHZ_API_URL
  }
  export -f kubehz::read_config

  local _hosted_called=0
  kubehz::provision_hosted() {
    _hosted_called=1
    echo "hosted_provision_called domain=$1"
  }
  export -f kubehz::provision_hosted

  # These should NOT be called in hosted path
  kubeone::detect_provider() { echo "SHOULD_NOT_REACH"; return 1; }
  export -f kubeone::detect_provider

  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"

  run driver::provision "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "hosted_provision_called"
}

@test "KubeOne driver::provision continues self-hosted when hosting=self" {
  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="self"
    export LOK8S_KUBEHZ_HOSTING
  }
  export -f kubehz::read_config

  # Mock the self-hosted path — detect_provider is the first call after the hosted check
  kubeone::detect_provider() { echo "hetzner"; }
  export -f kubeone::detect_provider

  kubeone::validate_credentials() { :; }
  export -f kubeone::validate_credentials

  kubeone::extract_vars() { :; }
  export -f kubeone::extract_vars

  hetzner::provision() { :; }
  export -f hetzner::provision

  hetzner::generate_tfjson() { echo '{}'; }
  export -f hetzner::generate_tfjson

  kubeone::generate_config() { :; }
  export -f kubeone::generate_config

  kubeone::apply() { :; }
  export -f kubeone::apply

  yq() {
    case "$2" in
      '.metadata.name') echo "test-self" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubeone::kubeconfig_path() {
    local path="${BATS_TEST_TMPDIR}/kubeconfig-test"
    touch "${path}"
    echo "${path}"
  }
  export -f kubeone::kubeconfig_path

  # Stub provider::provision — libs/provision would normally load a
  # provider before driver::provision runs, so we satisfy the contract.
  provider::provision() { :; }
  export -f provider::provision
  # Other kubeone internals that get called once the self-hosted
  # branch proceeds — stub to no-ops for this unit test.
  _tfjson_from_output() { echo '{}'; }
  export -f _tfjson_from_output
  kubeone() { :; }
  export -f kubeone

  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"

  # We only care that the hosting=self branch proceeds past the
  # provision_hosted guard — don't care if later kubeone/real-network
  # steps succeed.
  run driver::provision "test.kubehz.dev"
  refute_output --partial "KubeOne driver requires spec.provider"
  refute_output --partial "hosted"
}

# ── CAPI driver hosted branch ────────────────────────────

@test "CAPI driver::provision branches to hosted when no mgmt_domain + hosting=hosted" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="hosted"
    LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
    export LOK8S_KUBEHZ_HOSTING LOK8S_KUBEHZ_API_URL
  }
  export -f kubehz::read_config

  kubehz::provision_hosted() {
    echo "capi_hosted_provision_called domain=$1"
  }
  export -f kubehz::provision_hosted

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::provision "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "capi_hosted_provision_called"
}

@test "CAPI driver::provision errors when no mgmt_domain + hosting=self" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="self"
    export LOK8S_KUBEHZ_HOSTING
  }
  export -f kubehz::read_config

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::provision "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "spec.managementCluster.domain is required for self-hosted CAPI"
}

@test "CAPI driver::destroy uses hosted path when no mgmt_domain + hosting=hosted" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      '.metadata.name') echo "test-cluster" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="hosted"
    LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
    export LOK8S_KUBEHZ_HOSTING LOK8S_KUBEHZ_API_URL
  }
  export -f kubehz::read_config

  kubehz::destroy_hosted() {
    echo "capi_hosted_destroy_called domain=$1"
  }
  export -f kubehz::destroy_hosted

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::destroy "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "capi_hosted_destroy_called"
}

# ── provision_hosted → dispatch kubeconfig mirror ────────────────────────────
# provision::dispatch applies the bootstrap DAG with .kubeconfig/<metadata.name>
# .yaml — provision_hosted mirrors its domain-keyed download there when the two
# names differ (the hosted gates themselves live in bootstrap::apply and are
# covered by bootstrap_test.bats).

_hosted_kc_harness() {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  export LOK8S_KUBEHZ_API_URL="https://api.test" KUBEHZ_TOKEN="t"
  curl() {
    if [[ "$*" == *"/api/clusters/"*"/kubeconfig"* ]]; then echo "kc-bytes"; return 0; fi
    printf '{"data":{"id":"cl-1","status":"Running"}}\n201'
  }
  export -f curl
  kubehz::wait_for_cluster() { :; }
  debug() { :; }; error() { echo "ERR: $*" >&2; }
}

@test "provision_hosted: mirrors the kubeconfig to the metadata.name path when it differs" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "shortname" ;; *) echo "1" ;; esac; }
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_success
  [[ -f "${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml" ]] || fail "domain kubeconfig missing"
  [[ -f "${PATH_BASE}/.kubeconfig/shortname.yaml" ]] || fail "metadata.name mirror missing"
  run cat "${PATH_BASE}/.kubeconfig/shortname.yaml"
  assert_output "kc-bytes"
}

@test "provision_hosted: no mirror when metadata.name equals the domain" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "test.kubehz.dev" ;; *) echo "1" ;; esac; }
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_success
  [[ -f "${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml" ]] || fail "domain kubeconfig missing"
}

# ── the kubeconfig writes (kubehz::_write_private) ──────────────────────────
# The fixtures are the same as in internal/kubehz/hosted_test.go
# (writePrivateFile).

# _hosted_temps <dir> <file>: the temp files of kubehz::_write_private for
# <file> in <dir> (the name <file>.XXXXXX), one per line.
_hosted_temps() {
  find "${1}" -maxdepth 1 -name "${2}.*" -print
}

@test "provision_hosted: a failed download keeps the old kubeconfig and leaves no temp file" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "test.kubehz.dev" ;; *) echo "1" ;; esac; }
  curl() {
    if [[ "$*" == *"/api/clusters/"*"/kubeconfig"* ]]; then printf 'partial'; return 22; fi
    printf '{"data":{"id":"cl-1","status":"Running"}}\n201'
  }
  export -f curl
  local kc="${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml"
  printf 'old\n' > "${kc}"
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_failure
  assert_output --partial "ERR: Failed to download kubeconfig for hosted cluster cl-1"
  [ "$(cat "${kc}")" = old ]
  [ -z "$(_hosted_temps "${PATH_BASE}/.kubeconfig" test.kubehz.dev.yaml)" ]
}

# The mirror is never written in place (cp truncated the old file and
# wrote into it). The mirror here is a link to a file that the user can
# write, in a directory without write permission: no temp file can go
# beside it, so the old file stays, with a warning, and the provision
# succeeds.
@test "provision_hosted: the mirror is not written in place, and a mirror that cannot be written keeps the old file" {
  (( EUID != 0 )) || skip "root can write into a directory without write permission"
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "shortname" ;; *) echo "1" ;; esac; }
  local ro="${PATH_BASE}/ro"
  mkdir -p "${ro}"
  printf 'old-mirror\n' > "${ro}/shortname.yaml"
  chmod 600 "${ro}/shortname.yaml"
  ln -s ../ro/shortname.yaml "${PATH_BASE}/.kubeconfig/shortname.yaml"
  chmod 500 "${ro}"
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  chmod 700 "${ro}"
  assert_success
  assert_output --partial "could not mirror the kubeconfig to .kubeconfig/shortname.yaml"
  [ "$(cat "${ro}/shortname.yaml")" = old-mirror ]
  [ -z "$(_hosted_temps "${ro}" shortname.yaml)" ]
  [ "$(cat "${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml")" = kc-bytes ]
}

@test "provision_hosted: a symlinked kubeconfig keeps the link and gets the new content, mode 0600" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "test.kubehz.dev" ;; *) echo "1" ;; esac; }
  local real="${PATH_BASE}/real" kc="${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml"
  mkdir -p "${real}"
  printf 'old\n' > "${real}/test.kubehz.dev.yaml"
  chmod 644 "${real}/test.kubehz.dev.yaml"
  ln -s ../real/test.kubehz.dev.yaml "${kc}"
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_success
  [ "$(readlink "${kc}")" = ../real/test.kubehz.dev.yaml ]
  [ "$(cat "${real}/test.kubehz.dev.yaml")" = kc-bytes ]
  [ "$(stat -c %a "${real}/test.kubehz.dev.yaml")" = 600 ]
  [ -z "$(_hosted_temps "${real}" test.kubehz.dev.yaml)" ]
  [ -z "$(_hosted_temps "${PATH_BASE}/.kubeconfig" test.kubehz.dev.yaml)" ]
}

@test "kubehz::_write_private refuses a link to nothing and a directory, and leaves no temp file" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/wp"
  mkdir -p "${d}/sub.yaml"
  # The directory of the target exists: readlink -f alone would resolve it.
  ln -s "${d}/none.yaml" "${d}/dangling.yaml"
  run kubehz::_write_private "${d}/dangling.yaml" printf x
  assert_failure
  assert_output --partial "Cannot resolve the symlink ${d}/dangling.yaml"
  run kubehz::_write_private "${d}/sub.yaml" printf x
  assert_failure
  assert_output --partial "${d}/sub.yaml is a directory, not a file"
  [ "$(find "${d}" -mindepth 1 -maxdepth 1 | wc -l)" -eq 2 ]
}

# A TERM or a HUP during the download: the trap removes the temp file and
# ends the shell with that signal, and the old file stays. The download
# sends the signal to its own shell and then goes on, so without the trap
# the shell would end at once and leave the partial temp file. (INT has
# the same trap. It has no case: a shell that starts with INT ignored, as
# a background job does, cannot trap it.)
@test "kubehz::_write_private: TERM and HUP remove the temp file, and the old file stays" {
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" pair sig code
  for pair in TERM:143 HUP:129; do
    sig="${pair%:*}" code="${pair#*:}"
    printf 'old\n' > "${kc}"
    run bash -c 'import() { :; }
source "$3/utils/verbose.sh"
source "$3/utils/http.sh"
source "$3/libs/kubehz/hosted"
# A name of its own: the function sees the locals of its caller.
test_signal="$2"
dl() { printf partial; kill -s "${test_signal}" "$$"; printf rest; }
kubehz::_write_private "$1" dl
echo "returned $?"' bash "${kc}" "${sig}" "${_PROJECT_ROOT}/.lok8s"
    [ "${status}" -eq "${code}" ] || { echo "${sig}: status ${status}, want ${code}" >&2; return 1; }
    refute_output --partial returned
    [ "$(cat "${kc}")" = old ]
    [ -z "$(_hosted_temps "${BATS_TEST_TMPDIR}" kc.yaml)" ]
  done
}

@test "kubehz::_write_private gives the caller's INT, TERM and HUP traps back, on success and on failure" {
  run bash -c 'import() { :; }
source "$2/utils/verbose.sh"
source "$2/utils/http.sh"
source "$2/libs/kubehz/hosted"
trap "echo caller-term" TERM
trap "echo caller-hup" HUP
before=$(trap -p INT TERM HUP)
kubehz::_write_private "$1" printf new
echo "success rc=$?"
[ "$(trap -p INT TERM HUP)" = "${before}" ] && echo "same after success"
kubehz::_write_private "$1" false
echo "failure rc=$?"
[ "$(trap -p INT TERM HUP)" = "${before}" ] && echo "same after failure"
[ -n "${before}" ] && echo "the caller had traps"
cat "$1"' bash "${BATS_TEST_TMPDIR}/kc.yaml" "${_PROJECT_ROOT}/.lok8s"
  assert_success
  assert_output "success rc=0
same after success
failure rc=1
same after failure
the caller had traps
new"
}

@test "destroy_hosted: removes BOTH the domain kubeconfig and the metadata.name mirror" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name'*) echo "shortname" ;; *) echo "1" ;; esac; }
  # The harness curl answers the CREATE route; destroy resolves through the
  # registry list and DELETEs by id — answer those two.
  curl() {
    if [[ "$*" == *"/api/clusters?perPage=500"* ]]; then
      echo '{"ok":true,"data":[{"id":"cl-1","domain":"test.kubehz.dev","createdAt":"2026-01-01T00:00:00Z"}]}'
    elif [[ " $* " == *" DELETE "* ]]; then
      printf '{"ok":true,"data":{"deleted":true,"id":"cl-1"}}\n200'
    else
      printf '{"data":{"id":"cl-1","status":"Running"}}\n201'
    fi
  }
  export -f curl
  mkdir -p "${PATH_BASE}/.kubeconfig"
  echo kc > "${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml"
  echo kc > "${PATH_BASE}/.kubeconfig/shortname.yaml"
  run kubehz::destroy_hosted "test.kubehz.dev" "/dev/null"
  assert_success
  [[ ! -f "${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml" ]] || fail "domain kubeconfig leaked"
  [[ ! -f "${PATH_BASE}/.kubeconfig/shortname.yaml" ]] || fail "mirror kubeconfig leaked (build would prefer it)"
}

#!/usr/bin/env bats
# kubehz_hosted_test.bats — unit tests for hosted provisioning API client

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"

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

@test "destroy_hosted: looks up cluster and sends DELETE" {
  curl() {
    case "$*" in
      *"GET"*|*"api/clusters?domain"*)
        echo '{"id":"cl-001","status":"Running"}'
        ;;
      *DELETE*)
        echo '{"success":true}'
        ;;
      *) echo '{}' ;;
    esac
  }
  export -f curl

  jq() {
    echo "cl-001"
  }
  export -f jq

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

@test "destroy_hosted: succeeds silently when no cluster found" {
  curl() {
    return 1
  }
  export -f curl

  jq() { echo ""; }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::destroy_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
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

# ── provision_hosted → customer bootstrap addons ─────────────────────────────
# The tail of provision_hosted: after the kubeconfig lands, the spec's
# bootstrap DAG is applied onto the hosted cluster — node-gated (a CP-only
# cluster skips with a re-run notice) and entry-gated (no entries: quiet).

_hosted_bootstrap_harness() {
  # Source the hosted lib fresh with stubs for everything up to the tail.
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  export LOK8S_KUBEHZ_API_URL="https://api.test" KUBEHZ_TOKEN="t"
  export BLOG="${BATS_TEST_TMPDIR}/calls.log"; : > "${BLOG}"
  # API happy path: create -> 201 + id, then ready, then kubeconfig bytes.
  curl() {
    if [[ "$*" == *"/api/clusters/"*"/kubeconfig"* ]]; then echo "kc-bytes"; return 0; fi
    printf '{"data":{"id":"cl-1","status":"Running"}}\n201'
  }
  export -f curl
  kubehz::wait_for_cluster() { :; }
  bootstrap::apply() { echo "bootstrap::apply $*" >> "${BLOG}"; }
  debug() { :; }; error() { echo "ERR: $*" >&2; }
}

@test "provision_hosted: applies bootstrap when workers are Ready" {
  _hosted_bootstrap_harness
  yq() { case "$2" in *bootstrap*) echo "2" ;; *) echo "x" ;; esac; }
  kubectl() { printf 'w1 Ready worker 1d v1.33\nw2 Ready worker 1d v1.33\n'; }
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_success
  assert_output --partial "applying 2 bootstrap addon(s)"
  run cat "${BLOG}"
  assert_output --partial "bootstrap::apply test.kubehz.dev /dev/null"
}

@test "provision_hosted: skips bootstrap with a notice when no Ready workers" {
  _hosted_bootstrap_harness
  yq() { case "$2" in *bootstrap*) echo "3" ;; *) echo "x" ;; esac; }
  kubectl() { return 1; } # CP up, zero nodes visible
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_success
  assert_output --partial "no Ready workers yet"
  assert_output --partial "re-run 'lo provision'"
  run cat "${BLOG}"
  refute_output --partial "bootstrap::apply"
}

@test "provision_hosted: quiet when the spec has no bootstrap entries" {
  _hosted_bootstrap_harness
  yq() { case "$2" in *bootstrap*) echo "0" ;; *) echo "x" ;; esac; }
  kubectl() { printf 'w1 Ready worker 1d v1.33\n'; }
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_success
  refute_output --partial "applying"
  run cat "${BLOG}"
  refute_output --partial "bootstrap::apply"
}

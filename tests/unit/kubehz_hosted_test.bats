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
      '.spec.kubernetes.version // ""') echo "v1.31.10" ;;
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
      '.spec.kubernetes.version // ""') echo "v1.30.0" ;;
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

# Issue #272: an unset spec.kubernetes.version (no key, a YAML null,
# false, an empty string) is left out of the payload, and a set one is sent
# as written (a number as its text). The real yq and jq read a real spec
# file. Go twin: TestBuildClusterPayloadKubernetesVersion
# (internal/kubehz/hosted_test.go).
_payload_setup() {
  command -v yq &>/dev/null || skip "yq not available"
  command -v jq &>/dev/null || skip "jq not available"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
}
# _payload_for_version [<the YAML text of spec.kubernetes.version>] — no
# argument: the spec has no spec.kubernetes key. Prints the compact payload.
_payload_for_version() {
  local spec="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" block="" out
  if (( $# )); then
    block="  kubernetes:"$'\n'"    version: ${1}"$'\n'
  fi
  printf 'kind: KubeOne\nspec:\n  cluster:\n    domain: test.kubehz.dev\n%s  kubehz:\n    hosting: hosted\n' "${block}" > "${spec}"
  out=$(kubehz::build_cluster_payload "${spec}") || return 1
  jq -c . <<<"${out}"
}
_PAYLOAD_UNSET='{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"fsn1","controlPlaneReplicas":1,"hosting":"hosted","access":"none"}'

@test "build_cluster_payload: no spec.kubernetes.version key leaves kubernetesVersion out" {
  _payload_setup
  local got
  got=$(_payload_for_version)
  assert_equal "${got}" "${_PAYLOAD_UNSET}"
}

@test "build_cluster_payload: a null spec.kubernetes.version leaves kubernetesVersion out" {
  _payload_setup
  local got value
  for value in null '~' ''; do
    got=$(_payload_for_version "${value}")
    assert_equal "${got}" "${_PAYLOAD_UNSET}"
  done
}

@test "build_cluster_payload: an empty spec.kubernetes.version leaves kubernetesVersion out" {
  _payload_setup
  local got
  got=$(_payload_for_version '""')
  assert_equal "${got}" "${_PAYLOAD_UNSET}"
}

# `//` fires on false too: a read that drops only the word "null" would
# send the string "false".
@test "build_cluster_payload: a false spec.kubernetes.version leaves kubernetesVersion out" {
  _payload_setup
  local got
  got=$(_payload_for_version false)
  assert_equal "${got}" "${_PAYLOAD_UNSET}"
}

@test "build_cluster_payload: a set spec.kubernetes.version is sent as written" {
  _payload_setup
  local got
  got=$(_payload_for_version v1.31.10)
  assert_equal "${got}" '{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"fsn1","kubernetesVersion":"v1.31.10","controlPlaneReplicas":1,"hosting":"hosted","access":"none"}'
}

# A number is not unset: 0 is sent as the string "0", and 1.30 keeps its
# text (it is not read as the number 1.3).
@test "build_cluster_payload: a number spec.kubernetes.version is sent as a string" {
  _payload_setup
  local got value
  for value in 0 1.30; do
    got=$(_payload_for_version "${value}")
    assert_equal "${got}" '{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"fsn1","kubernetesVersion":"'"${value}"'","controlPlaneReplicas":1,"hosting":"hosted","access":"none"}'
  done
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
      '.spec.kubernetes.version // ""') echo "v1.31.10" ;;
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

  # A set version gets past the refusal of a spec with no version.
  yq() {
    case "$2" in
      '.spec.kubernetes.version // ""') echo "v1.31.10" ;;
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
  assert_output --partial "did not return a cluster ID"
}

# A hosted spec with no spec.kubernetes.version (no key, a YAML null,
# false, an empty string) is refused before any request: the platform
# refuses a create without it. The real yq reads a real spec file. curl
# records each call and fails (a stub that answers would let a provision
# without the refusal poll until its timeout), so without the refusal the
# output and the call log differ at once. Go twin:
# TestProvisionHostedRefusesASpecWithNoVersion.
@test "provision_hosted: a spec with no spec.kubernetes.version is refused before any request" {
  command -v yq &>/dev/null || skip "yq not available"
  local spec="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml" value block
  curl() {
    echo "curl $*" >> "${BATS_TEST_TMPDIR}/curl.calls"
    return 7
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  for value in no-key null false '""'; do
    block=""
    if [[ "${value}" != no-key ]]; then
      block="  kubernetes:"$'\n'"    version: ${value}"$'\n'
    fi
    printf 'kind: KubeOne\nmetadata:\n  name: test.kubehz.dev\nspec:\n  cluster:\n    domain: test.kubehz.dev\n%s  kubehz:\n    hosting: hosted\n' "${block}" > "${spec}"
    run kubehz::provision_hosted "test.kubehz.dev" "${spec}"
    assert_failure
    assert_output "[error] kubehz: spec.kubernetes.version is not set in ${spec}. A hosted cluster needs a Kubernetes version, so no cluster was created. Set it, then run the command again."
  done
  [[ ! -e "${BATS_TEST_TMPDIR}/curl.calls" ]]
  [[ ! -e "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]]
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
      '.spec.kubernetes.version // ""') echo "v1.31.10" ;;
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
      '.spec.kubernetes.version // ""') echo "v1.31.10" ;;
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
      '.spec.kubernetes.version // ""') echo "v1.31.10" ;;
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

# The mode is checked while the content is written too (the stdout of the
# download): a write in place through the link would write into the old
# 0644 file, and a chmod after it would make the end mode 0600 all the same.
@test "provision_hosted: a symlinked kubeconfig keeps the link and gets the new content, mode 0600" {
  [[ -e "/proc/${BASHPID}/fd/1" ]] || skip "no /proc/<pid>/fd"
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "test.kubehz.dev" ;; *) echo "1" ;; esac; }
  export KC_MODES="${BATS_TEST_TMPDIR}/modes"
  curl() {
    if [[ "$*" == *"/api/clusters/"*"/kubeconfig"* ]]; then
      stat -L -c %a "/proc/${BASHPID}/fd/1" >> "${KC_MODES}"
      echo "kc-bytes"
      return 0
    fi
    printf '{"data":{"id":"cl-1","status":"Running"}}\n201'
  }
  export -f curl
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
  [ "$(cat "${KC_MODES}")" = 600 ]
  [ -z "$(_hosted_temps "${real}" test.kubehz.dev.yaml)" ]
  [ -z "$(_hosted_temps "${PATH_BASE}/.kubeconfig" test.kubehz.dev.yaml)" ]
}

# A kubeconfig path that the write refuses (a directory, a link to one, a
# link to nothing, a directory that another user can write into) stops
# the provision before the create: the refusal
# creates no cluster (a re-run would create another one), and no
# download starts.
@test "provision_hosted: a refused kubeconfig path stops the provision before the create" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "test.kubehz.dev" ;; *) echo "1" ;; esac; }
  export CURL_CALLS="${BATS_TEST_TMPDIR}/curl.calls"
  curl() { printf '%s\n' "$*" >> "${CURL_CALLS}"; printf '{"data":{"id":"cl-1","status":"Running"}}\n201'; }
  export -f curl
  local kc="${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml"
  mkdir -p "${kc}"
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_failure
  assert_output "ERR: ${kc} is a directory, not a file"
  [ ! -e "${CURL_CALLS}" ]
  rmdir "${kc}"
  mkdir -p "${PATH_BASE}/realdir"
  ln -s "${PATH_BASE}/realdir" "${kc}"
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_failure
  assert_output "ERR: ${kc} is a directory, not a file"
  [ ! -e "${CURL_CALLS}" ]
  [ -z "$(ls -A "${PATH_BASE}/realdir")" ]
  rm -- "${kc}"
  ln -s "${PATH_BASE}/none.yaml" "${kc}"
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  assert_failure
  assert_output "ERR: Cannot resolve the symlink ${kc}"
  [ ! -e "${CURL_CALLS}" ]
  rm -- "${kc}"
  chmod 0777 "${PATH_BASE}/.kubeconfig"
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  chmod 0755 "${PATH_BASE}/.kubeconfig"
  assert_failure
  assert_output "ERR: $(_dir_refused world "${PATH_BASE}/.kubeconfig" | sed 's/^\[error\] //')"
  [ ! -e "${CURL_CALLS}" ]
  [ ! -e "${kc}" ]
}

# Under umask 000, mkdir -p would make the .kubeconfig directory with mode
# 0777, and the check before the temp file would refuse it after the
# create. The write makes it with mode 0700, so the provision goes on:
# the file is there (mode 0600), and the directory has mode 0700.
@test "provision_hosted: under umask 000, a new .kubeconfig directory gets mode 0700 and the write goes on" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "test.kubehz.dev" ;; *) echo "1" ;; esac; }
  local old
  # setup makes the directory (empty): the write must make it here.
  rmdir "${PATH_BASE}/.kubeconfig"
  old=$(umask)
  umask 000
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  umask "${old}"
  assert_success
  [ "$(stat -c %a "${PATH_BASE}/.kubeconfig")" = 700 ]
  [ "$(cat "${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml")" = kc-bytes ]
  [ "$(stat -c %a "${PATH_BASE}/.kubeconfig/test.kubehz.dev.yaml")" = 600 ]
}

@test "kubehz::_write_private: fewer than two arguments are refused, and nothing is written" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local f="${BATS_TEST_TMPDIR}/kc.yaml"
  printf 'old\n' > "${f}"
  run kubehz::_write_private "${f}"
  assert_failure
  assert_output --partial "kubehz::_write_private: it takes a file and a command (<dest> <command...>), not 1 argument(s); it writes nothing"
  [ "${#lines[@]}" -eq 1 ]
  run kubehz::_write_private
  assert_failure
  assert_output --partial "not 0 argument(s)"
  [ "$(cat "${f}")" = old ]
  [ -z "$(_hosted_temps "${BATS_TEST_TMPDIR}" kc.yaml)" ]
}

# Another operation puts a directory at the path during the write (the old
# file goes aside): with mv -T the rename fails on it, and without -T the
# check just before the rename refuses it. Nothing lands in the directory,
# the old file is as it was, no temp file stays, and one line names it.
@test "kubehz::_write_private: a path that becomes a directory during the write gets nothing, with mv -T and without" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" aside="${BATS_TEST_TMPDIR}/aside.yaml" exact
  swap() { printf new; mv -- "${kc}" "${aside}"; mkdir -- "${kc}"; }
  for exact in 1 0; do
    rm -rf -- "${kc:?}" "${aside:?}"
    printf 'old\n' > "${kc}"
    _KUBEHZ_MV_EXACT="${exact}"
    run kubehz::_write_private "${kc}" swap
    assert_failure
    assert_output --partial "${kc} is a directory, not a file"
    [ "${#lines[@]}" -eq 1 ] || { echo "mv -T ${exact}: ${output}" >&2; return 1; }
    [ -z "$(find "${kc}" -mindepth 1)" ]
    [ "$(cat "${aside}")" = old ]
    [ -z "$(_hosted_temps "${BATS_TEST_TMPDIR}" kc.yaml)" ]
  done
}

# Without mv -T, a directory can still come between the check and the
# rename (here the mv itself puts it there first): mv then moves the new
# file into it. The check after the rename finds no regular file at the
# path and fails with one line that names the path where the rename may
# have put the new file. That file stays (mode 0600), and nothing is
# removed: the mv here also puts another file at the old temp path, and
# the cleanup after the failed rename keeps it (it is not the file of the
# descriptor).
@test "kubehz::_write_private: without mv -T, a directory just before the rename keeps the new file, and the error names it" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" aside="${BATS_TEST_TMPDIR}/aside.yaml"
  local seen="${BATS_TEST_TMPDIR}/temp-path" f
  printf 'old\n' > "${kc}"
  _KUBEHZ_MV_EXACT=0
  mv() {
    if [[ -f "${kc}" ]]; then command mv -- "${kc}" "${aside}" && mkdir -- "${kc}"; fi
    command mv "$@" || return
    printf '%s\n' "${@: -2:1}" > "${seen}"
    printf other > "${@: -2:1}"
  }
  run kubehz::_write_private "${kc}" printf new
  assert_failure
  f=$(find "${kc}" -mindepth 1)
  [ -n "${f}" ]
  [ "$(cat "${f}")" = new ]
  [ "$(stat -c %a "${f}")" = 600 ]
  assert_output "[error] ${kc} is not a regular file after the rename: the rename may have moved the new file to ${f}"
  [ "$(cat "${aside}")" = old ]
  [ "$(cat "$(cat "${seen}")")" = other ]
}

# _hosted_moved_run <kc> <aside> <signal> <prelude> <step>: run
# kubehz::_write_private on <kc> without mv -T, in a new shell (bats runs
# it). The mv of that shell puts a directory at <kc> (the old file goes to
# <aside>), renames the temp file into it, runs <step> (shell code;
# test_f is the moved file), writes the number of files in <kc> to
# <aside>.seen, and then sends <signal> to the shell, before the check
# after the rename. <prelude> is shell code that runs after the library
# is sourced. The call of mv has 2>/dev/null: a real mv is a child
# process, and a trap runs after it ends, without that redirection. The
# mv here is a function, so it sends the signal with the stderr of the
# shell (fd 9).
_hosted_moved_run() {
  run bash -c 'exec 9>&2
import() { :; }
source "$6/utils/verbose.sh"
source "$6/utils/http.sh"
source "$6/libs/kubehz/hosted"
test_kc="$1" test_aside="$2" test_signal="$3" test_step="$5"
eval "$4"
_KUBEHZ_MV_EXACT=0
mv() {
  command mv -- "${test_kc}" "${test_aside}" && mkdir -- "${test_kc}" && command mv "$@" || return
  test_f=$(find "${test_kc}" -mindepth 1 -maxdepth 1)
  eval "${test_step}" || return
  find "${test_kc}" -mindepth 1 | wc -l > "${test_aside}.seen"
  { kill -s "${test_signal}" "$$"; } 2>&9
}
kubehz::_write_private "${test_kc}" printf new
echo "returned $?"' bash "$1" "$2" "$3" "$4" "$5" "${_PROJECT_ROOT}/.lok8s"
}

# Without mv -T, a TERM or a HUP during the rename into a directory that
# took the place of the file: the trap only records the signal until the
# rename and its check are done. The check names the path where the
# rename may have put the new file in one line, nothing in the directory
# is removed (the file stays, mode 0600), and then the shell ends with the
# signal.
@test "kubehz::_write_private: without mv -T, TERM and HUP during the rename into a directory name the moved file and remove nothing" {
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" aside="${BATS_TEST_TMPDIR}/aside.yaml" pair sig code f
  for pair in TERM:143 HUP:129; do
    sig="${pair%:*}" code="${pair#*:}"
    rm -rf -- "${kc:?}" "${aside:?}" "${aside}.seen"
    printf 'old\n' > "${kc}"
    _hosted_moved_run "${kc}" "${aside}" "${sig}" ':' ':'
    [ "${status}" -eq "${code}" ] || { echo "${sig}: status ${status}, want ${code}: ${output}" >&2; return 1; }
    refute_output --partial returned
    # The rename put the new file into the directory before the signal.
    [ "$(cat "${aside}.seen")" -eq 1 ]
    f=$(find "${kc}" -mindepth 1)
    [ -n "${f}" ] && [ "$(cat "${f}")" = new ] || { echo "${sig}: in ${kc}: $(ls -A "${kc}")" >&2; return 1; }
    [ "$(stat -c %a "${f}")" = 600 ]
    assert_output "[error] ${kc} is not a regular file after the rename: the rename may have moved the new file to ${f}"
    [ "$(cat "${aside}")" = old ]
    [ -z "$(_hosted_temps "${BATS_TEST_TMPDIR}" kc.yaml)" ]
  done
}

# A signal during that rename removes no file in that directory, also not
# one that another process put there. Here another file (of this user,
# mode 0600) takes the place of the moved one before the signal: it stays,
# and the line names its path (the line does not say whose file is there).
@test "kubehz::_write_private: the traps leave a file in that directory that this run did not make" {
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" aside="${BATS_TEST_TMPDIR}/aside.yaml" files
  printf 'old\n' > "${kc}"
  _hosted_moved_run "${kc}" "${aside}" TERM ':' \
    'printf other > "${test_aside}.other" && chmod 600 "${test_aside}.other" && command mv -f -- "${test_aside}.other" "${test_f}"'
  [ "${status}" -eq 143 ] || { echo "status ${status}, want 143: ${output}" >&2; return 1; }
  [ "$(cat "${aside}.seen")" -eq 1 ]
  files=("${kc}"/*)
  [ "${#files[@]}" -eq 1 ]
  [ "$(cat "${files[0]}")" = other ]
  assert_output "[error] ${kc} is not a regular file after the rename: the rename may have moved the new file to ${files[0]}"
}

# A TERM or a HUP after the rename, with a file that another process put
# at the old temp path: that file stays, with mv -T and without. The mv
# of the new shell renames the temp file, puts the other file at the old
# path (a new file for TERM, a hard link to the renamed file for HUP: the
# same file as the descriptor) and then sends the signal. The new file is
# in place, and the shell ends with the signal. The mv also records the
# path that the traps remove at that time: it is empty (the call clears
# it before the rename), so no trap can remove that file.
@test "kubehz::_write_private: a signal after the rename keeps a file that another process put at the old temp path, with mv -T and without" {
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" exact pair sig code old
  for exact in 1 0; do
    for pair in TERM:143 HUP:129; do
      sig="${pair%:*}" code="${pair#*:}"
      rm -f -- "${kc}" "${kc}.old" "${kc}.trap"
      printf 'old\n' > "${kc}"
      run bash -c 'import() { :; }
source "$4/utils/verbose.sh"
source "$4/utils/http.sh"
source "$4/libs/kubehz/hosted"
test_signal="$2"
_KUBEHZ_MV_EXACT="$3"
mv() {
  command mv "$@" || return
  printf "%s\n" "${@: -2:1}" > "${@: -1}.old"
  printf "%s" "${_kubehz_wp_tmp-unset}" > "${@: -1}.trap"
  if [ "${test_signal}" = TERM ]; then
    printf other > "${@: -2:1}"
  else
    ln -- "${@: -1}" "${@: -2:1}"
  fi
  { kill -s "${test_signal}" "$$"; } 2>&9
}
exec 9>&2
kubehz::_write_private "$1" printf new
echo "returned $?"' bash "${kc}" "${sig}" "${exact}" "${_PROJECT_ROOT}/.lok8s"
      [ "${status}" -eq "${code}" ] || { echo "mv -T ${exact} ${sig}: status ${status}, want ${code}: ${output}" >&2; return 1; }
      assert_output ""
      [ "$(cat "${kc}")" = new ]
      [ -f "${kc}.trap" ] && [ ! -s "${kc}.trap" ] || { echo "mv -T ${exact} ${sig}: the traps read <$(cat "${kc}.trap")>" >&2; return 1; }
      old=$(cat "${kc}.old")
      [ -e "${old}" ] || { echo "mv -T ${exact} ${sig}: ${old} is gone" >&2; return 1; }
      if [ "${sig}" = TERM ]; then [ "$(cat "${old}")" = other ]; else [ "${old}" -ef "${kc}" ]; fi
      rm -f -- "${old}"
    done
  done
}

# The write goes into the descriptor of the create, not to the path. Each
# case puts a link at the temp path (to a 0644 file that another process
# can read) after the create: at the end of the check of the open file
# (the find of the mode check: so the link is there when the command
# starts), and while the command runs. The linked file does not get the
# content, the rename refuses the temp path (it is not the file of the
# descriptor), the old file stays, and the link stays (another process
# made it). The last case: a TERM after the swap, and the trap removes no
# link that another process made.
@test "kubehz::_write_private: a link that takes the place of the temp file after the create does not get the content" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" other="${BATS_TEST_TMPDIR}/other" t at
  export TEST_OTHER="${other}" TEST_KC="${kc}"
  _hosted_swap_link() {
    local f
    for f in "${TEST_KC}".*; do rm -f -- "${f}" && ln -s -- "${TEST_OTHER}" "${f}"; done
  }
  for at in check command; do
    printf 'old\n' > "${kc}"
    : > "${other}"
    chmod 644 "${other}"
    if [ "${at}" = check ]; then
      find() { command find "$@"; local rc=$?; _hosted_swap_link; return "${rc}"; }
      run kubehz::_write_private "${kc}" printf 'kc-bytes\n'
      unset -f find
    else
      swap() { _hosted_swap_link; printf 'kc-bytes\n'; }
      run kubehz::_write_private "${kc}" swap
    fi
    assert_failure
    t=$(_hosted_temps "${BATS_TEST_TMPDIR}" kc.yaml)
    [ -L "${t}" ] || { echo "${at}: no link at the temp path: ${t}" >&2; return 1; }
    [ "$(readlink "${t}")" = "${other}" ]
    assert_output "[error] The temp file ${t} is not the file that this run wrote: nothing is renamed to ${kc}"
    [ ! -s "${other}" ] || { echo "${at}: the linked file got the content" >&2; return 1; }
    [ "$(cat "${kc}")" = old ]
    [ ! -L "${kc}" ]
    rm -f -- "${t}"
  done
  run bash -c 'import() { :; }
source "$4/utils/verbose.sh"
source "$4/utils/http.sh"
source "$4/libs/kubehz/hosted"
test_other="$2" test_kc="$1" test_seen="$3"
swap() {
  local f
  for f in "${test_kc}".*; do rm -f -- "${f}" && ln -s -- "${test_other}" "${f}" && printf "%s\n" "${f}" > "${test_seen}"; done
  printf "kc-bytes\n"
  kill -s TERM "$$"
}
kubehz::_write_private "$1" swap
echo "returned $?"' bash "${kc}" "${other}" "${BATS_TEST_TMPDIR}/seen" "${_PROJECT_ROOT}/.lok8s"
  [ "${status}" -eq 143 ] || { echo "status ${status}, want 143: ${output}" >&2; return 1; }
  assert_output ""
  t=$(cat "${BATS_TEST_TMPDIR}/seen")
  [ -L "${t}" ] || { echo "the trap removed the link ${t}" >&2; return 1; }
  [ "$(readlink "${t}")" = "${other}" ]
  [ ! -s "${other}" ]
  [ "$(cat "${kc}")" = old ]
}

# The check after the create: the open file must have mode 0600. Here the
# umask of the create is 022 (a umask function in place of the builtin),
# so the new file has mode 0644: the call writes nothing, removes the temp
# file (the file of this run) and keeps the old file.
@test "kubehz::_write_private: a temp file that the create does not make with mode 0600 gets nothing" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" ran="${BATS_TEST_TMPDIR}/ran"
  printf 'old\n' > "${kc}"
  umask() { if [ "${1:-}" = 077 ]; then builtin umask 022; else builtin umask "$@"; fi; }
  run kubehz::_write_private "${kc}" touch "${ran}"
  unset -f umask
  assert_failure
  assert_output --regexp "^\\[error\\] The temp file ${kc}\.[A-Za-z0-9]{6} is not a new file of this user with mode 0600: nothing is written$"
  [ ! -e "${ran}" ]
  [ "$(cat "${kc}")" = old ]
  [ -z "$(_hosted_temps "${BATS_TEST_TMPDIR}" kc.yaml)" ]
}

# The probe of mv -T runs once, in a temp directory of its own that it
# removes. A mv without -T (the BSD mv of macOS) selects the check before
# the rename, and a write through that path works.
@test "kubehz::_mv_exact_ok: probes mv once, and a mv without -T selects the directory check" {
  mv --help 2>&1 | grep -q -- '-T' || skip "the mv of this host has no -T"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local probe="${BATS_TEST_TMPDIR}/probe" kc="${BATS_TEST_TMPDIR}/kc.yaml" rc=0
  mkdir -p "${probe}"
  _KUBEHZ_MV_EXACT=""
  TMPDIR="${probe}" kubehz::_mv_exact_ok
  [ "${_KUBEHZ_MV_EXACT}" = 1 ]
  [ -z "$(ls -A "${probe}")" ]
  mv() {
    local a
    for a in "$@"; do
      [[ "${a}" != -T ]] || { echo "mv: illegal option -- T" >&2; return 64; }
    done
    command mv "$@"
  }
  _KUBEHZ_MV_EXACT=""
  TMPDIR="${probe}" kubehz::_mv_exact_ok || rc=$?
  [ "${rc}" -eq 1 ]
  [ "${_KUBEHZ_MV_EXACT}" = 0 ]
  [ -z "$(ls -A "${probe}")" ]
  printf 'old\n' > "${kc}"
  kubehz::_write_private "${kc}" printf new
  [ "$(cat "${kc}")" = new ]
  [ "$(stat -c %a "${kc}")" = 600 ]
  [ -z "$(_hosted_temps "${BATS_TEST_TMPDIR}" kc.yaml)" ]
}

# The refusals come before the temp file and before the command (the
# download): the command here makes a file, and it never runs.
@test "kubehz::_write_private refuses a link to nothing, a directory and a link to a directory before the command, and leaves no temp file" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/wp" ran="${BATS_TEST_TMPDIR}/ran"
  mkdir -p "${d}/sub.yaml" "${d}/real"
  # The directory of the target exists: readlink -f alone would resolve it.
  ln -s "${d}/none.yaml" "${d}/dangling.yaml"
  ln -s "${d}/real" "${d}/dirlink.yaml"
  run kubehz::_write_private "${d}/dangling.yaml" touch "${ran}"
  assert_failure
  assert_output --partial "Cannot resolve the symlink ${d}/dangling.yaml"
  run kubehz::_write_private "${d}/sub.yaml" touch "${ran}"
  assert_failure
  assert_output --partial "${d}/sub.yaml is a directory, not a file"
  run kubehz::_write_private "${d}/dirlink.yaml" touch "${ran}"
  assert_failure
  assert_output --partial "${d}/dirlink.yaml is a directory, not a file"
  [ ! -e "${ran}" ]
  [ "$(find "${d}" -mindepth 1 -maxdepth 1 | wc -l)" -eq 4 ]
  [ -z "$(ls -A "${d}/sub.yaml")" ]
  [ -z "$(ls -A "${d}/real")" ]
}

# ── a directory that another user can write into ─────────────────────────
# The fixtures are the same as in internal/kubehz/hosted_dir_unix_test.go.

# _dir_ids: replace id with a function that gives FAKE_UID for id -u (a
# value that is set but empty is given as it is), else the values of id.
# An option in FAKE_FAIL (such as "-u") prints its value and fails.
_dir_ids() {
  id() {
    case "${1:-}" in
      -u) echo "${FAKE_UID-$(command id -u)}" ;;
      *) command id "$@"; return ;;
    esac
    [[ " ${FAKE_FAIL:-} " != *" ${1} "* ]]
  }
}

# _dir_refused <kind> <path> [<uid>]: the error line of a refused path.
_dir_refused() {
  case "${1}" in
    owner) printf '[error] The directory %s belongs to uid %s, not to this user or to root, so another user can replace a file in it: nothing is written. Use a directory of your own.' "${2}" "${3}" ;;
    world) printf '[error] The directory %s is world-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.' "${2}" "${2}" ;;
    group) printf '[error] The directory %s is group-writable and has no sticky bit, so another user can replace a file in it: nothing is written. Run chmod o-w,g-w %s, or use a directory of your own.' "${2}" "${2}" ;;
    sticky) printf '[error] The directory %s has the sticky bit and other users can write into it, but this mv has no -T, so another user can put a directory at the file before the rename: nothing is written. Use a directory of your own, or a mv that takes -T (GNU coreutils).' "${2}" ;;
    notdir) printf '[error] The path %s is not a directory: nothing is written' "${2}" ;;
    unread) printf '[error] Cannot read the owner and the mode of the directory %s: nothing is written' "${2}" ;;
    link) printf '[error] The link %s belongs to uid %s, not to this user or to root, so another user can point it at a file of this user: nothing is written. Remove the link, or use a path of your own.' "${2}" "${3}" ;;
    linkread) printf '[error] Cannot read the owner of the link %s: nothing is written' "${2}" ;;
    resolve) printf '[error] Cannot resolve the symlink %s' "${2}" ;;
  esac
}

# _link_owner_ls <path> <uid>: replace ls with a function that gives, for
# ls -ldn <path> (the owner of the link itself), the line of a link of
# <uid> (gid 5353), and no line when <uid> is empty. Any other call runs
# ls.
_link_owner_ls() {
  FAKE_LINK="${1}" FAKE_LINK_UID="${2}"
  ls() {
    if [[ "${1}" == -ldn && "${!#}" == "${FAKE_LINK}" ]]; then
      [[ -z "${FAKE_LINK_UID}" ]] || printf 'lrwxrwxrwx 1 %s 5353 9 Nov 20 08:00 %s -> x\n' "${FAKE_LINK_UID}" "${FAKE_LINK}"
      return 0
    fi
    command ls "$@"
  }
}

# A world-writable directory without the sticky bit is refused before the
# temp file and before the command: the command makes a file, and it never
# runs. In a directory of this user only (0700), the write goes on (a
# directory with the sticky bit has a test of its own). A link to a
# directory gets the check of the directory that it names, where the
# create goes.
@test "kubehz::_write_private refuses a world-writable directory without the sticky bit before the command, and writes nothing" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/dirs" ran="${BATS_TEST_TMPDIR}/ran" mode
  for mode in 0777 0700; do
    mkdir -p "${d}/m${mode}"
    printf 'old\n' > "${d}/m${mode}/kc.yaml"
    chmod "${mode}" "${d}/m${mode}"
  done
  run kubehz::_write_private "${d}/m0777/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused world "${d}/m0777")"
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/m0777/kc.yaml")" = old ]
  [ -z "$(_hosted_temps "${d}/m0777" kc.yaml)" ]
  for mode in 0700; do
    run kubehz::_write_private "${d}/m${mode}/kc.yaml" printf new
    assert_success
    assert_output ""
    [ "$(cat "${d}/m${mode}/kc.yaml")" = new ]
    [ "$(stat -c %a "${d}/m${mode}/kc.yaml")" = 600 ]
    [ -z "$(_hosted_temps "${d}/m${mode}" kc.yaml)" ]
  done
  ln -s m0700 "${d}/l0700"
  ln -s m0777 "${d}/l0777"
  run kubehz::_private_dir_ok "${d}/l0700"
  assert_success
  run kubehz::_private_dir_ok "${d}/l0777"
  assert_failure
  assert_output "$(_dir_refused world "${d}/l0777")"
}

# A directory with the sticky bit that other users can write into (1777,
# 1770 for the group, 1757 for the others): another user cannot rename or
# remove a file of this user there, but can put a directory at the name of
# the file. With
# mv -T the rename fails on that directory, so the write goes on. A mv
# without -T would move the file into it: the call refuses such a
# directory before the command, and writes nothing. A directory with the
# sticky bit that only its owner can write into (1700) passes without -T.
@test "kubehz::_write_private writes into a sticky directory that others can write into only with mv -T" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/sticky" ran="${BATS_TEST_TMPDIR}/ran" mode
  for mode in 1777 1770 1757 1700; do
    mkdir -p "${d}/m${mode}"
    printf 'old\n' > "${d}/m${mode}/kc.yaml"
    chmod "${mode}" "${d}/m${mode}"
  done
  _KUBEHZ_MV_EXACT=0
  for mode in 1777 1770 1757; do
    run kubehz::_write_private "${d}/m${mode}/kc.yaml" touch "${ran}"
    assert_failure
    assert_output "$(_dir_refused sticky "${d}/m${mode}")"
    [ ! -e "${ran}" ]
    [ "$(cat "${d}/m${mode}/kc.yaml")" = old ]
    [ -z "$(_hosted_temps "${d}/m${mode}" kc.yaml)" ]
  done
  run kubehz::_write_private "${d}/m1700/kc.yaml" printf new
  assert_success
  assert_output ""
  [ "$(cat "${d}/m1700/kc.yaml")" = new ]
  _KUBEHZ_MV_EXACT=""
  kubehz::_mv_exact_ok || skip "the mv of this host has no -T"
  for mode in 1777 1770 1757; do
    run kubehz::_write_private "${d}/m${mode}/kc.yaml" printf new
    assert_success
    assert_output ""
    [ "$(cat "${d}/m${mode}/kc.yaml")" = new ]
    [ "$(stat -c %a "${d}/m${mode}/kc.yaml")" = 600 ]
    [ -z "$(_hosted_temps "${d}/m${mode}" kc.yaml)" ]
  done
}

# A group-writable directory (0775) of another group of this user is
# refused: that group can have other members.
@test "kubehz::_write_private refuses a group-writable directory of another group of this user, and writes nothing" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/grp" ran="${BATS_TEST_TMPDIR}/ran" g other=""
  for g in $(id -G); do
    [[ "${g}" == "$(id -g)" ]] || { other="${g}"; break; }
  done
  [[ -n "${other}" ]] || skip "this user is in no group other than its primary group"
  mkdir -p "${d}"
  printf 'old\n' > "${d}/kc.yaml"
  chgrp "${other}" "${d}"
  chmod 0775 "${d}"
  run kubehz::_write_private "${d}/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused group "${d}")"
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/kc.yaml")" = old ]
  [ -z "$(_hosted_temps "${d}" kc.yaml)" ]
}

# A group-writable directory (0775) of the primary group of this user is
# refused too: the check does not ask who else is in the group. (The
# write makes the .kubeconfig directory with mode 0700: see the test of
# provision_hosted under umask 000.)
@test "kubehz::_write_private refuses a group-writable directory of the primary group of this user, and writes nothing" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/upg" ran="${BATS_TEST_TMPDIR}/ran"
  mkdir -p "${d}"
  chgrp "$(id -g)" "${d}"
  chmod 0775 "${d}"
  printf 'old\n' > "${d}/kc.yaml"
  run kubehz::_write_private "${d}/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused group "${d}")"
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/kc.yaml")" = old ]
  [ -z "$(_hosted_temps "${d}" kc.yaml)" ]
}

# The owner of the directory and the uid of the process decide (id is
# faked here). A directory of another user is refused also with the
# sticky bit, and so is a directory of a user other than root when id -u
# fails. A directory that is not there passes only when its parent is a
# directory that this user can search: else the check cannot tell that it
# is not there (a path below a file, below a path that is not there, or
# below a directory that this user cannot search). A path that is there
# but is not a directory (a file, a link to nothing) is refused. A
# directory of root passes.
@test "kubehz::_private_dir_ok: the owner, a path that is not there, and a path that is not a directory" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  _dir_ids
  local d="${BATS_TEST_TMPDIR}/ids" uid
  mkdir -p "${d}"
  chmod 0700 "${d}"
  uid=$(stat -c %u "${d}")
  FAKE_UID="${uid}"
  run kubehz::_private_dir_ok "${d}"
  assert_success
  assert_output ""
  if (( uid != 0 )); then
    FAKE_UID=$((uid + 1))
    run kubehz::_private_dir_ok "${d}"
    assert_failure
    assert_output "$(_dir_refused owner "${d}" "${uid}")"
    chmod 1777 "${d}"
    _KUBEHZ_MV_EXACT=1
    run kubehz::_private_target "${d}/kc.yaml"
    chmod 0700 "${d}"
    assert_failure
    assert_output "$(_dir_refused owner "${d}" "${uid}")"
    FAKE_UID="${uid}" FAKE_FAIL=-u
    run kubehz::_private_dir_ok "${d}"
    assert_failure
    assert_output "$(_dir_refused owner "${d}" "${uid}")"
    unset FAKE_FAIL
  fi
  unset FAKE_UID
  run kubehz::_private_target "${BATS_TEST_TMPDIR}/none/kc.yaml"
  assert_success
  assert_output "${BATS_TEST_TMPDIR}/none/kc.yaml"
  printf 'x\n' > "${d}/file"
  ln -s "${d}/nowhere" "${d}/dangling"
  run kubehz::_private_dir_ok "${d}/file"
  assert_failure
  assert_output "$(_dir_refused notdir "${d}/file")"
  run kubehz::_private_target "${d}/file/kc.yaml"
  assert_failure
  assert_output "$(_dir_refused notdir "${d}/file")"
  run kubehz::_private_dir_ok "${d}/dangling"
  assert_failure
  assert_output "$(_dir_refused notdir "${d}/dangling")"
  run kubehz::_private_dir_ok "${d}/file/sub"
  assert_failure
  assert_output "$(_dir_refused unread "${d}/file/sub")"
  run kubehz::_private_dir_ok "${BATS_TEST_TMPDIR}/none/sub"
  assert_failure
  assert_output "$(_dir_refused unread "${BATS_TEST_TMPDIR}/none/sub")"
  if (( $(id -u) != 0 )); then
    mkdir "${d}/closed"
    chmod 0600 "${d}/closed"
    run kubehz::_private_dir_ok "${d}/closed/sub"
    chmod 0700 "${d}/closed"
    assert_failure
    assert_output "$(_dir_refused unread "${d}/closed/sub")"
  fi
  [ "$(stat -c %u /)" = 0 ] || skip "/ does not belong to root here"
  run kubehz::_private_target /lok8s-none.yaml
  assert_success
  assert_output /lok8s-none.yaml
}

# ls -ldnL gives the mode, the uid and the gid in the same form on GNU and
# BSD; the BSD form has more spaces and an @ (extended attributes) or a +
# (an ACL) after the mode. A line that the check cannot read is refused.
# The uid of the process (4242) and the gid of the lines (5353) are fake
# and not the same, so that a check that reads the gid as the owner fails.
@test "kubehz::_private_dir_ok reads the ls line of GNU and of BSD, and refuses a line that it cannot read" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  _dir_ids
  local d="${BATS_TEST_TMPDIR}/ls"
  mkdir -p "${d}"
  FAKE_UID=4242
  _KUBEHZ_MV_EXACT=1
  ls() { printf '%s\n' "${FAKE_LS}"; }
  FAKE_LS="drwxrwxrwx@ 3 4242  5353  96 Nov 20 08:00 ${d}"
  run kubehz::_private_dir_ok "${d}"
  assert_failure
  assert_output "$(_dir_refused world "${d}")"
  FAKE_LS="drwxrwxrwt+ 3 4242  5353  96 Nov 20 08:00 ${d}"
  run kubehz::_private_dir_ok "${d}"
  assert_success
  FAKE_LS="drwx------ 2 4242 5353 4096 Nov 20 08:00 ${d}"
  run kubehz::_private_dir_ok "${d}"
  assert_success
  FAKE_LS="drwx------ 2 5353 4242 4096 Nov 20 08:00 ${d}"
  run kubehz::_private_dir_ok "${d}"
  assert_failure
  assert_output "$(_dir_refused owner "${d}" 5353)"
  FAKE_LS="drwx------ 2 4242 staff 4096 Nov 20 08:00 ${d}"
  run kubehz::_private_dir_ok "${d}"
  assert_failure
  assert_output "$(_dir_refused unread "${d}")"
  FAKE_LS=""
  run kubehz::_private_dir_ok "${d}"
  assert_failure
  assert_output "$(_dir_refused unread "${d}")"
  unset -f ls
}

# In a directory with the sticky bit, another user can put a link at the
# name of the file, to a file of this user: the rename would replace that
# file. So each link that the name follows must belong to this user or to
# root (ls is faked for one link of the chain, uid 4242): a link of
# another user, at the name or as the second link, and a link whose owner
# cannot be read are refused before the command, and the file at the end
# of the chain stays as it was. A link of root passes, and so does a chain
# of links of this user: the write replaces the file at its end, and the
# links stay.
@test "kubehz::_write_private refuses a link of another user in the chain of the name, and follows the links of this user" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/links" ran="${BATS_TEST_TMPDIR}/ran"
  (( $(id -u) != 4242 )) || skip "the uid of this user is the fake uid of the test"
  mkdir -p "${d}"
  chmod 0700 "${d}"
  printf 'rc\n' > "${d}/home.rc"
  ln -s hop "${d}/kc.yaml"
  ln -s "${d}/home.rc" "${d}/hop"
  _link_owner_ls "${d}/kc.yaml" 4242
  run kubehz::_write_private "${d}/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused link "${d}/kc.yaml" 4242)"
  _link_owner_ls "${d}/hop" 4242
  run kubehz::_write_private "${d}/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused link "${d}/hop" 4242)"
  _link_owner_ls "${d}/hop" ""
  run kubehz::_private_target "${d}/kc.yaml"
  assert_failure
  assert_output "$(_dir_refused linkread "${d}/hop")"
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/home.rc")" = rc ]
  _link_owner_ls "${d}/hop" 0
  run kubehz::_private_target "${d}/kc.yaml"
  assert_success
  assert_output "$(readlink -f "${d}/home.rc")"
  unset -f ls
  run kubehz::_write_private "${d}/kc.yaml" printf new
  assert_success
  assert_output ""
  [ "$(cat "${d}/home.rc")" = new ]
  [ "$(stat -c %a "${d}/home.rc")" = 600 ]
  [ -L "${d}/kc.yaml" ]
  [ -L "${d}/hop" ]
}

# A chain of 8 links passes, and a chain of 9 links is refused.
@test "kubehz::_private_target follows a chain of 8 links and refuses a chain of 9" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/chain" i
  mkdir -p "${d}"
  chmod 0700 "${d}"
  printf 'x\n' > "${d}/f"
  ln -s f "${d}/c8"
  for i in 7 6 5 4 3 2 1 0; do
    ln -s "c$((i + 1))" "${d}/c${i}"
  done
  run kubehz::_private_target "${d}/c1"
  assert_success
  assert_output "$(readlink -f "${d}/f")"
  run kubehz::_private_target "${d}/c0"
  assert_failure
  assert_output "$(_dir_refused resolve "${d}/c0")"
}

# A TERM or a HUP during the download: the trap removes the temp file and
# ends the shell with that signal, and the old file stays. The download
# sends the signal to its own shell and then goes on, so without the trap
# the shell would end at once and leave the partial temp file. (INT has
# the same trap. It has no case: a shell that starts with INT ignored, as
# a background job does, cannot trap it.) Here a builtin sends the signal.
# In production the signal comes while curl runs in the pipeline: the trap
# runs when curl ends, and the call sets no time limit (--max-time), so a
# hung download holds the trap.
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

# A TERM or a HUP just after the create of the temp file: the traps are
# set before it, and the path that they read is set before the open, so
# the new temp file goes too. Here the signal comes when the call gives
# the umask of the caller back, the step just after the open (a umask
# function in place of the builtin).
@test "kubehz::_write_private: TERM and HUP just after the create remove the new temp file" {
  local kc="${BATS_TEST_TMPDIR}/kc.yaml" pair sig code
  for pair in TERM:143 HUP:129; do
    sig="${pair%:*}" code="${pair#*:}"
    printf 'old\n' > "${kc}"
    run bash -c 'import() { :; }
source "$3/utils/verbose.sh"
source "$3/utils/http.sh"
source "$3/libs/kubehz/hosted"
test_signal="$2"
umask() { builtin umask "$@"; if [ "$#" -eq 1 ] && [ "$1" != 077 ]; then kill -s "${test_signal}" "$$"; fi; }
kubehz::_write_private "$1" printf new
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

# The restore gives each trap of the caller back at once. A trap function
# here logs each call of trap: no signal that had a trap is set to the
# default on the way (a signal in that window would end the shell and
# not run the trap of the caller). A signal that had no trap goes back to
# the default, and an ignored signal stays ignored. Case all: INT, TERM
# and HUP have traps. Case some: INT has one, TERM has none, HUP is
# ignored.
@test "kubehz::_write_private gives each trap of the caller back at once, and a signal without one goes back to the default" {
  local log="${BATS_TEST_TMPDIR}/trap.log" c want_out want_log
  for c in all some; do
    : > "${log}"
    run bash -c 'import() { :; }
source "$2/utils/verbose.sh"
source "$2/utils/http.sh"
source "$2/libs/kubehz/hosted"
trap "echo caller-int" INT
if [ "$4" = all ]; then trap "echo caller-term" TERM; trap "echo caller-hup" HUP; else trap "" HUP; fi
before=$(trap -p INT TERM HUP)
test_log="$3"
trap() { printf "%s\n" "$*" >> "${test_log}"; builtin trap "$@"; }
kubehz::_write_private "$1" printf new
echo "rc=$?"
unset -f trap
[ "$(trap -p INT TERM HUP)" = "${before}" ] && echo same
trap -p TERM' bash "${BATS_TEST_TMPDIR}/kc.yaml" "${_PROJECT_ROOT}/.lok8s" "${log}" "${c}"
    assert_success
    if [ "${c}" = all ]; then
      want_out="rc=0
same
trap -- 'echo caller-term' SIGTERM"
      want_log=$'-- echo caller-int SIGINT\n-- echo caller-term SIGTERM\n-- echo caller-hup SIGHUP'
    else
      want_out="rc=0
same"
      want_log=$'-- echo caller-int SIGINT\n- TERM\n--  SIGHUP'
    fi
    assert_output "${want_out}"
    # The last three calls of trap are the restore, one per signal.
    [ "$(tail -n 3 "${log}")" = "${want_log}" ] || { echo "${c}: $(cat "${log}")" >&2; return 1; }
    # No other call sets a signal to the default (only TERM of case some).
    [ "$(sed -n '/^- /p' "${log}")" = "$([ "${c}" = all ] || echo '- TERM')" ] || { echo "${c}: $(cat "${log}")" >&2; return 1; }
  done
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

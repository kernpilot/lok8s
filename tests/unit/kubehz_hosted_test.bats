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

@test "wait_for_cluster: a status read that fails reads as Unknown, and the wait goes on under set -e" {
  curl() { return 22; }
  export -f curl
  sleep() { :; }
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  export KUBEHZ_TOKEN="test-token"
  # lo runs under set -euo pipefail: a failed read must not end the run.
  _wait_errexit() {
    set -euo pipefail
    kubehz::wait_for_cluster "https://api.kubehz.dev" "cl-001" 20
  }
  run _wait_errexit
  assert_failure
  assert_output --partial "Timed out waiting for hosted cluster cl-001 after 20s"
}

# ── provision_hosted ─────────────────────────────────────

# _hosted_no_cluster: the cluster list shows no cluster of the domain
# (kubehz::_hosted_listed rc 2). For the tests of the create after the
# list; the list has its own tests below.
_hosted_no_cluster() { kubehz::_hosted_listed() { return 2; }; }

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
  _hosted_no_cluster

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
  _hosted_no_cluster

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
  _hosted_no_cluster

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
  _hosted_no_cluster

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
  _hosted_no_cluster

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "HTTP 502"
  assert_output --partial "Failed to schedule the hosted control plane"
}

# ── provision_hosted: one cluster for one domain ─────────
# The list before the create (kubehz::_hosted_listed), the stop on the
# clusters of the domain, the use of the one ready cluster, and the lost
# answer of a create. Real jq and yq read a real spec. The curl stub
# answers by the request: the list from LIST_STATUS/LIST_BODY (the second
# read from LIST2_*), the create from CREATE_RC (a curl exit code) or
# CREATE_STATUS/CREATE_BODY, the status read from STATUS_RC/STATUS_BODY,
# the download from KC_RC. Each call goes to CALLS. Go twins:
# TestProvisionHostedStopsOnAClusterOfTheDomain and the tests after it
# (internal/kubehz/hosted_test.go).

_hosted_list_setup() {
  command -v yq &>/dev/null || skip "yq not available"
  command -v jq &>/dev/null || skip "jq not available"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/node"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev" KUBEHZ_TOKEN="test-token"
  export CALLS="${BATS_TEST_TMPDIR}/calls" LIST_STATUS=200 LIST_BODY='{"ok":true,"data":[]}'
  export LIST2_STATUS="" LIST2_BODY="" CREATE_RC=0 CREATE_STATUS=201 CREATE_BODY='{"data":{"id":"cl-1","status":"Running"}}'
  export STATUS_RC=0 STATUS_BODY='{"data":{"status":"Running"}}' KC_RC=0
  : > "${CALLS}"
  SPEC="${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
  printf 'kind: KubeOne\nmetadata:\n  name: test.kubehz.dev\nspec:\n  cluster:\n    domain: test.kubehz.dev\n  kubernetes:\n    version: v1.31.10\n  kubehz:\n    hosting: hosted\n' > "${SPEC}"
  curl() {
    local a="$*"
    case "${a}" in
      *"/api/clusters?perPage=500"*)
        echo "list ${a##* }" >> "${CALLS}"
        if [[ -n "${LIST2_STATUS}" ]] && (( $(grep -c '^list ' "${CALLS}") > 1 )); then
          [[ "${LIST2_STATUS}" != fail ]] || return 52
          printf '%s\n%s' "${LIST2_BODY}" "${LIST2_STATUS}"
          return 0
        fi
        [[ "${LIST_STATUS}" != fail ]] || return 52
        printf '%s\n%s' "${LIST_BODY}" "${LIST_STATUS}" ;;
      *"-X POST"*)
        echo "create" >> "${CALLS}"
        (( CREATE_RC == 0 )) || return "${CREATE_RC}"
        printf '%s\n%s' "${CREATE_BODY}" "${CREATE_STATUS}" ;;
      *"/kubeconfig"*)
        echo "get kubeconfig" >> "${CALLS}"
        (( KC_RC == 0 )) || return "${KC_RC}"
        printf 'kc\n' ;;
      *)
        echo "get ${a##* }" >> "${CALLS}"
        (( STATUS_RC == 0 )) || return "${STATUS_RC}"
        printf '%s' "${STATUS_BODY}" ;;
    esac
  }
  export -f curl
  kubehz::wait_for_cluster() { echo "wait" >> "${CALLS}"; }
}

# _listed <status|fail> <body> [domain]: one read of the list, as
# rc|ids|hostings|statuses|unconfirmed|cause|scope (each list joined by ",").
_listed() {
  local rc=0
  export LIST_STATUS="${1}" LIST_BODY="${2}"
  kubehz::_hosted_listed "${LOK8S_KUBEHZ_API_URL}" "${3-test.kubehz.dev}" || rc=$?
  local IFS=,
  printf '%s|%s|%s|%s|%s|%s|%s\n' "${rc}" "${KUBEHZ_HOSTED_LIST_IDS[*]}" "${KUBEHZ_HOSTED_LIST_HOSTINGS[*]}" \
    "${KUBEHZ_HOSTED_LIST_STATUSES[*]}" "${KUBEHZ_HOSTED_LIST_UNCONFIRMED[*]}" "${KUBEHZ_HOSTED_LIST_CAUSE}" "${KUBEHZ_HOSTED_LIST_SCOPE}"
}

@test "kubehz::_hosted_listed: the clusters of the domain, the oldest first, none, and each list it cannot read" {
  _hosted_list_setup
  local rows='{"id":"cl-other","domain":"other.example.com","createdAt":"2026-01-01T00:00:00Z"},'
  rows+='{"id":"cl-new","domain":"test.kubehz.dev","hosting":"self","status":"Running","createUnconfirmedAt":null,"createdAt":"2026-03-01T00:00:00Z"},'
  rows+='{"id":"cl-old","domain":"test.kubehz.dev","hosting":"hosted","status":"Pending-Capacity","createUnconfirmedAt":"2026-11-26T10:00:00Z","createdAt":"2026-02-01T00:00:00Z"}'
  assert_equal "$(_listed 200 '{"ok":true,"data":['"${rows}"'],"meta":{"pagination":{"total":3}}}')" "0|cl-old,cl-new|hosted,self|Pending-Capacity,Running|1,0||0"
  # A value in another shape is a server string that lo does not repeat; a
  # createUnconfirmedAt that is not null marks the create as not confirmed.
  assert_equal "$(_listed 200 '{"ok":true,"data":[{"id":"cl-\u001b[2J","domain":"test.kubehz.dev","hosting":"x|y","status":"Running\nx","createUnconfirmedAt":false}]}')" "0||||1||0"
  assert_equal "$(_listed 200 '{"ok":true,"data":[{"id":"cl-'"$(printf 'a%.0s' {1..65})"'","domain":"test.kubehz.dev","hosting":7,"status":"a'"$(printf 'b%.0s' {1..32})"'"}]}')" "0||||0||0"
  assert_equal "$(_listed 200 '{"ok":true,"data":[{"domain":"test.kubehz.dev"}]}')" "0||||0||0"
  assert_equal "$(_listed 200 '[{"id":"cl-bare","domain":"test.kubehz.dev"}]')" "0|cl-bare|||0||0"
  assert_equal "$(_listed 200 '{"ok":true,"data":[]}')" "2||||||0"
  assert_equal "$(_listed 200 '{"ok":true,"data":[{"id":"cl-1a","domain":"other.example.com"},7],"meta":{"pagination":{"total":2}}}')" "2||||||0"
  assert_equal "$(_listed 500 '{"ok":false,"data":{"code":"INTERNAL_ERROR","message":"boom"}}')" "1|||||HTTP 500 INTERNAL_ERROR|0"
  assert_equal "$(_listed 403 '{"ok":false,"data":{"code":"TOKEN_SCOPE_MISSING"}}')" "1|||||HTTP 403 TOKEN_SCOPE_MISSING|1"
  assert_equal "$(_listed 502 '{"data":{"code":"x\u001b[2J"}}')" "1|||||HTTP 502|0"
  assert_equal "$(_listed 200 '{"ok":true,"data":{"id":"cl-1"}}')" "1|||||an answer without a list|0"
  assert_equal "$(_listed 200 '{"ok":true,"data":null}')" "1|||||an answer without a list|0"
  assert_equal "$(_listed 200 '<html>proxy</html>')" "1|||||an answer without a list|0"
  assert_equal "$(_listed 200 '{"ok":true,"data":[{"id":"cl-2b","domain":"other.example.com"}],"meta":{"pagination":{"total":612}}}')" "1|||||it shows 1 of 612 clusters|0"
  # lo cannot tell that the cluster on the page is the only one.
  assert_equal "$(_listed 200 '{"ok":true,"data":[{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Running"}],"meta":{"pagination":{"total":2}}}')" "1|||||it shows 1 of 2 clusters|0"
  assert_equal "$(_listed fail '')" "1|||||no answer|0"
  # A list read clears the mark of an earlier status read that failed.
  KUBEHZ_HOSTED_LIST_UNREAD=(1)
  export LIST_STATUS=200 LIST_BODY='{"ok":true,"data":[{"id":"cl-1a","domain":"test.kubehz.dev"}]}'
  kubehz::_hosted_listed "${LOK8S_KUBEHZ_API_URL}" test.kubehz.dev
  assert_equal "${#KUBEHZ_HOSTED_LIST_UNREAD[@]}" 0
}

@test "kubehz::_hosted_listed: sends ?domain= only for a domain of the shape that the api takes" {
  _hosted_list_setup
  local label63 long253 d
  label63=$(printf 'a%.0s' {1..63})
  long253="${label63}.${label63}.${label63}.$(printf 'b%.0s' {1..61})"
  local -A want=(
    ["test.kubehz.dev"]="&domain=test.kubehz.dev"
    ["a-1.b2.example"]="&domain=a-1.b2.example"
    ["${long253}"]="&domain=${long253}"
    ["${long253}b"]=""
    ["Test.kubehz.dev"]=""
    ["localhost"]=""
    ["-a.example.com"]=""
    ["a-.example.com"]=""
    ["a..example.com"]=""
    ["a.example.com."]=""
    ["a&perPage=1.example.com"]=""
    [$'a.example.com\nb.example']=""
    ["${label63}a.example.com"]=""
    ["null"]=""
  )
  for d in "${!want[@]}"; do
    : > "${CALLS}"
    _listed 200 '{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com"}]}' "${d}" > /dev/null
    assert_equal "$(cat "${CALLS}")" "list https://api.kubehz.dev/api/clusters?perPage=500${want[${d}]}"
  done
  : > "${CALLS}"
  _listed 200 '{"ok":true,"data":[]}' "" > /dev/null
  assert_equal "$(cat "${CALLS}")" "list https://api.kubehz.dev/api/clusters?perPage=500"
}

# _hosted_stop <want>: a provision that stops before the create, with
# <want> as its output, no create, no wait and no download.
_hosted_stop() {
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_failure
  assert_output "${1}"
  refute grep -qE '^(create|wait|get kubeconfig)$' "${CALLS}"
  [[ ! -e "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]]
}

# _hosted_one_row <row>: a list with the row and a cluster of another
# domain, as an api that ignores ?domain= sends it.
_hosted_one_row() {
  export LIST_BODY='{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","hosting":"hosted","status":"Running"},'"${1}"'],"meta":{"pagination":{"total":2}}}'
  : > "${CALLS}"
}

@test "provision_hosted: one cluster of the domain that lo does not use stops the create, and names its id, hosting and status" {
  _hosted_list_setup
  local get='  See the cluster with:  lo kubehz cluster get cl-1a'
  local any='  lo downloads the kubeconfig only when the status is Running, Paused or Updating.'
  local creating='  The cluster is not ready yet. Run the command again when its status is Running.'
  local failed='  kubehz reports an error for the cluster. Contact support with the cluster id.'
  local unread='  lo could not read the status of the cluster. Run the command again.'
  local confirm='  kubehz is still checking the create of this cluster.
  Usually, kubehz keeps or removes the cluster in about 3 minutes.
  When the check fails, kubehz tries again about 15 minutes later.
  Run the command again after that.'
  local stop='[error] kubehz: your account already has cluster cl-1a for test.kubehz.dev'
  local creating_row='{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":null}'
  local status_read='get https://api.kubehz.dev/api/clusters/cl-1a' s body
  local -A step=([Pending-Capacity]="${creating}" [Failed]="${failed}" [Error]="${failed}" [Starting]="${any}")

  # The list keeps the status of the create: lo reads the live one.
  _hosted_one_row "${creating_row}"
  export STATUS_BODY='{"ok":true,"data":{"id":"cl-1a","status":"Deleting"}}'
  _hosted_stop "${stop} (hosted, Deleting), so lo did not create another one.
  kubehz deletes the cluster. After it leaves your cluster list, run the command again: lo then creates a new cluster.
${get}"
  assert_equal "$(cat "${CALLS}")" "list https://api.kubehz.dev/api/clusters?perPage=500&domain=test.kubehz.dev
${status_read}"

  # The live status decides, not the status of the list.
  _hosted_one_row '{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Running","createUnconfirmedAt":null}'
  export STATUS_BODY='{"ok":true,"data":{"id":"cl-1a","status":"Creating"}}'
  _hosted_stop "${stop} (hosted, Creating), so lo did not create another one.
${creating}
${get}"

  for s in "${!step[@]}"; do
    _hosted_one_row "${creating_row}"
    export STATUS_BODY='{"ok":true,"data":{"id":"cl-1a","status":"'"${s}"'"}}'
    _hosted_stop "${stop} (hosted, ${s}), so lo did not create another one.
${step[${s}]}
${get}"
  done

  _hosted_one_row '{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":"2026-11-26T10:00:00Z"}'
  export STATUS_BODY='{"ok":true,"data":{"id":"cl-1a","status":"Running"}}'
  _hosted_stop "${stop} (hosted, Running, create not confirmed), so lo did not create another one.
${confirm}
${get}"
  assert_equal "$(grep -c "^${status_read}$" "${CALLS}")" 1

  # A create that is not confirmed and a status read that fails.
  _hosted_one_row '{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":"2026-11-26T10:00:00Z"}'
  export STATUS_RC=22
  _hosted_stop "${stop} (hosted, status not read, create not confirmed), so lo did not create another one.
${confirm}
${get}"
  export STATUS_RC=0

  _hosted_one_row '{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"self","status":"Running"}'
  _hosted_stop "[error] kubehz: your account already has cluster cl-1a for test.kubehz.dev (self-hosted, Running), so lo did not create another one.
  It is not a hosted cluster, so lo does not download its kubeconfig.
${get}"
  refute grep -q "^${status_read}$" "${CALLS}"

  _hosted_one_row '{"id":"cl-1a","domain":"test.kubehz.dev","status":"Running"}'
  _hosted_stop "[error] kubehz: your account already has cluster cl-1a for test.kubehz.dev (unknown, Running), so lo did not create another one.
  It is not a hosted cluster, so lo does not download its kubeconfig.
${get}"
  refute grep -q "^${status_read}$" "${CALLS}"

  # lo cannot read the status: the read fails (curl -f: 22), or the answer
  # holds no status or is not JSON.
  _hosted_one_row "${creating_row}"
  export STATUS_RC=22
  _hosted_stop "${stop} (hosted, status not read), so lo did not create another one.
${unread}
${get}"
  export STATUS_RC=0
  for body in '{"ok":true,"data":{"id":"cl-1a"}}' '{"ok":true,"data":{"status":null}}' '<html>proxy</html>'; do
    _hosted_one_row "${creating_row}"
    export STATUS_BODY="${body}"
    _hosted_stop "${stop} (hosted, status not read), so lo did not create another one.
${unread}
${get}"
  done

  # A status that is not one word is a server string that lo does not
  # repeat; an empty status is one too.
  for body in '{"ok":true,"data":{"status":"Running\u001b[2J"}}' '{"ok":true,"data":{"status":""}}'; do
    _hosted_one_row "${creating_row}"
    export STATUS_BODY="${body}"
    _hosted_stop "${stop} (hosted, unknown), so lo did not create another one.
${any}
${get}"
  done

  # An id in another shape: lo does not repeat it and reads no status.
  _hosted_one_row '{"id":"cl-\u001b[2J","domain":"test.kubehz.dev","hosting":"hosted","status":"Running"}'
  _hosted_stop "[error] kubehz: your account already has a cluster for test.kubehz.dev (hosted, Running), so lo did not create another one.
  See your clusters with:  lo kubehz cluster list"
  refute grep -q '^get https' "${CALLS}"

  _hosted_one_row '{"domain":"test.kubehz.dev"}'
  _hosted_stop "[error] kubehz: your account already has a cluster for test.kubehz.dev (unknown, unknown), so lo did not create another one.
  See your clusters with:  lo kubehz cluster list"
}

@test "provision_hosted: more than one cluster of the domain stops the create, and names them, the oldest first, at most ten" {
  _hosted_list_setup
  local foot='  See your clusters with:  lo kubehz cluster list' rows i want
  local head='  lo downloads a kubeconfig only when the account has one cluster for the domain.'
  export LIST_BODY='{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com"},{"id":"cl-new","domain":"test.kubehz.dev","hosting":"hosted","status":"Running","createdAt":"2026-03-01T00:00:00Z"},{"id":"../x","domain":"test.kubehz.dev","hosting":"x\u001b[2J","createdAt":"2026-02-15T00:00:00Z"},{"id":"cl-old","domain":"test.kubehz.dev","hosting":"self","status":"Running","createdAt":"2026-02-01T00:00:00Z"}],"meta":{"pagination":{"total":4}}}'
  _hosted_stop "[error] kubehz: your account has 3 clusters for test.kubehz.dev, so lo did not create another one.
${head}
  - cluster cl-old (self-hosted)
  - a cluster (unknown)
  - cluster cl-new (hosted)
${foot}"
  refute grep -q '^get https' "${CALLS}"

  rows=""
  want="[error] kubehz: your account has 12 clusters for test.kubehz.dev, so lo did not create another one.
${head}"
  for i in $(seq 10 21); do
    rows+="${rows:+,}"'{"id":"cl-'"${i}"'","domain":"test.kubehz.dev","hosting":"hosted","status":"Running","createdAt":"2026-02-'"${i}"'T00:00:00Z"}'
    (( i > 19 )) || want+=$'\n'"  - cluster cl-${i} (hosted)"
  done
  want+=$'\n'"  - and 2 more"$'\n'"${foot}"
  export LIST_BODY='{"ok":true,"data":['"${rows}"']}'
  : > "${CALLS}"
  _hosted_stop "${want}"
}

@test "provision_hosted: the one hosted cluster of the domain that is Running, Paused or Updating gets its kubeconfig and the mirror, and no create" {
  _hosted_list_setup
  printf 'kind: KubeOne\nmetadata:\n  name: mirror-name\nspec:\n  cluster:\n    domain: test.kubehz.dev\n  kubernetes:\n    version: v1.31.10\n  kubehz:\n    hosting: hosted\n' > "${SPEC}"
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
  chmod 0700 "${BATS_TEST_TMPDIR}/.kubeconfig"
  printf 'old\n' > "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml"
  _hosted_one_row '{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":null}'
  export STATUS_BODY='{"ok":true,"data":{"id":"cl-1a","status":"Running"}}'
  local out="${BATS_TEST_TMPDIR}/adopt.out" err="${BATS_TEST_TMPDIR}/adopt.err"
  kubehz::provision_hosted test.kubehz.dev "${SPEC}" > "${out}" 2> "${err}"
  assert_equal "$(cat "${out}")" "kubehz: your account already has cluster cl-1a for test.kubehz.dev (hosted, Running).
  lo downloads its kubeconfig and does not create another cluster."
  assert_equal "$(cat "${err}")" ""
  assert_equal "$(cat "${CALLS}")" "list https://api.kubehz.dev/api/clusters?perPage=500&domain=test.kubehz.dev
get https://api.kubehz.dev/api/clusters/cl-1a
get kubeconfig"
  assert_equal "$(cat "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml")" "kc"
  assert_equal "$(cat "${BATS_TEST_TMPDIR}/.kubeconfig/mirror-name.yaml")" "kc"

  # A Paused or an Updating cluster gets its kubeconfig too, and a note.
  local s
  local -A note=(
    [Paused]="  The cluster is Paused. The download asks kubehz to resume it.
  A suspended account, an ended lease or an ended contract keeps it paused. See it with:  lo kubehz cluster get cl-1a"
    [Updating]="  The cluster is Updating: kubectl can fail for a short time, until the update ends."
  )
  for s in Paused Updating; do
    printf 'old\n' > "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml"
    : > "${CALLS}"
    export STATUS_BODY='{"ok":true,"data":{"id":"cl-1a","status":"'"${s}"'"}}'
    kubehz::provision_hosted test.kubehz.dev "${SPEC}" > "${out}" 2> "${err}"
    assert_equal "$(cat "${out}")" "kubehz: your account already has cluster cl-1a for test.kubehz.dev (hosted, ${s}).
  lo downloads its kubeconfig and does not create another cluster.
${note[${s}]}"
    assert_equal "$(cat "${err}")" ""
    assert_equal "$(cat "${CALLS}")" "list https://api.kubehz.dev/api/clusters?perPage=500&domain=test.kubehz.dev
get https://api.kubehz.dev/api/clusters/cl-1a
get kubeconfig"
    assert_equal "$(cat "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml")" "kc"
  done

  # A failed download fails the run and keeps the old kubeconfig. The note
  # of a Paused cluster follows a download only.
  printf 'old\n' > "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml"
  : > "${CALLS}"
  export KC_RC=22 STATUS_BODY='{"ok":true,"data":{"id":"cl-1a","status":"Paused"}}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_failure
  assert_output --partial "[error] Failed to download kubeconfig for hosted cluster cl-1a"
  refute_output --partial "The cluster is Paused"
  refute grep -qE '^(create|wait)$' "${CALLS}"
  assert_equal "$(cat "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml")" "old"
}

@test "provision_hosted: the domain of the create is spec.cluster.domain, and a list without it lets the create go" {
  _hosted_list_setup
  printf 'kind: KubeOne\nmetadata:\n  name: test.kubehz.dev\nspec:\n  cluster:\n    domain: api.test.kubehz.dev\n  kubernetes:\n    version: v1.31.10\n  kubehz:\n    hosting: hosted\n' > "${SPEC}"
  export LIST_BODY='{"ok":true,"data":[{"id":"cl-1a","domain":"test.kubehz.dev"},{"id":"cl-2b","domain":"other.example.com"}],"meta":{"pagination":{"total":2}}}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_success
  grep -qx 'create' "${CALLS}"
  grep -qx 'list https://api.kubehz.dev/api/clusters?perPage=500&domain=api.test.kubehz.dev' "${CALLS}"
  [[ -f "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]]
}

@test "provision_hosted: a list that lo cannot read stops the create" {
  _hosted_list_setup
  export LIST_STATUS=403 LIST_BODY='{"ok":false,"data":{"code":"TOKEN_SCOPE_MISSING","message":"This endpoint requires the read scope"}}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_failure
  assert_output "[error] kubehz: lo cannot read the cluster list (HTTP 403 TOKEN_SCOPE_MISSING), so it cannot check for a cluster of test.kubehz.dev. No cluster was created.
  KUBEHZ_TOKEN needs the read scope and the clusters:write scope. Mint a token with both in the dashboard (Access -> API Tokens)."
  refute grep -q '^create' "${CALLS}"

  export LIST_STATUS=fail
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_failure
  assert_output "[error] kubehz: lo cannot read the cluster list (no answer), so it cannot check for a cluster of test.kubehz.dev. No cluster was created.
  Run the command again when lo can read the list."
  refute grep -q '^create' "${CALLS}"
}

# Bash only: kubehz::_hosted_listed returns 0, 1 or 2. Any other code
# stops the create too, and the lost answer of a create does not read it
# as "no cluster".
@test "provision_hosted: a code of the list read other than 0, 1 or 2 stops the create" {
  _hosted_list_setup
  kubehz::_hosted_listed() { KUBEHZ_HOSTED_LIST_CAUSE=""; return 3; }
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_failure
  assert_output "[error] kubehz: lo cannot read the cluster list (an error in lo), so it cannot check for a cluster of test.kubehz.dev. No cluster was created.
  Run the command again when lo can read the list."
  refute grep -q '^create' "${CALLS}"

  run kubehz::_hosted_create_unknown "${LOK8S_KUBEHZ_API_URL}" test.kubehz.dev "no answer"
  assert_line --index 1 "  lo cannot read the cluster list either (an error in lo)."
}

# _hosted_unknown_tail: the end of each create whose result is open.
_hosted_unknown_tail() {
  printf '%s\n' "  If kubehz is still checking the create, it usually keeps or removes the cluster in about 3 minutes." \
    "  When the check fails, kubehz tries again about 15 minutes later." \
    "  Do not run lo provision again before kubehz keeps or removes the cluster." \
    "  Then run it again. lo downloads the kubeconfig of a kept cluster, or creates a new one."
}

@test "provision_hosted: a create with no answer reads the list again and names what it shows" {
  _hosted_list_setup
  local row='{"ok":true,"data":[{"id":"cl-1a2b3c4d","domain":"test.kubehz.dev","createdAt":"2026-02-01T00:00:00Z"}]}' line
  line="[error] kubehz: the result of the create for test.kubehz.dev is unknown (no answer). Possibly, kubehz made the cluster."
  # 52: curl got an empty reply, after the send.
  export CREATE_RC=52 LIST2_STATUS=200 LIST2_BODY="${row}"
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_failure
  assert_output "${line}
  The cluster list shows cluster cl-1a2b3c4d for test.kubehz.dev:  lo kubehz cluster get cl-1a2b3c4d
$(_hosted_unknown_tail)"
  assert_equal "$(grep -c '^list ' "${CALLS}")" 2
  refute grep -q '^wait' "${CALLS}"

  : > "${CALLS}"
  export LIST2_BODY='{"ok":true,"data":[{"id":"../x","domain":"test.kubehz.dev"}]}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_output "${line}
  The cluster list shows a cluster for test.kubehz.dev:  lo kubehz cluster list
$(_hosted_unknown_tail)"

  : > "${CALLS}"
  export LIST2_BODY='{"ok":true,"data":[{"id":"cl-1a","domain":"test.kubehz.dev"},{"id":"cl-2b","domain":"test.kubehz.dev"}]}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_output "${line}
  The cluster list shows 2 clusters for test.kubehz.dev:  lo kubehz cluster list
$(_hosted_unknown_tail)"

  : > "${CALLS}"
  export LIST2_BODY='{"ok":true,"data":[]}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_output "${line}
  The cluster list shows no cluster for test.kubehz.dev now.
$(_hosted_unknown_tail)"

  : > "${CALLS}"
  export LIST2_STATUS=500 LIST2_BODY='{"ok":false}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_output "${line}
  lo cannot read the cluster list either (HTTP 500).
$(_hosted_unknown_tail)"
}

@test "provision_hosted: the answer of a gateway in front of the api leaves the create open, the api's own 502 does not" {
  _hosted_list_setup
  export LIST2_STATUS=200 LIST2_BODY='{"ok":true,"data":[]}'
  local gw body
  # gw, not status: run sets status.
  for gw in 502 503 504; do
    for body in 'upstream request timeout' '{"ok":false}'; do
      : > "${CALLS}"
      export CREATE_STATUS="${gw}" CREATE_BODY="${body}"
      run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
      assert_failure
      assert_output "[error] kubehz: the result of the create for test.kubehz.dev is unknown (HTTP ${gw}). Possibly, kubehz made the cluster.
  The cluster list shows no cluster for test.kubehz.dev now.
$(_hosted_unknown_tail)"
      assert_equal "$(grep -c '^list ' "${CALLS}")" 2
    done
  done

  : > "${CALLS}"
  export CREATE_STATUS=502 CREATE_BODY='{"ok":false,"data":{"code":"HOSTED_BACKEND_ERROR","message":"The answer of the backend was lost","help":"kubehz checks the create."}}'
  run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
  assert_failure
  assert_output "[error] Failed to create hosted cluster via kubehz API (HTTP 502): The answer of the backend was lost
  kubehz checks the create."
  assert_equal "$(grep -c '^list ' "${CALLS}")" 1
}

@test "provision_hosted: a create that did not leave this machine made no cluster, and lo does not read the list again" {
  _hosted_list_setup
  local rc
  # 7: curl could not connect, so it sent nothing (node::curl_sent_nothing).
  for rc in 6 7 35 60; do
    : > "${CALLS}"
    export CREATE_RC="${rc}"
    run kubehz::provision_hosted test.kubehz.dev "${SPEC}"
    assert_failure
    assert_output "[error] Failed to create hosted cluster via kubehz API (network error)
  The request did not reach the api, so no cluster was created."
    assert_equal "$(grep -c '^list ' "${CALLS}")" 1
  done
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

  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
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

  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
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

  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
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
  _hosted_no_cluster
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

# The check before the create refuses a missing parent of the .kubeconfig
# directory, but a parent can go away later, during the wait for the
# cluster. mkdir -p -m sets the mode of the last directory only. Under
# umask 022 the write makes each missing directory with mode 0700: the
# parents too, not only the .kubeconfig directory (Go twin: os.MkdirAll
# with 0o700).
@test "provision_hosted: under umask 022, a missing parent and the .kubeconfig directory both get mode 0700" {
  _hosted_kc_harness
  yq() { case "$2" in '.metadata.name') echo "test.kubehz.dev" ;; *) echo "1" ;; esac; }
  local old top="${BATS_TEST_TMPDIR}/new"
  mkdir -p "${top}/base"
  PATH_BASE="${top}/base"
  kubehz::wait_for_cluster() { rmdir -- "${BATS_TEST_TMPDIR}/new/base" "${BATS_TEST_TMPDIR}/new"; }
  old=$(umask)
  umask 022
  run kubehz::provision_hosted "test.kubehz.dev" "/dev/null"
  umask "${old}"
  assert_success
  [ "$(stat -c %a "${top}")" = 700 ]
  [ "$(stat -c %a "${top}/base")" = 700 ]
  [ "$(stat -c %a "${top}/base/.kubeconfig")" = 700 ]
  [ "$(cat "${top}/base/.kubeconfig/test.kubehz.dev.yaml")" = kc-bytes ]
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
    lstat) printf '[error] Cannot read the path %s: nothing is written' "${2}" ;;
    resolve) printf '[error] Cannot resolve the symlink %s' "${2}" ;;
  esac
}

# _link_owner_ls <path> <uid>: replace ls with a function that gives, for
# ls -ldn <path> (the owner of the link itself), the line of a link of
# <uid> (gid 5353). A <uid> that is not a number is an owner that the
# check cannot read. Any other call runs ls.
_link_owner_ls() {
  FAKE_LINK="${1}" FAKE_LINK_UID="${2}"
  ls() {
    if [[ "${1}" == -ldn && "${!#}" == "${FAKE_LINK}" ]]; then
      printf 'lrwxrwxrwx 1 %s 5353 9 Nov 20 08:00 %s -> x\n' "${FAKE_LINK_UID}" "${FAKE_LINK}"
      return 0
    fi
    command ls "$@"
  }
}

# _lstat_error_ls <path> <error>: replace ls with a function that fails
# for ls -ldn <path> with the error line of GNU ls for <error>. Any other
# call runs ls.
_lstat_error_ls() {
  FAKE_LINK="${1}" FAKE_ERR="${2}"
  ls() {
    if [[ "${1}" == -ldn && "${!#}" == "${FAKE_LINK}" ]]; then
      printf "ls: cannot access '%s': %s\n" "${FAKE_LINK}" "${FAKE_ERR}" >&2
      return 2
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

# The lookup of the directory tells "not there" (ENOENT) from an error
# (kubehz::_lstat_line). ls is faked for ls -ldn of a directory that is
# not there, with a parent that this user can search: an error line of
# another cause (EIO, ESTALE, EACCES) is refused, because the directory
# can be there. The ENOENT line of GNU and of BSD passes. An error output
# of more than one line is refused. Before, -e and -L were false on each
# error, and the check passed. Go twin: privateDirRefusal reads the error
# of os.Stat.
@test "kubehz::_private_dir_ok refuses a lookup error other than not there" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/lookup/kube" err
  mkdir -p "${BATS_TEST_TMPDIR}/lookup"
  FAKE_DIR="${d}"
  ls() {
    if [[ "${1}" == -ldn && "${!#}" == "${FAKE_DIR}" ]]; then
      printf '%s\n' "${FAKE_ERR}" >&2
      return 2
    fi
    command ls "$@"
  }
  for err in "Input/output error" "Stale file handle" "Permission denied"; do
    FAKE_ERR="ls: cannot access '${d}': ${err}"
    run kubehz::_private_dir_ok "${d}"
    assert_failure
    assert_output "$(_dir_refused unread "${d}")"
  done
  FAKE_ERR="ls: cannot access '${d}': No such file or directory"
  run kubehz::_private_dir_ok "${d}"
  assert_success
  assert_output ""
  FAKE_ERR="ls: ${d}: No such file or directory"
  run kubehz::_private_dir_ok "${d}"
  assert_success
  assert_output ""
  FAKE_ERR=$'ls: cannot access \'a\nb\': No such file or directory'
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
  _link_owner_ls "${d}/hop" someone
  run kubehz::_private_target "${d}/kc.yaml"
  assert_failure
  assert_output "$(_dir_refused linkread "${d}/hop")"
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/home.rc")" = rc ]
  _link_owner_ls "${d}/hop" 0
  run kubehz::_private_target "${d}/kc.yaml"
  assert_success
  assert_output "${d}/home.rc"
  unset -f ls
  run kubehz::_write_private "${d}/kc.yaml" printf new
  assert_success
  assert_output ""
  [ "$(cat "${d}/home.rc")" = new ]
  [ "$(stat -c %a "${d}/home.rc")" = 600 ]
  [ -L "${d}/kc.yaml" ]
  [ -L "${d}/hop" ]
}

# A lookup of a name of the chain that fails for a cause other than "not
# there" (EIO, ESTALE, EACCES) is refused: the check cannot tell that the
# name is no link, and a link there can be a link of another user (ls is
# faked for one name of the chain). Before, -L was false on each error,
# and the walk ended as a pass. Nothing is written, and the file at the
# end of the chain stays as it was. Go twin:
# TestWritePrivateFileRefusesALookupErrorOfTheChain.
@test "kubehz::_write_private refuses a lookup error of a name of the chain" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/lookup-links" ran="${BATS_TEST_TMPDIR}/ran" p err
  mkdir -p "${d}"
  chmod 0700 "${d}"
  printf 'old\n' > "${d}/home.rc"
  ln -s hop "${d}/kc.yaml"
  ln -s "${d}/home.rc" "${d}/hop"
  for p in "${d}/kc.yaml" "${d}/hop"; do
    for err in "Input/output error" "Stale file handle" "Permission denied"; do
      _lstat_error_ls "${p}" "${err}"
      run kubehz::_write_private "${d}/kc.yaml" touch "${ran}"
      assert_failure
      assert_output "$(_dir_refused lstat "${p}")"
    done
  done
  unset -f ls
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/home.rc")" = old ]
  [ -z "$(_hosted_temps "${d}" home.rc)" ]
}

# _insert_link_after_lookup <dest> <victim> <mark>: replace
# kubehz::_lstat_line with a function that puts a link at <dest>, to
# <victim>, right after the first lookup of <dest> that finds nothing
# there, and then makes the file <mark>. Another user who can write into
# the directory can do that between the lookup and the write. The lookup
# runs in a subshell, so the file <mark> tells that the link was put
# there. Other lookups are not changed.
_insert_link_after_lookup() {
  FAKE_DEST="${1}" FAKE_VICTIM="${2}" FAKE_MARK="${3}"
  eval "$(declare -f kubehz::_lstat_line | sed '1s/kubehz::_lstat_line/_real_lstat_line/')"
  kubehz::_lstat_line() {
    local rc=0
    _real_lstat_line "$@" || rc=$?
    if [[ "${1}" == "${FAKE_DEST}" && "${rc}" == 1 && ! -e "${FAKE_MARK}" ]]; then
      ln -s -- "${FAKE_VICTIM}" "${FAKE_DEST}" && : > "${FAKE_MARK}"
    fi
    return "${rc}"
  }
}

# In a directory with the sticky bit and a shared group (mode 1770),
# another user can put a link at the name of the file when nothing is
# there, to a file of this user. The walk found nothing at the name, and
# the write does not look the name up again: mv -T replaces the link that
# was put there after the walk, and the file that the link names stays as
# it was (the lookup of the walk is faked to put the link there right
# after it). Before, readlink -f after the walk followed that link and
# replaced the file of this user. Go twin:
# TestWritePrivateFileReplacesALinkPutAfterTheWalk.
@test "kubehz::_write_private replaces a link put at the name after the walk" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/insert" mark="${BATS_TEST_TMPDIR}/inserted"
  _KUBEHZ_MV_EXACT=""
  kubehz::_mv_exact_ok || skip "the mv of this host has no -T"
  mkdir -p "${d}/home" "${d}/shared"
  printf 'old\n' > "${d}/home/kc.yaml"
  chmod 0700 "${d}/home"
  chmod 1770 "${d}/shared"
  _insert_link_after_lookup "${d}/shared/kc.yaml" "${d}/home/kc.yaml" "${mark}"
  run kubehz::_write_private "${d}/shared/kc.yaml" printf 'new\n'
  assert_success
  assert_output ""
  [ -e "${mark}" ]
  [ "$(cat "${d}/home/kc.yaml")" = old ]
  [ ! -L "${d}/shared/kc.yaml" ]
  [ -f "${d}/shared/kc.yaml" ]
  [ "$(cat "${d}/shared/kc.yaml")" = new ]
  [ "$(stat -c %a "${d}/shared/kc.yaml")" = 600 ]
  [ -z "$(_hosted_temps "${d}/shared" kc.yaml)" ]
  [ -z "$(_hosted_temps "${d}/home" kc.yaml)" ]
}

# Each link of the chain is read only when no other user can replace it:
# a link in a directory that another user can write into without the
# sticky bit (0777, 0770) is refused, at the name and as the second link,
# and the file that the chain names stays as it was. A link in a directory
# with the sticky bit passes, also when mv has no -T: the walk only reads
# the link there, and the rename goes into the directory of the file. Go
# twin: TestWritePrivateFileChecksTheDirectoryOfEachLink.
@test "kubehz::_write_private checks the directory of each link of the chain" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/linkdirs" ran="${BATS_TEST_TMPDIR}/ran" name mode
  mkdir -p "${d}/safe"
  printf 'old\n' > "${d}/safe/kc.yaml"
  chmod 0700 "${d}/safe"
  for name in world:0777 group:0770 sticky:1777; do
    mode="${name#*:}" name="${name%:*}"
    mkdir -p "${d}/${name}"
    ln -s "${d}/safe/kc.yaml" "${d}/${name}/kc.yaml"
    chmod "${mode}" "${d}/${name}"
  done
  mkdir -p "${d}/first"
  ln -s "${d}/world/kc.yaml" "${d}/first/kc.yaml"
  chmod 0700 "${d}/first"
  run kubehz::_write_private "${d}/world/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused world "${d}/world")"
  run kubehz::_write_private "${d}/group/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused group "${d}/group")"
  run kubehz::_write_private "${d}/first/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused world "${d}/world")"
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/safe/kc.yaml")" = old ]
  _KUBEHZ_MV_EXACT=0
  run kubehz::_write_private "${d}/sticky/kc.yaml" printf 'new\n'
  assert_success
  assert_output ""
  [ "$(cat "${d}/safe/kc.yaml")" = new ]
  [ "$(stat -c %a "${d}/safe/kc.yaml")" = 600 ]
  [ -L "${d}/sticky/kc.yaml" ]
  [ -z "$(_hosted_temps "${d}/safe" kc.yaml)" ]
}

# The end of the walk keeps a ".." after a link to a directory, and the
# kernel reads that ".." from the directory that the link names:
# d/alias/../store is d/deep/store, not d/store. So the check, the create
# and the rename take the directory of the path as it is (dirname does
# not clean it). A world-writable d/deep/store is refused although d/store
# is a directory of this user only, and the write goes into d/deep/store
# when no d/store is there. Go twin:
# TestWritePrivateFileTakesTheDirectoryOfTheWalkAsItIs.
@test "kubehz::_write_private takes the directory of the walk as it is" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
  local d="${BATS_TEST_TMPDIR}/dotdot" ran="${BATS_TEST_TMPDIR}/ran"
  mkdir -p "${d}/deep/store" "${d}/deep/real" "${d}/store"
  printf 'old\n' > "${d}/deep/store/kc.yaml"
  ln -s ../store/kc.yaml "${d}/deep/real/kc.yaml"
  ln -s "${d}/deep/real" "${d}/alias"
  chmod 0777 "${d}/deep/store"
  chmod 0700 "${d}/deep/real" "${d}/store"
  run kubehz::_write_private "${d}/alias/kc.yaml" touch "${ran}"
  assert_failure
  assert_output "$(_dir_refused world "${d}/alias/../store")"
  [ ! -e "${ran}" ]
  [ "$(cat "${d}/deep/store/kc.yaml")" = old ]
  rmdir "${d}/store"
  chmod 0700 "${d}/deep/store"
  run kubehz::_write_private "${d}/alias/kc.yaml" printf 'new\n'
  assert_success
  assert_output ""
  [ "$(cat "${d}/deep/store/kc.yaml")" = new ]
  [ "$(stat -c %a "${d}/deep/store/kc.yaml")" = 600 ]
  [ ! -e "${d}/store" ]
  [ -z "$(_hosted_temps "${d}/deep/store" kc.yaml)" ]
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
  assert_output "${d}/f"
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

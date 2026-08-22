#!/usr/bin/env bats
# capi_test.bats — unit tests for .lok8s/drivers/capi/generate

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/provider.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/generate"

  # Copy CAPI templates to tmpdir (needed by capi::generate)
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster"
  cp -r "${_PROJECT_ROOT}/.lok8s/drivers/capi/cluster/"* \
    "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster/"

  # Create Hetzner cluster spec fixture
  cp "${FIXTURES_DIR}/capi-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml"

  # Create an AWS cluster spec fixture (inline)
  cat > "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-aws
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: aws.lok8s.dev
    namespace: default
  aws:
    region: eu-central-1
    sshKeyName: test-aws-key
  controlPlane:
    replicas: 1
    type: t3.large
  workers:
    general:
      replicas: 2
      type: t3.xlarge
YAML

  # Copy full AWS fixture too
  if [[ -f "${FIXTURES_DIR}/aws-cluster.lok8s.yaml" ]]; then
    cp "${FIXTURES_DIR}/aws-cluster.lok8s.yaml" \
      "${BATS_TEST_TMPDIR}/aws-cluster-full.lok8s.yaml"
  fi
}

teardown() {
  teardown_tmpdir
}

# --- capi::detect_provider ---

@test "capi::detect_provider returns hetzner for hcloud spec" {
  if ! command -v yq &>/dev/null; then
    yq() {
      if [[ "$1" == "-e" && "$2" == ".spec.hcloud" ]]; then
        return 0
      fi
      return 1
    }
    export -f yq
  fi

  run capi::detect_provider "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml"
  assert_success
  assert_output "hetzner"
}

@test "capi::detect_provider returns aws for aws spec" {
  if ! command -v yq &>/dev/null; then
    yq() {
      if [[ "$1" == "-e" && "$2" == ".spec.hcloud" ]]; then
        return 1
      fi
      if [[ "$1" == "-e" && "$2" == ".spec.aws" ]]; then
        return 0
      fi
      return 1
    }
    export -f yq
  fi

  run capi::detect_provider "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml"
  assert_success
  assert_output "aws"
}

@test "capi::detect_provider fails for unknown provider" {
  cat > "${BATS_TEST_TMPDIR}/unknown-cluster.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-unknown
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: unknown.lok8s.dev
YAML

  if ! command -v yq &>/dev/null; then
    yq() { return 1; }
    export -f yq
  fi

  run capi::detect_provider "${BATS_TEST_TMPDIR}/unknown-cluster.yaml"
  assert_failure
  assert_output --partial "No provider found in cluster spec"
}

# --- capi::generate ---

@test "capi::generate produces CAPI Cluster resource for hetzner" {
  if ! command -v yq &>/dev/null; then
    skip "yq required for capi::generate test"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_success
  assert_output --partial "kind: Cluster"
  assert_output --partial "kind: KubeadmControlPlane"
  assert_output --partial "kind: HetznerCluster"
}

@test "capi::generate includes worker machine deployments" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_success
  assert_output --partial "kind: MachineDeployment"
}

@test "capi::generate fails for missing template directory" {
  rm -rf "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster"

  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.metadata.name') echo "test-production" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        '.spec.cluster.domain') echo "prod.lok8s.dev" ;;
        '.spec.kubernetes.version') echo "v1.31.10" ;;
        '.spec.controlPlane.replicas // 1') echo "3" ;;
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.hcloud.region') echo "fsn1" ;;
        '.spec.hcloud.sshKeyName') echo "test-key" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_failure
  assert_output --partial "CAPI template directory not found"
}

@test "capi::generate fails for unsupported provider" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.metadata.name') echo "test" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "gcp"
  assert_failure
  assert_output --partial "not supported yet"
}

# --- capi::ensure_credentials ---

@test "capi::ensure_credentials creates hetzner secret" {
  kubectl() { echo "kubectl $*"; }
  export -f kubectl

  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  export HCLOUD_TOKEN="test-token"

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" \
    "hetzner" \
    "/tmp/kubeconfig.yaml"
  assert_success
}

@test "capi::ensure_credentials fails for unsupported provider" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" \
    "gcp" \
    "/tmp/kubeconfig.yaml"
  assert_failure
  assert_output --partial "unknown provider"
}

# --- capi::wait_ready ---

@test "capi::wait_ready returns when cluster is Provisioned" {
  kubectl() {
    echo "Provisioned"
  }
  export -f kubectl

  run capi::wait_ready "/tmp/kubeconfig.yaml" "test-cluster" 5
  assert_success
}

@test "capi::wait_ready fails on timeout" {
  kubectl() { echo "Pending"; }
  export -f kubectl

  # Use very short timeout (1 second) with a sleep override
  sleep() { :; }
  export -f sleep

  run capi::wait_ready "/tmp/kubeconfig.yaml" "test-cluster" 1
  assert_failure
  assert_output --partial "Timed out"
}

# --- AWS provider ---
# capi::generate is scoped to hetzner-hcloud (the shared control-plane/worker
# templates install the kubeadm stack via Hetzner-flavored cloud-init, so they
# are not reusable for AWS). Generation cleanly errors for non-hetzner providers;
# credential handling (capi::ensure_credentials) still supports AWS.

@test "capi::generate fails for aws (only hetzner generation is supported)" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  run capi::generate "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" "aws"
  assert_failure
  assert_output --partial "not supported yet"
}

@test "capi::ensure_credentials creates aws secret" {
  kubectl() { echo "kubectl $*"; }
  export -f kubectl

  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-aws-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  export AWS_ACCESS_KEY_ID="AKIAIOSFODNN7EXAMPLE"
  export AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
  export AWS_REGION="eu-central-1"

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" \
    "aws" \
    "/tmp/kubeconfig.yaml"
  assert_success
}

@test "capi::ensure_credentials fails without AWS_ACCESS_KEY_ID" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  unset AWS_ACCESS_KEY_ID 2>/dev/null || true
  unset AWS_SECRET_ACCESS_KEY 2>/dev/null || true
  unset AWS_REGION 2>/dev/null || true

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" \
    "aws" \
    "/tmp/kubeconfig.yaml"
  assert_failure
  assert_output --partial "AWS_ACCESS_KEY_ID"
}

# --- Conditional hrobot rendering ---
# The rewritten generator targets hcloud only; bare-metal (hrobot) rendering was
# dropped, so the hcloud output must never contain a bare-metal template.

@test "capi::generate does not render hrobot template (hcloud only)" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_success
  refute_output --partial "HetznerBareMetalMachineTemplate"
}

# --- template-variable containment (POST-REVIEW finding 6) ---

@test "capi::generate leaves no template variable in the caller's environment" {
  command -v yq &>/dev/null || skip "yq required"
  command -v envsubst >/dev/null || skip "envsubst not available"

  # The variables have to be EXPORTED for envsubst's child process, but they
  # must not outlive the call: CLUSTER_NAME and K8S_VERSION are also read by
  # the kubeone driver, and a leaked value renders the wrong cluster on the
  # next call in the same `lo` process.
  local v
  for v in "${_CAPI_TEMPLATE_VARS[@]}"; do unset "${v}"; done

  # Called DIRECTLY with a FILE redirect. Neither `run` nor `$( … )` works here:
  # both put the call in a subshell, so an environment leak is discarded before
  # the assertions can see it. Two drafts of this test passed against a
  # deliberately leaking mutant for exactly that reason.
  local out_file="${BATS_TEST_TMPDIR}/generated.yaml"
  capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner" > "${out_file}"
  local out; out="$(cat "${out_file}")"
  # Positive control: the render really did use them, so an "all unset" result
  # below cannot mean "nothing happened".
  [[ "${out}" == *"name: test-production"* ]] || {
    echo "the render produced nothing recognisable:" >&2
    echo "${out}" >&2
    return 1
  }

  local -a leaked=()
  for v in "${_CAPI_TEMPLATE_VARS[@]}"; do
    [[ -z "${!v+set}" ]] || leaked+=("${v}=${!v}")
  done
  [ "${#leaked[@]}" -eq 0 ] || {
    echo "capi::generate exported these into the caller:" >&2
    printf '  %s\n' "${leaked[@]}" >&2
    return 1
  }
}

@test "the envsubst whitelist is DERIVED from the variable list" {
  # The drift gate. The whitelist and the export set were two hand-kept copies;
  # a variable in one and not the other ships its ${PLACEHOLDER} verbatim into
  # an applied manifest.
  local want=""
  local v
  for v in "${_CAPI_TEMPLATE_VARS[@]}"; do want+="\${${v}} "; done
  want="${want% }"
  [ "$(capi::_allowed_vars)" = "${want}" ]

  # And no hardcoded list survives in the source.
  local hits
  hits=$(grep -n 'allowed_vars=.\${CLUSTER_NAME}' \
    "${_PROJECT_ROOT}/.lok8s/drivers/capi/generate" || true)
  [ -z "${hits}" ] || {
    echo "a literal envsubst whitelist is back:" >&2
    echo "${hits}" >&2
    echo "Build it from _CAPI_TEMPLATE_VARS instead." >&2
    return 1
  }
}

@test "capi::generate FAILS on a bad pool name instead of emitting a partial stream" {
  # libs/provision calls the driver as `driver::provision || rc=$?`, which
  # disables errexit below it, so the unguarded `rendered=$(…)` let a failed
  # render flow on as a control plane with no workers.
  command -v yq &>/dev/null || skip "yq required"
  command -v envsubst >/dev/null || skip "envsubst not available"

  local spec="${BATS_TEST_TMPDIR}/bad-pool.lok8s.yaml"
  cp "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "${spec}"
  yq -i '.spec.workers = {"-nope": {"replicas": 1, "type": "cax11"}}' "${spec}"

  run capi::generate "${spec}" "hetzner"
  assert_failure
  refute_output --partial "kind: Cluster"
}

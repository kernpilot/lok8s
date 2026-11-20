#!/usr/bin/env bats
# capi_status_test.bats — the bound of the Capi status read
# (.lok8s/drivers/capi/main: driver::status, capi::_status_bounded).
#
# driver::status reads the Cluster phase from the management cluster. A
# management cluster that does not answer, or an exec credential plugin of
# its kubeconfig, must not stop the read: it runs under timeout(1) when it
# is on PATH, and a read that the bound stopped gives NotFound. Go twin:
# TestStatusReadIsBounded in internal/driver/capi/capi_test.go.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v yq &>/dev/null || skip "yq not available"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-prod
spec:
  managementCluster:
    domain: mgmt.dev
YAML
  printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/mgmt.dev.yaml"
  KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${KUBECTL_LOG}"

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"
  :args() { domain="test.dev"; }
  capi::_status_bound() { echo 1; }
}

teardown() { teardown_tmpdir; }

# capi_kubectl_stub <phase|hang>: a real kubectl program in a directory of
# this test, put first on PATH. It writes its arguments to KUBECTL_LOG. With
# "phase" it prints Provisioned. With "hang" it sleeps for 10 s (a read
# that does not end). The test checks that kubectl is the stub, so no other
# kubectl can run.
capi_kubectl_stub() {
  local bin="${BATS_TEST_TMPDIR}/bin"
  mkdir -p "${bin}"
  {
    printf '#!/bin/sh\nPATH=/usr/bin:/bin\necho "$*" >> "%s"\n' "${KUBECTL_LOG}"
    if [[ "${1}" == phase ]]; then
      printf 'printf Provisioned\n'
    else
      printf 'exec sleep 10\n'
    fi
  } > "${bin}/kubectl"
  chmod +x "${bin}/kubectl"
  export PATH="${bin}:${PATH}"
  [[ "$(type -P kubectl)" == "${bin}/kubectl" ]] || { echo "kubectl is not the stub: $(type -P kubectl)" >&2; return 1; }
}

_capi_status_read() {
  echo "get cluster test-prod --kubeconfig ${PATH_BASE}/.kubeconfig/mgmt.dev.yaml -o jsonpath={.status.phase}"
}

@test "capi driver::status stops a management read that does not end at the bound" {
  command -v timeout &>/dev/null || skip "timeout(1) not available"
  capi_kubectl_stub hang
  local start="${SECONDS}"
  run driver::status test.dev
  local took=$(( SECONDS - start ))
  assert_success
  assert_output "NotFound"
  (( took <= 4 )) || { echo "driver::status took ${took} s" >&2; return 1; }
  run cat "${KUBECTL_LOG}"
  assert_output "$(_capi_status_read)"
}

@test "capi driver::status passes the phase through under the bound" {
  command -v timeout &>/dev/null || skip "timeout(1) not available"
  capi_kubectl_stub phase
  run driver::status test.dev
  assert_success
  assert_output "Provisioned"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_capi_status_read)"
}

# Without timeout on PATH, the read runs with no bound, as before. PATH is
# a directory with yq only; kubectl is a function of this test.
@test "capi driver::status reads without a bound when timeout is not on PATH" {
  local yq_path dir="${BATS_TEST_TMPDIR}/yq-only"
  yq_path="$(type -P yq)"
  mkdir -p "${dir}"
  ln -s "${yq_path}" "${dir}/yq"
  kubectl() { echo "$*" >> "${KUBECTL_LOG}"; printf 'Provisioned'; }
  PATH="${dir}" run driver::status test.dev
  assert_success
  assert_output "Provisioned"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_capi_status_read)"
}

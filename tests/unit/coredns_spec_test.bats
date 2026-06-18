#!/usr/bin/env bats
# coredns_spec_test.bats — lo::coredns_custom renders spec.coredns into the
# coredns-custom ConfigMap. Guards:
#   1. structured hosts[] -> a generated `<name>:53` server block, with
#      `target: gateway` resolved to the FIRST loadBalancer.pool IP.
#   2. NO leaked RETURN trap — the function must be set -u-safe for its CALLER.
#      Regression: a `trap 'rm -rf "${tmp}"' RETURN` is not function-local
#      without `set -o functrace`, so it re-fired when the caller (lo::coredns)
#      returned, where ${tmp} is out of scope → "tmp: unbound variable" under
#      set -u → aborted `lo up` mid-bootstrap.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export CAPTURE="${BATS_TEST_TMPDIR}/capture"
  mkdir -p "${PATH_CLUSTERS}/x.dev" "${CAPTURE}"

  cat >"${PATH_CLUSTERS}/x.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: x-dev
spec:
  loadBalancer:
    pool: "10.20.30.1-10.20.30.9"
  coredns:
    hosts:
      - name: x.dev
        target: gateway
YAML

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/services.sh"

  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  # Stub kubectl: capture the rendered --from-file dir before it is cleaned up,
  # and any loadBalancerIPs annotation; consume piped stdin of `| kubectl apply`.
  kubectl() {
    local a
    for a in "$@"; do
      case "$a" in
        --from-file=*) cp "${a#--from-file=}"/* "${CAPTURE}/" 2>/dev/null || true ;;
        metallb.universe.tf/loadBalancerIPs=*) echo "${a}" >>"${CAPTURE}/annotate.log" ;;
      esac
    done
    cat >/dev/null 2>&1 || true
    return 0
  }
}

@test "coredns_custom: hosts -> server block, gateway resolves to pool[0]" {
  lo::coredns_custom "x.dev" "${PATH_CLUSTERS}/x.dev/cluster.lok8s.yaml" "/dev/null"
  run cat "${CAPTURE}"/*.server
  assert_success
  assert_output --partial "x.dev:53"
  assert_output --partial "10.20.30.1"   # pool[0] — the `gateway` shorthand
  refute_output --partial "gateway"       # literal keyword must be resolved away
}

@test "coredns_custom: no leaked RETURN trap (caller returns set -u-safe)" {
  _caller() {
    lo::coredns_custom "x.dev" "${PATH_CLUSTERS}/x.dev/cluster.lok8s.yaml" "/dev/null"
    return 0   # a leaked RETURN trap would fire HERE on the unset ${tmp}
  }
  set -u
  run _caller
  set +u
  assert_success
  refute_output --partial "unbound variable"
}

@test "coredns: pins coredns-external to the LAST pool IP (avoids gateway pool[0] race)" {
  # lo::coredns reads metadata.name -> kubeconfig path; kubectl is stubbed.
  lo::coredns "x.dev"
  run cat "${CAPTURE}/annotate.log"
  assert_success
  # pool is 10.20.30.1-10.20.30.9 -> coredns-external must take .9, NOT .1
  assert_output --partial "metallb.universe.tf/loadBalancerIPs=10.20.30.9"
}

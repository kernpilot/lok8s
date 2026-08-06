#!/usr/bin/env bats
# lo_validate_ips_honored_test.bats — a validator whose verdict is discarded is
# not a validator.
#
# Why this exists
# ---------------
# `lo::validate_ips` counts every bad IP, prints
#
#     error: N IP validation error(s). Aborting.
#
# and returns 1. The word is "Aborting." — and nothing aborts. The call site in
# drivers/lo/main takes neither its status nor an `|| return`, and errexit is
# suppressed for the whole tree because libs/provision runs the driver as
# `driver::provision || provision_rc=$?`. The lo driver documents that trap in
# its own comments a dozen lines above this call.
#
# So `lo up` proceeded with a subnet, registry IP or MetalLB pool the validator
# had already rejected, after printing that it was aborting. Same for
# `lo::read_config` on the line before: a failed config read continued with
# whatever happened to be in the environment.
#
# Checked and found CORRECT while here, so it is not re-litigated later: the
# validator's own error counting survives `registry::each`, because that loop is
# fed by process substitution (`done < <(jq …)`) rather than a pipe, so the
# callback runs in the current shell. A pipe there would have silently zeroed
# `errors` and given the same symptom from a different cause.

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}"
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: validatetest
spec:
  runtime: kind
  kubernetes:
    version: "1.31.0"
YAML
}

teardown() { teardown_tmpdir; }

# Load the driver with everything stubbed to succeed, and a trace file that
# records which steps AFTER the validator actually ran.
_load_lo() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  :args() { domain="test.dev"; }
  TRACE="${BATS_TEST_TMPDIR}/trace"; : > "${TRACE}"
  lo::read_config()   { export LOK8S_NETWORK_SUBNET="10.0.0.0/24" LOK8S_LB_POOL=""; return 0; }
  lo::validate_ips()  { return 0; }
  lo::network()       { echo network        >> "${TRACE}"; return 0; }
  lo::registry_network() { return 0; }
  registry::is_shared() { return 1; }
  lo::registries_tls_cert() { echo tls_cert >> "${TRACE}"; return 0; }
  kapply::run()       { echo "kapply:${1}"  >> "${TRACE}"; return 0; }
  lo::write_certs_d() { echo certs_d        >> "${TRACE}"; return 0; }
  lo::write_oidc_auth_config() { echo oidc  >> "${TRACE}"; return 99; }

  # HERMETIC, and not optional. The first version of this test relied on the
  # stub above returning 99 to stop the run — but that call site is unguarded
  # too, which is the very bug under test, so execution sailed past it and
  # `kind create cluster` built a REAL cluster on the developer's machine
  # (found in the failure log, deleted, kubehz-dev and local untouched).
  # Shadowing the binaries as shell functions means no stub gap can reach
  # docker or kind, whatever the driver does next.
  kind()    { echo "kind:${*}"   >> "${TRACE}"; return 0; }
  docker()  { echo "docker:${*}" >> "${TRACE}"; return 0; }
  kubectl() { echo "kubectl:${*}" >> "${TRACE}"; return 0; }
  export LOK8S_REMOTE=""
}

@test "a REJECTED IP config stops the provision" {
  _load_lo
  lo::validate_ips() { echo "error: 2 IP validation error(s). Aborting." >&2; return 1; }

  # The caller's context — libs/provision runs `driver::provision || rc=$?`,
  # which is what disables errexit. Without this the test would pass on the
  # broken code.
  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 after lo::validate_ips REJECTED the config" >&2
    echo "— it printed 'Aborting.' and then did not abort." >&2
    return 1
  }
}

@test "a REJECTED IP config stops BEFORE anything is built" {
  # Non-zero at the end is not enough: by then the docker networks, registry
  # TLS cert and containerd config would already exist for a config that was
  # rejected. Nothing after the validator may run.
  _load_lo
  lo::validate_ips() { return 1; }

  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -ne 0 ]

  [ ! -s "${TRACE}" ] || {
    echo "steps ran AFTER the IP config was rejected:" >&2
    sed 's/^/    /' "${TRACE}" >&2
    return 1
  }
}

@test "a FAILED config read stops the provision" {
  _load_lo
  lo::read_config() { return 1; }

  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although lo::read_config FAILED —" >&2
    echo "it would continue with whatever was already in the environment." >&2
    return 1
  }
}

@test "an ACCEPTED config still proceeds past the validator" {
  # Guards against 'fixing' the above by making the provision always fail.
  _load_lo
  local rc=0
  driver::provision test.dev || rc=$?

  # Assert PROGRESS, not a return code. An earlier draft keyed this on a
  # sentinel exit status from a later stub — but that call site is unguarded
  # too, and this round deliberately does not touch it, so the sentinel could
  # never have arrived. The trace is the honest signal: the steps after the
  # validator ran.
  grep -q '^network$' "${TRACE}" || {
    echo "execution did not reach the steps after the validator:" >&2
    sed 's/^/    /' "${TRACE}" >&2
    return 1
  }
}

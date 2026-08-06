#!/usr/bin/env bats
# kubeone_provision_guards_test.bats — driver::provision must not report success
# when a step it depends on failed.
#
# Why this exists
# ---------------
# Issue #91 was fixed one call site at a time; this covers the rest of the same
# call tree. `libs/provision:341` runs the driver as `driver::provision ||
# provision_rc=$?`, which DISABLES ERREXIT for everything below it, so every
# unguarded call in this function is a silent-continue waiting to happen.
#
# The worst of them is `kubeone::apply`, because the code AFTER it decides the
# return value:
#
#     kubeone::apply "${work_dir}"          # fails, execution continues
#     ...
#     if [[ -f "${src_kubeconfig}" ]]; then # a kubeconfig from a PREVIOUS run
#       cp ...                              # is copied over
#     fi                                    # → function returns 0
#
# On a cluster that was provisioned before — a worker re-join, the exact scenario
# in #91 — the kubeconfig is always already there. So a failed apply reports
# success. On a first-ever provision the file is absent and it fails correctly,
# which is why this never showed up in a fresh-install test.
#
# Each test drives the REAL driver::provision under the caller's `|| rc=$?`
# context. Without that context errexit would catch these on its own and the
# tests would pass against the broken code.

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}"
  import() { :; }
  export -f import
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: guardtest
spec:
  provider:
    name: hetzner
YAML
}

teardown() { teardown_tmpdir; }

# Load the driver with every collaborator stubbed to SUCCEED. Individual tests
# then break exactly one of them, so a failure can only come from that step.
_load_provision() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"

  # :args normally parses the arg spec; here the domain is passed positionally.
  :args() { domain="test.dev"; }
  kubehz::read_config()      { export LOK8S_KUBEHZ_HOSTING="self-hosted"; return 0; }
  provider::provision()      { return 0; }
  kubeone::generate_config() { return 0; }
  _append_inventory()        { return 0; }
  kubeone::apply()           { return 0; }
  kubeone::kubeconfig_path()  { printf '%s' "${BATS_TEST_TMPDIR}/prev-kubeconfig.yaml"; }
  export PROVIDER_CONFIG_FILE="${BATS_TEST_TMPDIR}/provider.json"
  export PROVIDER_NAME=hetzner
  echo '{}' > "${PROVIDER_CONFIG_FILE}"
}

# The kubeconfig a PREVIOUS successful provision left behind.
_seed_previous_kubeconfig() {
  printf 'apiVersion: v1\nkind: Config\n' > "${BATS_TEST_TMPDIR}/prev-kubeconfig.yaml"
}

@test "a FAILED kubeone apply does not report success when an old kubeconfig exists" {
  _load_provision
  _seed_previous_kubeconfig
  kubeone::apply() { return 1; }

  # The caller's context — this is what suppresses errexit.
  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although kubeone apply FAILED." >&2
    echo "The stale kubeconfig from a previous run satisfied the check after it," >&2
    echo "so a failed re-join reports success — issue #91's scenario." >&2
    return 1
  }
}

@test "a FAILED provider::provision stops before the manifest and the apply" {
  _load_provision
  _seed_previous_kubeconfig
  local trace="${BATS_TEST_TMPDIR}/trace"; : > "${trace}"
  provider::provision()      { return 1; }
  kubeone::generate_config() { echo generate_config >> "${trace}"; return 0; }
  kubeone::apply()           { echo apply >> "${trace}"; return 0; }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although the INFRASTRUCTURE step failed" >&2
    return 1
  }
  # …and it must not have carried on regardless: applying a manifest against
  # infrastructure that was never created is worse than the non-zero exit.
  [ ! -s "${trace}" ] || {
    echo "steps ran AFTER provider::provision failed:" >&2
    sed 's/^/    /' "${trace}" >&2
    return 1
  }
}

@test "a FAILED kubehz::read_config stops the provision" {
  _load_provision
  _seed_previous_kubeconfig
  kubehz::read_config() { return 1; }

  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although the config read FAILED" >&2
    return 1
  }
}

@test "the fully-successful path still returns 0" {
  # Guards against 'fixing' the above by making driver::provision always fail.
  _load_provision
  _seed_previous_kubeconfig
  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -eq 0 ] || {
    echo "the happy path regressed (rc=${rc})" >&2
    return 1
  }
  [ -f "${PATH_BASE}/.kubeconfig/guardtest.yaml" ]
}

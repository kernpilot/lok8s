#!/usr/bin/env bats
# lo_down_routing_test.bats — `lo down` must not reach driver-destroy on a
# malformed spec.
#
# Why this exists
# ---------------
# main::down decides between two teardowns: delete the local kind cluster, or
# call provision::dispatch_destroy, which deprovisions real servers, load
# balancers and volumes at a cloud provider. It chose with a private read of
# `.kind`:
#
#     kind=$(yq -r '.kind' "${spec}" …)
#     if [[ -n "${kind}" && "${kind}" != "lo" ]]; then   # → driver destroy
#
# Without `// ""` a spec that has NO `.kind` reads back as yq's literal string
# "null". That is non-empty and it is not "lo", so a missing key selected the
# destructive branch. The driver name then went on to `drivers/null/main`, but
# the kubehz deregistration in dispatch_destroy runs before that check.
#
# The repo already owns one validating reader — provision::read_kind — which
# coerces with `// ""`, rejects empty, and constrains the value to a bare driver
# name before anything interpolates it into a sourced path. main::down now calls
# it. This file pins the routing itself, not the reader: the tests below run the
# REAL main::down, extracted from .lok8s/lo, with dispatch_destroy replaced by a
# recorder. A copy of the function mirrored into the test would pass on the
# broken code, which is how this class of bug survived.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v yq &>/dev/null || skip "yq not available"

  export DOMAIN_NAME="test.dev"
  mkdir -p "${PATH_CLUSTERS}/${DOMAIN_NAME}"
  ACTED="${BATS_TEST_TMPDIR}/acted.log"
  : > "${ACTED}"

  # main::down reads `cluster` from main's dynamic scope.
  # shellcheck disable=SC2034
  cluster="test-down"

  import() { :; }
  export -f import
  # provision::read_kind — the reader under test, sourced as the CLI gets it.
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  # Every side effect main::down can have, recorded instead of performed.
  provision::dispatch_destroy() { echo "driver-destroy ${1}" >> "${ACTED}"; }
  tilt::down() { echo "tilt-down" >> "${ACTED}"; }
  kind() { echo "kind $*" >> "${ACTED}"; return 1; }
  docker() { echo "docker $*" >> "${ACTED}"; }

  eval "$(sed -n '/^main::down()/,/^}/p' "${_PROJECT_ROOT}/.lok8s/lo")"
}

teardown() { teardown_tmpdir; }

_write_spec() {
  cat > "${PATH_CLUSTERS}/${DOMAIN_NAME}/cluster.lok8s.yaml"
}

# Assert on the recorder. NOT written as `! grep -q …`: bash ignores errexit for
# a command inverted by `!`, so that spelling never fails a bats test — it reads
# like an assertion and asserts nothing. (Caught here by mutation: the mutant
# reached driver-destroy and the `! grep` line stayed green.)
_refute_acted() {
  grep -q "${1}" "${ACTED}" || return 0
  echo "expected NOT to find '${1}' in the recorded actions:" >&2
  cat "${ACTED}" >&2
  return 1
}

_assert_acted() {
  grep -qx "${1}" "${ACTED}" && return 0
  echo "expected '${1}' in the recorded actions:" >&2
  cat "${ACTED}" >&2
  return 1
}

@test "the real main::down is under test, not a copy of it" {
  # ANTI-VACUITY. If the extraction breaks — the function is renamed, moved out
  # of .lok8s/lo, or reindented — every assertion below would run against
  # nothing and pass. Fail loudly instead.
  declare -f main::down >/dev/null || {
    echo "could not extract main::down from .lok8s/lo — the sed in setup() is" >&2
    echo "stale, not the code. Do not delete this file; repoint the extraction." >&2
    return 1
  }
  local body; body="$(declare -f main::down)"
  [[ "${body}" == *provision::dispatch_destroy* ]] || {
    echo "the extracted main::down never calls provision::dispatch_destroy —" >&2
    echo "the destructive branch this file guards is gone or renamed." >&2
    return 1
  }
}

@test "a cloud spec DOES reach driver-destroy" {
  # The positive control. Without it the negative tests below could pass because
  # the harness never reaches driver-destroy at all.
  _write_spec <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: prod
YAML
  run main::down
  [ "${status}" -eq 0 ]
  _assert_acted "driver-destroy test.dev"
}

@test "a spec with no .kind must NOT reach driver-destroy" {
  # THE gate. Pre-fix this spec produced kind="null", which passed
  # `[[ -n … && != lo ]]` and deprovisioned infrastructure on a malformed file.
  _write_spec <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
metadata:
  name: prod
spec:
  kubernetes:
    version: "1.31.0"
YAML
  run main::down
  _refute_acted "driver-destroy"
  # And it refuses rather than falling through to the local teardown: the spec
  # exists but does not say what it is, so neither teardown is the right one.
  [ "${status}" -ne 0 ]
  _refute_acted "tilt-down"
}

@test "a traversal-shaped kind must NOT reach driver-destroy" {
  # Same branch, hostile input: the value is interpolated into a sourced path
  # downstream. read_kind's `^[a-z][a-z0-9]*$` is what stops it.
  _write_spec <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: ../../evil
metadata:
  name: prod
YAML
  run main::down
  _refute_acted "driver-destroy"
  [ "${status}" -ne 0 ]
}

@test "an empty .kind must NOT reach driver-destroy" {
  _write_spec <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: ""
metadata:
  name: prod
YAML
  run main::down
  _refute_acted "driver-destroy"
  [ "${status}" -ne 0 ]
}

@test "a Lo spec takes the local teardown" {
  _write_spec <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: dev
YAML
  run main::down
  [ "${status}" -eq 0 ]
  _assert_acted "tilt-down"
  _refute_acted "driver-destroy"
}

@test "no cluster spec at all takes the local teardown" {
  # An unprovisioned or deploy-only domain has only a kind cluster to remove.
  # This is why main::down reads the kind instead of calling domain::driver,
  # which answers "deploy" here and would select the driver branch.
  run main::down
  [ "${status}" -eq 0 ]
  _assert_acted "tilt-down"
  _refute_acted "driver-destroy"
}

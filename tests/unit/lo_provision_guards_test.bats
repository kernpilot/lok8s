#!/usr/bin/env bats
# lo_provision_guards_test.bats — `lo up` on the default driver must not report
# success when the cluster was never created.
#
# Why this exists
# ---------------
# Fourth and last driver in the #91 sweep, and the one that matters most to
# users: the `lo` driver is the default runtime and what `lo up` runs on a fresh
# checkout. `libs/provision:341` invokes it as
# `driver::provision "${domain}" || provision_rc=$?`, which disables errexit for
# the whole tree below.
#
# The kind branch of driver::provision ends like this:
#
#     rendered_config=$(lo::render_kind_config …)      # unguarded assignment
#     if ! kind get clusters | grep -q "^${name}$"; then
#       kind create cluster --config <(echo "${rendered_config}")   # unguarded
#     fi
#     …
#     lo::registries_tls_nudge                          # ← LAST statement
#   }
#
# so the function's exit status is the NUDGE's. That function returns 0 on every
# path (its own last statement is a `warn`), and it is about whether the host
# trusts the dev CA — nothing to do with whether a cluster exists. So the kind
# branch returned 0 unconditionally: a failed `kind create`, an empty rendered
# config, a kubeconfig that was never extracted, all reported as a provisioned
# cluster.
#
# The empty-render case is the same shape as #91 itself: a failed render left the
# variable empty and the empty value flowed downstream, here into
# `kind create --config <(echo "")`.
#
# Every test reproduces the caller's `|| rc=$?`. Without it errexit alone would
# catch these, which is precisely why they survived.

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export KIND_EXPERIMENTAL_DOCKER_NETWORK="lok8s-test"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: lotest
spec:
  runtime: kind
  kubernetes:
    version: "1.31.0"
YAML
}

teardown() { teardown_tmpdir; }

# Everything stubbed to SUCCEED; each test breaks exactly one collaborator.
_load() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  :args() { domain="test.dev"; }

  lo::read_config()      { return 0; }
  lo::validate_ips()     { return 0; }
  lo::network()          { return 0; }
  lo::registry_network() { return 0; }
  registry::is_shared()  { return 1; }   # not shared — keeps the path short
  lo::registries_tls_cert()  { return 0; }
  lo::write_certs_d()        { return 0; }
  lo::write_oidc_auth_config() { return 0; }
  lo::registries_tls_nudge() { return 0; }
  lo::render_kind_config()   { echo "kind: Cluster"; }
  _lo_extract_kubeconfig()   { echo "${PATH_BASE}/.kubeconfig/lotest.yaml"; }
  lo::coredns()              { return 0; }
  lo::apply_local_registry_hosting() { return 0; }
  lo::registry_configmap()   { return 0; }
  # kapply::run <phase> <fn> [args…] — run the function, as the real one does.
  kapply::run() { local _phase="${1}" fn="${2}"; shift 2; "${fn}" "${@}"; }
  # `kind get clusters` must come back EMPTY so the create branch is taken.
  kind() { case "${1:-}" in get) return 0 ;; *) return 0 ;; esac; }
}

@test "lo provision: a FAILED 'kind create' does not report success" {
  _load
  kind() {
    case "${1:-}" in
      get)    return 0 ;;      # no existing cluster
      create) return 1 ;;      # creation fails
      *)      return 0 ;;
    esac
  }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although 'kind create cluster' FAILED." >&2
    echo "lo up reports a provisioned cluster that does not exist; the function's" >&2
    echo "exit status is lo::registries_tls_nudge's, which only concerns whether" >&2
    echo "the host trusts the dev CA." >&2
    return 1
  }
}

@test "lo provision: a FAILED kind-config render does not report success" {
  # The #91 shape exactly: the assignment is unguarded, so a failed render leaves
  # the variable EMPTY and the empty value is handed to kind create --config.
  _load
  lo::render_kind_config() { return 1; }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although the kind config render FAILED —" >&2
    echo "an empty config was passed to 'kind create cluster'." >&2
    return 1
  }
}

@test "lo provision: an empty rendered config never reaches 'kind create'" {
  # Returning non-zero is not enough on its own: the property that can only mean
  # one thing is that kind is never asked to create a cluster from nothing.
  _load
  lo::render_kind_config() { echo ""; }
  local marker="${BATS_TEST_TMPDIR}/kind-create-was-called"
  kind() {
    case "${1:-}" in
      get)    return 0 ;;
      create) touch "${marker}"; return 0 ;;
      *)      return 0 ;;
    esac
  }

  local rc=0
  driver::provision test.dev || rc=$?

  [ ! -f "${marker}" ] || {
    echo "'kind create cluster' was invoked with an EMPTY --config." >&2
    return 1
  }
}

@test "lo provision: a FAILED kubeconfig extraction does not report success" {
  _load
  _lo_extract_kubeconfig() { return 1; }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although the kubeconfig extraction FAILED." >&2
    echo "Everything downstream (bootstrap, addons) reads that file." >&2
    return 1
  }
}

@test "lo provision: the fully-successful path still returns 0" {
  # ANTI-VACUITY for all four above.
  _load

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -eq 0 ] || { echo "lo provision happy path regressed (rc=${rc})" >&2; return 1; }
}

@test "lo provision: an unhappy TLS nudge does not fail a good provision" {
  # The other direction, and what makes the explicit `return 0` load-bearing.
  # lo::registries_tls_nudge is advisory — it reports whether the HOST trusts the
  # dev CA, which says nothing about whether a cluster was created. It must
  # therefore never decide this function's verdict, in either direction: the bug
  # was that its 0 masked real failures, and the mirror of that bug would be a
  # non-zero from it failing a cluster that came up fine.
  #
  # Without this test the `return 0` is untested: with every call above guarded,
  # the nudge is only reached on the successful path, where it returns 0 anyway.
  # Verified by mutation — removing the `return 0` turns THIS test red and no
  # other.
  _load
  lo::registries_tls_nudge() { return 3; }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -eq 0 ] || {
    echo "driver::provision returned ${rc} because the advisory TLS nudge did." >&2
    echo "The cluster was created successfully; a host trust-store nag must not" >&2
    echo "fail the provision." >&2
    return 1
  }
}

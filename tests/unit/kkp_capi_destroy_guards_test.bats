#!/usr/bin/env bats
# kkp_capi_destroy_guards_test.bats — the #91 silent-success class, in the two
# destroy paths nobody swept.
#
# Why this exists
# ---------------
# The libs/provision destroy call site (provision::dispatch_destroy) runs
# `driver::destroy "${domain}" || destroy_rc=$?`, which
# disables errexit for the whole call tree. kubeone_destroy_guards_test.bats pins
# that driver. The kkp and capi drivers have the same caller and were never
# checked, and both end in a command that cannot fail:
#
#   kkp:   kkp::delete_cluster ... || warn "…continuing cleanup"
#          rm -f  "${PATH_BASE}/.kubeconfig/${name}.yaml"
#          rm -rf "${work_dir}"          # ← deletes cluster_id
#          debug  "…destroyed"           # ← function returns THIS status: 0
#
#   capi:  kubectl delete cluster --wait --timeout=600s || del_rc=$?
#          … if del_rc != 0: error "KEEPING the management cluster…"
#          rm -f "${PATH_BASE}/.kubeconfig/${name}.yaml"   # always 0
#        }                                                 # → returns 0
#
# kkp is the worse of the two, and it is not symmetric with kubeone. The driver
# itself documents (main, destroy step 1) that without a saved cluster_id a
# destroy is impossible — it errors "Cannot destroy cluster without a cluster ID"
# and returns 1. So `rm -rf "${work_dir}"` after a FAILED delete does not merely
# lose state: it removes the only handle that could ever retry, on a cluster that
# is still running and still billing, and then reports success.
#
# capi's failure is louder but ends the same way. It prints "KEEPING the
# management cluster so CAPH can finish deprovisioning" — good advice — and then
# returns 0, so `main::down` reads success and suppresses the orphaned-infra
# warning that the rc=3 remap at that call site explicitly exists to preserve.
#
# What this does NOT change: `kkp::delete_cluster || warn` and kubeone's
# `kubeone::reset || warn` are both deliberate on their own terms — a failed
# remote call should not abort local cleanup. The property under test is narrower
# and is the one that can only mean one thing: do not report success you did not
# have, and do not destroy the evidence on your way out.
#
# Every test reproduces the caller's `|| rc=$?`. Without it these pass on the
# broken code, because errexit alone would catch it — which is exactly how this
# class survives review.

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
}

teardown() { teardown_tmpdir; }

# ── kkp ──────────────────────────────────────────────────────────────────────

_kkp_yaml() {
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: destroytest
spec:
  kkp:
    apiUrl: https://kkp.example.test
    projectId: proj-abc
YAML
}

# Everything stubbed to SUCCEED; each test breaks exactly one collaborator.
_load_kkp_destroy() {
  _kkp_yaml
  export KKP_WORK="${PATH_CLUSTERS}/test.dev/.kkp"
  mkdir -p "${KKP_WORK}"
  printf 'cl-123' > "${KKP_WORK}/cluster_id"
  printf 'proj-abc' > "${KKP_WORK}/project_id"
  printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/destroytest.yaml"

  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/main"
  :args() { domain="test.dev"; }
  kkp::validate_url()   { return 0; }
  kkp::delete_cluster() { return 0; }
  export KKP_API_URL="https://kkp.example.test"
}

@test "kkp: a FAILED cluster delete does not report success" {
  _load_kkp_destroy
  kkp::delete_cluster() { return 1; }

  local rc=0
  driver::destroy test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::destroy returned 0 although kkp::delete_cluster FAILED —" >&2
    echo "the KKP user cluster is still running and still billing, and lo down" >&2
    echo "reports success. This is issue #91's class on the kkp destroy path." >&2
    return 1
  }
}

@test "kkp: a FAILED delete KEEPS cluster_id — the only handle to retry" {
  # Returning non-zero is not sufficient. The driver refuses to destroy without a
  # saved cluster_id ("Cannot destroy cluster without a cluster ID"), so wiping
  # work_dir on a failed delete makes the orphan PERMANENTLY unreachable. The
  # surviving file is the property that can only mean one thing.
  _load_kkp_destroy
  kkp::delete_cluster() { return 1; }

  local rc=0
  driver::destroy test.dev || rc=$?

  [ -f "${KKP_WORK}/cluster_id" ] || {
    echo "cluster_id was deleted after a FAILED KKP delete — the cluster is" >&2
    echo "orphaned AND can no longer be addressed: driver::destroy hard-fails" >&2
    echo "without it. Nothing left can clean this up." >&2
    return 1
  }
}

@test "kkp: the fully-successful path returns 0 and cleans local state" {
  # Guards against 'fixing' the above by making destroy always fail.
  _load_kkp_destroy

  local rc=0
  driver::destroy test.dev || rc=$?

  [ "${rc}" -eq 0 ] || { echo "kkp happy path regressed (rc=${rc})" >&2; return 1; }
  [ ! -d "${KKP_WORK}" ] || {
    echo "a SUCCESSFUL destroy left ${KKP_WORK} behind — stale cluster_id will" >&2
    echo "make the next destroy address a cluster that no longer exists." >&2
    return 1
  }
}

# ── capi ─────────────────────────────────────────────────────────────────────

_capi_yaml() {
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: destroytest
spec:
  managementCluster:
    domain: mgmt.dev
    local: false
  cluster:
    namespace: default
YAML
}

_load_capi_destroy() {
  _capi_yaml
  printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/mgmt.dev.yaml"
  printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/destroytest.yaml"

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"
  :args() { domain="test.dev"; }
  kubehz::read_config()    { export LOK8S_KUBEHZ_HOSTING="self-hosted"; return 0; }
  capi::mgmt_kind_name()   { echo "lok8s-mgmt"; }
  kubectl()                { return 0; }
  kind()                   { return 0; }
}

@test "capi: a FAILED workload cluster delete does not report success" {
  _load_capi_destroy
  # The real failure is the --wait --timeout=600s delete giving up while CAPH is
  # still deprovisioning Hetzner servers.
  kubectl() { return 1; }

  local rc=0
  driver::destroy test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::destroy returned 0 although the workload cluster delete" >&2
    echo "FAILED. The driver even prints 'KEEPING the management cluster' —" >&2
    echo "then reports success, so main::down suppresses its orphaned-infra" >&2
    echo "warning (the case provision::dispatch_destroy's rc remap protects)." >&2
    return 1
  }
}

@test "capi: a FAILED delete KEEPS the workload kubeconfig" {
  # Same reasoning as the kubeone guard: the failed teardown leaves live
  # infrastructure, and the kubeconfig is how an operator reaches it.
  _load_capi_destroy
  kubectl() { return 1; }

  local rc=0
  driver::destroy test.dev || rc=$?

  [ -f "${PATH_BASE}/.kubeconfig/destroytest.yaml" ] || {
    echo "the workload kubeconfig was deleted after a FAILED teardown —" >&2
    echo "servers may still be up and the handle to them is gone." >&2
    return 1
  }
}

@test "capi: a MISSING mgmt kubeconfig (remote mgmt) is a failed destroy, not a success" {
  # With the management kubeconfig gone and the management cluster REMOTE
  # (mgmt_local=false — no kind-based recovery possible), the delete cannot even
  # be attempted. The old code skipped it silently (del_rc stayed 0), removed
  # the workload kubeconfig, and returned success — the same silent-success
  # class, entered one step earlier.
  _load_capi_destroy
  rm -f "${PATH_BASE}/.kubeconfig/mgmt.dev.yaml"

  local rc=0
  driver::destroy test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::destroy returned 0 although the mgmt kubeconfig is missing" >&2
    echo "and the management cluster is remote — no delete was ever attempted," >&2
    echo "yet lo down reports success while Hetzner keeps billing." >&2
    return 1
  }
}

@test "capi: a MISSING mgmt kubeconfig (remote mgmt) KEEPS the workload kubeconfig" {
  _load_capi_destroy
  rm -f "${PATH_BASE}/.kubeconfig/mgmt.dev.yaml"

  local rc=0
  driver::destroy test.dev || rc=$?

  [ -f "${PATH_BASE}/.kubeconfig/destroytest.yaml" ] || {
    echo "the workload kubeconfig was deleted although the destroy never ran —" >&2
    echo "the servers are still up and the handle to them is gone." >&2
    return 1
  }
}

@test "capi: the fully-successful path returns 0 and removes the kubeconfig" {
  _load_capi_destroy

  local rc=0
  driver::destroy test.dev || rc=$?

  [ "${rc}" -eq 0 ] || { echo "capi happy path regressed (rc=${rc})" >&2; return 1; }
  [ ! -f "${PATH_BASE}/.kubeconfig/destroytest.yaml" ] || {
    echo "a SUCCESSFUL destroy left the workload kubeconfig behind." >&2
    return 1
  }
}

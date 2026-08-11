#!/usr/bin/env bats
# capi_provision_guards_test.bats — the #91 silent-success class on the CAPI
# PROVISION path.
#
# Why this exists
# ---------------
# `libs/provision:341` runs `driver::provision "${domain}" || provision_rc=$?`,
# which disables errexit for the whole call tree. Issue #91 was that trap on the
# kubeone driver; kkp_capi_destroy_guards_test.bats pinned the DESTROY half for
# kkp and capi. Nobody swept capi's provision, and it has the same shape:
#
#     capi::ensure_credentials "${cluster_yaml}" "${provider}" "${mgmt_kubeconfig}"
#     resources=$(capi::generate …) || return 1
#     kubectl apply -f <(echo "${resources}")      # ← runs regardless
#
# `capi::ensure_credentials` creates the infra credential Secret on the
# management cluster. Without it the apply still SUCCEEDS — applying CAPI custom
# resources is just writing CRs — and then CAPH cannot authenticate to Hetzner,
# so no Machine is ever created. The failure surfaces (if at all) fifteen minutes
# later in `capi::wait_ready`, attributed to the wrong thing entirely, on a
# cluster the operator was told was being provisioned.
#
# The same paragraph applies to `capi::detect_provider` (an empty provider flows
# into credentials, generate and the mgmt bootstrap) and to the namespace apply
# (credentials and resources then land in a namespace that does not exist).
#
# NOT changed by the fix these tests pin: `capi::bootstrap` is already correct —
# it is followed by `return $?`, which propagates. It is easy to misread as
# unguarded; it is not.
#
# Every test reproduces the caller's `|| rc=$?`. Without it they pass on the
# broken code, because errexit alone would have caught it — which is exactly how
# this class survives review.

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

_yaml() {
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: provtest
spec:
  managementCluster:
    domain: mgmt.dev
    local: false
  cluster:
    namespace: default
YAML
}

# Everything stubbed to SUCCEED; each test breaks exactly one collaborator.
_load() {
  _yaml
  printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/mgmt.dev.yaml"

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"
  :args() { domain="test.dev"; }
  # "self" is the real value (libs/kubehz/main validates hosting ∈ self|hosted);
  # any stub must speak the production vocabulary or it pins a scenario that
  # cannot occur.
  kubehz::read_config()       { export LOK8S_KUBEHZ_HOSTING="self"; return 0; }
  capi::detect_provider()     { echo "hetzner"; }
  capi::ensure_credentials()  { return 0; }
  capi::ensure_local_mgmt()   { return 0; }
  capi::generate()            { echo "# capi resources"; }
  capi::wait_ready()          { return 0; }
  kubectl()                   { return 0; }
  # The kubeconfig-extraction loop writes through clusterctl and then requires a
  # NON-EMPTY file, so the happy path needs real content here.
  clusterctl() { printf 'apiVersion: v1\nkind: Config\n'; }
}

@test "capi provision: a FAILED credential setup does not report success" {
  _load
  capi::ensure_credentials() { return 1; }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although capi::ensure_credentials FAILED." >&2
    echo "The CAPI resources are applied anyway (applying CRs succeeds), CAPH then" >&2
    echo "cannot authenticate to the provider, and no Machine is ever created —" >&2
    echo "while lo provision reports a provisioned cluster." >&2
    return 1
  }
}

@test "capi provision: a FAILED provider detection does not report success" {
  # An empty provider flows into ensure_credentials, generate and the local-mgmt
  # bootstrap, each of which would build the wrong thing rather than refuse.
  _load
  capi::detect_provider() { return 1; }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although capi::detect_provider FAILED —" >&2
    echo "the provision continued with an empty provider." >&2
    return 1
  }
}

@test "capi provision: a FAILED namespace apply does not report success" {
  # Credentials and CAPI resources would otherwise be applied into a namespace
  # that does not exist.
  #
  # The stub fails ONLY the namespace calls and succeeds for everything after.
  # A blanket `kubectl() { return 1; }` also breaks the readyz loop at the end,
  # so the provision returns non-zero for an unrelated reason and the test passes
  # against the broken code — it did, on the first run of this file. Isolating
  # the failure is what makes the assertion mean the thing it claims.
  _load
  kubectl() {
    local a
    for a in "${@}"; do
      [[ "${a}" == "namespace" ]] && return 1
    done
    return 0
  }

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although the namespace apply FAILED —" >&2
    echo "credentials and CAPI resources are applied into a namespace that does" >&2
    echo "not exist." >&2
    return 1
  }
}

@test "capi provision: a FAILED config read does not misdiagnose a hosted cluster" {
  # Here the RETURN CODE was already non-zero before the fix, so asserting on it
  # would prove nothing. The defect is the DIAGNOSIS: with the read unguarded,
  # LOK8S_KUBEHZ_HOSTING stays unset, the != "hosted" branch is taken, and the
  # operator of a HOSTED cluster is told to set spec.managementCluster.domain —
  # advice that is wrong for their configuration.
  #
  # PRODUCTION read_config, not a stub: review round 1 found the first version
  # of this test proved nothing about production — it stubbed
  # `kubehz::read_config() { return 1; }`, but the production function could
  # NEVER return non-zero (yq failures died inside assignments; the trailing
  # `export` reset the status to 0), so the guard it pinned was vacuous. The
  # production repro is a spec file that cannot be read: read_config now refuses
  # it (see kubehz_config_test.bats), and the driver must surface THAT error,
  # not the managementCluster.domain advice.
  _load
  # Restore the real read_config over _load's stub (import is a no-op here, so
  # sourcing the driver did not bring the lib in).
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  # No spec file at all → every yq at the top of driver::provision fails softly
  # (errexit is off in this call tree), mgmt_domain stays empty, and the driver
  # reaches the read_config guard with a file it cannot read.
  rm -f "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"

  local rc=0 out
  out="$(driver::provision test.dev 2>&1)" || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::provision returned 0 although the cluster spec could not be read." >&2
    return 1
  }
  [[ "${out}" == *"cannot read cluster spec"* ]] || {
    echo "expected read_config's own diagnosis ('cannot read cluster spec')." >&2
    echo "Output was:" >&2
    printf '%s\n' "${out}" | sed 's/^/    /' >&2
    return 1
  }
  [[ "${out}" != *"managementCluster.domain is required"* ]] || {
    echo "a failed kubehz::read_config was reported as a missing" >&2
    echo "spec.managementCluster.domain. Output was:" >&2
    printf '%s\n' "${out}" | sed 's/^/    /' >&2
    return 1
  }
}

@test "capi provision: the fully-successful path still returns 0" {
  # ANTI-VACUITY for all four above: without this, 'fixing' them by making
  # provision always fail would look like a pass.
  _load

  local rc=0
  driver::provision test.dev || rc=$?

  [ "${rc}" -eq 0 ] || { echo "capi provision happy path regressed (rc=${rc})" >&2; return 1; }
}

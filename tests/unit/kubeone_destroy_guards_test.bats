#!/usr/bin/env bats
# kubeone_destroy_guards_test.bats — a failed destroy must not report success,
# and must not throw away the operator's access on the way out.
#
# Why this exists
# ---------------
# Same trap as #91, on the other entry point: `libs/provision:463` runs
# `driver::destroy "${domain}" || destroy_rc=$?`, which disables errexit for the
# whole tree. In driver::destroy the tail is:
#
#     provider::destroy ...                       # fails, execution continues
#     cluster_name=$(yq -r '.metadata.name' ...)
#     rm -f "${PATH_BASE}/.kubeconfig/${name}.yaml"   # always exits 0
#   }                                             # → function returns 0
#
# So a failed INFRASTRUCTURE destroy returned success — while deleting the
# kubeconfig. The servers keep running and billing on Hetzner, `lo destroy` says
# it worked, and the one handle that could still reach them is gone. That is
# worse than the provision case, which at least left the evidence in place.
#
# `kubeone::reset || warn "…continuing"` on the line above is deliberate and is
# left alone: a reset failure genuinely should not block the infrastructure
# teardown. The distinction matters — this is not "guard everything that can
# fail", it is "do not report success you did not have".

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  import() { :; }
  export -f import
  cat > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: destroytest
spec:
  provider:
    name: hetzner
YAML
  KCFG="${PATH_BASE}/.kubeconfig/destroytest.yaml"
  printf 'apiVersion: v1\nkind: Config\n' > "${KCFG}"
}

teardown() { teardown_tmpdir; }

# Everything stubbed to SUCCEED; each test breaks exactly one collaborator.
_load_destroy() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"
  :args() { domain="test.dev"; }
  kubehz::read_config() { export LOK8S_KUBEHZ_HOSTING="self-hosted"; return 0; }
  kubeone::reset()      { return 0; }
  provider::destroy()   { return 0; }
  export PROVIDER_CONFIG_FILE="${BATS_TEST_TMPDIR}/provider.json"
  echo '{}' > "${PROVIDER_CONFIG_FILE}"
}

@test "a FAILED infrastructure destroy does not report success" {
  _load_destroy
  provider::destroy() { return 1; }

  # The caller's context — this is what suppresses errexit (libs/provision:463).
  local rc=0
  driver::destroy test.dev || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "driver::destroy returned 0 although provider::destroy FAILED —" >&2
    echo "the infrastructure is still up and still billing, and lo said it worked." >&2
    return 1
  }
}

@test "a FAILED infrastructure destroy KEEPS the kubeconfig" {
  # The property that can only mean one thing. Returning non-zero is not enough:
  # if the kubeconfig is deleted anyway, the operator has lost the only handle on
  # servers that are still running.
  _load_destroy
  provider::destroy() { return 1; }

  local rc=0
  driver::destroy test.dev || rc=$?
  [ "${rc}" -ne 0 ]

  [ -f "${PATH_BASE}/.kubeconfig/destroytest.yaml" ] || {
    echo "the kubeconfig was deleted even though the infrastructure destroy FAILED" >&2
    echo "— the cluster is still up and nothing can reach it any more." >&2
    return 1
  }
}

@test "a FAILED config read stops the destroy" {
  # Without this, LOK8S_KUBEHZ_HOSTING is never set, the hosted branch is not
  # taken, and a HOSTED cluster gets the self-hosted teardown path instead of
  # the kubehz API call — the wrong destroy entirely.
  _load_destroy
  kubehz::read_config() { return 1; }

  local rc=0
  driver::destroy test.dev || rc=$?
  [ "${rc}" -ne 0 ] || {
    echo "driver::destroy returned 0 although the config read FAILED" >&2
    return 1
  }
}

@test "a reset failure still proceeds to the infrastructure destroy" {
  # Deliberate behaviour, pinned so a later 'guard everything' sweep does not
  # quietly turn a recoverable case into a blocker: if kubeone reset fails, the
  # infrastructure must STILL be torn down, or the servers leak.
  _load_destroy
  mkdir -p "${PATH_CLUSTERS}/test.dev/.kubeone"
  printf 'kind: KubeOneCluster\n' > "${PATH_CLUSTERS}/test.dev/.kubeone/kubeone.yaml"
  local trace="${BATS_TEST_TMPDIR}/trace"; : > "${trace}"
  kubeone::reset()    { return 1; }
  provider::destroy() { echo destroyed >> "${trace}"; return 0; }

  local rc=0
  driver::destroy test.dev || rc=$?
  [ "${rc}" -eq 0 ] || { echo "a reset failure must not fail the destroy (rc=${rc})" >&2; return 1; }
  grep -q destroyed "${trace}" || {
    echo "provider::destroy never ran after the reset failed — the servers would leak" >&2
    return 1
  }
}

@test "the fully-successful path returns 0 and removes the kubeconfig" {
  _load_destroy
  local rc=0
  driver::destroy test.dev || rc=$?
  [ "${rc}" -eq 0 ] || { echo "happy path regressed (rc=${rc})" >&2; return 1; }
  [ ! -f "${PATH_BASE}/.kubeconfig/destroytest.yaml" ] || {
    echo "the kubeconfig survived a SUCCESSFUL destroy" >&2
    return 1
  }
}

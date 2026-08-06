#!/usr/bin/env bats
# kubeone_render_failure_test.bats — a failed manifest render must STOP the
# provision, not fall through it.
#
# Why this exists
# ---------------
# Issue #91: a `lo provision` (kubeone driver) worker re-join "exited 0 but never
# executed kubeone apply — the node was simply not joined, with nothing in the
# output indicating a failure."
#
# The mechanism is a bash trap, not a typo. `.lok8s/libs/provision` invokes the
# driver as `driver::provision "${domain}" || provision_rc=$?` (line 341), and
# testing a command's status **disables errexit for its entire call tree**. The
# `lo` driver documents exactly this and defends every risky call with an
# explicit `|| return 1`. The kubeone driver did not: `kubeone::generate_config`
# was called unguarded, and inside it the manifest render was a bare assignment.
# So a render failure left `rendered` empty and execution simply continued.
#
# Issue #91's first suggestion — route through `template::envsubst` — had already
# landed. Its second, "the driver should hard-fail when the config render errors,
# rather than continuing to a 0 exit", is the half this pins, and the issue calls
# it "arguably the worse half". A wrong manifest is loud eventually; a skipped
# apply that exits 0 is not.
#
# These tests run the render under the SAME errexit-suppressed context the real
# dispatch creates. Running them without that context would pass on the broken
# code, because errexit alone would have caught it — which is precisely why the
# bug survived: it is invisible unless you reproduce the caller.

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
}

teardown() { teardown_tmpdir; }

_load_driver() {
  provider::detect() { echo "hetzner"; }
  addons::render() { echo "# stub addon"; }
  export -f provider::detect addons::render
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
}

_cluster_yaml() {
  local f="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  mkdir -p "$(dirname "${f}")"
  cat > "${f}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: rendertest
spec:
  provider:
    name: hetzner
  kubernetes:
    version: "1.31.0"
  network:
    podSubnet: 10.244.0.0/16
    serviceSubnet: 10.96.0.0/12
YAML
  printf '%s' "${f}"
}

@test "generate_config FAILS when the manifest render fails" {
  _load_driver
  local cy; cy="$(_cluster_yaml)"

  # renvsubst rejecting the positional whitelist is what issue #91 reports; any
  # render failure has the same shape, so model it at the seam.
  template::envsubst() { return 1; }

  # THE CALLER'S CONTEXT: `|| rc=$?` is what libs/provision:341 does, and it
  # switches errexit off for everything below. Without this line the test would
  # pass on the broken code and prove nothing.
  local rc=0
  kubeone::generate_config "${cy}" hetzner "${BATS_TEST_TMPDIR}/work" || rc=$?

  [ "${rc}" -ne 0 ] || {
    echo "generate_config returned 0 despite the manifest render failing —" >&2
    echo "the provision continues and kubeone apply runs on (or skips) a manifest" >&2
    echo "nobody rendered. This is issue #91." >&2
    return 1
  }
}

@test "a failed render leaves NO kubeone.yaml behind to be applied" {
  # Returning non-zero is not sufficient on its own: if a half-written manifest
  # is left on disk, a later step (or a rerun with --bootstrap) can still pick it
  # up. The absence of the artifact is the property that can only mean one thing.
  _load_driver
  local cy; cy="$(_cluster_yaml)"
  local work="${BATS_TEST_TMPDIR}/work"

  template::envsubst() { return 1; }
  local rc=0
  kubeone::generate_config "${cy}" hetzner "${work}" || rc=$?
  [ "${rc}" -ne 0 ]

  [ ! -s "${work}/kubeone.yaml" ] || {
    echo "a non-empty ${work}/kubeone.yaml exists after a FAILED render:" >&2
    sed 's/^/    /' "${work}/kubeone.yaml" >&2
    return 1
  }
}

@test "the happy path still renders a usable manifest" {
  # Guards against 'fixing' the above by making generate_config always fail.
  _load_driver
  command -v jq &>/dev/null || skip "jq not available (template::envsubst native path)"
  local cy; cy="$(_cluster_yaml)"
  local work="${BATS_TEST_TMPDIR}/work-ok"

  local rc=0
  kubeone::generate_config "${cy}" hetzner "${work}" || rc=$?
  [ "${rc}" -eq 0 ] || { echo "happy path regressed (rc=${rc})" >&2; return 1; }
  [ -s "${work}/kubeone.yaml" ]
  grep -q 'kind: KubeOneCluster' "${work}/kubeone.yaml"
  # …and the render actually substituted, rather than leaving the placeholders.
  ! grep -q '\${CLUSTER_NAME}' "${work}/kubeone.yaml"
}

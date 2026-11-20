#!/usr/bin/env bats
# kuberc_test.bats — every kubectl apply and kubectl diff of the bash tree
# runs with the kubectl user defaults off: `KUBERC=off kubectl …`. A kuberc
# can set a flag default for any kubectl command (for example --show-secrets
# on a diff, kubectl v1.36 and later). kubectl v1.33 and later read KUBERC,
# and the value off loads no kuberc file; an older kubectl ignores it. The
# Go twin is execx.KubeRCOff (the Env of each such call), with a test in
# each package that runs one.

setup() {
  load "../test_helper"
}

# The call sites: a kubectl, an optional --kubeconfig "<path>" or the
# kubectl_flags array, then apply or diff and a flag. A comment line and the
# addon and Tilt data are not code of lo.
_kubectl_apply_sites() {
  grep -rnE 'kubectl( --kubeconfig "[^"]*"| "\$\{kubectl_flags\[@\]\}")? (apply|diff) -' \
    --exclude-dir=addons --exclude-dir=tilt "${_PROJECT_ROOT}/.lok8s" \
    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#'
}

@test "kuberc: every kubectl apply and diff of the bash tree runs with KUBERC=off" {
  local sites missing
  sites="$(_kubectl_apply_sites)"
  # Anti-vacuity: the sweep finds the call sites (kapply, the bootstrap diff
  # and restore.d, the drivers, the inventory, the agent deploy).
  [ "$(printf '%s\n' "${sites}" | grep -c .)" -ge 20 ]
  printf '%s\n' "${sites}" | grep -q 'utils/kapply.sh:'
  printf '%s\n' "${sites}" | grep -q 'libs/bootstrap:.* diff --server-side'
  missing="$(printf '%s\n' "${sites}" | grep -v 'KUBERC=off kubectl' || true)"
  if [[ -n "${missing}" ]]; then
    printf 'a kubectl apply or diff without KUBERC=off:\n%s\n' "${missing}" >&2
    return 1
  fi
}

@test "kuberc: a KUBERC=off prefix reaches the kubectl child, and the shell keeps its own KUBERC" {
  export KUBERC=/user/kuberc
  local seen
  seen="$(KUBERC=off bash -c 'printf %s "${KUBERC}"')"
  [ "${seen}" = off ]
  [ "${KUBERC}" = /user/kuberc ]
}

#!/usr/bin/env bats
# provision_gate_test.bats — unit tests for the real-infrastructure gate
# (provision::confirm_infra). Cloud drivers (kubeone/capi/kkp) must show a
# summary and get an interactive yes before any provider/driver runs; the
# local kind driver stays frictionless; --force (the global flag, inherited
# via argsh dynamic scoping) and `lo recover`'s pre-authorization bypass it.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  # A minimal cloud-driver spec — confirm_infra reads it directly.
  SPEC="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  cat > "${SPEC}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: test-prod
spec:
  kubernetes:
    version: "v1.35.5"
  cluster:
    domain: test.prod
  provider:
    name: hetzner
  bootstrap:
    - name: cilium
    - name: metallb
    - name: cert-manager
YAML
}

teardown() {
  teardown_tmpdir
}

# Force the interactive/non-interactive branch without a real terminal.
_assume_tty()  { provision::_interactive() { return 0; }; }
_assume_no_tty() { provision::_interactive() { return 1; }; }

# ── Exemptions ───────────────────────────────────────────

@test "gate: local kind driver (lo) passes without prompt or output" {
  _assume_no_tty
  run provision::confirm_infra "test.dev" "${SPEC}" "lo" "reconcile"
  assert_success
  assert_output ""
}

@test "gate: --force (inherited via dynamic scoping) bypasses silently" {
  _assume_no_tty
  local force=1
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "reconcile"
  assert_success
  assert_output ""
}

@test "gate: remote-mode lo driver is NOT exempt (cloud VM)" {
  _assume_no_tty
  export LOK8S_REMOTE=1
  run provision::confirm_infra "test.dev" "${SPEC}" "lo" "reconcile"
  [ "${status}" -eq 3 ]
  assert_output --partial "provisions/updates the remote VM"
  assert_output --partial "refusing to reconcile"
}

# ── Non-interactive refusal ──────────────────────────────

@test "gate: non-interactive reconcile refuses with summary + hint" {
  _assume_no_tty
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "reconcile"
  assert_failure
  assert_output --partial "targets"
  assert_output --partial "real infrastructure"
  assert_output --partial "kubeone driver · provider hetzner"
  assert_output --partial "bootstrap DAG (3 addons)"
  assert_output --partial "refusing to reconcile 'test.prod' non-interactively — re-run with --force"
}

@test "gate: LOK8S_NONINTERACTIVE forces the refusal branch" {
  export LOK8S_NONINTERACTIVE=1
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "reconcile"
  assert_failure
  assert_output --partial "refusing to reconcile"
}

# ── Interactive prompt ───────────────────────────────────

@test "gate: reconcile accepts y and yes" {
  _assume_tty
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "reconcile" <<< "y"
  assert_success
  assert_output --partial "proceed? [y/N]"

  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "reconcile" <<< "yes"
  assert_success
}

@test "gate: reconcile aborts on n / empty answer with sentinel rc 3" {
  _assume_tty
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "reconcile" <<< "n"
  [ "${status}" -eq 3 ]
  assert_output --partial "aborted — 'test.prod' left untouched"

  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "reconcile" <<< ""
  [ "${status}" -eq 3 ]
}

@test "gate: bootstrap action names the addon re-apply" {
  _assume_tty
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "bootstrap" <<< "y"
  assert_success
  assert_output --partial "re-apply 3 bootstrap addons on the LIVE cluster"
}

# ── Destroy: literal yes ─────────────────────────────────

@test "gate: destroy rejects a mere y" {
  _assume_tty
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "destroy" <<< "y"
  assert_failure
  assert_output --partial "type yes to continue"
  assert_output --partial "aborted"
}

@test "gate: destroy accepts a literal yes" {
  _assume_tty
  run provision::confirm_infra "test.prod" "${SPEC}" "kubeone" "destroy" <<< "yes"
  assert_success
  assert_output --partial "deprovisions the cluster's cloud resources"
}

# ── Dispatch wiring ──────────────────────────────────────

# Minimal dispatch harness: resolve/creds stubbed, a fake kubeone driver dir
# so the kind check passes, recorders proving the gate fires FIRST.
_setup_dispatch_stubs() {
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/kubeone"
  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/kubeone/main" <<'EOF'
driver::provision() { echo "DRIVER-PROVISION-RAN"; }
driver::destroy()   { echo "DRIVER-DESTROY-RAN"; }
EOF
  provision::resolve_spec() { LOK8S_SPEC_FILE="${SPEC}"; LOK8S_SPEC_KIND="kubeone"; }
  provision::load_provider_creds() { :; }
  kubehz::read_config() { :; }
  kubehz::deregister_cluster() { echo "DEREGISTERED"; }
  LOK8S_KUBEHZ_ACCESS="agent"
}

@test "dispatch_destroy: decline stops before deregistration and the driver" {
  _assume_tty
  _setup_dispatch_stubs
  run provision::dispatch_destroy "test.prod" <<< "no"
  [ "${status}" -eq 3 ]
  refute_output --partial "DEREGISTERED"
  refute_output --partial "DRIVER-DESTROY-RAN"
}

@test "dispatch_destroy: accept runs deregistration then the driver" {
  _assume_tty
  _setup_dispatch_stubs
  run provision::dispatch_destroy "test.prod" <<< "yes"
  assert_success
  assert_output --partial "DEREGISTERED"
  assert_output --partial "DRIVER-DESTROY-RAN"
}

@test "dispatch: bootstrap_only maps to the bootstrap gate action" {
  _assume_tty
  _setup_dispatch_stubs
  run provision::dispatch "test.prod" 1 <<< "n"
  [ "${status}" -eq 3 ]
  assert_output --partial "re-apply 3 bootstrap addons"
  refute_output --partial "DRIVER-PROVISION-RAN"
}

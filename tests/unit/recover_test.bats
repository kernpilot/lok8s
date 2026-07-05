#!/usr/bin/env bats
# recover_test.bats — `lo recover <domain>`, the bare-metal DR orchestrator.
#
# recover::_run drives the phase sequence
#   resolve → doctor → consent → rebuild → provision → verify
# reusing existing primitives (provision::resolve_spec / provision::dispatch,
# the provider contract's provider::rebuild / provider::doctor / provider::output).
# These tests source the lib and replace those primitives with RECORDING fakes,
# then assert the phase order, the timing summary, the flag behaviours
# (--skip-rebuild / --dry-run), the destructive consent guard, and the resolve
# guards (non-cluster domain, provider without provider::rebuild).
#
# The orchestration is tested via recover::_run (not main::recover) so it runs
# without argsh's :args / ~domain parser — the codebase convention (see
# lo_commands_test.bats).

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/recover"

  # Every fake appends its name to this file, so the parent can assert call order.
  export RECORD="${BATS_TEST_TMPDIR}/record"
  : > "${RECORD}"

  # A non-existent spec/config path — recover::_cluster_name tolerates it and
  # falls back to the domain (no yq call), keeping the tests hermetic.
  export _FAKE_SPEC="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  export _FAKE_CFG="${BATS_TEST_TMPDIR}/hetzner.json"

  # Ensure the confirm defaults to interactive+blocking unless a test opts out.
  unset LOK8S_NONINTERACTIVE
  force=0
}

teardown() { teardown_tmpdir; }

# ── shared fakes ─────────────────────────────────────────────────────────────

# A resolve that yields a cluster domain (the happy path).
_fake_resolve_cluster() {
  provision::resolve_spec() {
    echo "resolve" >> "${RECORD}"
    LOK8S_SPEC_KIND="cluster"
    LOK8S_SPEC_FILE="${_FAKE_SPEC}"
  }
  export -f provision::resolve_spec
}

# Provider loading stubbed — the test defines provider::rebuild/doctor directly,
# so "loading" only needs to set the module state recover::_run consumes.
_fake_load_provider() {
  recover::_load_provider() {
    _RECOVER_PROVIDER="mock"
    _RECOVER_CONFIG="${_FAKE_CFG}"
  }
  export -f recover::_load_provider
}

_fake_doctor() {
  provider::doctor() {
    echo "doctor" >> "${RECORD}"
    printf 'ok\thcloud API reachable\n'
    printf 'warn\tRobot creds unset\n'
    printf 'summary\t1 ok, 1 warn\n'
  }
  export -f provider::doctor
}

# provider::rebuild records the config + work_dir it received and whether it saw
# CLOUD_DRY_RUN. Optionally fails when _REBUILD_FAIL=1.
_fake_rebuild() {
  provider::rebuild() {
    echo "rebuild cfg=$1 wd=$2 dry=${CLOUD_DRY_RUN:-}" >> "${RECORD}"
    [[ "${_REBUILD_FAIL:-0}" == 1 ]] && return 1
    return 0
  }
  export -f provider::rebuild
}

_fake_provision() {
  provision::dispatch() {
    echo "provision $1" >> "${RECORD}"
  }
  export -f provision::dispatch
}

# Verify seams — a 3-node cluster, all Ready.
_fake_verify_ok() {
  recover::_inventory_count() { echo 3; }
  recover::_ready_nodes() { echo 3; }
  export -f recover::_inventory_count recover::_ready_nodes
}

# The full happy-path fake set.
_fakes_all() {
  _fake_resolve_cluster
  _fake_load_provider
  _fake_doctor
  _fake_rebuild
  _fake_provision
  _fake_verify_ok
}

# ── phase order + timing summary ─────────────────────────────────────────────

@test "recover runs the phases in order and prints the timing summary" {
  _fakes_all
  force=1   # opt out of the interactive confirm

  run recover::_run "test.dom" 0 0
  assert_success

  # The timing summary lists every phase, in execution order (assert on the
  # command output BEFORE the RECORD read below clobbers $output).
  assert_output --partial "⏱ resolve took"
  assert_output --partial "⏱ verify took"
  [[ "${output}" == *"phases: resolve="*"doctor="*"rebuild="*"provision="*"verify="* ]]

  # Recorded call order of the reused primitives.
  run cat "${RECORD}"
  assert_line --index 0 "resolve"
  assert_line --index 1 "doctor"
  assert_line --index 2 "rebuild cfg=${_FAKE_CFG} wd=${BATS_TEST_TMPDIR}/clusters/test.dom/.provider dry="
  assert_line --index 3 "provision test.dom"
}

@test "recover verify compares Ready nodes to inventory count" {
  _fakes_all
  force=1

  run recover::_run "test.dom" 0 0
  assert_success
  assert_output --partial "--- verify ---"
  assert_output --partial "nodes Ready: 3/3"
  assert_output --partial "all 3 node(s) Ready"
}

# ── --skip-rebuild ───────────────────────────────────────────────────────────

@test "recover --skip-rebuild skips rebuild but still provisions + verifies" {
  _fakes_all
  force=1

  run recover::_run "test.dom" 1 0     # skip_rebuild=1
  assert_success
  assert_output --partial "skipping the node rebuild"
  assert_output --partial "rebuild=skipped"
  assert_output --partial "nodes Ready: 3/3"

  # provider::rebuild must NOT have been called; provision must have.
  run cat "${RECORD}"
  refute_line --partial "rebuild cfg="
  assert_line "provision test.dom"
}

# ── --dry-run ────────────────────────────────────────────────────────────────

@test "recover --dry-run sets CLOUD_DRY_RUN, calls rebuild, skips provision/verify, prints DRY RUN" {
  _fakes_all
  # No --force needed: dry-run never prompts (it changes nothing).

  run recover::_run "test.dom" 0 1     # dry_run=1
  assert_success
  assert_output --partial "DRY RUN — nothing changed"
  assert_output --partial "lo provision WOULD run next"

  # rebuild ran WITH CLOUD_DRY_RUN=1; provision/verify did NOT run.
  run cat "${RECORD}"
  assert_line --partial "rebuild cfg=${_FAKE_CFG}"
  assert_line --partial "dry=1"
  refute_line --partial "provision test.dom"
}

@test "recover --dry-run does not prompt even without --force" {
  _fakes_all
  force=0
  unset LOK8S_NONINTERACTIVE

  # No stdin provided; if it tried to prompt+read it would hang/fail. It must not.
  run recover::_run "test.dom" 0 1
  assert_success
  assert_output --partial "DRY RUN"
}

# ── consent guard ────────────────────────────────────────────────────────────

@test "recover confirm blocks without --force: a 'no' aborts and rebuild is NOT called" {
  _fakes_all
  force=0
  unset LOK8S_NONINTERACTIVE

  run recover::_run "test.dom" 0 0 <<<"no"
  assert_failure
  assert_output --partial "aborted by operator"

  # Nothing destructive happened.
  run cat "${RECORD}"
  refute_line --partial "rebuild cfg="
  refute_line --partial "provision test.dom"
}

@test "recover proceeds when --force is set (no prompt)" {
  _fakes_all
  force=1

  run recover::_run "test.dom" 0 0     # no stdin — must not read
  assert_success
  run cat "${RECORD}"
  assert_line --partial "rebuild cfg="
  assert_line "provision test.dom"
}

@test "recover proceeds non-interactively with LOK8S_NONINTERACTIVE" {
  _fakes_all
  force=0
  export LOK8S_NONINTERACTIVE=1

  run recover::_run "test.dom" 0 0
  assert_success
  run cat "${RECORD}"
  assert_line --partial "rebuild cfg="
}

# ── rebuild failure aborts before provision ──────────────────────────────────

@test "recover aborts before provision when rebuild fails" {
  _fakes_all
  force=1
  export _REBUILD_FAIL=1

  run recover::_run "test.dom" 0 0
  assert_failure
  assert_output --partial "rebuild failed"
  assert_output --partial "NOT provisioning on a half-reset cluster"

  run cat "${RECORD}"
  assert_line --partial "rebuild cfg="
  refute_line --partial "provision test.dom"
}

# ── resolve guards ───────────────────────────────────────────────────────────

@test "recover declines cleanly when the provider has no provider::rebuild" {
  # Cluster domain, provider loads, but no provider::rebuild is defined.
  _fake_resolve_cluster
  recover::_load_provider() { _RECOVER_PROVIDER="norebuild"; }
  export -f recover::_load_provider
  _fake_doctor
  _fake_provision

  run recover::_run "test.dom" 0 0
  assert_failure
  assert_output --partial "does not support recover (no provider::rebuild)"

  # It stopped at resolve — nothing else ran.
  run cat "${RECORD}"
  assert_output "resolve"
}

@test "recover rejects a non-cluster (deploy) domain" {
  provision::resolve_spec() {
    echo "resolve" >> "${RECORD}"
    LOK8S_SPEC_KIND="deploy"
    LOK8S_SPEC_FILE="${_FAKE_SPEC}"
  }
  export -f provision::resolve_spec
  # A load fake that would flag if (wrongly) reached.
  recover::_load_provider() { echo "load" >> "${RECORD}"; }
  export -f recover::_load_provider

  run recover::_run "test.dom" 0 0
  assert_failure
  assert_output --partial "is not a cluster domain"

  run cat "${RECORD}"
  assert_output "resolve"    # never reached the provider load
}

@test "recover aborts when resolve_spec itself fails (bad/missing domain)" {
  provision::resolve_spec() { echo "resolve" >> "${RECORD}"; return 1; }
  export -f provision::resolve_spec

  run recover::_run "nope.dom" 0 0
  assert_failure
  run cat "${RECORD}"
  assert_output "resolve"
}

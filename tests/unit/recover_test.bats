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

# The REAL parse (no _fake_verify_ok): recover::_verify pins KUBECONFIG to the
# recovered cluster's own kubeconfig and runs the real recover::_ready_nodes over
# stubbed `kubectl get nodes`. A cordoned-but-Ready node (Ready,SchedulingDisabled)
# MUST count as Ready; a NotReady node MUST NOT.
@test "recover verify counts a Ready,SchedulingDisabled node as Ready (real parse)" {
  _RECOVER_DOMAIN="test.dom"
  _RECOVER_CLUSTER_NAME="test.dom"
  _RECOVER_SPEC="${_FAKE_SPEC}"   # non-existent → name falls back to cluster name

  # The kubeconfig the driver would have written (PATH_BASE/.kubeconfig/<name>.yaml).
  mkdir -p "${PATH_BASE}/.kubeconfig"
  : > "${PATH_BASE}/.kubeconfig/test.dom.yaml"

  # Inventory (want) = 3; kubectl reports 4 nodes: 3 Ready (incl. a cordoned one)
  # + 1 NotReady.
  recover::_inventory_count() { echo 3; }
  kubectl() {
    [[ "$1" == "get" ]] || return 0
    cat <<'EOF'
cp1  Ready                     control-plane  10d  v1.30.0
cp2  Ready,SchedulingDisabled  control-plane  10d  v1.30.0
w1   Ready                     <none>         10d  v1.30.0
w2   NotReady                  <none>         10d  v1.30.0
EOF
  }
  export -f recover::_inventory_count kubectl

  run recover::_verify
  assert_success
  assert_output --partial "nodes Ready: 3/3"
  assert_output --partial "all 3 node(s) Ready"
  # per-node detail: the cordoned node is shown yet counted Ready; NotReady is not.
  assert_output --partial "cp2 Ready,SchedulingDisabled"
  assert_output --partial "w2 NotReady"
}

# The kubeconfig pin has no false green: with the recovered cluster's kubeconfig
# ABSENT, verify must report 0 Ready (never fall back to the ambient KUBECONFIG).
@test "recover verify reports not-ready when the recovered kubeconfig is missing" {
  _RECOVER_DOMAIN="test.dom"
  _RECOVER_CLUSTER_NAME="test.dom"
  _RECOVER_SPEC="${_FAKE_SPEC}"
  # NOTE: no .kubeconfig/test.dom.yaml created.

  recover::_inventory_count() { echo 3; }
  # A kubectl that WOULD report a healthy cluster if it were ever consulted —
  # it must not be, because the pinned kubeconfig does not exist.
  kubectl() { echo "x Ready a b c"; echo "y Ready a b c"; echo "z Ready a b c"; }
  export -f recover::_inventory_count kubectl

  run recover::_verify
  assert_success
  assert_output --partial "recovered kubeconfig not found"
  assert_output --partial "nodes Ready: 0/3"
  refute_output --partial "all 3 node(s) Ready"
}

# ── inventory count: a failed resolve vs a genuine 0 ─────────────────────────

# A FAILED inventory resolution (provider::output errors — missing creds / API
# error) must return the EMPTY sentinel, NOT a misleading "0".
@test "recover inventory_count returns the empty sentinel when provider::output fails" {
  provider::output() { return 1; }
  export -f provider::output

  run recover::_inventory_count
  assert_success
  assert_output ""
}

# A genuine resolve still yields a real node count.
@test "recover inventory_count returns the real node count on success" {
  provider::output() { printf '{"nodes":[{},{},{}]}'; }
  export -f provider::output

  run recover::_inventory_count
  assert_success
  assert_output "3"
}

# Non-JSON output (jq failure) is also "unresolved" → empty sentinel, not 0.
@test "recover inventory_count returns the empty sentinel on non-JSON provider output" {
  provider::output() { printf 'not json'; }
  export -f provider::output

  run recover::_inventory_count
  assert_success
  assert_output ""
}

# verify must not read the empty sentinel as a real 0: it shows "unknown" and
# never declares success (a bogus 0 must not read as "all node(s) Ready").
@test "recover verify shows 'unknown' (not 0) when the inventory could not be resolved" {
  _RECOVER_DOMAIN="test.dom"
  _RECOVER_CLUSTER_NAME="test.dom"
  _RECOVER_SPEC="${_FAKE_SPEC}"
  mkdir -p "${PATH_BASE}/.kubeconfig"
  : > "${PATH_BASE}/.kubeconfig/test.dom.yaml"

  recover::_inventory_count() { return 0; }   # empty sentinel = unresolved
  recover::_ready_nodes() { echo 3; }
  export -f recover::_inventory_count recover::_ready_nodes

  run recover::_verify
  assert_success
  assert_output --partial "nodes Ready: 3/unknown"
  refute_output --partial "back from bare metal"
}

# ── kubeconfig path: reuse the driver's resolver ─────────────────────────────

# When the driver exposes driver::kubeconfig, recover uses IT (lockstep with the
# driver — no path drift).
@test "recover kubeconfig path prefers driver::kubeconfig when it is loaded" {
  _RECOVER_DOMAIN="test.dom"
  _RECOVER_CLUSTER_NAME="test.dom"
  driver::kubeconfig() { echo "/from/driver/${1}.yaml"; }
  export -f driver::kubeconfig

  run recover::_kubeconfig_path
  assert_success
  assert_output "/from/driver/test.dom.yaml"
}

# With no driver in scope, it falls back to the local resolution — the SAME path
# the verify tests rely on (PATH_BASE/.kubeconfig/<name>.yaml).
@test "recover kubeconfig path falls back to local resolution without a driver" {
  _RECOVER_DOMAIN="test.dom"
  _RECOVER_CLUSTER_NAME="test.dom"
  _RECOVER_SPEC="${_FAKE_SPEC}"   # non-existent → name falls back to cluster name

  run recover::_kubeconfig_path
  assert_success
  assert_output "${PATH_BASE}/.kubeconfig/test.dom.yaml"
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

# ── main::recover flag wiring ────────────────────────────────────────────────

# main::recover is the thin :args wrapper; the argsh :args builtin is absent when
# a lib is sourced in bats, so shim it to parse the flags exactly as argsh does
# (same approach as deploy_test.bats), then record what recover::_run receives —
# proving the flag → positional mapping (domain, skip_rebuild, dry_run).
@test "main::recover maps --skip-rebuild and --dry-run flags to recover::_run positionals" {
  :args() {
    shift  # drop the description
    while (( $# )); do
      case "$1" in
        --skip-rebuild) skip_rebuild=1 ;;
        --dry-run)      dry_run=1 ;;
        -*)             : ;;
        *)              domain="$1" ;;
      esac
      shift
    done
  }
  recover::_run() { echo "run domain=$1 skip=$2 dry=$3" >> "${RECORD}"; }
  export -f :args recover::_run

  run main::recover --skip-rebuild --dry-run test.dom
  assert_success
  run cat "${RECORD}"
  assert_output "run domain=test.dom skip=1 dry=1"
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

# The consent gate must default to ABORT for any non-"yes": an empty answer, a
# closed stdin (EOF), and a whitespace-only answer all decline — touch nothing.
@test "recover confirm: an EMPTY answer aborts and rebuild is NOT called" {
  _fakes_all
  force=0
  unset LOK8S_NONINTERACTIVE

  run recover::_run "test.dom" 0 0 <<<""
  assert_failure
  assert_output --partial "aborted by operator"
  run cat "${RECORD}"
  refute_line --partial "rebuild cfg="
  refute_line --partial "provision test.dom"
}

@test "recover confirm: EOF (closed stdin) aborts and rebuild is NOT called" {
  _fakes_all
  force=0
  unset LOK8S_NONINTERACTIVE

  run recover::_run "test.dom" 0 0 </dev/null
  assert_failure
  assert_output --partial "aborted by operator"
  run cat "${RECORD}"
  refute_line --partial "rebuild cfg="
  refute_line --partial "provision test.dom"
}

@test "recover confirm: a WHITESPACE-only answer aborts and rebuild is NOT called" {
  _fakes_all
  force=0
  unset LOK8S_NONINTERACTIVE

  run recover::_run "test.dom" 0 0 <<<"   "
  assert_failure
  assert_output --partial "aborted by operator"
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

# ── consent wording ──────────────────────────────────────────────────────────

# The consent count is HONEST about a failed inventory resolution: an empty
# sentinel (missing creds / API error) prompts "an unknown number of nodes",
# NOT a misleading "0".
@test "recover confirm says 'unknown' when the inventory could not be resolved" {
  force=0
  unset LOK8S_NONINTERACTIVE
  _RECOVER_CLUSTER_NAME="test.dom"
  recover::_inventory_count() { return 0; }   # empty sentinel = unresolved
  export -f recover::_inventory_count

  run recover::_confirm 0 <<<"no"
  assert_failure   # "no" declines — nothing destructive
  assert_output --partial "unknown number of nodes"
  refute_output --partial "reset 0 node(s)"
}

# Under --skip-rebuild the prompt describes the (still destructive) re-provision,
# NOT a bare-metal reset that will not happen.
@test "recover confirm under --skip-rebuild wording reflects re-provision, not a bare-metal reset" {
  force=0
  unset LOK8S_NONINTERACTIVE
  _RECOVER_CLUSTER_NAME="test.dom"
  recover::_inventory_count() { echo 3; }
  export -f recover::_inventory_count

  run recover::_confirm 1 <<<"no"   # skip_rebuild=1
  assert_failure
  assert_output --partial "re-provision"
  assert_output --partial "3 node(s)"
  refute_output --partial "from bare metal"
}

# Without --skip-rebuild the prompt DOES describe the bare-metal reset.
@test "recover confirm without --skip-rebuild describes a bare-metal reset" {
  force=0
  unset LOK8S_NONINTERACTIVE
  _RECOVER_CLUSTER_NAME="test.dom"
  recover::_inventory_count() { echo 3; }
  export -f recover::_inventory_count

  run recover::_confirm 0 <<<"no"
  assert_failure
  assert_output --partial "reset 3 node(s) of cluster test.dom from bare metal"
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

# An INVALID spec.provider.name is DISTINCT from a MISSING one. _load_provider
# must (1) surface provider::read_name's OWN diagnostic (not swallow it with a
# 2>/dev/null) and (2) not assert a single "no spec.provider" cause.
@test "recover _load_provider surfaces read_name's diagnostic on an invalid provider name" {
  _RECOVER_DOMAIN="test.dom"
  _RECOVER_SPEC="${_FAKE_SPEC}"
  # Reproduce provider::read_name's invalid-name path: diagnostic on stderr,
  # nothing on stdout (no captured name), non-zero exit.
  provider::read_name() {
    error "provider name 'bad name!' is invalid (must be alphanumeric + hyphens/underscores)"
    return 1
  }
  export -f provider::read_name

  run recover::_load_provider
  assert_failure
  # read_name's diagnostic surfaces — a 2>/dev/null regression would hide it.
  assert_output --partial "provider name 'bad name!' is invalid"
  # recover's own message no longer asserts a bare "no spec.provider" cause.
  assert_output --partial "no usable spec.provider"
  refute_output --partial "has no spec.provider"
}

# ── _workdir: no shared-/tmp last resort ─────────────────────────────────────

# When the per-domain dir is unusable AND mktemp fails, _workdir must NOT echo
# the SHARED /tmp (concurrent-run collisions + a global leak) — it errors and
# returns non-zero with EMPTY stdout.
@test "recover _workdir returns non-zero and never echoes /tmp when mktemp fails" {
  mktemp() { return 1; }   # force the last-resort branch to fail
  export -f mktemp

  run recover::_workdir "../bad"   # invalid domain → skips the per-domain mkdir
  assert_failure
  refute_output --partial "/tmp"
  assert_output --partial "could not create a work directory"
}

# recover::_run must abort the moment the work dir can't be created — BEFORE
# resolve/doctor/consent/rebuild — so nothing destructive runs.
@test "recover aborts (nothing destructive) when the work dir cannot be created" {
  _fakes_all
  force=1                  # would otherwise proceed through the whole flow
  mktemp() { return 1; }   # make the mktemp fallback fail
  export -f mktemp

  run recover::_run "../bad" 0 0   # invalid domain → per-domain mkdir skipped → mktemp fails
  assert_failure
  assert_output --partial "could not create a work directory"

  # It aborted at the work-dir step: the RECORD is EMPTY — no phase ran
  # (no resolve/rebuild/provision).
  run cat "${RECORD}"
  assert_output ""
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

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

# The consent prompt reads stdin, and nothing reads it first. On the
# kubehz.in.net recreate (2026-10-23) a piped `yes` never reached the prompt:
# the ssh probes of provider::doctor read stdin to the end. Each fake below
# reads its stdin to the end and records it, like ssh without -n.
@test "recover: a piped yes reaches the prompt; no provider call reads the prompt's input" {
  _fake_resolve_cluster
  _fake_load_provider
  force=0
  unset LOK8S_NONINTERACTIVE
  provider::doctor() { echo "doctor read: $(cat)" >> "${RECORD}"; printf 'ok\tssh reachable\n'; }
  provider::output() { echo "output read: $(cat)" >> "${RECORD}"; printf '{"nodes":[{},{},{}]}'; }
  provider::rebuild() { echo "rebuild read: $(cat)" >> "${RECORD}"; }
  provision::dispatch() { echo "provision read: $(cat)" >> "${RECORD}"; }
  export -f provider::doctor provider::output provider::rebuild provision::dispatch

  run recover::_run "test.dom" 0 0 <<<$'yes\nyes\nyes'
  assert_success
  refute_output --partial "aborted by operator"
  assert_output --partial "reset 3 node(s) of cluster test.dom"

  run cat "${RECORD}"
  assert_line "doctor read: "
  assert_line "rebuild read: "
  assert_line "provision read: "
  refute_line --partial "read: yes"
  # provider::output ran for the prompt's node count and for the verify.
  run grep -c '^output read: $' "${RECORD}"
  assert_output "2"
}

# The provider load runs before the consent too: a plugin that reads stdin
# while it loads must not get the prompt's input. The real
# recover::_load_provider runs here; the test above replaces it.
@test "recover: the provider load does not read the prompt's input" {
  _RECOVER_SPEC="${_FAKE_SPEC}" _RECOVER_DOMAIN="test.dom"
  provider::read_name() { echo "mock"; }
  provider::write_config() { PROVIDER_CONFIG_FILE="${_FAKE_CFG}"; }
  provider::load() { echo "load ${1} read: $(cat)" >> "${RECORD}"; }

  recover::_load_provider <<<"yes"
  [ "${_RECOVER_PROVIDER}" = "mock" ]
  [ "${_RECOVER_CONFIG}" = "${_FAKE_CFG}" ]
  run cat "${RECORD}"
  assert_output "load mock read: "
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

# ── recover::_pick_domain — the positional/flag precedence + arity guard ────

@test "recover _pick_domain: positional outranks the flag/active fallback" {
  run recover::_pick_domain "active.dom" "explicit.dom"
  [ "$status" -eq 0 ]
  [ "$output" = "explicit.dom" ]
}

@test "recover _pick_domain: falls back to --domain/active without a positional" {
  # Under `set -u` an empty array expands to ONE empty arg — must fall through.
  run recover::_pick_domain "active.dom" ""
  [ "$status" -eq 0 ]
  [ "$output" = "active.dom" ]
}

@test "recover _pick_domain: rejects a second positional loudly (greedy-array guard)" {
  run recover::_pick_domain "active.dom" "explicit.dom" "stray-token"
  [ "$status" -ne 0 ]
  [[ "$output" == *"too many arguments"*"stray-token"* ]]
}

# ── the cluster_name guard ───────────────────────────────────────────────────
# The hetzner provider::rebuild resets the servers with
# lok8s.dev/cluster=<cluster_name> and the owner mark of the domain. A
# cluster_name that another domain under clusters/ also has in its provider
# config is refused at resolve (recover::_cluster_name_guard, Go twin:
# Runner.guardClusterName), before the doctor, the dry run and the consent.
# As in the destroy guard, nothing skips it. The real
# provision::_cluster_name_users of the provision lib scans clusters/.

# _shared_name_setup: the provision lib (for its scan), the fakes, and a
# provider::rebuild that records a line that the refusal tests refute. The
# provider config of test.dom and the hetzner.json of copy.dom have the
# cluster_name "shared".
_shared_name_setup() {
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  _fakes_all
  provision::load_provider_creds() { :; }
  provider::rebuild() { echo "REBUILD-RAN" >> "${RECORD}"; return 1; }
  printf '{"cluster_name": "shared"}\n' > "${_FAKE_CFG}"
  mkdir -p "${PATH_CLUSTERS}/copy.dom"
  printf '{"cluster_name": "shared"}\n' > "${PATH_CLUSTERS}/copy.dom/hetzner.json"
}

# _assert_refused: the refusal, and nothing ran after the resolve (no doctor,
# no rebuild, no provision).
_assert_refused() {
  assert_failure 1
  assert_output --partial "[error] recover: cluster_name 'shared' of 'test.dom' is also the cluster_name of: copy.dom. The rebuild resets only the servers with this cluster_name and the owner mark of 'test.dom'."
  assert_output --partial "Nothing was changed. Give each domain its own cluster_name, then run lo recover again."
  refute_output --partial "continue? [type yes"
  refute_output --partial "DRY RUN"
  run cat "${RECORD}"
  assert_output "resolve"
}

@test "recover refuses a cluster_name that another domain has: a yes at the prompt" {
  _shared_name_setup
  run recover::_run "test.dom" 0 0 <<<"yes"
  _assert_refused
}

@test "recover refuses a cluster_name that another domain has: --force" {
  _shared_name_setup
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  _assert_refused
}

@test "recover refuses a cluster_name that another domain has: LOK8S_NONINTERACTIVE" {
  _shared_name_setup
  export LOK8S_NONINTERACTIVE=1
  run recover::_run "test.dom" 0 0 </dev/null
  _assert_refused
}

@test "recover refuses a cluster_name that another domain has: --dry-run" {
  _shared_name_setup
  run recover::_run "test.dom" 0 1 </dev/null
  _assert_refused
}

@test "recover refuses a cluster_name that another domain has: --force --skip-rebuild" {
  _shared_name_setup
  force=1
  run recover::_run "test.dom" 1 0 </dev/null
  _assert_refused
}

@test "recover refuses a cluster_name of an inline config and a configRef too, and names each domain, sorted" {
  _shared_name_setup
  mkdir -p "${PATH_CLUSTERS}/b.dom" "${PATH_CLUSTERS}/ref.dom/infra"
  printf 'kind: KubeOne\nmetadata:\n  name: b\nspec:\n  provider:\n    name: mock\n    config:\n      cluster_name: shared\n' \
    > "${PATH_CLUSTERS}/b.dom/cluster.lok8s.yaml"
  printf 'kind: KubeOne\nmetadata:\n  name: ref\nspec:\n  provider:\n    name: mock\n    configRef: infra/desc.yaml\n' \
    > "${PATH_CLUSTERS}/ref.dom/cluster.lok8s.yaml"
  printf 'cluster_name: shared\n' > "${PATH_CLUSTERS}/ref.dom/infra/desc.yaml"
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  assert_failure 1
  assert_output --partial "is also the cluster_name of: b.dom, copy.dom, ref.dom. "
  run cat "${RECORD}"
  assert_output "resolve"
}

@test "recover runs when only this domain has the cluster_name (its own files, a dot dir, another name)" {
  _shared_name_setup
  _fake_rebuild
  printf '{"cluster_name": "other"}\n' > "${PATH_CLUSTERS}/copy.dom/hetzner.json"
  mkdir -p "${PATH_CLUSTERS}/test.dom" "${PATH_CLUSTERS}/.old"
  printf '{"cluster_name": "shared"}\n' > "${PATH_CLUSTERS}/test.dom/hetzner.json"
  printf '{"cluster_name": "shared"}\n' > "${PATH_CLUSTERS}/.old/hetzner.json"
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  assert_success
  refute_output --partial "is also the cluster_name of"
  run cat "${RECORD}"
  assert_output --partial "rebuild cfg=${_FAKE_CFG}"
  assert_output --partial "provision test.dom"
}

@test "recover checks the cluster_name of the provider config only, not the name of the prompt" {
  _shared_name_setup
  _fake_rebuild
  printf '{}\n' > "${_FAKE_CFG}"
  printf '{"cluster_name": "test.dom"}\n' > "${PATH_CLUSTERS}/copy.dom/hetzner.json"
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  assert_success
  refute_output --partial "is also the cluster_name of"
  run cat "${RECORD}"
  assert_output --partial "provision test.dom"
}

# The real scan (provision::_cluster_name_users) fails only when its
# printf | sort pipeline fails: a yq error reads as no name. So this test
# stubs the scan to pin the fail-closed branch of the guard.
@test "recover stops when the cluster_name scan fails (a stub: the real scan fails only when its printf | sort fails)" {
  _shared_name_setup
  provision::_cluster_name_users() { return 1; }
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  assert_failure 1
  assert_output --partial "[error] recover: could not check if another domain has the cluster_name 'shared' of 'test.dom'. Nothing was changed. Run lo recover again."
  run cat "${RECORD}"
  assert_output "resolve"
}

@test "recover stops when jq cannot check the cluster_name (a stub jq that fails)" {
  _shared_name_setup
  jq() { return 1; }
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  assert_failure 1
  assert_output --partial "[error] recover: could not check the cluster_name of the provider config of 'test.dom'. Nothing was changed. Check that jq runs, then run lo recover again."
  refute_output --partial "is also the cluster_name of"
  run cat "${RECORD}"
  assert_output "resolve"
}

# ── the cluster_name guard: the shapes of the provider config ───────────────
# The same table as configShapes in internal/recover/recover_test.go. Each row
# runs the guard on a provider config of test.dom with that text. A row
# "none:<text>" or "shared:<text>" gives copy.dom the cluster_name <text>:
# for "none", the text that a wrong read gives (yq without `// ""` prints
# "null" and "false"). Other rows give copy.dom a name that no row reads.
# The config file is hetzner.json, so a YAML row also checks that the guard
# parses the config by its content, not by its extension.

_msg_config="[error] recover: the provider config of 'test.dom' is not one YAML or JSON document whose top is a mapping with a plain cluster_name (not an alias, a mapping or a list), so recover cannot compare its cluster_name with the other domains. Nothing was changed. Correct the provider config, then run lo recover again."
_msg_merge="[error] recover: the provider config of 'test.dom' has a merge key (<<) at the top. The rebuild can take the cluster_name from the merged mapping, so recover cannot compare it with the cluster_name of the other domains. Nothing was changed. Write cluster_name as a literal key, without a merge key, then run lo recover again."
_msg_quote="[error] recover: the cluster_name of the provider config of 'test.dom' is not a text: YAML reads it as a number, as true or as another type. The rebuild writes it as JSON, which can change how it is written, so recover cannot compare it with the cluster_name of the other domains. Nothing was changed. Write the cluster_name in quotes, then run lo recover again."
_msg_unshown="[error] recover: the provider config of 'test.dom' names a cluster_name with a character that a message cannot show as it is (a control, separator, bidi or invisible character, a backslash, U+FFFD, or a byte that is not UTF-8). Nothing was changed. Correct the cluster_name in the provider config, then run lo recover again."
_msg_expand="[error] recover: could not expand the variables of the provider config of 'test.dom' as the rebuild does (envsubst failed). Nothing was changed. Check the provider config and that envsubst runs, then run lo recover again."
_msg_changed="[error] recover: the provider config of 'test.dom' gives another cluster_name when the rebuild expands its variables (a variable adds or changes it), so recover cannot compare the cluster_name that the rebuild uses with the other domains. Nothing was changed. Write the provider config so that no variable sets or changes the cluster_name, then run lo recover again."

# _guard_row <row name> <want> <config text>: run the guard on the config and
# check the result. <want>: none:<text> (no refusal), shared:<n> (the refusal
# names <n>), or config, merge, quote, unshown or changed (that refusal and
# nothing else).
_guard_row() {
  local row="${1}" want="${2}" other="a-name-no-row-reads"
  echo "row: ${row}"
  [[ "${want}" != *:* ]] || other="${want#*:}"
  printf '%s' "${3}" > "${_FAKE_CFG}"
  jq -n --arg n "${other}" '{cluster_name: $n}' > "${PATH_CLUSTERS}/copy.dom/hetzner.json"
  _RECOVER_DOMAIN="test.dom"
  _RECOVER_CONFIG="${_FAKE_CFG}"
  run recover::_cluster_name_guard
  case "${want}" in
    none:*)
      assert_success
      assert_output "" ;;
    shared:*)
      assert_failure 1
      assert_output --partial "[error] recover: cluster_name '${other}' of 'test.dom' is also the cluster_name of: copy.dom. " ;;
    config | merge | quote | unshown | changed)
      local msg="_msg_${want}"
      assert_failure 1
      assert_output "${!msg}" ;;
    *)
      echo "unknown want: ${want}"
      return 1 ;;
  esac
}

@test "recover guard: a provider config without a cluster_name is not checked" {
  _shared_name_setup
  _guard_row "empty file" none:null ""
  _guard_row "only a comment" none:null $'# a comment\n'
  _guard_row "null document" none:null $'null\n'
  _guard_row "empty mapping" none:null $'{}\n'
  _guard_row "a mapping without the key" none:null $'x: 1\n'
  _guard_row "null" none:null $'cluster_name: null\n'
  _guard_row "false" none:false $'cluster_name: false\n'
  _guard_row "False" none:False $'cluster_name: False\n'
}

@test "recover guard: reads a cluster_name that is a text, as yq prints it" {
  _shared_name_setup
  _guard_row "json" shared:a '{"cluster_name": "a"}'
  _guard_row "json with tabs" shared:a $'{\n\t"cluster_name": "a"\n}\n'
  _guard_row "yaml with a leading document marker" shared:a $'---\ncluster_name: a\n'
  _guard_row "a quoted number is a text" shared:0123 $'cluster_name: "0123"\n'
  _guard_row "a quoted false is a text" shared:false $'cluster_name: "false"\n'
  _guard_row "yes is a text" shared:yes $'cluster_name: yes\n'
  _guard_row "two keys: the last one" shared:b $'cluster_name: a\ncluster_name: b\n'
}

@test "recover guard: refuses a provider config that does not parse or is not one mapping" {
  _shared_name_setup
  _guard_row "does not parse" config '{"cluster_name": "a", "token": ${TOKEN}}'
  _guard_row "top is a list" config $'- a\n- b\n'
  _guard_row "top is a text" config $'hello\n'
  _guard_row "top has a custom tag" config $'!foo {cluster_name: a}\n'
  _guard_row "two documents" config $'cluster_name: a\n---\ncluster_name: b\n'
  _guard_row "an empty second document" config $'cluster_name: a\n---\n'
}

@test "recover guard: refuses a cluster_name that is an alias, a mapping or a list" {
  _shared_name_setup
  _guard_row "name is a mapping" config $'cluster_name:\n  x: 1\n'
  _guard_row "name is a list" config $'cluster_name: [a]\n'
  _guard_row "name is an alias" config $'n: &x a\ncluster_name: *x\n'
}

@test "recover guard: refuses a merge key at the top" {
  _shared_name_setup
  _guard_row "a merge key gives the name" merge $'base: &b {cluster_name: merged}\n<<: *b\n'
  _guard_row "a merge key after the name" merge $'base: &b {cluster_name: merged}\ncluster_name: a\n<<: *b\n'
  _guard_row "a merge key with a list" merge $'base: &b {x: 1}\n<<: [*b]\ncluster_name: a\n'
  _guard_row "a quoted << key" merge $'"<<": x\ncluster_name: a\n'
}

@test "recover guard: refuses a cluster_name that is not a text, and says to quote it" {
  _shared_name_setup
  _guard_row "int" quote $'cluster_name: 0123\n'
  _guard_row "hex int" quote $'cluster_name: 0x1F\n'
  _guard_row "float" quote $'cluster_name: 1.5\n'
  _guard_row "true" quote $'cluster_name: true\n'
  _guard_row "True" quote $'cluster_name: True\n'
  _guard_row "name has a custom tag" quote $'cluster_name: !foo a\n'
  _guard_row "name is a timestamp" quote $'cluster_name: 2001-12-14\n'
}

@test "recover guard: keeps the newline at the end of a cluster_name and a NUL, and refuses them" {
  _shared_name_setup
  _guard_row "name ends with a newline" unshown $'cluster_name: "a\\n"\n'
  _guard_row "name has a NUL" unshown $'cluster_name: "a\\0b"\n'
}

# ── the cluster_name guard: the expansion of the rebuild ────────────────────
# The guard reads the config again after the real envsubst on PATH (the
# expansion of template::descriptor_json). GUARD_NOTE is the variable of
# each row.

# _expand_row <row name> <GUARD_NOTE> <want> <config text>: _guard_row with
# GUARD_NOTE exported.
_expand_row() {
  export GUARD_NOTE="${2}"
  _guard_row "${1}" "${3}" "${4}"
  unset GUARD_NOTE
}

@test "recover guard: refuses a variable that adds or changes the cluster_name (the real envsubst)" {
  _shared_name_setup
  _expand_row "a variable adds a cluster_name" 'a", "cluster_name": "shared' changed '{"x": 1, "note": "${GUARD_NOTE}"}'
  _expand_row "a variable adds a cluster_name in yaml" $'a\ncluster_name: shared' changed $'x: 1\nnote: ${GUARD_NOTE}\n'
  _expand_row "a variable changes the cluster_name" 'b", "cluster_name": "shared' changed '{"cluster_name": "a", "note": "${GUARD_NOTE}"}'
  _expand_row "a variable takes the cluster_name away" 'x", "cluster_name": null, "y": "' changed '{"cluster_name": "a", "note": "${GUARD_NOTE}"}'
  _expand_row "a variable adds a merge key" $'x\n<<: {cluster_name: shared}' changed $'cluster_name: a\nnote: ${GUARD_NOTE}\n'
  _expand_row "a variable breaks the config" $'[\n' changed $'cluster_name: a\nnote: ${GUARD_NOTE}\n'
}

@test "recover guard: an expansion that keeps the cluster_name passes (the real envsubst)" {
  _shared_name_setup
  _expand_row "an expansion that keeps the cluster_name" xyz shared:a '{"cluster_name": "a", "token": "${GUARD_NOTE}"}'
  _expand_row "an unset variable" "" shared:a '{"cluster_name": "a", "token": "${GUARD_UNSET_NOTE}"}'
  _expand_row "an expansion without a cluster_name" xyz none:null '{"token": "${GUARD_NOTE}"}'
}

@test "recover stops when envsubst fails (a stub envsubst that fails)" {
  _shared_name_setup
  envsubst() { return 1; }
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  assert_failure 1
  assert_output --partial "${_msg_expand}"
  refute_output --partial "is also the cluster_name of"
  run cat "${RECORD}"
  assert_output "resolve"
}

# ── the cluster_name guard: names it cannot show or compare ─────────────────
# Each row runs recover with --force, and copy.dom has the same text: the
# guard must refuse before it compares, and before a message shows the name.

# _refuses_name <row name> <config text> <message>: recover refuses with
# <message>, does not compare, does not show an escape of the name, and stops
# after the resolve.
_refuses_name() {
  echo "row: ${1}"
  printf '%s\n' "${2}" > "${_FAKE_CFG}"
  printf '%s\n' "${2}" > "${PATH_CLUSTERS}/copy.dom/hetzner.json"
  : > "${RECORD}"
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  assert_failure 1
  assert_output --partial "${3}"
  refute_output --partial "is also the cluster_name of"
  refute_output --partial $'\e[2J'
  run cat "${RECORD}"
  assert_output "resolve"
}

@test "recover refuses a cluster_name that a message cannot show (as lo destroy does)" {
  _shared_name_setup
  local v
  for v in $'a\e[2Jb' 'a\b' $'a\xe2\x80\xaeb' $'a\xe2\x80\x8bb' $'a\xef\xbf\xbdb'; do
    _refuses_name "$(printf '%q' "${v}")" "$(jq -nc --arg n "${v}" '{cluster_name: $n}')" "${_msg_unshown}"
  done
  # A NUL: bash drops it from a string, so the guard reads the name as JSON.
  _refuses_name "NUL" '{"cluster_name": "a\u0000b"}' "${_msg_unshown}"
  # yaml (both twins) does not parse a file that is not UTF-8.
  _refuses_name "not UTF-8" $'cluster_name: a\xffb' "${_msg_config}"
}

@test "recover refuses a cluster_name with a variable: the rebuild expands it" {
  _shared_name_setup
  local v
  for v in '${NAME}-a' 'prod-$ENV'; do
    _refuses_name "${v}" "$(jq -nc --arg n "${v}" '{cluster_name: $n}')" \
      "[error] recover: the cluster_name '${v}' of 'test.dom' has a '\$'. The rebuild expands the variables in the provider config, so the cluster_name that it uses can differ from this text, and recover cannot compare it with the cluster_name of the other domains. Nothing was changed. Write the cluster_name without a variable, then run lo recover again."
  done
}

# ── the cluster_name guard: this domain's real provider config ──────────────
# The real recover::_load_provider reads the provider config of the spec of
# clusters/test.dom (provider::write_config: a configRef, or an inline config
# that it writes to a temp file). Only provider::load is a stub (it would
# source the provider). The fake config holds another name, so a fake load
# does not give the refusal.

_real_config_setup() {
  _shared_name_setup
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/provider.sh"
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/recover"
  provider::load() { :; }
  printf '{"cluster_name": "the-fake-config"}\n' > "${_FAKE_CFG}"
  mkdir -p "${PATH_CLUSTERS}/test.dom"
  _FAKE_SPEC="${PATH_CLUSTERS}/test.dom/cluster.lok8s.yaml"
  unset PROVIDER_CONFIG_FILE PROVIDER_DOMAIN
}

@test "recover refuses a cluster_name of this domain's real configRef" {
  _real_config_setup
  mkdir -p "${PATH_CLUSTERS}/test.dom/infra"
  printf 'kind: KubeOne\nmetadata:\n  name: test\nspec:\n  provider:\n    name: mock\n    configRef: infra/desc.yaml\n' > "${_FAKE_SPEC}"
  printf 'cluster_name: shared\n' > "${PATH_CLUSTERS}/test.dom/infra/desc.yaml"
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  _assert_refused
}

@test "recover refuses a cluster_name of this domain's real inline config" {
  _real_config_setup
  printf 'kind: KubeOne\nmetadata:\n  name: test\nspec:\n  provider:\n    name: mock\n    config:\n      cluster_name: shared\n' > "${_FAKE_SPEC}"
  force=1
  run recover::_run "test.dom" 0 0 </dev/null
  _assert_refused
}

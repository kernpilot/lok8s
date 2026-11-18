#!/usr/bin/env bats
# destroy_guard_test.bats — the cluster_name guard of `lo destroy`
# (provision::_destroy_guard, Go twin: internal/provision/destroy_guard.go).
#
# A provider with the provider::destroy_plan hook finds its resources by the
# cluster_name: the hetzner provider deletes the resources in the token's
# project that carry lok8s.dev/cluster=<cluster_name> and the owner mark of
# the domain. A domain copied from another one that kept the cluster_name
# and the token labels its resources the same way; only the mark keeps the
# two apart.
# The guard refuses a cluster_name that another domain uses, prints the
# plan, and needs the cluster_name: typed on a terminal, or --confirm-cluster
# (`confirm_cluster`, dynamic scoping from main::destroy). --force does not
# skip it.
#
# The real provision lib and utils/provider.sh run: provider::read_name,
# write_config and load source a mock provider from the project's .lok8s
# tree. The mock answers the plan in the provider::destroy_plan format.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/provider.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  export LOK8S_REMOTE=1
  CALLS="${BATS_TEST_TMPDIR}/calls.log"
  export CALLS
  : > "${CALLS}"

  # The mock provider: the contract, plus the destroy_plan hook (PLAN).
  mkdir -p "${PATH_LOK8S}/providers/mock" "${PATH_LOK8S}/drivers/kubeone"
  cat > "${PATH_LOK8S}/providers/mock/main" <<'SH'
provider::validate() { return 0; }
provider::credential_data() { return 0; }
provider::provision() { return 0; }
provider::destroy() { echo "PROVIDER-DESTROY-RAN" >> "${CALLS}"; }
provider::output() { printf '{}\n'; }
provider::destroy_plan() {
  echo "plan:${1##*/}:${2}" >> "${CALLS}"
  [[ -z "${PLAN_FAIL:-}" ]] || { error "cannot determine cluster name for destroy"; return 1; }
  printf '%b' "${PLAN-cluster\tprod-a\nload-balancer\t11\tapi-lb\nserver\t42\tcp-0\n}"
}
SH
  # The driver records the confirmed plan its provider::destroy would read.
  cat > "${PATH_LOK8S}/drivers/kubeone/main" <<'SH'
driver::destroy() {
  echo "DRIVER-DESTROY-RAN" >> "${CALLS}"
  echo "plan-env:${LOK8S_DESTROY_PLAN//$'\t'/ }" >> "${CALLS}"
}
SH
  mkdir -p "${PATH_LOK8S}/drivers/capi"
  cp "${PATH_LOK8S}/drivers/kubeone/main" "${PATH_LOK8S}/drivers/capi/main"

  _domain test.prod prod-a
  kubehz::read_config() { :; }
  kubehz::deregister_cluster() { echo "DEREGISTERED" >> "${CALLS}"; }
  LOK8S_KUBEHZ_ACCESS="agent"
}

teardown() { teardown_tmpdir; }

# _domain <domain> <cluster_name>: a KubeOne domain on the mock provider,
# its cluster_name in hetzner.json (configRef), as the hetzner skill writes.
_domain() {
  mkdir -p "${PATH_CLUSTERS}/${1}"
  cat > "${PATH_CLUSTERS}/${1}/cluster.lok8s.yaml" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: ${1%%.*}
spec:
  kubernetes:
    version: "v1.35.5"
  provider:
    name: mock
    configRef: hetzner.json
YAML
  printf '{"cluster_name": "%s"}\n' "${2}" > "${PATH_CLUSTERS}/${1}/hetzner.json"
}

_assume_tty()    { provision::_interactive() { return 0; }; }
_assume_no_tty() { provision::_interactive() { return 1; }; }

_nothing_ran() {
  run cat "${CALLS}"
  refute_output --partial "DEREGISTERED"
  refute_output --partial "DRIVER-DESTROY-RAN"
}

@test "destroy guard: a cluster_name another domain uses is refused, even with --force and --confirm-cluster" {
  _assume_no_tty
  _domain copy.prod prod-a
  local force=1 confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial "cluster_name 'prod-a' of 'test.prod' is also the cluster_name of: copy.prod. The provider deletes only the resources with this cluster_name and the owner mark of 'test.prod'. The resources of those domains have the same cluster label, so only the mark keeps them apart: a resource of another domain with a wrong mark would be deleted."
  refute_output --partial "deletes every resource"
  assert_output --partial "Nothing was deleted. Give each domain its own cluster_name, then run lo destroy again."
  _nothing_ran
}

# The list of the other domains is the only input of this message that can
# still hold a backslash (the name check refuses one in the cluster_name): a
# directory under clusters/ with the text "\e[2J" in its name is shown as
# text, with no ESC byte.
@test "destroy guard: another domain whose directory name has a backslash is shown as text" {
  _assume_no_tty
  _domain 'copy\e[2J.prod' prod-a
  local confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial "[error] cluster_name 'prod-a' of 'test.prod' is also the cluster_name of: copy\e[2J.prod. The provider deletes only"
  _refute_esc
  _nothing_ran
}

@test "destroy guard: an inline config and a configRef of another name count too" {
  _assume_no_tty
  mkdir -p "${PATH_CLUSTERS}/inline.prod" "${PATH_CLUSTERS}/ref.prod/infra"
  printf 'kind: KubeOne\nmetadata:\n  name: inline\nspec:\n  provider:\n    name: mock\n    config:\n      cluster_name: prod-a\n' \
    > "${PATH_CLUSTERS}/inline.prod/cluster.lok8s.yaml"
  printf 'kind: KubeOne\nmetadata:\n  name: ref\nspec:\n  provider:\n    name: mock\n    configRef: infra/desc.yaml\n' \
    > "${PATH_CLUSTERS}/ref.prod/cluster.lok8s.yaml"
  printf 'cluster_name: prod-a\n' > "${PATH_CLUSTERS}/ref.prod/infra/desc.yaml"
  local confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial "is also the cluster_name of: inline.prod, ref.prod."
  _nothing_ran
}

@test "destroy guard: the plan is listed, and off a terminal --force alone is refused" {
  _assume_no_tty
  local force=1
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial $'destroy deletes these resources of cluster_name \033[1mprod-a\033[0m:\n    load-balancer api-lb (11)\n    server cp-0 (42)\n'
  assert_output --partial "there is no terminal to ask for the cluster_name of 'test.prod'. Nothing was deleted. To confirm, run: lo destroy --domain test.prod -r --confirm-cluster prod-a"
  refute_output --partial "type yes to continue"
  _nothing_ran
}

# A name in the plan comes from the provider's API. The plan shows it
# through verbose::shown_names: each control character (C0, DEL, C1), each
# backslash and each bidi or zero-width character is '?', and a multibyte
# name stays. The confirmed plan that the driver gets stays as the provider
# gave it. The Go twin is TestDestroyGuardShowsEachNameOfThePlanSafely.
@test "destroy guard: the plan shows each name safely, and the confirmed plan stays as the provider gave it" {
  _assume_no_tty
  local confirm_cluster=prod-a
  PLAN='cluster\tprod-a\nserver\t70\tö-測試\nserver\t71\ta\x7fb\xc2\x85c\nserver\t72\t\\\nserver\t73\tx\xe2\x80\xaey\xe2\x80\x8bz\nserver\t74\te\x1b[31mvil\n'
  export PLAN
  run provision::dispatch_destroy test.prod < /dev/null
  assert_success
  assert_output --partial $'destroy deletes these resources of cluster_name \033[1mprod-a\033[0m:\n    server ö-測試 (70)\n    server a?b?c (71)\n    server ? (72)\n    server x?y?z (73)\n    server e?[31mvil (74)'
  refute_output --partial $'\x1b[31m'
  refute_output --partial $'\xe2\x80\xae'
  run cat "${CALLS}"
  assert_output --partial $'plan-env:cluster prod-a\nserver 70 ö-測試\nserver 71 a\x7fb\xc2\x85c\nserver 72 \\\nserver 73 x\xe2\x80\xaey\xe2\x80\x8bz\nserver 74 e\x1b[31mvil'
}

# The kind and the id of a plan line come from the provider too (a
# third-party provider can give any text), so the plan shows them through
# verbose::shown_names as well. A last row without a name shows an empty
# name. The confirmed plan stays as the provider gave it. The Go twin is
# TestDestroyGuardShowsEachKindAndIDSafely.
@test "destroy guard: the plan shows each kind and id safely too" {
  _assume_no_tty
  local confirm_cluster=prod-a
  PLAN='cluster\tprod-a\nser\x1b[2Kver\t7\xe2\x80\xae6\tcp-0\nnetwork\t9\n'
  export PLAN
  run provision::dispatch_destroy test.prod < /dev/null
  assert_success
  assert_output --partial $'cluster_name \033[1mprod-a\033[0m:\n    ser?[2Kver cp-0 (7?6)\n    network  (9)'
  refute_output --partial $'\x1b[2K'
  refute_output --partial $'\xe2\x80\xae'
  run cat "${CALLS}"
  assert_output --partial $'plan-env:cluster prod-a\nser\x1b[2Kver 7\xe2\x80\xae6 cp-0\nnetwork 9'
}

# bash reads a plan line with `IFS=$'\t' read -r kind id name`: the name
# keeps the tabs in it, so two tabs in a row show as two '?'. The Go twin
# reads the line the same way (TestDestroyGuardShowsANameWithTwoTabsAsBashReadsIt).
@test "destroy guard: a name with two tabs in a row shows two '?'" {
  _assume_no_tty
  local confirm_cluster=prod-a
  PLAN='cluster\tprod-a\n\tserver\t\t75\t\ta\t\tb\t\n'
  export PLAN
  run provision::dispatch_destroy test.prod < /dev/null
  assert_success
  assert_output --partial $'cluster_name \033[1mprod-a\033[0m:\n    server a??b (75)'
}

# A jq that fails stops the plan display: nothing runs.
@test "destroy guard: a plan whose names cannot be shown stops the destroy" {
  _assume_no_tty
  local confirm_cluster=prod-a
  verbose::shown_names() { return 5; }
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "could not show the names of the destroy plan of 'test.prod'. Nothing was deleted. Check that jq runs, then run lo destroy again."
  _nothing_ran
}

@test "destroy guard: a --confirm-cluster that does not match is refused" {
  _assume_no_tty
  local confirm_cluster=prod-b
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial "--confirm-cluster 'prod-b' does not match the cluster_name 'prod-a' of 'test.prod'. Nothing was deleted."
  _nothing_ran
}

@test "destroy guard: the matching --confirm-cluster runs the destroy, and the yes-prompt is not asked" {
  _assume_no_tty
  local confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  assert_success
  refute_output --partial "type yes to continue"
  refute_output --partial "[error]"
  run cat "${CALLS}"
  assert_output "plan:hetzner.json:${PATH_CLUSTERS}/test.prod/.kubeone
DEREGISTERED
DRIVER-DESTROY-RAN
plan-env:cluster prod-a
load-balancer 11 api-lb
server 42 cp-0"
}

@test "destroy guard: on a terminal the typed cluster_name runs the destroy, yes does not" {
  _assume_tty
  run provision::dispatch_destroy test.prod <<< "prod-a"
  assert_success
  assert_output --partial "type the cluster_name (prod-a) to delete these resources: "
  run grep -c DRIVER-DESTROY-RAN "${CALLS}"
  assert_output "1"

  : > "${CALLS}"
  run provision::dispatch_destroy test.prod <<< "yes"
  [ "${status}" -eq 3 ]
  assert_output --partial "the answer is not the cluster_name 'prod-a'. Nothing was deleted."
  _nothing_ran

  # A last line without a newline, with spaces and a CR, counts (Go twin:
  # Dispatcher.readAnswer).
  : > "${CALLS}"
  run provision::dispatch_destroy test.prod < <(printf ' prod-a\r')
  assert_success
  run grep -c DRIVER-DESTROY-RAN "${CALLS}"
  assert_output "1"
}

@test "destroy guard: a failed plan stops the destroy" {
  _assume_no_tty
  export PLAN_FAIL=1
  local confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "cannot determine cluster name for destroy"
  _nothing_ran
}

# A hook that fails leaves no plan file. A mktemp that fails stops the
# destroy before the hook runs, and says what failed.
@test "destroy guard: a failed plan leaves no plan file, and a failed mktemp stops the destroy" {
  _assume_no_tty
  local confirm_cluster=prod-a
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  export PLAN_FAIL=1
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "cannot determine cluster name for destroy"
  _nothing_ran
  run find "${TMPDIR}" -mindepth 1
  assert_output ""

  unset PLAN_FAIL
  export TMPDIR="${BATS_TEST_TMPDIR}/missing"
  : > "${CALLS}"
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "[error] could not make a temporary file for the destroy plan of 'test.prod' (mktemp failed in TMPDIR '${TMPDIR}'). Nothing was deleted. Set TMPDIR to a directory that you can write to, then run lo destroy again."
  run grep -c '^plan:' "${CALLS}"
  assert_output "0"
  _nothing_ran
}

@test "destroy guard: a plan without a cluster line is refused" {
  _assume_no_tty
  export PLAN=$'server\t42\tcp-0\n'
  local confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial "the provider gave no cluster_name for 'test.prod'. Nothing was deleted."
  _nothing_ran
}

# _assert_names_refused <field> <name> [<field> <name> ...]: for each pair, a
# plan whose cluster line has <field> (as printf %b reads it) names <name>,
# and the guard refuses it (rc 3) before any message shows the name: with
# the matching --confirm-cluster, off a terminal, and on a terminal with the
# name typed. Nothing runs.
_assert_names_refused() {
  local want="the destroy plan of 'test.prod' names a cluster_name with a character that a message cannot show as it is (a control, separator, bidi or invisible character, a backslash, U+FFFD, or a byte that is not UTF-8). Nothing was deleted. Correct the cluster_name in the provider config or the destroy_plan hook of the provider, then run lo destroy again."
  local name confirm_cluster
  while (( $# )); do
    PLAN="cluster\t${1}\nserver\t42\tcp-0\n"
    export PLAN
    name="${2}"
    shift 2

    _assume_no_tty
    confirm_cluster="${name}"
    : > "${CALLS}"
    run provision::dispatch_destroy test.prod < /dev/null
    [ "${status}" -eq 3 ]
    assert_output --partial "${want}"
    refute_output --partial "${name}"
    refute_output --partial "destroy deletes these resources"
    _nothing_ran

    confirm_cluster=""
    : > "${CALLS}"
    run provision::dispatch_destroy test.prod < /dev/null
    [ "${status}" -eq 3 ]
    assert_output --partial "${want}"
    refute_output --partial "${name}"
    refute_output --partial "there is no terminal"
    _nothing_ran

    _assume_tty
    : > "${CALLS}"
    run provision::dispatch_destroy test.prod <<< "${name}"
    [ "${status}" -eq 3 ]
    assert_output --partial "${want}"
    refute_output --partial "${name}"
    refute_output --partial "type the cluster_name"
    _nothing_ran
  done
}

# The cluster_name is shown in the guard's messages and typed as the
# answer, so a shown form must be the name itself. A name with a code point
# of the jq def unshown is refused before any message shows it. The Go twin
# is TestDestroyGuardRefusesAClusterNameThatCannotBeShown.
@test "destroy guard: a cluster_name with a character that cannot be shown is refused before it is shown" {
  # <the cluster field of the plan line, as printf %b reads it> <the name>
  _assert_names_refused \
    'prod\x1b[2J-a'         $'prod\e[2J-a' \
    'prod\xe2\x80\xae-a'    $'prod\xe2\x80\xae-a' \
    'prod\xe2\x80\x8b-a'    $'prod\xe2\x80\x8b-a' \
    'prod\t\t-a'            $'prod\t-a' \
    'prod-a\r'              $'prod-a\r' \
    'prod\xc2\x85-a'        $'prod\xc2\x85-a' \
    'prod\xf3\xa0\x81\x81a' $'prod\xf3\xa0\x81\x81a'
}

# A backslash: error() prints with echo -e, which reads a backslash sequence
# ("\e[2J" would reach the terminal as an ESC). The Go twin is
# TestDestroyGuardRefusesAClusterNameWithABackslash.
@test "destroy guard: a cluster_name with a backslash is refused" {
  _assert_names_refused \
    'prod\\e[2J-a' 'prod\e[2J-a' \
    'a\\b'         'a\b' \
    'prod\\'       'prod\'
}

# A byte that is not UTF-8: jq reads each such sequence as U+FFFD, and the
# guard refuses U+FFFD. The Go twin is
# TestDestroyGuardRefusesAClusterNameThatIsNotUTF8.
@test "destroy guard: a cluster_name that is not UTF-8 is refused" {
  _assert_names_refused \
    'prod\xff-a'             $'prod\xff-a' \
    'prod\xe2\x80-a'         $'prod\xe2\x80-a' \
    'prod\xc0\x80-a'         $'prod\xc0\x80-a' \
    'prod\xed\xa0\x80-a'     $'prod\xed\xa0\x80-a' \
    'prod\xf4\x90\x80\x80-a' $'prod\xf4\x90\x80\x80-a' \
    'prod\xf0\x9f\x98-a'     $'prod\xf0\x9f\x98-a'
}

# U+FFFD itself (the UTF-8 bytes EF BF BD): jq cannot tell it from a byte
# that is not UTF-8, so both twins refuse it. The Go twin is
# TestDestroyGuardRefusesAClusterNameWithUFFFD.
@test "destroy guard: a cluster_name with U+FFFD is refused" {
  _assert_names_refused \
    'prod\xef\xbf\xbd-a' $'prod\xef\xbf\xbd-a' \
    '\xef\xbf\xbd'       $'\xef\xbf\xbd'
}

# _refute_esc: the output holds no ESC byte besides the fixed colour codes
# of the guard header (bold yellow, dim, bold, reset), which go on the
# header line only. The same codes on any other line stay, so a value that
# becomes one of them fails the check.
_refute_esc() {
  local line
  while IFS= read -r line; do
    if [[ "${line}" == *"destroy deletes these resources of cluster_name"* ]]; then
      line="${line//$'\e[1;33m'/}"
      line="${line//$'\e[0m'/}"
      line="${line//$'\e[2m'/}"
      line="${line//$'\e[1m'/}"
    fi
    [[ "${line}" != *$'\e'* ]] || { printf 'an ESC byte in the output line:\n%q\n' "${line}" >&2; return 1; }
  done <<< "${output}"
}

# The messages that carry the cluster_name or the --confirm-cluster value
# print with printf '%s', not with error()'s echo -e: a --confirm-cluster
# value with the text "\e[2J" is shown as text. The guard does not show the
# typed answer: a typed "\e[2J" gives the wrong-answer error, and neither the
# text nor an ESC byte (besides the colour codes of the header) is in the
# output. The Go twin is TestDestroyGuardShowsABackslashSequenceAsText.
@test "destroy guard: a backslash sequence in --confirm-cluster or in the answer stays text" {
  _assume_no_tty
  local confirm_cluster='\e[2J'
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial "[error] --confirm-cluster '\e[2J' does not match the cluster_name 'prod-a' of 'test.prod'. Nothing was deleted."
  _refute_esc
  _nothing_ran

  _assume_tty
  confirm_cluster=""
  : > "${CALLS}"
  run provision::dispatch_destroy test.prod <<< '\e[2J'
  [ "${status}" -eq 3 ]
  assert_output --partial "[error] the answer is not the cluster_name 'prod-a'. Nothing was deleted."
  _refute_esc
  refute_output --partial '\e[2J'
  _nothing_ran
}

# A jq that fails stops the check of the cluster_name: nothing runs.
@test "destroy guard: a cluster_name that jq cannot check stops the destroy" {
  _assume_no_tty
  local confirm_cluster=prod-a
  jq() { [[ "${*}" != *'any(unshown'* ]] || return 5; command jq "${@}"; }
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "could not check the cluster_name of the destroy plan of 'test.prod'. Nothing was deleted. Check that jq runs, then run lo destroy again."
  refute_output --partial "destroy deletes these resources"
  _nothing_ran
}

# A NUL byte in the plan: a command substitution drops it with only a
# warning, so the plan would change on its way to the provider ("4<NUL>2"
# would become the id 42). The guard reads the plan through a file and
# refuses it before anything is shown, also with the matching
# --confirm-cluster. The file is removed. The Go twin is
# TestDestroyGuardRefusesAPlanWithANulByte.
@test "destroy guard: a plan with a NUL byte is refused, and nothing is deleted" {
  _assume_no_tty
  local confirm_cluster=prod-a plan
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  for plan in \
    'cluster\tprod-a\nserver\t4\x002\tcp-0\n' \
    'cluster\tprod-a\nserver\t42\tcp\x00-0\n' \
    'cluster\tprod\x00-a\nserver\t42\tcp-0\n' \
    '\x00'; do
    export PLAN="${plan}"
    : > "${CALLS}"
    run provision::dispatch_destroy test.prod < /dev/null
    [ "${status}" -eq 3 ]
    assert_output --partial "the destroy plan of 'test.prod' has a NUL byte, and the confirmed plan cannot carry it to the provider. Nothing was deleted. Correct the destroy_plan hook of the provider, then run lo destroy again."
    refute_output --partial "destroy deletes these resources"
    refute_output --partial "ignored null byte"
    _nothing_ran
    run find "${TMPDIR}" -mindepth 1
    assert_output ""
  done

  # A plan without a NUL byte runs, and its file is removed too.
  unset PLAN
  : > "${CALLS}"
  run provision::dispatch_destroy test.prod < /dev/null
  assert_success
  run grep -c DRIVER-DESTROY-RAN "${CALLS}"
  assert_output "1"
  run find "${TMPDIR}" -mindepth 1
  assert_output ""
}

@test "destroy guard: a provider without the hook keeps the yes-prompt" {
  _assume_tty
  sed -i '/^provider::destroy_plan()/,/^}/d' "${PATH_LOK8S}/providers/mock/main"
  run provision::dispatch_destroy test.prod <<< "yes"
  assert_success
  assert_output --partial "type yes to continue"
  refute_output --partial "type the cluster_name"
  run grep -c DRIVER-DESTROY-RAN "${CALLS}"
  assert_output "1"
}

# Without -r no provider is loaded and no guard runs: --confirm-cluster must
# not let the destroy run as if it were confirmed.
@test "destroy guard: --confirm-cluster without -r is refused, and nothing runs" {
  _assume_no_tty
  unset LOK8S_REMOTE
  local force=1 confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "--confirm-cluster 'prod-a' is set, but the destroy of 'test.prod' checks no cluster_name: it runs without -r (--remote), so no provider is loaded. Nothing was deleted. Add -r for a provider cluster, or remove --confirm-cluster."
  _nothing_ran

  # Without --confirm-cluster the forced destroy runs as before.
  confirm_cluster=""
  run provision::dispatch_destroy test.prod < /dev/null
  assert_success
  run grep -c DRIVER-DESTROY-RAN "${CALLS}"
  assert_output "1"
}

# The refusal of --confirm-cluster on a destroy that checks no cluster_name
# prints with printf '%s', as the guard does: a value with the text "\e[2J"
# is shown as text, with no ESC byte (Go: ui.ErrorTo in DispatchDestroy).
@test "destroy guard: a backslash sequence in --confirm-cluster stays text when no guard runs" {
  _assume_no_tty
  unset LOK8S_REMOTE
  local force=1 confirm_cluster='\e[2J'
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "[error] --confirm-cluster '\e[2J' is set, but the destroy of 'test.prod' checks no cluster_name: it runs without -r (--remote), so no provider is loaded. Nothing was deleted."
  _refute_esc
  _nothing_ran
}

@test "destroy guard: a hosted KubeOne cluster and a capi cluster get no plan, and --confirm-cluster is refused" {
  _assume_tty
  yq -i '.spec.kubehz = {"hosting": "hosted", "apiUrl": "https://api.example.com"}' "${PATH_CLUSTERS}/test.prod/cluster.lok8s.yaml"
  run provision::dispatch_destroy test.prod <<< "yes"
  assert_success
  assert_output --partial "type yes to continue"
  refute_output --partial "destroy deletes these resources"
  run grep -c '^plan:' "${CALLS}"
  assert_output "0"
  local confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "checks no cluster_name: a hosted cluster is destroyed through the kubehz API, not the provider."

  confirm_cluster=""
  _domain capi.prod prod-c
  yq -i '.kind = "Capi"' "${PATH_CLUSTERS}/capi.prod/cluster.lok8s.yaml"
  : > "${CALLS}"
  run provision::dispatch_destroy capi.prod <<< "yes"
  assert_success
  refute_output --partial "destroy deletes these resources"
  confirm_cluster=prod-c
  run provision::dispatch_destroy capi.prod < /dev/null
  [ "${status}" -eq 1 ]
  assert_output --partial "checks no cluster_name: the capi driver does not destroy through the provider."
}

# An inherited LOK8S_DESTROY_PLAN must never reach a destroy that the guard
# did not confirm (Go twin: TestDestroyGuardClearsAnInheritedPlan).
@test "destroy guard: an inherited plan is cleared on a guard-less destroy" {
  _assume_no_tty
  unset LOK8S_REMOTE
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0'
  local force=1
  run provision::dispatch_destroy test.prod < /dev/null
  assert_success
  run grep '^plan-env:' "${CALLS}"
  assert_output "plan-env:"
}

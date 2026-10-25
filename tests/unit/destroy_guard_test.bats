#!/usr/bin/env bats
# destroy_guard_test.bats — the cluster_name guard of `lo destroy`
# (provision::_destroy_guard, Go twin: internal/provision/destroy_guard.go).
#
# A provider with the provider::destroy_plan hook deletes by a name: the
# hetzner provider deletes every resource in the token's project that
# carries lok8s.dev/cluster=<cluster_name>. A domain copied from another one
# that kept the cluster_name and the token would delete the other cluster.
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
  assert_output --partial "cluster_name 'prod-a' of 'test.prod' is also the cluster_name of: copy.prod. The provider deletes every resource of a cluster_name"
  assert_output --partial "Nothing was deleted. Give each domain its own cluster_name, then run lo destroy again."
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

@test "destroy guard: a plan without a cluster line is refused" {
  _assume_no_tty
  export PLAN=$'server\t42\tcp-0\n'
  local confirm_cluster=prod-a
  run provision::dispatch_destroy test.prod < /dev/null
  [ "${status}" -eq 3 ]
  assert_output --partial "the provider gave no cluster_name for 'test.prod'. Nothing was deleted."
  _nothing_ran
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

#!/usr/bin/env bats
# kubeone_addon_values_test.bats — issue #157: the addons kubeone::render_addons
# writes for `kubeone apply` must carry the CLUSTER's inline bootstrap values
# (`values:`/`valueFiles:`), the same overlay the bootstrap path applies. The
# incident this pins: an inline-only cilium setting reverted mid-node-walk
# because render_addons rendered from the framework value stack alone.
#
# addons::render is stubbed to RECORD its arguments; the assertions read the
# recorded inline-values arg — wiring proof. The value-extraction semantics are
# bootstrap::_parse_entry's own (tested in bootstrap_test.bats), reused via
# bootstrap::inline_values, so there is no second parser to drift.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  import() { :; }
  export -f import
  if ! command -v yq &>/dev/null; then skip "yq not available"; fi

  provider::detect() { echo "hetzner"; }
  addons::render() {
    # Record every call: dir|kind|provider|inline (inline may be multi-line —
    # record it base64'd so one line = one call).
    printf '%s|%s|%s|%s\n' "$1" "$2" "$3" "$(printf '%s' "${4:-}" | base64 | tr -d '\n' )" \
      >> "${BATS_TEST_TMPDIR}/render.calls"
    echo "# stub addon"
  }
  export -f provider::detect addons::render
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/utils/keysonly.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"

  export HCLOUD_TOKEN="test-token"
  unset HROBOT_USER HROBOT_PASSWORD || true

  work_dir="${BATS_TEST_TMPDIR}/work"
  mkdir -p "${work_dir}" "${PATH_CLUSTERS}/test.lok8s.dev"
  cat > "${work_dir}/kubeone.yaml" <<'YAML'
apiVersion: kubeone.k8c.io/v1beta2
kind: KubeOneCluster
cloudProvider:
  hetzner: {}
apiEndpoint:
  host: 10.0.0.1
YAML
}

teardown() {
  teardown_tmpdir
}

_cluster_yaml() {
  # $1 = the spec.bootstrap YAML block (indented list), may be empty.
  local path="${PATH_CLUSTERS}/test.lok8s.dev/cluster.lok8s.yaml"
  { echo "apiVersion: lok8s.dev/v1alpha1"
    echo "kind: Cluster"
    echo "metadata: {name: test}"
    echo "spec:"
    if [[ -n "${1:-}" ]]; then
      echo "  bootstrap:"
      printf '%s\n' "${1}"
    else
      echo "  bootstrap: []"
    fi
  } > "${path}"
  echo "${path}"
}

_inline_of() {
  # Decode the recorded inline arg of the call whose addon dir ends in $1.
  # The call line MUST exist — "never rendered" and "rendered with empty
  # inline" are different verdicts, and a silent grep-miss would green both.
  local line
  line=$(grep "addons/${1}|" "${BATS_TEST_TMPDIR}/render.calls" | head -1)
  [[ -n "${line}" ]] || { echo "NO-RENDER-CALL-FOR-${1}" >&2; return 1; }
  awk -F'|' '{print $4}' <<<"${line}" | base64 -d
}

@test "render_addons: the cluster's inline cilium values ride the kubeone render (issue #157)" {
  local cy; cy=$(_cluster_yaml '    - cilium:
        values:
          kubeProxyReplacement: false
          policyAuditMode: true')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  local inline; inline=$(_inline_of cilium)
  [ "$(yq -r '.kubeProxyReplacement' <<<"${inline}")" = "false" ]
  [ "$(yq -r '.policyAuditMode' <<<"${inline}")" = "true" ]
}

@test "render_addons: no bootstrap entry for cilium -> empty inline (unchanged behavior)" {
  local cy; cy=$(_cluster_yaml '')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  [ -z "$(_inline_of cilium)" ]
}

@test "render_addons: no cluster yaml at all (legacy call shape) still renders — under set -u like the argsh runtime" {
  # bats does not run `set -u`, the argsh entrypoint DOES — a bare "${2}"
  # in the signature would pass here and crash in production (review find).
  # A shell FUNCTION wrapper keeps the sourced definitions in scope (a
  # `bash -c` child would see none of them).
  _legacy_call() { set -u; kubeone::render_addons "${1}"; }
  run _legacy_call "${work_dir}"
  [ "${status}" -eq 0 ]
  [ -z "$(_inline_of cilium)" ]
}

@test "render_addons: ccm merges the cluster's values ON TOP of the robot-derived inline" {
  export HROBOT_USER="u" HROBOT_PASSWORD="p"
  yq -i '.cloudProvider.hetzner.networkID = "net-1"' "${work_dir}/kubeone.yaml"
  local cy; cy=$(_cluster_yaml '    - ccm:
        values:
          env:
            HCLOUD_LOAD_BALANCERS_ENABLED: {value: "false"}')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  local inline; inline=$(_inline_of ccm)
  # Driver-derived facts survive…
  [ "$(yq -r '.env.ROBOT_ENABLED.value' <<<"${inline}")" = "true" ]
  [ "$(yq -r '.env.HCLOUD_NETWORK.value' <<<"${inline}")" = "net-1" ]
  # …and the cluster's explicit intent lands on top.
  [ "$(yq -r '.env.HCLOUD_LOAD_BALANCERS_ENABLED.value' <<<"${inline}")" = "false" ]
}

@test "render_addons: the network reaches the CCM WITHOUT Robot credentials (cloud-only cluster)" {
  # A cloud-only cluster with a private network: the CCM must learn the
  # network id or it refuses every node's private InternalIP and the
  # nodes never initialize (2026-09-23, a one-node KubeOne cluster hung
  # in "waiting for nodes to initialize by CCM").
  unset HROBOT_USER HROBOT_PASSWORD
  yq -i '.cloudProvider.hetzner.networkID = "net-7"' "${work_dir}/kubeone.yaml"
  local cy; cy=$(_cluster_yaml '')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  local inline; inline=$(_inline_of ccm)
  [ "$(yq -r '.env.HCLOUD_NETWORK.value' <<<"${inline}")" = "net-7" ]
  [ "$(yq -r '.env.ROBOT_ENABLED // "absent"' <<<"${inline}")" = "absent" ]
}

@test "render_addons: a static CLOUD worker needs no Robot credentials" {
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  # A cloud VM declared as a worker in server[] is a static host the hcloud
  # CCM finds by itself; only a #cloud.root server needs Robot access
  # (2026-09-23: a cloud-only cluster with one declared worker was refused).
  unset HROBOT_USER HROBOT_PASSWORD
  yq -i '.staticWorkers.hosts = [{"publicAddress": "10.0.0.9"}]' "${work_dir}/kubeone.yaml"
  printf '%s\n' '{"server":[{"name":"cp-1","label":"lok8s.dev/role=control-plane"},{"name":"w-1","label":"lok8s.dev/role=worker"}]}' > "${work_dir}/hetzner.dump.json"
  local cy; cy=$(_cluster_yaml '')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
}

@test "render_addons: a bare-metal server without Robot credentials is refused early" {
  command -v jq >/dev/null 2>&1 || skip "jq not installed"
  unset HROBOT_USER HROBOT_PASSWORD
  yq -i '.staticWorkers.hosts = [{"publicAddress": "10.0.0.9"}]' "${work_dir}/kubeone.yaml"
  printf '%s\n' '{"server":[{"name":"cp-1"},{"name":"metal-1","#cloud.root":"true","label":"lok8s.dev/role=worker"}]}' > "${work_dir}/hetzner.dump.json"
  local cy; cy=$(_cluster_yaml '')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"bare-metal server present but Robot creds missing"* ]]
}

@test "render_addons: on a COLLIDING key the cluster's ccm value wins (precedence, not just merge)" {
  # Non-colliding keys green under either operand order — this collision is
  # the only assertion that pins 'explicit cluster intent beats derived
  # facts' (swap the merge operands and it fails).
  export HROBOT_USER="u" HROBOT_PASSWORD="p"
  local cy; cy=$(_cluster_yaml '    - ccm:
        values:
          env:
            HCLOUD_NETWORK_ROUTES_ENABLED: {value: "true"}')
  run kubeone::render_addons "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  local inline; inline=$(_inline_of ccm)
  [ "$(yq -r '.env.HCLOUD_NETWORK_ROUTES_ENABLED.value' <<<"${inline}")" = "true" ]
}

@test "bootstrap::inline_values: a same-named ./targets entry does not shadow the framework addon" {
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/targets/cilium"
  local cy; cy=$(_cluster_yaml '    - ./targets/cilium
    - cilium:
        values:
          kubeProxyReplacement: false')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" cilium
  [ "${status}" -eq 0 ]
  [ "$(yq -r '.kubeProxyReplacement' <<<"${output}")" = "false" ]
}

@test "bootstrap::inline_values: legacy whole-map form extracts too" {
  local cy; cy=$(_cluster_yaml '    - cilium:
        encryption:
          enabled: true')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" cilium
  [ "${status}" -eq 0 ]
  [ "$(yq -r '.encryption.enabled' <<<"${output}")" = "true" ]
}

@test "bootstrap::inline_values: valueFiles pre-merge with values on top (cluster-dir relative)" {
  cat > "${PATH_CLUSTERS}/test.lok8s.dev/extra.yaml" <<'YAML'
kubeProxyReplacement: true
tunnel: disabled
YAML
  local cy; cy=$(_cluster_yaml '    - cilium:
        valueFiles: [./extra.yaml]
        values:
          kubeProxyReplacement: false')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" cilium
  [ "${status}" -eq 0 ]
  [ "$(yq -r '.kubeProxyReplacement' <<<"${output}")" = "false" ] # values: wins
  [ "$(yq -r '.tunnel' <<<"${output}")" = "disabled" ]            # file survives
}

# The Go twin is TestApplyRefusesAKeysOnlySecretInAnAddon. KubeOne applies
# the two renders itself: a render with a keys-only Secret (lo build
# --keys-only), or one that does not parse, stops the apply before kubeone
# runs. Clean renders run kubeone.
@test "render_addons: a render with a keys-only Secret stops the apply before kubeone runs" {
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  export -f kubeone
  addons::render() {
    case "${1##*/}:${BAD_ADDON:-}" in
      cilium:cilium-marked|ccm:ccm-marked)
        printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: hcloud\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\ndata:\n  token: ""\n' ;;
      ccm:ccm-not-yaml) printf 'kind: [Secret\n' ;;
      *) echo "# stub addon" ;;
    esac
  }
  local bad want
  for bad in cilium-marked ccm-marked ccm-not-yaml; do
    rm -f "${KCALLS}"
    case "${bad}" in
      cilium-marked) want="kubeone: refusing to apply addons/cilium/cilium.yaml: it holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one" ;;
      ccm-marked)    want="kubeone: refusing to apply addons/ccm-hetzner/ccm.yaml: it holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one" ;;
      *)             want="kubeone: refusing to apply addons/ccm-hetzner/ccm.yaml: it does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)" ;;
    esac
    BAD_ADDON="${bad}" run kubeone::apply "${work_dir}" "${cy}"
    [ "${status}" -ne 0 ] || { echo "${bad}: status 0"; return 1; }
    [[ "${output}" == *"${want}"* ]] || { echo "${bad}: ${output}"; return 1; }
    [ ! -f "${KCALLS}" ] || { echo "${bad}: kubeone ran: $(cat "${KCALLS}")"; return 1; }
  done
  # Control: clean renders run kubeone.
  BAD_ADDON="" run kubeone::apply "${work_dir}" "${cy}"
  [ "${status}" -eq 0 ]
  grep -q "^kubeone apply --manifest kubeone.yaml" "${KCALLS}"
}

# The Go twin is TestApplyKubeOneReadsTheCheckedCopyOfTheAddons. KubeOne
# reads the addons when it gets to them, after lo checked them, and the
# work dir is shared. lo writes the bytes that it checked into a private
# run directory, and the manifest copy that kubeone reads points there.
# The stub kubeone pauses before it reads the addons; the test replaces the
# shared files with a keys-only Secret meanwhile; then the stub resumes and
# copies what its addons path holds: the checked bytes, with modes 0700 and
# 0600, the empty csi-hetzner directory too. Nothing of the run is left.
@test "kubeone::apply: kubeone reads the checked copy of the addons, not the shared files" {
  local cy; cy=$(_cluster_yaml '')
  yq -i '.addons.enable = true | .addons.path = "./addons"' "${work_dir}/kubeone.yaml"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" SEEN="${BATS_TEST_TMPDIR}/seen"
  export PAUSED="${BATS_TEST_TMPDIR}/paused" RESUME="${BATS_TEST_TMPDIR}/resume"
  mkfifo "${PAUSED}" "${RESUME}"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  addons::render() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\ndata:\n  tail: |+\n    x\n\n\n' "${1##*/}"; }
  kubeone() {
    local manifest="" prev="" a path go
    for a in "$@"; do [[ "${prev}" != --manifest ]] || manifest="${a}"; prev="${a}"; done
    echo "kubeone $* (in ${PWD})" >> "${KCALLS}"
    exec 7<>"${PAUSED}" 8<>"${RESUME}"
    echo paused >&7
    read -r -t 30 -u 8 go || return 9
    path="$(yq -r '.addons.path' "${manifest}")"
    { stat -c '%a %n' "${manifest}" "${path%/addons}"
      find "${path}" -exec stat -c '%a %n' {} + | sed "s|${path}|<addons>|" | LC_ALL=C sort -k2
    } > "${SEEN}.modes"
    yq -r '.apiEndpoint.host' "${manifest}" > "${SEEN}.host"
    cp -a "${path}" "${SEEN}"
  }
  local marked=$'apiVersion: v1\nkind: Secret\nmetadata:\n  name: hcloud\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\ndata:\n  token: ""\n'
  kubeone::apply "${work_dir}" "${cy}" > "${BATS_TEST_TMPDIR}/apply.out" 2>&1 3>&- &
  local pid=$! word
  exec 5<>"${PAUSED}" 6<>"${RESUME}"
  read -r -t 30 -u 5 word || { kill "${pid}"; echo "kubeone did not start: $(cat "${BATS_TEST_TMPDIR}/apply.out")"; return 1; }
  # kubeone waits: replace the shared files.
  printf '%s' "${marked}" > "${work_dir}/addons/cilium/cilium.yaml"
  printf '%s' "${marked}" > "${work_dir}/addons/ccm-hetzner/ccm.yaml"
  echo go >&6
  local rc=0
  wait "${pid}" || rc=$?
  exec 5>&- 6>&-
  [ "${rc}" -eq 0 ] || { echo "rc ${rc}: $(cat "${BATS_TEST_TMPDIR}/apply.out")"; return 1; }
  grep -qE "^kubeone apply --manifest \.kubeone-run-[A-Za-z0-9]+\.yaml --auto-approve \(in ${work_dir}\)$" "${KCALLS}"
  [ "$(cat "${SEEN}.host")" = "10.0.0.1" ]
  cmp "${SEEN}/cilium/cilium.yaml" <(addons::render x/cilium)
  cmp "${SEEN}/ccm-hetzner/ccm.yaml" <(addons::render x/ccm)
  [ -d "${SEEN}/csi-hetzner" ]
  run ls -A "${SEEN}/csi-hetzner"
  assert_output ""
  run sed -E 's|\.kubeone-run-[A-Za-z0-9]+|<run>|' "${SEEN}.modes"
  assert_output "$(printf '%s\n' "600 <run>.yaml" "700 ${work_dir}/<run>" "700 <addons>" \
    "700 <addons>/ccm-hetzner" "600 <addons>/ccm-hetzner/ccm.yaml" \
    "700 <addons>/cilium" "600 <addons>/cilium/cilium.yaml" "700 <addons>/csi-hetzner")"
  run ls -A "${work_dir}"
  refute_output --partial ".kubeone-run-"
}

# Each file of the addons directory passes the keys-only apply guard when
# lo copies it into the run directory, not only the two renders: a file
# with a keys-only Secret, one that does not parse and a directory that
# cannot be read stop the apply before kubeone runs, and the run copy is
# removed (Go: TestApplyRefusesAKeysOnlySecretInAnAddon).
@test "kubeone::apply: every file of the addons directory passes the guard" {
  local cy; cy=$(_cluster_yaml '')
  yq -i '.addons.enable = true | .addons.path = "./addons"' "${work_dir}/kubeone.yaml"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" BAD=""
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  kubeone::render_addons() {
    rm -rf "${1}/addons"
    mkdir -p "${1}/addons/cilium" "${1}/addons/extra/deep"
    printf 'kind: ConfigMap\n' > "${1}/addons/cilium/cilium.yaml"
    case "${BAD}" in
      marked) printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\n' > "${1}/addons/extra/deep/x.yaml" ;;
      not-yaml) printf 'kind: [Secret\n' > "${1}/addons/extra/deep/x.yaml" ;;
      unreadable) chmod 0 "${1}/addons/extra/deep" ;;
    esac
  }
  local bad want
  for bad in marked not-yaml unreadable; do
    if [[ "${bad}" == unreadable && "$(id -u)" -eq 0 ]]; then continue; fi
    case "${bad}" in
      marked)     want="kubeone: refusing to apply addons/extra/deep/x.yaml: it holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one" ;;
      not-yaml)   want="kubeone: refusing to apply addons/extra/deep/x.yaml: it does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)" ;;
      unreadable) want="kubeone: refusing to apply addons/extra/deep: it cannot be read, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)" ;;
    esac
    rm -f "${KCALLS}"
    BAD="${bad}" run kubeone::apply "${work_dir}" "${cy}"
    chmod -R u+rwx "${work_dir}/addons"
    [ "${status}" -ne 0 ] || { echo "${bad}: status 0"; return 1; }
    [[ "${output}" == *"${want}"* ]] || { echo "${bad}: ${output}"; return 1; }
    [ ! -f "${KCALLS}" ] || { echo "${bad}: kubeone ran: $(cat "${KCALLS}")"; return 1; }
    [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]] || { echo "${bad}: the run copy is left"; return 1; }
  done
  BAD="" run kubeone::apply "${work_dir}" "${cy}"
  assert_success
  grep -qE '^kubeone apply --manifest \.kubeone-run-[A-Za-z0-9]+\.yaml --auto-approve$' "${KCALLS}"
}

# The private run directory does not change a manifest without an addons
# path: kubeone reads kubeone.yaml itself, and no run copy is made (the
# control of the keys-only test above runs that way too).
@test "kubeone::apply: a manifest without an addons path runs as it is" {
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; ls -A >> "${KCALLS}"; }
  run kubeone::apply "${work_dir}" "${cy}"
  assert_success
  run cat "${KCALLS}"
  assert_line --index 0 "kubeone apply --manifest kubeone.yaml --auto-approve"
  refute_output --partial ".kubeone-run-"
}

# kubeone::apply and render_addons hold the Hetzner credentials with
# tracing off (the rules at the top of utils/keysonly.sh): under bash -x,
# and with BASH_XTRACEFD on a file, neither HCLOUD_TOKEN nor
# HROBOT_PASSWORD is in stdout, stderr or the trace. The kubeone stub shows
# that the Robot password reached kubeone.
@test "kubeone::apply and render_addons trace no credential" {
  local cy; cy=$(_cluster_yaml '')
  export HCLOUD_TOKEN="xtrace-canary-kubeone-hcloud" HROBOT_USER="robot" HROBOT_PASSWORD="xtrace-canary-kubeone-robot"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { printf '%s\n' "${ROBOT_PASSWORD:-}" >> "${KCALLS}"; }
  assert_no_trace_leak xtrace-canary-kubeone kubeone::apply "${work_dir}" "${cy}"
  [ "$(sort -u "${KCALLS}")" = "xtrace-canary-kubeone-robot" ]
  assert_no_trace_leak xtrace-canary-kubeone kubeone::render_addons "${work_dir}" "${cy}"
}

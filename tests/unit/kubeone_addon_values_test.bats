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
  grep -qE '^kubeone apply --manifest \.kubeone-run-[A-Za-z0-9]+\.yaml' "${KCALLS}"
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

# The Go twin is TestApplyKubeOneReadsACopyOfAManifestWithoutAddons.
# KubeOne reads its manifest when it starts, after lo read it, and the
# work dir is shared: lo gives kubeone a private copy of the bytes that it
# read, also when the manifest names no addons directory. The kubeone stub
# replaces the shared kubeone.yaml (with one that names an addons
# directory with a keys-only Secret) before it reads its manifest: it
# reads the bytes that lo read, unchanged, and nothing of the run is left.
@test "kubeone::apply: kubeone reads a copy of a manifest without an addons path" {
  local cy; cy=$(_cluster_yaml '')
  printf '# no addons path\n' >> "${work_dir}/kubeone.yaml"
  cp "${work_dir}/kubeone.yaml" "${BATS_TEST_TMPDIR}/lo-read.yaml"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" SEEN="${BATS_TEST_TMPDIR}/seen.yaml"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() {
    local manifest="" prev="" a
    for a in "$@"; do [[ "${prev}" != --manifest ]] || manifest="${a}"; prev="${a}"; done
    echo "kubeone $*" >> "${KCALLS}"
    printf 'addons:\n  enable: true\n  path: ./addons\n' > kubeone.yaml
    printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: hcloud\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\n' > addons/ccm-hetzner/ccm.yaml
    stat -c '%a' "${manifest}" >> "${KCALLS}"
    cp "${manifest}" "${SEEN}"
  }
  run kubeone::apply "${work_dir}" "${cy}"
  assert_success
  run cat "${KCALLS}"
  assert_line --index 0 --regexp '^kubeone apply --manifest \.kubeone-run-[A-Za-z0-9]+\.yaml --auto-approve$'
  assert_line --index 1 600
  cmp "${SEEN}" "${BATS_TEST_TMPDIR}/lo-read.yaml"
  run ls -A "${work_dir}"
  refute_output --partial ".kubeone-run-"
}

# _run_copy_spec <manifest copy>: the name, addons.enable and addons.path
# that kubeone reads, one per line.
_run_copy_spec() {
  yq -r '.name, .addons.enable, .addons.path' "${1}"
}

# The Go twin is TestApplyRunCopyKeepsMergedSettingsAndPathForms. KubeOne
# resolves the aliases and merge keys of its manifest: the manifest copy
# keeps the addon settings that a merge key (<<) or an alias gives
# (enable), with addons.path set to the run's addons directory. A trailing
# slash and a leading ./ name the same directory: the copy holds its
# files, and a refusal names them as addons/<file>.
@test "kubeone::apply: the run copy keeps the merged settings and each form of the addons path" {
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" SEEN="${BATS_TEST_TMPDIR}/seen" BAD=""
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone::render_addons() {
    mkdir -p "${1}/addons/ccm-hetzner"
    if [[ -n "${BAD}" ]]; then
      printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\n' > "${1}/addons/ccm-hetzner/ccm.yaml"
    else
      printf 'kind: ConfigMap\n' > "${1}/addons/ccm-hetzner/ccm.yaml"
    fi
  }
  kubeone() {
    local manifest="" prev="" a
    for a in "$@"; do [[ "${prev}" != --manifest ]] || manifest="${a}"; prev="${a}"; done
    _run_copy_spec "${manifest}" > "${SEEN}"
    cp "$(yq -r '.addons.path' "${manifest}")/ccm-hetzner/ccm.yaml" "${SEEN}.ccm"
  }
  local -A manifests=(
    [plain]=$'name: test-prod\naddons:\n  enable: true\n  path: ./addons\n'
    [trailing-slash]=$'name: test-prod\naddons:\n  enable: true\n  path: ./addons/\n'
    [no-leading-dot]=$'name: test-prod\naddons:\n  enable: true\n  path: addons\n'
    [root-merge-key]=$'name: test-prod\n<<: {addons: {enable: true, path: ./addons}}\n'
    [merge-in-addons]=$'defaults: &d {enable: true, path: ./elsewhere}\nname: test-prod\naddons:\n  <<: *d\n  path: ./addons/\n'
    [alias-of-addons]=$'base: &a {enable: true, path: ./addons}\nname: test-prod\naddons: *a\n'
    [merge-sequence]=$'a: &a {enable: true}\nb: &b {enable: false, path: ./addons}\nname: test-prod\naddons:\n  <<: [*a, *b]\n'
    [first-merge-wins]=$'a: &a {enable: true, path: ./addons}\nb: &b {enable: false, path: ./elsewhere}\nname: test-prod\naddons:\n  <<: [*a, *b]\n'
    [root-merge-alias]=$'x: &x {addons: {enable: true, path: ./addons/}}\nname: test-prod\n<<: *x\n'
    [tagged-addons]=$'name: test-prod\naddons: !foo {enable: true, path: ./addons}\n'
    [tagged-root]=$'!foo\nname: test-prod\naddons:\n  enable: true\n  path: ./addons\n'
  )
  local name spec
  for name in "${!manifests[@]}"; do
    printf '%s' "${manifests[${name}]}" > "${work_dir}/kubeone.yaml"
    rm -f "${SEEN}" "${SEEN}.ccm"
    run kubeone::apply "${work_dir}" "${cy}"
    [ "${status}" -eq 0 ] || { echo "${name}: ${output}"; return 1; }
    spec="$(cat "${SEEN}")"
    [[ "${spec}" =~ ^test-prod$'\n'true$'\n'"${work_dir}"/\.kubeone-run-[A-Za-z0-9]+/addons$ ]] || { echo "${name}: ${spec}"; return 1; }
    [ "$(cat "${SEEN}.ccm")" = "kind: ConfigMap" ] || { echo "${name}: the run copy holds $(cat "${SEEN}.ccm")"; return 1; }
    [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]] || { echo "${name}: the run copy is left"; return 1; }
  done
  local path
  for path in ./addons/ addons .//addons//; do
    printf 'name: test-prod\naddons:\n  enable: true\n  path: %s\n' "${path}" > "${work_dir}/kubeone.yaml"
    BAD=1 run kubeone::apply "${work_dir}" "${cy}"
    [ "${status}" -ne 0 ] || { echo "${path}: status 0"; return 1; }
    [[ "${output}" == *"kubeone: refusing to apply addons/ccm-hetzner/ccm.yaml: it holds a keys-only Secret"* ]] || { echo "${path}: ${output}"; return 1; }
    [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]] || { echo "${path}: the run copy is left"; return 1; }
  done
  # A tag on the addons mapping or on the root does not change what the
  # node is: the guard checks the files of a tagged addons.path too.
  for name in tagged-addons tagged-root; do
    printf '%s' "${manifests[${name}]}" > "${work_dir}/kubeone.yaml"
    BAD=1 run kubeone::apply "${work_dir}" "${cy}"
    [ "${status}" -ne 0 ] || { echo "${name}: status 0"; return 1; }
    [[ "${output}" == *"kubeone: refusing to apply addons/ccm-hetzner/ccm.yaml: it holds a keys-only Secret"* ]] || { echo "${name}: ${output}"; return 1; }
    [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]] || { echo "${name}: the run copy is left"; return 1; }
  done
}

# The Go twin is TestApplyRunCopyOrder. The copy walks the addons
# directory as Go's filepath.WalkDir does: directory by directory, the
# names sorted by bytes. a/ comes before a-c.yaml ("a" is a prefix of
# "a-c.yaml"), so a/b.yaml is the first file that the guard refuses,
# although "a-c.yaml" < "a/b.yaml" as whole paths.
@test "kubeone::apply: both implementations refuse the same first file" {
  local cy; cy=$(_cluster_yaml '')
  yq -i '.addons.enable = true | .addons.path = "./addons"' "${work_dir}/kubeone.yaml"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { echo "kubeone ran" >&2; }
  kubeone::render_addons() {
    local marked='apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\n'
    mkdir -p "${1}/addons/a"
    printf "${marked}" > "${1}/addons/a/b.yaml"
    printf "${marked}" > "${1}/addons/a-c.yaml"
  }
  run kubeone::apply "${work_dir}" "${cy}"
  assert_failure
  assert_output --partial "kubeone: refusing to apply addons/a/b.yaml: it holds a keys-only Secret"
  refute_output --partial "a-c.yaml"
  refute_output --partial "kubeone ran"
}

# The Go twin is TestApplyWithAMissingAddonsDirectory. An addons path that
# does not exist gets no copy, and it is not refused: the manifest copy
# names the run's addons directory, which does not exist, and KubeOne
# reports a missing addons directory as it did before.
@test "kubeone::apply: an addons path that does not exist gets no copy" {
  local cy; cy=$(_cluster_yaml '')
  yq -i '.addons.enable = true | .addons.path = "./missing"' "${work_dir}/kubeone.yaml"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone::render_addons() { :; }
  kubeone() {
    local manifest="" prev="" a addons
    for a in "$@"; do [[ "${prev}" != --manifest ]] || manifest="${a}"; prev="${a}"; done
    addons="$(yq -r '.addons.path' "${manifest}")"
    [[ "${addons}" == "${work_dir}"/.kubeone-run-*/addons && ! -e "${addons}" ]] && echo "no addons directory" >> "${KCALLS}"
  }
  run kubeone::apply "${work_dir}" "${cy}"
  assert_success
  refute_output --partial "refusing"
  [ "$(cat "${KCALLS}")" = "no addons directory" ]
  run ls -A "${work_dir}"
  refute_output --partial ".kubeone-run-"
}

# _tree <pid>: <pid> and each process under it, the deepest first
# (_children of test_helper: the CI bats container has no pgrep).
_tree() {
  local c
  for c in $(_children "${1}"); do _tree "${c}"; done
  echo "${1}"
}

# _apply_within <seconds> <args>: run kubeone::apply in the background and
# fail when it does not end in time (a read or a write that waits on a
# FIFO). Then each process of the run is stopped, so none holds the output
# of bats open. Sets status and output as bats' run does.
_apply_within() {
  local limit="${1}" i pid; shift
  kubeone::apply "$@" > "${BATS_TEST_TMPDIR}/apply.out" 2>&1 &
  pid=$!
  for (( i = 0; i < limit * 10; i++ )); do
    kill -0 "${pid}" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "${pid}" 2>/dev/null; then
    # shellcheck disable=SC2046 # one pid per word
    kill $(_tree "${pid}") 2>/dev/null || true
    echo "kubeone::apply did not end within ${limit}s" >&2
    return 1
  fi
  status=0
  wait "${pid}" || status=$?
  output="$(cat "${BATS_TEST_TMPDIR}/apply.out")"
}

# The Go twin is TestApplyRunCopyRefusesWhatItCannotRead. A part of the
# addons path that lo cannot search is not "no addons directory": lo
# refuses the path. A FIFO in the addons directory is refused as a file
# that cannot be read: lo does not open it (an open would wait for a
# writer).
@test "kubeone::apply: an addons path that cannot be read, or a file of it that is not a regular file, stops the apply" {
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  if [[ "$(id -u)" -ne 0 ]]; then
    kubeone::render_addons() { mkdir -p "${1}/locked/addons"; printf 'kind: ConfigMap\n' > "${1}/locked/addons/ccm.yaml"; chmod 0 "${1}/locked"; }
    yq -i '.addons.enable = true | .addons.path = "./locked/addons"' "${work_dir}/kubeone.yaml"
    _apply_within 20 "${work_dir}" "${cy}"
    chmod 755 "${work_dir}/locked"
    [ "${status}" -ne 0 ]
    [[ "${output}" == *"kubeone: refusing to apply locked/addons: it cannot be read, so it cannot be checked for a keys-only Secret"* ]] || { echo "${output}"; return 1; }
    [ ! -f "${KCALLS}" ]
    [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]]
  fi
  kubeone::render_addons() { mkdir -p "${1}/addons/ccm-hetzner"; printf 'kind: ConfigMap\n' > "${1}/addons/ccm-hetzner/ccm.yaml"; mkfifo "${1}/addons/ccm-hetzner/fifo.yaml"; }
  yq -i '.addons.path = "./addons"' "${work_dir}/kubeone.yaml"
  _apply_within 20 "${work_dir}" "${cy}"
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"kubeone: refusing to apply addons/ccm-hetzner/fifo.yaml: it cannot be read, so it cannot be checked for a keys-only Secret"* ]] || { echo "${output}"; return 1; }
  [ ! -f "${KCALLS}" ]
  [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]]
}

# The Go twin is the "a parent without search permission" case of
# TestApplyRunCopyRefusesWhatItCannotRead (Go refuses a directory that
# WalkDir cannot read). A find that fails while it lists a directory (here
# after it listed each entry) stops the apply: the copy never holds a part
# of the addons directory.
@test "kubeone::apply: a listing of the addons directory that fails stops the apply" {
  local cy; cy=$(_cluster_yaml '')
  yq -i '.addons.enable = true | .addons.path = "./addons"' "${work_dir}/kubeone.yaml"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone::render_addons() { mkdir -p "${1}/addons/sub"; printf 'kind: ConfigMap\n' > "${1}/addons/a.yaml"; printf 'kind: ConfigMap\n' > "${1}/addons/sub/b.yaml"; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  find() { command find "$@"; [[ "${2-}" != */addons/sub ]] || return 1; }
  run kubeone::apply "${work_dir}" "${cy}"
  assert_failure
  assert_output --partial "kubeone: refusing to apply addons/sub: it cannot be read, so it cannot be checked for a keys-only Secret"
  [ ! -f "${KCALLS}" ]
  [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]]
}

# The Go twin is TestApplyRunCopyRefusesAManifestItCannotStage. Go reads
# the first document of kubeone.yaml, and yq here reads each one: so both
# refuse a manifest with more than one document (a last --- starts an
# empty one). An addons.path that is not a string is refused too, also one
# that a merge key gives. A first ---, an addons that is not a mapping,
# and a null or empty addons.path are no addons path: kubeone runs.
@test "kubeone::apply: a manifest with more than one document, or an addons.path that is not a string, stops the apply" {
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone::render_addons() { mkdir -p "${1}/addons/ccm-hetzner"; printf 'kind: ConfigMap\n' > "${1}/addons/ccm-hetzner/ccm.yaml"; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  local docs="kubeone: ${work_dir}/kubeone.yaml holds more than one YAML document"
  local not_str="kubeone: addons.path in ${work_dir}/kubeone.yaml is not a string"
  local plain=$'name: test-prod\naddons:\n  enable: true\n  path: ./addons\n'
  local -A manifests=(
    [two-documents]=$'name: test-prod\n---\nname: other\naddons:\n  enable: true\n  path: ./addons\n'
    [a-last-separator]="${plain}"$'---\n'
    [path-false]=$'name: test-prod\naddons:\n  enable: true\n  path: false\n'
    [path-a-number]=$'name: test-prod\naddons:\n  enable: true\n  path: 5\n'
    [path-a-mapping]=$'name: test-prod\naddons:\n  enable: true\n  path: {dir: ./addons}\n'
    [path-a-list]=$'name: test-prod\naddons:\n  enable: true\n  path: [./addons]\n'
    [a-merged-false]=$'d: &d {path: false}\nname: test-prod\naddons:\n  <<: *d\n  enable: true\n'
    [a-first-separator]=$'---\n'"${plain}"
    [addons-false]=$'name: test-prod\naddons: false\n'
    [addons-a-list]=$'name: test-prod\naddons: [./addons]\n'
    [a-null-path]=$'name: test-prod\naddons:\n  enable: true\n  path: ~\n'
    [an-empty-path]=$'name: test-prod\naddons:\n  enable: true\n  path: ""\n'
  )
  local -A want=(
    [two-documents]="${docs}" [a-last-separator]="${docs}"
    [path-false]="${not_str}" [path-a-number]="${not_str}" [path-a-mapping]="${not_str}"
    [path-a-list]="${not_str}" [a-merged-false]="${not_str}"
  )
  local name
  for name in "${!manifests[@]}"; do
    printf '%s' "${manifests[${name}]}" > "${work_dir}/kubeone.yaml"
    rm -f "${KCALLS}"
    run kubeone::apply "${work_dir}" "${cy}"
    if [[ -z "${want[${name}]-}" ]]; then
      [ "${status}" -eq 0 ] || { echo "${name}: ${output}"; return 1; }
      [ "$(wc -l < "${KCALLS}")" -eq 1 ] || { echo "${name}: kubeone did not run once"; return 1; }
    else
      [ "${status}" -ne 0 ] || { echo "${name}: status 0"; return 1; }
      [[ "${output}" == *"${want[${name}]}"* ]] || { echo "${name}: ${output}"; return 1; }
      [ ! -f "${KCALLS}" ] || { echo "${name}: kubeone ran"; return 1; }
    fi
    [[ "$(ls -A "${work_dir}")" != *".kubeone-run-"* ]] || { echo "${name}: the run copy is left"; return 1; }
  done
}

# The Go twin is TestApplyRunCopyRefusesAnExistingManifestCopy. lo writes
# the manifest copy as a new file: a FIFO that is at its path already
# (made after lo named the run directory, here by a mktemp stub) gets no
# byte, the apply stops, the run directory goes, and the FIFO stays.
@test "kubeone::apply: a file at the path of the manifest copy stops the apply" {
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" FIXED="${work_dir}/.kubeone-run-fixed"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  mktemp() { mkdir -m 700 "${FIXED}" && mkfifo "${FIXED}.yaml" && printf '%s\n' "${FIXED}"; }
  local with
  for with in none addons; do
    [[ "${with}" == none ]] || yq -i '.addons.enable = true | .addons.path = "./addons"' "${work_dir}/kubeone.yaml"
    _apply_within 20 "${work_dir}" "${cy}"
    [ "${status}" -ne 0 ] || { echo "${with}: status 0"; return 1; }
    [[ "${output}" == *"kubeone: cannot write the manifest copy ${FIXED}.yaml"* ]] || { echo "${with}: ${output}"; return 1; }
    [ ! -f "${KCALLS}" ]
    [ ! -e "${FIXED}" ] || { echo "${with}: the run directory is left"; return 1; }
    [ -p "${FIXED}.yaml" ] || { echo "${with}: the FIFO that was there is gone"; return 1; }
    rm -f "${FIXED}.yaml"
  done
}

# No Go twin: Go does not enter the run directory. lo names the run
# directory for the cleanup as soon as mktemp made it: a directory that lo
# then cannot enter (here a mktemp stub makes it with mode 0) stops the
# apply, and the cleanup removes it.
@test "kubeone::apply: a run directory that lo cannot enter is removed" {
  [[ "$(id -u)" -ne 0 ]] || skip "root can enter a directory with mode 0"
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" FIXED="${work_dir}/.kubeone-run-fixed"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  mktemp() { mkdir -m 0 "${FIXED}" && printf '%s\n' "${FIXED}"; }
  run kubeone::apply "${work_dir}" "${cy}"
  assert_failure
  assert_output --partial "kubeone: cannot make a run directory in ${work_dir}"
  [ ! -f "${KCALLS}" ]
  [ ! -e "${FIXED}" ] || { chmod 700 "${FIXED}"; echo "the run directory is left"; return 1; }
}

# The Go twin is TestApplyNamesARunCopyThatItCannotRemove. kubeone makes
# the work dir read-only while it runs: the EXIT trap cannot remove the
# run directory and the manifest copy, and rm names both. The apply
# succeeds, as in Go. The output holds no value of the run copy. LC_ALL=C:
# rm writes its message in English.
@test "kubeone::apply: a run copy that the EXIT trap cannot remove is named" {
  [[ "$(id -u)" -ne 0 ]] || skip "root removes files in a directory without write permission"
  export LC_ALL=C
  local cy; cy=$(_cluster_yaml '')
  printf 'name: test-prod\ntoken: left-copy-canary\naddons:\n  enable: true\n  path: ./addons\n' > "${work_dir}/kubeone.yaml"
  export SEEN="${BATS_TEST_TMPDIR}/seen"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone::render_addons() {
    mkdir -p "${1}/addons/ccm-hetzner"
    printf 'kind: ConfigMap\ndata:\n  token: left-copy-canary\n' > "${1}/addons/ccm-hetzner/ccm.yaml"
  }
  kubeone() {
    local manifest="" prev="" a
    for a in "$@"; do [[ "${prev}" != --manifest ]] || manifest="${a}"; prev="${a}"; done
    printf '%s\n' "${PWD}/${manifest%.yaml}" > "${SEEN}"
    chmod 500 "${PWD}"
  }
  run kubeone::apply "${work_dir}" "${cy}"
  chmod 700 "${work_dir}"
  assert_success
  local made; made="$(cat "${SEEN}")"
  [[ "${made}" == "${work_dir}"/.kubeone-run-* ]] || { echo "run directory: ${made}"; return 1; }
  # The match leaves out the quotes around the path.
  local line dir=0 copy=0
  while IFS= read -r line; do
    [[ "${line}" == *"cannot remove"* ]] || continue
    if [[ "${line}" == *"${made}.yaml"* ]]; then copy=1; elif [[ "${line}" == *"${made}"* ]]; then dir=1; fi
  done <<< "${output}"
  (( dir && copy )) || { echo "rm did not name the run directory (${dir}) and the manifest copy (${copy}): ${output}"; return 1; }
  refute_output --partial "left-copy-canary"
}

# The Go twin is TestApplyNamesTheCauseBeforeARunDirectoryThatItCannotRemove.
# A mktemp stub makes the work dir read-only after it made the run
# directory: the write of the manifest copy fails, and then the EXIT trap
# cannot remove the run directory. lo shows the cause first, then rm
# names the run directory, as in Go. LC_ALL=C: rm writes its message in
# English.
@test "kubeone::apply: the cause comes before a run directory that the EXIT trap cannot remove" {
  [[ "$(id -u)" -ne 0 ]] || skip "root writes in a directory without write permission"
  export LC_ALL=C
  local cy; cy=$(_cluster_yaml '')
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" FIXED="${work_dir}/.kubeone-run-fixed"
  printf 'name: test-prod\ntoken: cause-first-canary\n' > "${work_dir}/kubeone.yaml"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone::render_addons() { :; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  mktemp() { mkdir -m 700 "${FIXED}" && chmod 500 "${work_dir}" && printf '%s\n' "${FIXED}"; }
  run kubeone::apply "${work_dir}" "${cy}"
  chmod 700 "${work_dir}"
  assert_failure
  [ ! -f "${KCALLS}" ]
  # The first line of each: no rm line may come before the cause.
  local line n=0 cause=0 left=0
  while IFS= read -r line; do
    n=$((n + 1))
    [[ "${line}" != *"kubeone: cannot write the manifest copy ${FIXED}.yaml"* ]] || (( cause )) || cause=${n}
    [[ "${line}" != *"cannot remove '${FIXED}'"* ]] || (( left )) || left=${n}
  done <<< "${output}"
  (( cause > 0 && left > cause )) || { echo "the cause (line ${cause}) must come before the run directory (line ${left}): ${output}"; return 1; }
  refute_output --partial "cause-first-canary"
}

# The Go twins are TestApplyRemovesTheRunCopyOnASIGHUP and
# TestApplyHangupEndsLoAfterTheOuterCleanup. The subshell that makes the
# run directory owns it from the start: a SIGTERM, a SIGHUP or a SIGINT
# while lo copies the addons (after the first copied file) ends the
# subshell with the signal, its EXIT trap removes the run copy, and kubeone
# does not run. A terminal sends its SIGINT (Ctrl-C) to each process of
# the group: here the process that reads the file ends with it too, as
# bash then ends with the SIGINT of its child.
@test "kubeone::apply: a signal after the first copied file removes the run copy" {
  local cy; cy=$(_cluster_yaml '')
  yq -i '.addons.enable = true | .addons.path = "./addons"' "${work_dir}/kubeone.yaml"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls" READS="${BATS_TEST_TMPDIR}/reads" COPIED="${BATS_TEST_TMPDIR}/copied" SIG
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  kubeone() { echo "kubeone $*" >> "${KCALLS}"; }
  kubeone::render_addons() {
    mkdir -p "${1}/addons/a" "${1}/addons/b"
    printf 'kind: ConfigMap\n' > "${1}/addons/a/x.yaml"
    printf 'kind: ConfigMap\n' > "${1}/addons/b/y.yaml"
  }
  eval "$(declare -f keysonly::read_checked_file | sed '1s/^keysonly::read_checked_file/_test_read_checked_file/')"
  keysonly::read_checked_file() {
    echo "${2}" >> "${READS}"
    if [[ "${2}" == addons/b/y.yaml ]]; then
      find "${work_dir}" -path '*/.kubeone-run-*/addons/a/x.yaml' > "${COPIED}"
      # This runs in the command substitution of kubeone::_copy_dir: its
      # parent is the subshell of kubeone::apply that owns the run.
      local me="${BASHPID}"
      kill "-${SIG}" "$(_ppid "${me}")"
      [[ "${SIG}" != INT ]] || kill -INT "${me}"
    fi
    _test_read_checked_file "$@"
  }
  # With /proc (Linux), as in the CI bats container: no ps. Without /proc
  # (macOS), _ppid needs the real ps.
  [[ ! -d /proc/self ]] || ps() { echo "ps ran" >&2; return 127; }
  local want
  for SIG in TERM HUP INT; do
    rm -f "${READS}" "${COPIED}"
    case "${SIG}" in TERM) want=143 ;; HUP) want=129 ;; INT) want=130 ;; esac
    run kubeone::apply "${work_dir}" "${cy}"
    [ "${status}" -eq "${want}" ] || { echo "${SIG}: status ${status}: ${output}"; return 1; }
    [ "$(cat "${READS}")" = $'addons/a/x.yaml\naddons/b/y.yaml' ] || { echo "${SIG}: reads $(cat "${READS}")"; return 1; }
    [ -s "${COPIED}" ] || { echo "${SIG}: no file was copied"; return 1; }
    [ ! -f "${KCALLS}" ] || { echo "${SIG}: kubeone ran"; return 1; }
    run ls -A "${work_dir}"
    refute_output --partial ".kubeone-run-"
  done
}

# kubeone::apply and render_addons hold the Hetzner credentials with
# tracing off (the rules at the top of utils/keysonly.sh): in each run of
# assert_no_trace_leak (bash -x, BASH_XTRACEFD, and the RETURN, DEBUG and
# ERR traps), neither HCLOUD_TOKEN nor HROBOT_PASSWORD is in stdout,
# stderr or the trace. The manifest names an addons directory, so the run
# copy (kubeone::_stage_run, kubeone::_copy_dir) holds the CCM render with
# the token under the trace too. The kubeone stub shows that the Robot
# password and the token in the copied CCM render reached kubeone. A work
# dir without kubeone.yaml stops kubeone::apply: under errexit and
# errtrace, it returns that failure to its caller (trace_fail of
# assert_no_trace_leak).
@test "kubeone::apply and render_addons trace no credential" {
  local cy; cy=$(_cluster_yaml '')
  yq -i '.addons.enable = true | .addons.path = "./addons"' "${work_dir}/kubeone.yaml"
  export HCLOUD_TOKEN="xtrace-canary-kubeone-hcloud" HROBOT_USER="robot" HROBOT_PASSWORD="xtrace-canary-kubeone-robot"
  export KCALLS="${BATS_TEST_TMPDIR}/kubeone.calls"
  kubeone::_clean_reinstalled_workers() { :; }
  kubeone::_name_robot_workers() { :; }
  addons::render() { printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: hcloud\nstringData:\n  token: %s\n' "${HCLOUD_TOKEN}"; }
  kubeone() {
    local manifest="" prev="" a
    for a in "$@"; do [[ "${prev}" != --manifest ]] || manifest="${a}"; prev="${a}"; done
    { printf '%s\n' "${ROBOT_PASSWORD:-}"
      yq -r '.stringData.token' "$(yq -r '.addons.path' "${manifest}")/ccm-hetzner/ccm.yaml"
    } >> "${KCALLS}" 2>&1
  }
  local -a trace_fail=(kubeone::apply "${BATS_TEST_TMPDIR}/no-work-dir" "${cy}")
  assert_no_trace_leak xtrace-canary-kubeone kubeone::apply "${work_dir}" "${cy}"
  unset trace_fail
  # Each of the five runs reached kubeone with both values.
  [ "$(grep -c '^xtrace-canary-kubeone-robot$' "${KCALLS}")" -eq 5 ] || { cat "${KCALLS}"; return 1; }
  [ "$(grep -c '^xtrace-canary-kubeone-hcloud$' "${KCALLS}")" -eq 5 ] || { cat "${KCALLS}"; return 1; }
  [ "$(wc -l < "${KCALLS}")" -eq 10 ] || { cat "${KCALLS}"; return 1; }
  assert_no_trace_leak xtrace-canary-kubeone kubeone::render_addons "${work_dir}" "${cy}"
}

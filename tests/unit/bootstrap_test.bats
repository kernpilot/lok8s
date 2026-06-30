#!/usr/bin/env bats
# bootstrap_test.bats — unit tests for .lok8s/libs/bootstrap
#
# Covers the values precedence chain for bootstrap addons:
#   base (values.yaml)
#     < driver  (values.${kind}.yaml)
#       < provider (values.${provider_name}.yaml)
#         < inline  (spec.bootstrap: [name: {overrides}])
#
# Exercises bootstrap::apply with a fake framework addon and stubbed
# kustomize/kubectl/envsubst so we only assert on the merged values file
# that gets staged into the temp build dir.

setup() {
  load "../test_helper"
  setup_tmpdir
  export LOK8S_NONINTERACTIVE=1

  # bootstrap is an argsh script with `import` — stub it out.
  import() { :; }
  export -f import

  # Make template::envsubst_whitelist return something harmless.
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  # bootstrap::apply now renders via addons::render (libs/addons); with `import`
  # stubbed we source it directly so the shared render path is available.
  source "${_PROJECT_ROOT}/.lok8s/libs/addons"

  # Fake addon dir: .lok8s/addons/testcni/
  ADDON_DIR="${PATH_LOK8S}/addons/testcni"
  mkdir -p "${ADDON_DIR}"

  # Minimal chart.yaml so the "framework addon with chart.yaml" branch
  # in bootstrap::apply is taken (that's the branch doing values merging).
  cat > "${ADDON_DIR}/chart.yaml" <<'YAML'
apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: testcni
valueFiles:
  - values.yaml
YAML

  # Layered values files. Keys are designed so we can tell which layer "won":
  #   - only_base          → only in base
  #   - only_driver        → only in driver
  #   - only_provider      → only in provider
  #   - shared_all         → set in base, driver, provider, and inline
  #   - nested.*           → tests deep merge
  cat > "${ADDON_DIR}/values.yaml" <<'YAML'
only_base: "base"
shared_all: "base"
nested:
  from_base: true
  overridden: "base"
YAML

  cat > "${ADDON_DIR}/values.lo.yaml" <<'YAML'
only_driver: "driver"
shared_all: "driver"
nested:
  from_driver: true
  overridden: "driver"
YAML

  cat > "${ADDON_DIR}/values.hetzner.yaml" <<'YAML'
only_provider: "provider"
shared_all: "provider"
nested:
  from_provider: true
  overridden: "provider"
YAML

  # Cluster spec under PATH_CLUSTERS
  CLUSTER_YAML="${PATH_CLUSTERS}/test.lok8s.dev/cluster.lok8s.yaml"
  mkdir -p "$(dirname "${CLUSTER_YAML}")"

  # Fake kubeconfig so the [[ -f kubeconfig ]] guard passes.
  KUBECONFIG_FILE="${PATH_BASE}/.kubeconfig/e2e-test.yaml"
  mkdir -p "$(dirname "${KUBECONFIG_FILE}")"
  : > "${KUBECONFIG_FILE}"

  # Intercept kustomize & kubectl & envsubst so no real tools run.
  # kustomize captures the build dir it was invoked with so tests can
  # inspect the merged values file after bootstrap::apply returns.
  export CAPTURED_BUILD_DIR_FILE="${BATS_TEST_TMPDIR}/captured_build_dir"
  : > "${CAPTURED_BUILD_DIR_FILE}"

  kustomize() {
    # Walk args to find the build dir (last positional after flags).
    local arg build_dir=""
    for arg in "$@"; do
      case "${arg}" in
        --enable-alpha-plugins|build) ;;
        -*) ;;
        *) build_dir="${arg}" ;;
      esac
    done
    [[ -z "${build_dir}" ]] || echo "${build_dir}" >> "${CAPTURED_BUILD_DIR_FILE}"
    # Copy the merged values file out so the tmp build dir's cleanup
    # doesn't delete it before the test can assert on it.
    if [[ -n "${build_dir}" && -f "${build_dir}/values.merged.yaml" ]]; then
      cp "${build_dir}/values.merged.yaml" "${BATS_TEST_TMPDIR}/last_merged.yaml"
    fi
    echo "---"
    echo "apiVersion: v1"
    echo "kind: ConfigMap"
    echo "metadata:"
    echo "  name: testcni"
  }
  export -f kustomize

  envsubst() { cat; }
  export -f envsubst

  kubectl() { return 0; }
  export -f kubectl

  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
}

teardown() {
  teardown_tmpdir
}

# --- Helpers ------------------------------------------------------------------

# Writes a cluster spec with kind=Lo, provider=hetzner, and the given
# bootstrap entries. Usage: write_cluster_spec "testcni" "testcni: {shared_all: inline}"
write_cluster_spec() {
  local entries=""
  local e
  for e in "$@"; do
    entries+="  - ${e}
"
  done
  cat > "${CLUSTER_YAML}" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
${entries}
YAML
}

# --- Tests --------------------------------------------------------------------

@test "bootstrap::apply merges base < driver < provider (three-layer stack)" {
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # Each layer's unique key must survive the merge (no layer dropped).
  [ "$(yq -r '.only_base' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider' "${merged}")" = "provider" ]

  # Precedence: provider wins over driver wins over base.
  [ "$(yq -r '.shared_all' "${merged}")" = "provider" ]

  # Deep merge: all nested keys preserved, overridden key follows precedence.
  [ "$(yq -r '.nested.from_base' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_driver' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_provider' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "provider" ]
}

@test "bootstrap::apply inline overrides beat provider, driver, and base values" {
  # Inline override uses the map form: "- name: {key: value}"
  write_cluster_spec "testcni: {shared_all: inline, nested: {overridden: inline, from_inline: true}}"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # Inline must be highest precedence.
  [ "$(yq -r '.shared_all' "${merged}")" = "inline" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "inline" ]

  # Lower layers still contribute their unique keys.
  [ "$(yq -r '.only_base' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider' "${merged}")" = "provider" ]
  [ "$(yq -r '.nested.from_base' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_driver' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_provider' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_inline' "${merged}")" = "true" ]
}

@test "bootstrap::apply falls back to driver precedence when no provider values" {
  # Remove provider-specific values.
  rm -f "${ADDON_DIR}/values.hetzner.yaml"
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # Driver wins over base; provider-only keys absent.
  [ "$(yq -r '.shared_all' "${merged}")" = "driver" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider // "missing"' "${merged}")" = "missing" ]
}

@test "bootstrap::apply uses only base values when no driver/provider files exist" {
  rm -f "${ADDON_DIR}/values.lo.yaml" "${ADDON_DIR}/values.hetzner.yaml"
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  [ "$(yq -r '.shared_all' "${merged}")" = "base" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver // "missing"' "${merged}")" = "missing" ]
  [ "$(yq -r '.only_provider // "missing"' "${merged}")" = "missing" ]
}

@test "bootstrap::apply defaults to [cilium] when spec.bootstrap is empty" {
  # Create a minimal cilium addon stub so the default resolves without real helm.
  local cilium_dir="${PATH_LOK8S}/addons/cilium"
  mkdir -p "${cilium_dir}"
  cat > "${cilium_dir}/chart.yaml" <<'YAML'
apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: cilium
valueFiles:
  - values.yaml
YAML
  cat > "${cilium_dir}/values.yaml" <<'YAML'
marker: "cilium-default"
YAML

  # Cluster spec with NO spec.bootstrap section.
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
YAML

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  # kustomize must have been invoked exactly once (for the default cilium addon).
  run wc -l < "${CAPTURED_BUILD_DIR_FILE}"
  assert_success
  assert_output "1"

  # The merged values file must come from the cilium addon (marker key present).
  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]
  [ "$(yq -r '.marker' "${merged}")" = "cilium-default" ]
}

@test "bootstrap::apply skips entirely when spec.bootstrap is an explicit empty list" {
  # `bootstrap: []` is authoritative opt-out — no cilium default.
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Kkp
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap: []
YAML

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  # kustomize must NOT have been invoked.
  if [ -f "${CAPTURED_BUILD_DIR_FILE}" ]; then
    run wc -l < "${CAPTURED_BUILD_DIR_FILE}"
    assert_output "0"
  fi
}

@test "bootstrap::apply fails when kubeconfig missing" {
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${PATH_BASE}/.kubeconfig/does-not-exist.yaml"
  assert_failure
  assert_output --partial "kubeconfig not found"
}

@test "bootstrap::apply fails when addon directory missing" {
  write_cluster_spec "doesnotexist"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "addon not found"
}

# --- per-driver default policy (bootstrap::_resolve_entries) -----------------
# The default for an ABSENT spec.bootstrap is per-driver, not one-size-fits-all:
# only `lo` (kind) ships without a CNI. KubeOne deploys its own cilium during
# apply; Capi/Kkp bring their CNI from the management cluster. (FRICTION
# 2026-06-12: the blanket [cilium] default caused stray cilium applies on
# managed clusters.)

@test "_resolve_entries: explicit non-empty list returns entries in order" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Lo
spec:
  bootstrap: [cilium, ./targets/foo, /abs/bar]
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" lo
  assert_success
  # Entries are emitted as compact JSON (one per line) so map entries survive.
  [ "${lines[0]}" = '"cilium"' ]
  [ "${lines[1]}" = '"./targets/foo"' ]
  [ "${lines[2]}" = '"/abs/bar"' ]
}

@test "_resolve_entries: drops comments, keeps inline-override map entries (block form)" {
  # Comments must not become bogus addon names. A map entry written in BLOCK
  # style — what a YAML formatter rewrites flow maps to — must survive as ONE
  # entry; compact JSON keeps it on a single line (mapfile would otherwise
  # shatter a multi-line block map across array elements).
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Capi
spec:
  # leading comment
  bootstrap:
    - cilium
    # a comment between entries
    - ccm:
        networking:
          enabled: true
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" capi
  assert_success
  [ "${#lines[@]}" -eq 2 ]
  [ "${lines[0]}" = '"cilium"' ]
  [ "${lines[1]}" = '{"ccm":{"networking":{"enabled":true}}}' ]
}

@test "_resolve_entries: explicit empty list opts out (lo)" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Lo
spec:
  bootstrap: []
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" lo
  assert_success
  [ -z "$output" ]
}

@test "_resolve_entries: absent bootstrap defaults to cilium for lo" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Lo
spec:
  network: {cidr: 10.0.0.0/16}
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" lo
  assert_output "cilium"
}

@test "_resolve_entries: absent bootstrap is empty for kubeone (driver owns CNI)" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: KubeOne
spec:
  network: {cidr: 10.0.0.0/16}
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" kubeone
  [ -z "$output" ]
}

@test "_resolve_entries: absent bootstrap is empty for capi and kkp" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Capi
spec:
  provider: {name: hetzner}
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" capi
  [ -z "$output" ]
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" kkp
  [ -z "$output" ]
}

# --- entry parser (bootstrap::_parse_entry) ----------------------------------
# Entries arrive as the compact JSON that _resolve_entries emits: a scalar
# ("cilium", "./targets/x", "/abs/x") or a map {"<name-or-path>": <value>}.
# The map value is either the NEW schema (reserved keys values/env/wait) or, for
# backward-compat, the LEGACY whole-map-is-helm-values form.

@test "_parse_entry: bare name resolves to a framework addon, no overrides" {
  local p_name p_dir p_inline p_env p_wait
  bootstrap::_parse_entry "test.lok8s.dev" '"cilium"' p_name p_dir p_inline p_env p_wait
  [ "${p_name}" = "cilium" ]
  [ "${p_dir}" = "${PATH_LOK8S}/addons/cilium" ]
  [ -z "${p_inline}" ]
  [ -z "${p_env}" ]
  [ "${p_wait}" = "false" ]
}

@test "_parse_entry: ./path resolves under the cluster dir" {
  local p_name p_dir p_inline p_env p_wait
  bootstrap::_parse_entry "test.lok8s.dev" '"./targets/foo"' p_name p_dir p_inline p_env p_wait
  [ "${p_name}" = "foo" ]
  [ "${p_dir}" = "${PATH_CLUSTERS}/test.lok8s.dev/./targets/foo" ]
  [ -z "${p_inline}" ]
  [ "${p_wait}" = "false" ]
}

@test "_parse_entry: /abs path resolves under PATH_BASE" {
  local p_name p_dir p_inline p_env p_wait
  bootstrap::_parse_entry "test.lok8s.dev" '"/abs/bar"' p_name p_dir p_inline p_env p_wait
  [ "${p_name}" = "bar" ]
  [ "${p_dir}" = "${PATH_BASE}/abs/bar" ]
}

@test "_parse_entry: map {values} → inline helm values (chart addon)" {
  # testcni has chart.yaml (setup), so `values:` is allowed.
  local p_name p_dir p_inline p_env p_wait
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"values":{"shared_all":"inline","nested":{"k":1}}}}' \
    p_name p_dir p_inline p_env p_wait
  [ "${p_name}" = "testcni" ]
  [ "$(yq -r '.shared_all' <<<"${p_inline}")" = "inline" ]
  [ "$(yq -r '.nested.k' <<<"${p_inline}")" = "1" ]
  [ -z "${p_env}" ]
  [ "${p_wait}" = "false" ]
}

@test "_parse_entry: map {values, env, wait} → all three parsed" {
  local p_name p_dir p_inline p_env p_wait
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"values":{"a":1},"env":{"LOK8S_USER_FOO":"bar","LOK8S_USER_BAZ":"qux"},"wait":true}}' \
    p_name p_dir p_inline p_env p_wait
  [ "$(yq -r '.a' <<<"${p_inline}")" = "1" ]
  [ "${p_wait}" = "true" ]
  # env flattened to KEY=VALUE lines (order-independent membership check).
  grep -qx 'LOK8S_USER_FOO=bar' <<<"${p_env}"
  grep -qx 'LOK8S_USER_BAZ=qux' <<<"${p_env}"
}

@test "_parse_entry: legacy map (no reserved key) is the whole-map helm values" {
  # Backward-compat: `- cilium: {encryption: {enabled: true}}` — the value map
  # has no reserved key, so the WHOLE map is the inline helm values.
  local p_name p_dir p_inline p_env p_wait
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"encryption":{"enabled":true}}}' \
    p_name p_dir p_inline p_env p_wait
  [ "${p_name}" = "testcni" ]
  [ "$(yq -r '.encryption.enabled' <<<"${p_inline}")" = "true" ]
  [ -z "${p_env}" ]
  [ "${p_wait}" = "false" ]
}

@test "_parse_entry: 'values:' on a non-chart (kustomize) target is an error" {
  # A ./targets/ dir with only a kustomization.yaml (no chart.yaml) is not a
  # chart addon — `values:` (helm-only) must be rejected.
  local raw_dir="${PATH_CLUSTERS}/test.lok8s.dev/targets/raw"
  mkdir -p "${raw_dir}"
  cat > "${raw_dir}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
YAML
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"./targets/raw":{"values":{"x":1}}}' n d i e w
  assert_failure
  assert_output --partial "not a chart addon"
}

@test "_parse_entry: map-valued env entry is rejected (the ccm-break case)" {
  # `env:` takes KEY: scalar only. A map value — e.g. the hcloud CCM chart's own
  # top-level `env:` block accidentally left at the reserved-key level instead of
  # nested under `values:` — would tostring-flatten to a bogus string. Reject it.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":{"ROBOT_ENABLED":{"value":"true"}}}}' n d i e w
  assert_failure
  assert_output --partial "env: values must be scalars"
}

@test "_parse_entry: env as a list (wrong CONTAINER type) is rejected" {
  # The env CONTAINER must be a map of KEY: scalar. A YAML list — e.g. someone
  # writing `env: [LOK8S_USER_FOO=bar]` expecting docker-style entries — would
  # make to_entries[] emit bogus numeric keys (0=…). Reject the container itself.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":["LOK8S_USER_FOO=bar"]}}' n d i e w
  assert_failure
  assert_output --partial "env: must be a map"
}

@test "_parse_entry: env as a scalar (wrong CONTAINER type) is rejected" {
  # A scalar `.env` (e.g. `env: LOK8S_USER_FOO=bar`) would make to_entries[]
  # error — reject the container type up front with a clear message.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":"LOK8S_USER_FOO=bar"}}' n d i e w
  assert_failure
  assert_output --partial "env: must be a map"
}

@test "_parse_entry: multi-key map entry is rejected" {
  # A bootstrap entry must be a SINGLE-key map; two keys is a config mistake.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"wait":true},"other":{"wait":false}}' n d i e w
  assert_failure
  assert_output --partial "single-key map"
}

@test "_parse_entry: non-boolean wait is rejected" {
  # `wait: yes` (or on/1) must NOT silently become a non-barrier — only a real
  # boolean true/false is accepted.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"wait":"yes"}}' n d i e w
  assert_failure
  assert_output --partial "non-boolean wait"
}

@test "_parse_entry: map entry with a non-map value is rejected" {
  # A map-form entry's VALUE must itself be a map (the {values,env,wait} schema
  # or legacy chart values) or null. A scalar — `- addon: true` → {"addon":true}
  # — or a sequence — `- addon: []` → {"addon":[]} — would let the reserved-key /
  # legacy-values logic run yq '.values'/'.wait' against a scalar/seq. Reject it.
  run bootstrap::_parse_entry "test.lok8s.dev" '{"testcni":true}' n d i e w
  assert_failure
  assert_output --partial "entry value must be a map"

  run bootstrap::_parse_entry "test.lok8s.dev" '{"testcni":[]}' n d i e w
  assert_failure
  assert_output --partial "entry value must be a map"
}

@test "_parse_entry: env: key with an illegal shell name is rejected" {
  # Each env: key is exported VERBATIM by _apply_one, so it must be a valid POSIX
  # shell variable name. A name with a '-' would make `export` fail/misbehave.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":{"MY-VAR":"x"}}}' n d i e w
  assert_failure
  assert_output --partial "not a valid shell variable name"

  # A leading-digit name is equally invalid.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":{"1X":"y"}}}' n d i e w
  assert_failure
  assert_output --partial "not a valid shell variable name"
}

# --- env wiring: env: {…} → exported around addons::render --------------------

@test "bootstrap::apply exports env: overrides into the addons::render env" {
  # The new `env:` map is exported (this addon's subshell only) before render,
  # so render's envsubst whitelist picks up LOK8S_USER_*/LOK8S_SPEC_* names.
  local capture="${BATS_TEST_TMPDIR}/env_capture"
  : > "${capture}"
  # Capture what addons::render sees for the var, then emit a dummy manifest.
  addons::render() {
    printf '%s\n' "${LOK8S_USER_TESTVAR:-UNSET}" > "${capture}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'
    return 0
  }

  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - testcni:
        env:
          LOK8S_USER_TESTVAR: hello
YAML

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  [ "$(cat "${capture}")" = "hello" ]
}

# --- scheduler: parallel batches split by wait: true barriers ----------------

@test "bootstrap::apply runs non-barrier entries concurrently; wait:true serializes" {
  # Mock the per-entry apply with a timestamped START/END log + a fixed sleep so
  # we can assert real-time interleaving from the line order in the log.
  local log="${BATS_TEST_TMPDIR}/order.log"
  : > "${log}"
  bootstrap::_apply_one() {
    local name="$1"
    printf 'START %s\n' "${name}" >> "${log}"
    sleep 0.4
    printf 'END %s\n' "${name}" >> "${log}"
    return 0
  }

  # Five framework addons must exist (the -d addon_dir guard).
  local n
  for n in a b c d e; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done

  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - a
    - b
    - c: { wait: true }
    - d
    - e
YAML

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  # 1-based line number of the first exact-match line, or empty.
  ln() { grep -n -- "^$1\$" "${log}" | head -1 | cut -d: -f1; }
  local sa eb_ sb ea_ sc ec_ sd se
  sa=$(ln "START a"); sb=$(ln "START b"); ea_=$(ln "END a"); eb_=$(ln "END b")
  sc=$(ln "START c"); ec_=$(ln "END c"); sd=$(ln "START d"); se=$(ln "START e")

  # a and b overlap: each STARTED before the other ENDED.
  [ "${sa}" -lt "${eb_}" ]
  [ "${sb}" -lt "${ea_}" ]

  # Barrier c: it STARTS only after BOTH a and b have ENDED.
  [ "${sc}" -gt "${ea_}" ]
  [ "${sc}" -gt "${eb_}" ]

  # d and e (after the barrier) START only after c has ENDED.
  [ "${sd}" -gt "${ec_}" ]
  [ "${se}" -gt "${ec_}" ]
}

@test "bootstrap::apply returns non-zero if any parallel entry fails (no orphans, temp dir cleaned)" {
  # One entry fails; the others succeed. apply must drain the whole batch and
  # then report failure — never leaving a background job behind AND removing the
  # scheduler's rc temp dir.
  local done_log="${BATS_TEST_TMPDIR}/done.log"
  : > "${done_log}"
  bootstrap::_apply_one() {
    local name="$1"
    sleep 0.1
    printf '%s\n' "${name}" >> "${done_log}"
    [ "${name}" = "b" ] && return 1
    return 0
  }
  # Capture the scheduler's rc temp dir (the only `mktemp -d` reached here, since
  # _apply_one is stubbed) so we can assert it is cleaned up afterward.
  local rcdir_marker="${BATS_TEST_TMPDIR}/rcdir_path"
  : > "${rcdir_marker}"
  mktemp() {
    if [ "$1" = "-d" ]; then
      local d; d="$(command mktemp -d "${BATS_TEST_TMPDIR}/rcdir.XXXXXX")"
      printf '%s\n' "${d}" > "${rcdir_marker}"
      printf '%s\n' "${d}"
      return 0
    fi
    command mktemp "$@"
  }
  local n
  for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - a
    - b
    - c
YAML
  # Call directly (NOT via `run`, which forks a subshell) so the scheduler's
  # background jobs land in THIS shell's job table — lets us prove no orphan
  # survives. `|| rc=$?` keeps a non-zero from tripping bats' errexit.
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" || rc=$?
  [ "${rc}" -ne 0 ]

  # All three were launched in the same batch and drained (no orphan left).
  run wc -l < "${done_log}"
  assert_output "3"

  # No surviving background jobs. `jobs -p` must run in the current shell (not a
  # `$()`/`run` subshell, which has its own empty job table) — redirect to a file.
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]

  # The scheduler's rc temp dir was removed.
  local rcdir; rcdir="$(cat "${rcdir_marker}")"
  [ -n "${rcdir}" ]
  [ ! -d "${rcdir}" ]
}

@test "bootstrap::apply throttle frees a slot when ANY job finishes (not just the oldest)" {
  # Verify the free-any-slot throttle: with a low cap and one slow leading entry,
  # the faster trailing entries must still complete promptly — they can't be
  # blocked waiting on the oldest (slow) pid.
  (( BASH_VERSINFO[0] > 5 || (BASH_VERSINFO[0] == 5 && BASH_VERSINFO[1] >= 1) )) \
    || skip "wait -n -p needs bash 5.1+ (FIFO fallback is order-sensitive)"
  export LOK8S_BOOTSTRAP_PARALLEL=2
  local log="${BATS_TEST_TMPDIR}/throttle.log"
  : > "${log}"
  bootstrap::_apply_one() {
    local name="$1"
    if [ "${name}" = "a" ]; then sleep 0.8; else sleep 0.1; fi
    printf '%s\n' "${name}" >> "${log}"
    return 0
  }
  local n; for n in a b c d; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - a
    - b
    - c
    - d
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  # All four completed.
  run wc -l < "${log}"
  assert_output "4"
  # 'a' started first but is slowest: with free-any throttling b/c/d finish while
  # 'a' is still running, so 'a' is recorded LAST. (FIFO-on-oldest would block on
  # 'a' and let 'd' land last instead.)
  run tail -1 "${log}"
  assert_output "a"
}

@test "bootstrap::apply FIFO reap fallback: rc accounting + completeness hold" {
  # Force the bash<5.1 FIFO fallback in _reap_one (wait on the OLDEST pid) even on
  # a newer bash, by stubbing the feature probe to report unavailable. That path
  # is the only one on bash<5.1 and is otherwise uncovered here — the throttle
  # test above skips when `wait -n -p` is unavailable, which it isn't on this
  # runner. We assert the two invariants that must hold on BOTH reap paths: every
  # entry still runs, and a failing entry still propagates to a non-zero rc.
  bootstrap::_have_wait_n_p() { return 1; }
  export LOK8S_BOOTSTRAP_PARALLEL=2
  local log="${BATS_TEST_TMPDIR}/fifo.log"
  : > "${log}"
  bootstrap::_apply_one() {
    local name="$1"
    sleep 0.1
    printf '%s\n' "${name}" >> "${log}"
    # One entry fails so we prove rc accounting survives the FIFO reap+prune.
    [ "${name}" = "c" ] && return 1
    return 0
  }
  local n; for n in a b c d; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - a
    - b
    - c
    - d
YAML
  # Call directly (NOT via `run`) so the scheduler's background jobs land in THIS
  # shell's job table — lets us prove no orphan survives. `|| rc=$?` keeps the
  # non-zero from tripping bats' errexit.
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" || rc=$?

  # rc accounting: the failing entry 'c' propagated through the FIFO reap path.
  [ "${rc}" -ne 0 ]

  # Completeness: all four entries ran (the FIFO reap pruned finished jobs so new
  # ones could launch — no entry was starved or dropped).
  run wc -l < "${log}"
  assert_output "4"

  # No surviving background jobs (no orphan left by the FIFO reap+drain).
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]
}

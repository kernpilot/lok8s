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
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" '"cilium"' p_name p_dir p_inline p_env p_wait p_deps
  [ "${p_name}" = "cilium" ]
  [ "${p_dir}" = "${PATH_LOK8S}/addons/cilium" ]
  [ -z "${p_inline}" ]
  [ -z "${p_env}" ]
  [ "${p_wait}" = "false" ]
  [ -z "${p_deps}" ]
}

@test "_parse_entry: ./path resolves under the cluster dir" {
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" '"./targets/foo"' p_name p_dir p_inline p_env p_wait p_deps
  [ "${p_name}" = "foo" ]
  [ "${p_dir}" = "${PATH_CLUSTERS}/test.lok8s.dev/./targets/foo" ]
  [ -z "${p_inline}" ]
  [ "${p_wait}" = "false" ]
}

@test "_parse_entry: /abs path resolves under PATH_BASE" {
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" '"/abs/bar"' p_name p_dir p_inline p_env p_wait p_deps
  [ "${p_name}" = "bar" ]
  [ "${p_dir}" = "${PATH_BASE}/abs/bar" ]
}

@test "_parse_entry: map {values} → inline helm values (chart addon)" {
  # testcni has chart.yaml (setup), so `values:` is allowed.
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"values":{"shared_all":"inline","nested":{"k":1}}}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  [ "${p_name}" = "testcni" ]
  [ "$(yq -r '.shared_all' <<<"${p_inline}")" = "inline" ]
  [ "$(yq -r '.nested.k' <<<"${p_inline}")" = "1" ]
  [ -z "${p_env}" ]
  [ "${p_wait}" = "false" ]
  [ -z "${p_deps}" ]
}

@test "_parse_entry: map {values, env, wait} → all three parsed" {
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"values":{"a":1},"env":{"LOK8S_USER_FOO":"bar","LOK8S_USER_BAZ":"qux"},"wait":true}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  [ "$(yq -r '.a' <<<"${p_inline}")" = "1" ]
  [ "${p_wait}" = "true" ]
  # env flattened to KEY=VALUE lines (order-independent membership check).
  grep -qx 'LOK8S_USER_FOO=bar' <<<"${p_env}"
  grep -qx 'LOK8S_USER_BAZ=qux' <<<"${p_env}"
}

@test "_parse_entry: legacy map (no reserved key) is the whole-map helm values" {
  # Backward-compat: `- cilium: {encryption: {enabled: true}}` — the value map
  # has no reserved key, so the WHOLE map is the inline helm values.
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"encryption":{"enabled":true}}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  [ "${p_name}" = "testcni" ]
  [ "$(yq -r '.encryption.enabled' <<<"${p_inline}")" = "true" ]
  [ -z "${p_env}" ]
  [ "${p_wait}" = "false" ]
  [ -z "${p_deps}" ]
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
    '{"./targets/raw":{"values":{"x":1}}}' n d i e w x
  assert_failure
  assert_output --partial "not a chart addon"
}

@test "_parse_entry: map-valued env entry is rejected (the ccm-break case)" {
  # `env:` takes KEY: scalar only. A map value — e.g. the hcloud CCM chart's own
  # top-level `env:` block accidentally left at the reserved-key level instead of
  # nested under `values:` — would tostring-flatten to a bogus string. Reject it.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":{"ROBOT_ENABLED":{"value":"true"}}}}' n d i e w x
  assert_failure
  assert_output --partial "env: values must be scalars"
}

@test "_parse_entry: env as a list (wrong CONTAINER type) is rejected" {
  # The env CONTAINER must be a map of KEY: scalar. A YAML list — e.g. someone
  # writing `env: [LOK8S_USER_FOO=bar]` expecting docker-style entries — would
  # make to_entries[] emit bogus numeric keys (0=…). Reject the container itself.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":["LOK8S_USER_FOO=bar"]}}' n d i e w x
  assert_failure
  assert_output --partial "env: must be a map"
}

@test "_parse_entry: env as a scalar (wrong CONTAINER type) is rejected" {
  # A scalar `.env` (e.g. `env: LOK8S_USER_FOO=bar`) would make to_entries[]
  # error — reject the container type up front with a clear message.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":"LOK8S_USER_FOO=bar"}}' n d i e w x
  assert_failure
  assert_output --partial "env: must be a map"
}

@test "_parse_entry: multi-key map entry is rejected" {
  # A bootstrap entry must be a SINGLE-key map; two keys is a config mistake.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"wait":true},"other":{"wait":false}}' n d i e w x
  assert_failure
  assert_output --partial "single-key map"
}

@test "_parse_entry: non-boolean wait is rejected" {
  # `wait: yes` (or on/1) must NOT silently become a non-barrier — only a real
  # boolean true/false is accepted.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"wait":"yes"}}' n d i e w x
  assert_failure
  assert_output --partial "non-boolean wait"
}

@test "_parse_entry: map entry with a non-map value is rejected" {
  # A map-form entry's VALUE must itself be a map (the {values,env,wait} schema
  # or legacy chart values) or null. A scalar — `- addon: true` → {"addon":true}
  # — or a sequence — `- addon: []` → {"addon":[]} — would let the reserved-key /
  # legacy-values logic run yq '.values'/'.wait' against a scalar/seq. Reject it.
  run bootstrap::_parse_entry "test.lok8s.dev" '{"testcni":true}' n d i e w x
  assert_failure
  assert_output --partial "entry value must be a map"

  run bootstrap::_parse_entry "test.lok8s.dev" '{"testcni":[]}' n d i e w x
  assert_failure
  assert_output --partial "entry value must be a map"
}

@test "_parse_entry: env: key with an illegal shell name is rejected" {
  # Each env: key is exported VERBATIM by _apply_one, so it must be a valid POSIX
  # shell variable name. A name with a '-' would make `export` fail/misbehave.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":{"MY-VAR":"x"}}}' n d i e w x
  assert_failure
  assert_output --partial "not a valid shell variable name"

  # A leading-digit name is equally invalid.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"env":{"1X":"y"}}}' n d i e w x
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
  # Pin parallelism so the overlap assertions are deterministic regardless of the
  # ambient env (a CI runner exporting LOK8S_BOOTSTRAP_PARALLEL=1 would serialize).
  export LOK8S_BOOTSTRAP_PARALLEL=8
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
  # Pin parallelism so all three launch in one wave (the runner stops launching
  # after a failure surfaces) regardless of the ambient LOK8S_BOOTSTRAP_PARALLEL.
  export LOK8S_BOOTSTRAP_PARALLEL=8
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

# --- dependsOn: parse (bootstrap::_parse_entry) ------------------------------
# dependsOn is the 6th out-param: a newline-separated list of entry names this
# entry waits on. It is a reserved key (like values/env/wait) so an entry with
# ONLY dependsOn is the new schema, not legacy whole-map helm values.

@test "_parse_entry: dependsOn list of names is parsed (6th out-param)" {
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"dependsOn":["cert-manager","ccm"]}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  [ "${p_name}" = "testcni" ]
  # Newline-separated names (order-independent membership check).
  grep -qx 'cert-manager' <<<"${p_deps}"
  grep -qx 'ccm' <<<"${p_deps}"
  # dependsOn alone is the NEW schema — no inline helm values leak from it.
  [ -z "${p_inline}" ]
  [ "${p_wait}" = "false" ]
}

@test "_parse_entry: dependsOn that is not a list is rejected" {
  # `dependsOn: cert-manager` (a bare scalar, not a YAML list) is a config
  # mistake — the container must be a sequence of names.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"dependsOn":"cert-manager"}}' n d i e w x
  assert_failure
  assert_output --partial "dependsOn: must be a list"
}

@test "_parse_entry: dependsOn with a non-scalar element is rejected" {
  # Every element must be a scalar entry NAME; a map/list element is rejected.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"dependsOn":[{"name":"x"}]}}' n d i e w x
  assert_failure
  assert_output --partial "must be a scalar entry name"
}

@test "_parse_entry: dependsOn with a null element is rejected" {
  # A null element (`dependsOn: [~]` / a bare `-` list item) is NOT a name: it
  # would coerce to the literal string "null" and later fail as a confusing
  # "unknown entry 'null'". Reject it at the element-type validation instead.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"dependsOn":[null]}}' n d i e w x
  assert_failure
  assert_output --partial "null element"
}

# --- name: override (bootstrap::_parse_entry) --------------------------------
# `name:` is a reserved key (like values/env/wait/dependsOn): it OVERRIDES this
# entry's identifier (basename for paths / map-key for chart entries) for the
# dependsOn registry — but NOT addon_dir. The 9th out-param reports whether it was
# set explicitly. A non-empty [A-Za-z0-9._-]+ scalar is required.

@test "_parse_entry: name: overrides the resolved identity but not addon_dir" {
  local p_name p_dir p_inline p_env p_wait p_deps p_explicit
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"./x":{"name":"bar"}}' \
    p_name p_dir p_inline p_env p_wait p_deps p_explicit
  # name: REPLACES the basename ("x") as the identity …
  [ "${p_name}" = "bar" ]
  # … but addon_dir still comes from the path (UNCHANGED by name:).
  [ "${p_dir}" = "${PATH_CLUSTERS}/test.lok8s.dev/./x" ]
  [ "${p_explicit}" = "true" ]
}

@test "_parse_entry: name: alone is the new schema (no legacy helm-values leak)" {
  local p_name p_dir p_inline p_env p_wait p_deps p_explicit
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":"renamed"}}' \
    p_name p_dir p_inline p_env p_wait p_deps p_explicit
  [ "${p_name}" = "renamed" ]
  [ -z "${p_inline}" ]
  [ "${p_explicit}" = "true" ]
}

@test "_parse_entry: name: combines with dependsOn (the addon-vs-target case)" {
  local p_name p_dir p_inline p_env p_wait p_deps p_explicit
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"./targets/rook-ceph":{"name":"rook-ceph-cluster","dependsOn":["rook-ceph"]}}' \
    p_name p_dir p_inline p_env p_wait p_deps p_explicit
  [ "${p_name}" = "rook-ceph-cluster" ]
  [ "${p_dir}" = "${PATH_CLUSTERS}/test.lok8s.dev/./targets/rook-ceph" ]
  grep -qx 'rook-ceph' <<<"${p_deps}"
  [ "${p_explicit}" = "true" ]
}

@test "_parse_entry: a bare entry reports explicit=false (no name:)" {
  local p_name p_dir p_inline p_env p_wait p_deps p_explicit
  bootstrap::_parse_entry "test.lok8s.dev" '"cilium"' \
    p_name p_dir p_inline p_env p_wait p_deps p_explicit
  [ "${p_name}" = "cilium" ]
  [ "${p_explicit}" = "false" ]
}

@test "_parse_entry: empty name: is rejected" {
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":""}}' n d i e w x
  assert_failure
  assert_output --partial "non-empty string"
}

@test "_parse_entry: name: with an illegal character is rejected" {
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":"bad/name"}}' n d i e w x
  assert_failure
  assert_output --partial "not a valid entry name"
}

@test "_parse_entry: non-scalar name: is rejected" {
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":{"k":"v"}}}' n d i e w x
  assert_failure
  assert_output --partial "non-empty scalar"
}

@test "_parse_entry: null name: is rejected" {
  # `name: null` / `name: ~` — a null scalar is NOT a name; rejected by the same
  # non-empty-scalar validation as a map/seq (got !!null), before the empty-string
  # check. Same family as the empty/bad-charset name: tests.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":null}}' n d i e w x
  assert_failure
  assert_output --partial "non-empty scalar"
}

@test "_parse_entry: seq name: is rejected" {
  # `name: [a, b]` — a sequence is not a scalar identifier; rejected by the same
  # non-empty-scalar validation (got !!seq) as the map case above.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":["a","b"]}}' n d i e w x
  assert_failure
  assert_output --partial "non-empty scalar"
}

@test "_parse_entry: unquoted bool name: is rejected (must be a string)" {
  # `name: true` (an unquoted YAML bool, tag !!bool) coerces to "true" and would
  # slip past the [A-Za-z0-9._-]+ charset check — almost certainly a mistake. The
  # name: identifier must be a STRING scalar, so reject the bare bool.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":true}}' n d i e w x
  assert_failure
  assert_output --partial "non-empty scalar"
}

@test "_parse_entry: unquoted int name: is rejected (must be a string)" {
  # `name: 123` (an unquoted YAML int, tag !!int) coerces to "123" and would pass
  # the charset check — reject it the same way (require a string scalar).
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":123}}' n d i e w x
  assert_failure
  assert_output --partial "non-empty scalar"
}

@test "_parse_entry: a QUOTED string name: that looks like a bool/int is accepted" {
  # The flip side of the two above: name: "true" / "123" are !!str — a deliberate
  # string identity, NOT the unquoted-bool/int mistake — so they pass the charset
  # check and are used verbatim as the entry identifier.
  local p_name p_dir p_inline p_env p_wait p_deps p_explicit
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"name":"true"}}' \
    p_name p_dir p_inline p_env p_wait p_deps p_explicit
  [ "${p_name}" = "true" ]
  [ "${p_explicit}" = "true" ]
}

# --- dependsOn: graph validation (bootstrap::apply) --------------------------

@test "bootstrap::apply errors on dependsOn to an unknown entry" {
  bootstrap::_apply_one() { return 0; }
  local n; for n in a b; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
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
    - b:
        dependsOn: [nope]
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "unknown entry 'nope'"
}

@test "bootstrap::apply errors on a dependsOn cycle (A→B→A)" {
  bootstrap::_apply_one() { return 0; }
  local n; for n in a b; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - a:
        dependsOn: [b]
    - b:
        dependsOn: [a]
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "cycle detected"
}

# --- dependsOn: DAG scheduling (bootstrap::apply) ----------------------------

@test "bootstrap::apply: dependsOn gates the dependent; independents stay parallel" {
  # A (dep-target), B dependsOn:[a], C (independent). B must START only after A
  # ENDS; C must run CONCURRENTLY with A (not gated behind it). Same timestamped
  # START/END + sleep technique as the wait:true barrier test.
  # Pin parallelism so the a/c overlap assertion doesn't depend on the ambient env.
  export LOK8S_BOOTSTRAP_PARALLEL=8
  local log="${BATS_TEST_TMPDIR}/dag.log"
  : > "${log}"
  bootstrap::_apply_one() {
    local name="$1"
    printf 'START %s\n' "${name}" >> "${log}"
    sleep 0.4
    printf 'END %s\n' "${name}" >> "${log}"
    return 0
  }
  local n; for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
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
    - b:
        dependsOn: [a]
    - c
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  ln() { grep -n -- "^$1\$" "${log}" | head -1 | cut -d: -f1; }
  local sa ea_ sb sc ec_
  sa=$(ln "START a"); ea_=$(ln "END a")
  sb=$(ln "START b"); sc=$(ln "START c"); ec_=$(ln "END c")

  # a and c overlap (c is NOT gated behind a): each STARTED before the other ENDED.
  [ "${sa}" -lt "${ec_}" ]
  [ "${sc}" -lt "${ea_}" ]

  # b (dependsOn a) STARTS only after a has ENDED.
  [ "${sb}" -gt "${ea_}" ]
}

@test "bootstrap::apply: wait-gate + dependsOn — both wait for the gate, X also waits for Y" {
  # A wait-gate G first; then Y and X (dependsOn:[y]) after it. Both Y and X must
  # wait for G (the gate's drain-all/ready-before-later semantics); X must also
  # wait for Y (its explicit edge).
  local log="${BATS_TEST_TMPDIR}/gatedag.log"
  : > "${log}"
  bootstrap::_apply_one() {
    local name="$1"
    printf 'START %s\n' "${name}" >> "${log}"
    sleep 0.3
    printf 'END %s\n' "${name}" >> "${log}"
    return 0
  }
  local n; for n in g y x; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - g:
        wait: true
    - y
    - x:
        dependsOn: [y]
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  ln() { grep -n -- "^$1\$" "${log}" | head -1 | cut -d: -f1; }
  local eg sy sx ey
  eg=$(ln "END g"); sy=$(ln "START y"); sx=$(ln "START x"); ey=$(ln "END y")

  # The gate G gates everything after it: both Y and X start only after G ends.
  [ "${sy}" -gt "${eg}" ]
  [ "${sx}" -gt "${eg}" ]
  # X (dependsOn y) starts only after Y ends.
  [ "${sx}" -gt "${ey}" ]
}

@test "bootstrap::apply: only dep-targets do the readiness wait; pure leaves skip it" {
  # _apply_one receives do_wait (arg 7) — non-empty ⇔ it runs kapply::wait_ready.
  # The scheduler must set it for a dep-target and leave it empty for a pure leaf.
  # a is a dep-target (b dependsOn:[a]); b and c are leaves (nothing depends on
  # them). Record the names called WITH the wait flag set.
  local waitlog="${BATS_TEST_TMPDIR}/wait.log"
  : > "${waitlog}"
  bootstrap::_apply_one() {
    local name="$1" do_wait="$7"
    [[ -n "${do_wait}" ]] && printf '%s\n' "${name}" >> "${waitlog}"
    return 0
  }
  local n; for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
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
    - b:
        dependsOn: [a]
    - c
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  # a (dep-target) DID the readiness wait.
  grep -qx 'a' "${waitlog}"
  # the pure leaves b and c did NOT.
  ! grep -qx 'b' "${waitlog}"
  ! grep -qx 'c' "${waitlog}"
}

@test "bootstrap::apply: a failed entry's dependents are skipped; independents still run" {
  # A fails; B dependsOn:[a] must be SKIPPED (you never start work behind a broken
  # dependency); C is independent and STILL runs. apply drains the batch, returns
  # non-zero, leaves no orphan job, and removes its rc temp dir. The test itself
  # completing at all proves the DAG still terminates (no hang) on the failure path.
  # Pin parallelism so A and the independent C launch in the SAME wave (the runner
  # stops launching after A's failure surfaces); else C could be starved at =1.
  export LOK8S_BOOTSTRAP_PARALLEL=8
  local ran_log="${BATS_TEST_TMPDIR}/ran.log"
  : > "${ran_log}"
  bootstrap::_apply_one() {
    local name="$1"
    sleep 0.1
    printf '%s\n' "${name}" >> "${ran_log}"
    [ "${name}" = "a" ] && return 1   # A fails
    return 0
  }
  # Capture the scheduler's rc temp dir (the only `mktemp -d` reached, since
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
  local n; for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
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
    - b:
        dependsOn: [a]
    - c
YAML
  # Call directly (NOT via `run`, which forks a subshell) so the scheduler's
  # background jobs land in THIS shell's job table — lets us prove no orphan
  # survives. `|| rc=$?` keeps the non-zero from tripping bats' errexit.
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" || rc=$?

  # apply reports failure (A failed).
  [ "${rc}" -ne 0 ]

  # A ran and failed; C (independent of A) still ran.
  grep -qx 'a' "${ran_log}"
  grep -qx 'c' "${ran_log}"

  # B (dependsOn the failed A) was NEVER applied — a failed entry's dependents are
  # skipped (the runner stops launching once a failure surfaces, before B's now-
  # "completed" dep could make it launchable).
  ! grep -qx 'b' "${ran_log}"

  # No surviving background jobs (no orphan left behind on the failure path).
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]

  # The scheduler's rc temp dir was removed.
  local rcdir; rcdir="$(cat "${rcdir_marker}")"
  [ -n "${rcdir}" ]
  [ ! -d "${rcdir}" ]
}

# --- name: override + ambiguity (bootstrap::apply) ---------------------------

@test "bootstrap::apply: name: override is the dependsOn reference target" {
  # foo (basename) + ./x renamed to bar; c dependsOn:[bar] must wait for the ./x
  # entry — resolved by the OVERRIDE name, not ./x's basename. foo stays parallel.
  # Pin parallelism so the foo/bar overlap assertion doesn't depend on the ambient env.
  export LOK8S_BOOTSTRAP_PARALLEL=8
  local log="${BATS_TEST_TMPDIR}/name.log"
  : > "${log}"
  bootstrap::_apply_one() {
    local name="$1"
    printf 'START %s\n' "${name}" >> "${log}"
    sleep 0.4
    printf 'END %s\n' "${name}" >> "${log}"
    return 0
  }
  mkdir -p "${PATH_LOK8S}/addons/foo" "${PATH_LOK8S}/addons/c" \
           "${PATH_CLUSTERS}/test.lok8s.dev/x"
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - foo
    - ./x:
        name: bar
    - c:
        dependsOn: [bar]
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  ln() { grep -n -- "^$1\$" "${log}" | head -1 | cut -d: -f1; }
  local sbar ebar sc_ sfoo efoo
  sbar=$(ln "START bar"); ebar=$(ln "END bar")
  sc_=$(ln "START c"); sfoo=$(ln "START foo"); efoo=$(ln "END foo")

  # The ./x entry is keyed by its OVERRIDE name "bar" (its basename would be "x").
  [ -n "${sbar}" ]
  # c (dependsOn the renamed ./x = "bar") starts only AFTER bar ENDS.
  [ "${sc_}" -gt "${ebar}" ]
  # foo is independent — it overlaps bar (each started before the other ended).
  [ "${sfoo}" -lt "${ebar}" ]
  [ "${sbar}" -lt "${efoo}" ]
}

@test "bootstrap::apply: name: replaces the basename (old basename no longer resolves)" {
  # ./x renamed to bar; a dependsOn on the OLD basename 'x' is now unknown —
  # proving name: REPLACES (not augments) the resolved identity.
  bootstrap::_apply_one() { return 0; }
  mkdir -p "${PATH_LOK8S}/addons/foo" "${PATH_LOK8S}/addons/c" \
           "${PATH_CLUSTERS}/test.lok8s.dev/x"
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - foo
    - ./x:
        name: bar
    - c:
        dependsOn: [x]
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "unknown entry 'x'"
}

@test "bootstrap::apply: dependsOn to an ambiguous (collided) name is an error" {
  # The rook-ceph addon AND ./targets/rook-ceph both resolve to 'rook-ceph'; a
  # dependsOn referencing it can't pick one → hard error (set an explicit name:).
  bootstrap::_apply_one() { return 0; }
  mkdir -p "${PATH_LOK8S}/addons/rook-ceph" "${PATH_LOK8S}/addons/consumer" \
           "${PATH_CLUSTERS}/test.lok8s.dev/targets/rook-ceph"
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - rook-ceph
    - ./targets/rook-ceph
    - consumer:
        dependsOn: [rook-ceph]
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "ambiguous entry 'rook-ceph'"
}

@test "bootstrap::apply: a basename collision with no dependsOn still applies (warn only)" {
  # The SAME rook-ceph addon + ./targets/rook-ceph collision, but NOTHING
  # dependsOn it — the current barrier-only kubehz config — MUST still apply
  # (warn, not error). This is the compatibility guarantee.
  bootstrap::_apply_one() { return 0; }
  mkdir -p "${PATH_LOK8S}/addons/rook-ceph" \
           "${PATH_CLUSTERS}/test.lok8s.dev/targets/rook-ceph"
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - rook-ceph
    - ./targets/rook-ceph
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
}

@test "bootstrap::apply: two entries with the same explicit name: is an error" {
  bootstrap::_apply_one() { return 0; }
  mkdir -p "${PATH_LOK8S}/addons/a" "${PATH_LOK8S}/addons/b"
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - a:
        name: dup
    - b:
        name: dup
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "name: must be unique"
}

@test "bootstrap::apply: an explicit name: colliding with a resolved name is an error" {
  # name: dup on b collides with the bare 'dup' entry's resolved name → hard error
  # (an explicit name must be unique, whether the other side is explicit or not).
  bootstrap::_apply_one() { return 0; }
  mkdir -p "${PATH_LOK8S}/addons/dup" "${PATH_LOK8S}/addons/b"
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - dup
    - b:
        name: dup
YAML
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "name: must be unique"
}

@test "bootstrap::apply: a failed wait-gate skips later entries; no orphan, temp dir cleaned" {
  # Distinct from the failing-*dependency* test above: this pins the FOREGROUND
  # GATE failure path. G is a wait:true gate; X is positioned AFTER it, so the
  # gate's drain-all/ready-before-later edges make X depend on G. The gate runs
  # synchronously in the foreground and FAILS — the launch guard
  # `(( _BS_OVERALL_RC == 0 ))` (and the inner `|| break`) then stops new launches,
  # so X is NEVER applied. apply drains, returns non-zero, leaves no orphan job,
  # and removes its rc temp dir.
  local ran_log="${BATS_TEST_TMPDIR}/ran.log"
  : > "${ran_log}"
  bootstrap::_apply_one() {
    local name="$1"
    printf '%s\n' "${name}" >> "${ran_log}"
    [ "${name}" = "g" ] && return 1   # the gate fails
    return 0
  }
  # Capture the scheduler's rc temp dir (the only `mktemp -d` reached, since
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
  local n; for n in g x; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - g:
        wait: true
    - x
YAML
  # Call directly (NOT via `run`, which forks a subshell) so the scheduler's
  # background jobs land in THIS shell's job table — lets us prove no orphan
  # survives. `|| rc=$?` keeps the non-zero from tripping bats' errexit.
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" || rc=$?

  # apply reports failure (the gate G failed).
  [ "${rc}" -ne 0 ]

  # G ran and failed.
  grep -qx 'g' "${ran_log}"

  # X (behind the failed gate) was NEVER applied — entries positioned after a
  # failed foreground gate are skipped by the launch guard.
  ! grep -qx 'x' "${ran_log}"

  # No surviving background jobs (no orphan left behind on the gate-failure path).
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]

  # The scheduler's rc temp dir was removed.
  local rcdir; rcdir="$(cat "${rcdir_marker}")"
  [ -n "${rcdir}" ]
  [ ! -d "${rcdir}" ]
}

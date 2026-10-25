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

  # `envsubst --version` (template::envsubst_flavor's probe) must not drain
  # the manifest the render pipes through.
  envsubst() { [[ "${1:-}" != --version ]] || return 0; cat; }
  export -f envsubst

  kubectl() { return 0; }
  export -f kubectl

  # The dry run's boundary links the project's plugins (none here) and runs
  # the Secret plugin's capability probe only when there is one: never let an
  # inherited plugin home (CI exports one) leak into these tests.
  export KUSTOMIZE_PLUGIN_HOME="${BATS_TEST_TMPDIR}/.kustomize"

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

@test "bootstrap::apply passes env: overrides to the render only, never the orchestration shell" {
  # The entry's `env:` reaches the render's processes (kustomize, envsubst:
  # render's envsubst whitelist picks up LOK8S_USER_*/LOK8S_SPEC_* names) and
  # nothing else: the apply in the orchestration shell never sees it (the Go
  # twin passes it to the render process only).
  export KCAP="${BATS_TEST_TMPDIR}/kustomize_env" ACAP="${BATS_TEST_TMPDIR}/kubectl_env"
  : > "${KCAP}"; : > "${ACAP}"
  kustomize() {
    printf '%s\n' "${LOK8S_USER_TESTVAR:-UNSET}" > "${KCAP}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'
  }
  kubectl() {
    printf '%s\n' "${LOK8S_USER_TESTVAR:-UNSET}" >> "${ACAP}"
    [[ " $* " != *" -f - "* ]] || cat >/dev/null
    return 0
  }
  export -f kustomize kubectl

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

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  [ "$(cat "${KCAP}")" = "hello" ]
  [ -s "${ACAP}" ]
  ! grep -qv '^UNSET$' "${ACAP}"

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

@test "bootstrap::apply: a failed LEAF skips nothing; all independents still apply (rc=1)" {
  # THE live-run regression pin: a failed LEAF (nothing depends on it) must NOT
  # halt the bootstrap — every unrelated entry still applies. a/b/c are independent
  # leaves; b fails. All three still run, apply reports non-zero (b failed), NO entry
  # is skipped (a leaf failure invalidates no dependents), no orphan job is left, and
  # the scheduler's rc temp dir is removed. (Before the fix, b's failure stopped the
  # world and starved unrelated consumers — networking/cnpg/harbor in the live run.)
  # Pin parallelism so all three launch in one wave regardless of the ambient
  # LOK8S_BOOTSTRAP_PARALLEL.
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
  # survives. Redirect stderr so we can assert NOTHING was skipped. `|| rc=$?`
  # keeps a non-zero from tripping bats' errexit.
  local err_log="${BATS_TEST_TMPDIR}/err.log"
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" \
    2>"${err_log}" || rc=$?
  [ "${rc}" -ne 0 ]

  # All three ran (a leaf failure skips nothing) — no unrelated entry starved.
  run wc -l < "${done_log}"
  assert_output "3"

  # NOTHING was skipped — a failed leaf has no dependents to invalidate.
  ! grep -q 'skipping' "${err_log}"

  # No surviving background jobs. `jobs -p` must run in the current shell (not a
  # `$()`/`run` subshell, which has its own empty job table) — redirect to a file.
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]

  # The scheduler's rc temp dir was removed.
  local rcdir; rcdir="$(cat "${rcdir_marker}")"
  [ -n "${rcdir}" ]
  [ ! -d "${rcdir}" ]
}

@test "bootstrap::apply PARALLEL=1: an independent entry behind a failure still runs (later wave)" {
  # THE anti-starvation pin. Every other failure test launches the independent
  # sibling in the SAME wave as the failing entry (LOK8S_BOOTSTRAP_PARALLEL=8), so
  # the OLD stop-the-world code — which `break`s the whole run on the first failure
  # — would have passed them too: the sibling was already in flight before the
  # failure surfaced. That proves nothing about work that hasn't launched yet.
  #
  # Here cap=1 forces strictly one entry per wave: `a` (fails) launches and is
  # reaped ALONE, so its failure surfaces BEFORE `b` (independent, no deps, and
  # positioned AFTER `a`) ever gets a slot. With the fix, `b` STILL launches in the
  # next wave (an unrelated leaf is never starved by a prior failure). Under the old
  # stop-the-world behavior it would be starved — a real regression this pins.
  export LOK8S_BOOTSTRAP_PARALLEL=1
  local done_log="${BATS_TEST_TMPDIR}/done.log"
  : > "${done_log}"
  bootstrap::_apply_one() {
    local name="$1"
    sleep 0.1
    printf '%s\n' "${name}" >> "${done_log}"
    [ "${name}" = "a" ] && return 1   # the FIRST entry fails
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
  for n in a b; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
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
YAML
  # Call directly (NOT via `run`, which forks a subshell) so the scheduler's
  # background jobs land in THIS shell's job table — lets us prove no orphan
  # survives. Redirect stderr so we can assert NOTHING was skipped. `|| rc=$?`
  # keeps a non-zero from tripping bats' errexit.
  local err_log="${BATS_TEST_TMPDIR}/err.log"
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" \
    2>"${err_log}" || rc=$?

  # apply reports failure (a failed).
  [ "${rc}" -ne 0 ]

  # a ran and failed; b — starved behind a's failure at cap=1 — STILL applied in a
  # LATER wave. This exact assertion FAILS under the old stop-the-world `break`.
  grep -qx 'a' "${done_log}"
  grep -qx 'b' "${done_log}"

  # NOTHING was skipped — a failed leaf has no dependents to invalidate.
  ! grep -q 'skipping' "${err_log}"

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

# --- immutable/terminating conflict: park + batched heal ---------------------

@test "bootstrap: a background immutable conflict PARKS, then fails fast non-interactively (no --force)" {
  # A background kapply runs non-interactive, so on an immutable/terminating conflict
  # it cannot prompt — it errors. _reap_one must PARK the entry (not fail it inline),
  # and at the drain _resolve_parked must fail-fast (no tty, no --force) with a
  # --force hint + skip its dependents. Stub _apply_one to emit kubectl's immutable
  # marker + fail for the gate entry 'b'.
  bootstrap::_apply_one() {
    local name="$1"
    if [ "${name}" = "b" ]; then
      echo "The Deployment \"b\" is invalid: spec.selector: field is immutable" >&2
      return 1
    fi
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
        wait: true
    - c
YAML
  # Call directly (background jobs → this shell); `|| rc=$?` keeps the non-zero from
  # tripping bats' errexit. Combined output → a file we grep.
  local out="${BATS_TEST_TMPDIR}/park.out" rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2>&1 || rc=$?

  # Non-interactive + no --force → the parked heal fails the run.
  [ "${rc}" -ne 0 ]
  # It PARKED (deferred), not plain-failed: the reap park marker appears ...
  grep -q 'will confirm at the end' "${out}"
  # ... the underlying kubectl conflict detail is surfaced (not silently dropped on
  # park) so the user can resolve by hand ...
  grep -q 'field is immutable' "${out}"
  # ... and the batched drain resolve gave the actionable --force hint.
  grep -q 'needs recreate' "${out}"
  grep -q -- '--force' "${out}"
  # 'c' waits on the gate 'b'; a failed heal skips it (dependents BFS).
  grep -q 'skipping' "${out}"
}

@test "bootstrap::_resolve_parked: --force auto-recreates a parked entry (no prompt)" {
  # Directly exercise the ACCEPT branch: with LOK8S_FORCE_RECREATE set, _resolve_parked
  # re-applies each parked entry FOREGROUND with force (kapply deletes+recreates) and
  # marks it completed — no tty prompt, dependents NOT skipped. Relies on dynamic
  # scoping for the launch locals, so we set them in a wrapper that calls it.
  local applied="${BATS_TEST_TMPDIR}/reapplied.log"; : > "${applied}"
  bootstrap::_apply_one() { echo "$1" >> "${applied}"; return 0; }
  bootstrap::_skip_dependents() { echo "skip:$1"; }   # must NOT run on a clean heal

  _run_resolve() {
    local -a _names=(addonA) _dirs=(/x) _inlines=("") _envs=("") _waits=("false")
    local -A _istarget=()
    local kind=lo provider_name=hetzner kubeconfig=/kc
    declare -gA _BS_COMPLETED=(); _BS_DONE=0; _BS_OVERALL_RC=0; _BS_PARKED=(0)
    export LOK8S_FORCE_RECREATE=1
    bootstrap::_resolve_parked
    echo "rc=${_BS_OVERALL_RC} done=${_BS_DONE} done0=${_BS_COMPLETED[0]:-}"
  }
  run _run_resolve
  assert_success
  assert_output --partial "recreated"              # "✓ addonA · recreated"
  assert_output --partial "rc=0 done=1 done0=1"     # completed cleanly, no failure
  refute_output --partial "skip:"                   # dependents NOT skipped on success
  # the re-apply ran exactly once for the parked entry
  run cat "${applied}"
  assert_output "addonA"
}

# --- the batched prompt must name what a `y` destroys -----------------------
#
# Accepting at this prompt re-applies every parked entry under
# LOK8S_FORCE_RECREATE=1, which makes kapply::_confirm_ns_finalize return 0
# WITHOUT asking. So the pointed per-namespace confirm never runs, and whatever
# this prompt does not say is not said at all.
#
# On 2026-07-30 it said only "restarts pods". One `y` force-finalized
# kubehz-system, mla and element on the dev cluster — CNPG volumes, the
# runtime-minted PATs and the minio buckets went with them.

# Drive _resolve_parked through the real dynamic-scope contract, capturing the
# prompt instead of writing it to a tty bats does not have. Answers "n": these
# tests are about the TEXT, and declining keeps the heal out of the picture.
_run_prompt() {   # <prompt-capture-file> [stuck-ns...]
  local capture="${1}"; shift
  bootstrap::_interactive() { return 0; }
  bootstrap::_ask() { printf '%s' "${1}" > "${capture}"; return 1; }
  bootstrap::_skip_dependents() { :; }
  bootstrap::_apply_one() { :; }
  local -a _names=(addonA) _dirs=(/x) _inlines=("") _envs=("") _waits=("false")
  local -A _istarget=()
  local kind=lo provider_name=hetzner kubeconfig=/kc
  declare -gA _BS_COMPLETED=(); _BS_DONE=0; _BS_OVERALL_RC=0
  _BS_PARKED=(0); _BS_PARKED_NS=("${@}")
  unset LOK8S_FORCE_RECREATE
  bootstrap::_resolve_parked
}

@test "bootstrap::_resolve_parked: the prompt NAMES every namespace a 'y' force-finalizes" {
  local p="${BATS_TEST_TMPDIR}/prompt.txt"
  _run_prompt "${p}" kubehz-system mla element

  run cat "${p}"
  # Every namespace named — an operator cannot consent to destroying something
  # the prompt never mentioned.
  assert_output --partial "kubehz-system"
  assert_output --partial "mla"
  assert_output --partial "element"
  # …and named as DESTRUCTION, not as a restart. These three phrases are the
  # difference between the wording that lost three namespaces and one that
  # would not have.
  assert_output --partial "FORCE-FINALIZED"
  assert_output --partial "COMPLETES their deletion"
  assert_output --partial "DESTROYED IRREVERSIBLY"
  # The pre-existing sealed-Secret warning must survive alongside it.
  assert_output --partial "RE-KEYED"
  assert_output --partial "[y/N]"
}

@test "bootstrap::_resolve_parked: no stuck namespace → no namespace warning" {
  # A warning printed on every heal is one nobody reads by the third time. With
  # nothing to force-finalize the destructive block must be absent — that is
  # what keeps it meaningful when it does appear.
  local p="${BATS_TEST_TMPDIR}/prompt.txt"
  _run_prompt "${p}"

  run cat "${p}"
  refute_output --partial "FORCE-FINALIZED"
  refute_output --partial "DESTROYED IRREVERSIBLY"
  # The prompt is otherwise intact.
  assert_output --partial "need recreate to reconcile"
  assert_output --partial "RE-KEYED"
  assert_output --partial "[y/N]"
}

@test "bootstrap: a terminating-namespace 403 reaches the prompt from the real apply output" {
  # The wiring test. The two above would pass even if _BS_PARKED_NS were never
  # populated from anything real, so this one starts where the incident did: a
  # background apply failing with the apiserver's actual 403 text, and asserts
  # the namespace named in THAT text is the one the prompt warns about.
  local p="${BATS_TEST_TMPDIR}/prompt.txt"
  bootstrap::_apply_one() {
    [ "${1}" = "b" ] || return 0
    echo 'Error from server (Forbidden): error when creating "x": secrets "s" is' \
         'forbidden: unable to create new content in namespace kubehz-system' \
         'because it is being terminated' >&2
    return 1
  }
  bootstrap::_interactive() { return 0; }
  bootstrap::_ask() { printf '%s' "${1}" > "${p}"; return 1; }
  unset LOK8S_NONINTERACTIVE LOK8S_FORCE_RECREATE

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
        wait: true
    - c
YAML
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" >/dev/null 2>&1 || rc=$?

  [ -s "${p}" ]   # the prompt was actually composed and offered
  run cat "${p}"
  assert_output --partial "kubehz-system"
  assert_output --partial "FORCE-FINALIZED"
}

@test "bootstrap: --force (LOK8S_FORCE_RECREATE) heals inline — the entry never parks" {
  # With --force set, the background kapply auto-recreates in-job, so the entry never
  # reaches the reap park branch. Model that: the stub succeeds when force is set,
  # else emits the conflict marker + fails. Expect a clean run, no park, no prompt —
  # this also proves the force env actually reaches the backgrounded apply.
  bootstrap::_apply_one() {
    local name="$1"
    if [ "${name}" = "b" ] && [ -z "${LOK8S_FORCE_RECREATE:-}" ]; then
      echo "The Deployment \"b\" is invalid: spec.selector: field is immutable" >&2
      return 1
    fi
    return 0
  }
  export LOK8S_FORCE_RECREATE=1
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
        wait: true
    - c
YAML
  local out="${BATS_TEST_TMPDIR}/force.out" rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2>&1 || rc=$?
  # Force → healed inline → clean run, NO park marker, NO batch prompt.
  [ "${rc}" -eq 0 ]
  ! grep -q 'will confirm at the end' "${out}"
  ! grep -q 'needs recreate' "${out}"
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

# --- valueFiles: helm values files (bootstrap::_parse_entry + apply) ---------
# valueFiles is a reserved key (like values/env/wait/dependsOn/name): a !!seq of
# helm values FILE paths, each resolved relative to the CLUSTER DIR (the same
# base ./targets/... entries resolve from; absolute paths pass through) and
# pre-merged — in list order, inline `values:` on top — into the inline-values
# string handed to addons::render, so the effective stack is:
#   base < driver < provider < valueFiles (in order) < values:

# Write a values file under the cluster dir. Usage: write_value_file <relpath> <<yaml
write_value_file() {
  local f="${PATH_CLUSTERS}/test.lok8s.dev/$1"
  mkdir -p "$(dirname "${f}")"
  cat > "${f}"
}

@test "_parse_entry: valueFiles alone is the new schema (file contents become the inline values)" {
  write_value_file "values/extra.yaml" <<'YAML'
marker: "filevalue"
nested:
  from_file: true
YAML
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":["./values/extra.yaml"]}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  [ "${p_name}" = "testcni" ]
  # The file's values ride the inline channel …
  [ "$(yq -r '.marker' <<<"${p_inline}")" = "filevalue" ]
  [ "$(yq -r '.nested.from_file' <<<"${p_inline}")" = "true" ]
  # … and valueFiles is a RESERVED key, not a legacy whole-map helm value.
  [ "$(yq -r 'has("valueFiles")' <<<"${p_inline}")" = "false" ]
  [ -z "${p_env}" ]
  [ "${p_wait}" = "false" ]
}

@test "_parse_entry: two valueFiles merge in list order (later wins, deep-merge preserved)" {
  write_value_file "vf/a.yaml" <<'YAML'
shared: "first"
nested:
  overridden: "first"
  from_first: true
YAML
  write_value_file "vf/b.yaml" <<'YAML'
shared: "second"
nested:
  overridden: "second"
  from_second: true
YAML
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":["./vf/a.yaml","./vf/b.yaml"]}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  # Later file wins on the conflicting keys …
  [ "$(yq -r '.shared' <<<"${p_inline}")" = "second" ]
  [ "$(yq -r '.nested.overridden' <<<"${p_inline}")" = "second" ]
  # … while each file's unique nested keys survive (deep merge, not replace).
  [ "$(yq -r '.nested.from_first' <<<"${p_inline}")" = "true" ]
  [ "$(yq -r '.nested.from_second' <<<"${p_inline}")" = "true" ]
}

@test "_parse_entry: inline values: wins over valueFiles (files under, map on top)" {
  write_value_file "vf/base.yaml" <<'YAML'
shared: "file"
nested:
  overridden: "file"
  from_file: true
YAML
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":["./vf/base.yaml"],"values":{"shared":"inline","nested":{"overridden":"inline"}}}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  # The inline values: map is the most explicit layer — it wins the conflicts …
  [ "$(yq -r '.shared' <<<"${p_inline}")" = "inline" ]
  [ "$(yq -r '.nested.overridden' <<<"${p_inline}")" = "inline" ]
  # … but the file's non-conflicting key deep-merges through.
  [ "$(yq -r '.nested.from_file' <<<"${p_inline}")" = "true" ]
}

@test "_parse_entry: an absolute valueFiles path passes through unchanged" {
  local abs="${BATS_TEST_TMPDIR}/abs-values.yaml"
  printf 'marker: "absolute"\n' > "${abs}"
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    "{\"testcni\":{\"valueFiles\":[\"${abs}\"]}}" \
    p_name p_dir p_inline p_env p_wait p_deps
  [ "$(yq -r '.marker' <<<"${p_inline}")" = "absolute" ]
}

@test "_parse_entry: valueFiles with a missing file is a hard error naming the resolved path" {
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":["./vf/does-not-exist.yaml"]}}' n d i e w x
  assert_failure
  assert_output --partial "valueFiles: file not found"
  # The error names the RESOLVED path (cluster-dir base), not just the raw ref.
  assert_output --partial "${PATH_CLUSTERS}/test.lok8s.dev/./vf/does-not-exist.yaml"
}

@test "_parse_entry: valueFiles that is not a list is rejected" {
  # `valueFiles: ./x.yaml` (a bare scalar) is a config mistake — the container
  # must be a sequence of file paths.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":"./x.yaml"}}' n d i e w x
  assert_failure
  assert_output --partial "valueFiles: must be a list of file paths (got str)"

  # A map container is equally wrong.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":{"f":"./x.yaml"}}}' n d i e w x
  assert_failure
  assert_output --partial "valueFiles: must be a list of file paths (got map)"
}

@test "_parse_entry: valueFiles with a non-string element is rejected" {
  # A null element (`[~]` / a bare `-`) is not a path …
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":[null]}}' n d i e w x
  assert_failure
  assert_output --partial "each element must be a file path string (got null)"

  # … and neither is a nested map.
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":[{"f":"./x.yaml"}]}}' n d i e w x
  assert_failure
  assert_output --partial "each element must be a file path string (got map)"
}

@test "_parse_entry: an empty valueFiles list is a harmless no-op (like an empty values:)" {
  # `valueFiles: []` is vacuous config, not an error — nothing merges, inline
  # values (when present) pass through untouched. Pinned so the behavior is a
  # decision, not an accident (review round 2).
  local p_name p_dir p_inline p_env p_wait p_deps
  bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":[],"values":{"kept":"yes"}}}' \
    p_name p_dir p_inline p_env p_wait p_deps
  [ "$(yq -r '.kept' <<<"${p_inline}")" = "yes" ]
}

@test "_parse_entry: an empty-string valueFiles element is a hard error (not silently skipped)" {
  # "" is !!str so it passes the element-tag check — but it is not a path.
  # Review round 1 (Copilot): the resolve loop used to `continue` past it,
  # contradicting the fail-fast contract ("never render with half the values").
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"testcni":{"valueFiles":[""]}}' n d i e w x
  assert_failure
  assert_output --partial "valueFiles: empty element"
}

@test "_parse_entry: 'valueFiles:' on a non-chart (kustomize) target is an error" {
  # Same helm-only rule as values: — a ./targets/ dir with only a
  # kustomization.yaml has no chart to feed the values to.
  local raw_dir="${PATH_CLUSTERS}/test.lok8s.dev/targets/rawvf"
  mkdir -p "${raw_dir}"
  cat > "${raw_dir}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
YAML
  write_value_file "vf/x.yaml" <<'YAML'
marker: x
YAML
  run bootstrap::_parse_entry "test.lok8s.dev" \
    '{"./targets/rawvf":{"valueFiles":["./vf/x.yaml"]}}' n d i e w x
  assert_failure
  assert_output --partial "not a chart addon"
}

@test "bootstrap::apply: valueFiles layer beats provider, loses to inline values: (end-to-end stack)" {
  # The full precedence chain through the real render staging:
  #   base < driver < provider < valueFiles < values:
  # The value file overrides shared_all/nested.overridden (beats provider); the
  # inline values: map overrides them again (beats the file); every layer's
  # unique key survives the deep merge.
  write_value_file "values/testcni.dev.yaml" <<'YAML'
shared_all: "valuefile"
only_valuefile: "valuefile"
nested:
  overridden: "valuefile"
  from_valuefile: true
YAML
  write_cluster_spec "testcni: {valueFiles: [./values/testcni.dev.yaml], values: {shared_all: inline, nested: {overridden: inline}}}"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # Inline values: is the top layer — it wins over the value file.
  [ "$(yq -r '.shared_all' "${merged}")" = "inline" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "inline" ]

  # The value file's unique keys made it through (it beat provider on the
  # conflicts before inline re-overrode them, and deep merge kept the rest).
  [ "$(yq -r '.only_valuefile' "${merged}")" = "valuefile" ]
  [ "$(yq -r '.nested.from_valuefile' "${merged}")" = "true" ]

  # Lower layers still contribute their unique keys.
  [ "$(yq -r '.only_base' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider' "${merged}")" = "provider" ]
  [ "$(yq -r '.nested.from_base' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_driver' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_provider' "${merged}")" = "true" ]
}

@test "bootstrap::apply: valueFiles-only entry renders with the file's values applied" {
  # No inline values: — the file alone is the top layer and must win over the
  # provider values in the staged merge.
  write_value_file "values/testcni.dev.yaml" <<'YAML'
shared_all: "valuefile"
nested:
  overridden: "valuefile"
  from_valuefile: true
YAML
  write_cluster_spec "testcni: {valueFiles: [./values/testcni.dev.yaml]}"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # The file wins over provider < driver < base …
  [ "$(yq -r '.shared_all' "${merged}")" = "valuefile" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "valuefile" ]
  [ "$(yq -r '.nested.from_valuefile' "${merged}")" = "true" ]
  # … and the lower layers' unique keys survive.
  [ "$(yq -r '.only_base' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider' "${merged}")" = "provider" ]
  # The reserved key itself never leaks into the helm values.
  [ "$(yq -r 'has("valueFiles")' "${merged}")" = "false" ]
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

@test "bootstrap::apply: a failed entry's DEPENDENT is skipped, but siblings still run" {
  # A fails; B dependsOn:[a] must be SKIPPED (you never start work behind a broken
  # dependency) — and logged as such; C is INDEPENDENT and STILL runs (a failure
  # only invalidates the failed entry's own dependents, not unrelated siblings).
  # apply drains the batch, returns non-zero, leaves no orphan job, and removes its
  # rc temp dir. The test completing at all proves the DAG still terminates on the
  # failure path (no hang). Pin parallelism so A and the independent C launch in the
  # SAME wave; else C could be starved at =1.
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
  # survives. Redirect stderr to assert on the skip log. `|| rc=$?` keeps the
  # non-zero from tripping bats' errexit.
  local err_log="${BATS_TEST_TMPDIR}/err.log"
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" \
    2>"${err_log}" || rc=$?

  # apply reports failure (A failed).
  [ "${rc}" -ne 0 ]

  # A ran and failed; C (independent of A) STILL ran — not stopped by A's failure.
  grep -qx 'a' "${ran_log}"
  grep -qx 'c' "${ran_log}"

  # B (dependsOn the failed A) was NEVER applied — a failed entry's transitive
  # dependents are skipped, and the skip is logged with its cause.
  ! grep -qx 'b' "${ran_log}"
  grep -q "skipping 'b' — a dependency failed (a)" "${err_log}"

  # No surviving background jobs (no orphan left behind on the failure path).
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]

  # The scheduler's rc temp dir was removed.
  local rcdir; rcdir="$(cat "${rcdir_marker}")"
  [ -n "${rcdir}" ]
  [ ! -d "${rcdir}" ]
}

@test "bootstrap::apply: a failed entry's TRANSITIVE dependents are all skipped" {
  # A fails; B dependsOn:[a]; C dependsOn:[b]. Both B AND C must be skipped (the
  # whole subtree behind the broken dep), each logged, and the run reports non-zero.
  # Proves the skip propagation is transitive (BFS over the dependents graph), and
  # that a chain of skipped entries — whose deps never complete — can't wedge the
  # loop (the test completing proves termination).
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
    - c:
        dependsOn: [b]
YAML
  local err_log="${BATS_TEST_TMPDIR}/err.log"
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" \
    2>"${err_log}" || rc=$?

  # apply reports failure.
  [ "${rc}" -ne 0 ]

  # A ran and failed; neither B nor C (transitively behind A) ever applied.
  grep -qx 'a' "${ran_log}"
  ! grep -qx 'b' "${ran_log}"
  ! grep -qx 'c' "${ran_log}"

  # Both skips are logged (cause = the root failed entry a).
  grep -q "skipping 'b' — a dependency failed (a)" "${err_log}"
  grep -q "skipping 'c' — a dependency failed (a)" "${err_log}"

  # No orphan job survived the transitive-skip path.
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]
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

@test "bootstrap::apply: a failed wait-gate skips ALL entries after it; no orphan, temp dir cleaned" {
  # Distinct from the failing-*dependency* test above: this pins the FOREGROUND
  # GATE failure path. G is a wait:true gate; X and Y are positioned AFTER it, so
  # the gate's drain-all/ready-before-later edges make BOTH depend on G. The gate
  # runs synchronously in the foreground and FAILS — its transitive dependents are
  # every entry after it, so X and Y are skipped (and logged), NEVER applied. This
  # is the old stop-all behavior, now expressed via the dependents skip (no special
  # launch guard). apply drains, returns non-zero, leaves no orphan job, and removes
  # its rc temp dir.
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
  local n; for n in g x y; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
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
    - y
YAML
  # Call directly (NOT via `run`, which forks a subshell) so the scheduler's
  # background jobs land in THIS shell's job table — lets us prove no orphan
  # survives. Redirect stderr to assert on the skip log. `|| rc=$?` keeps the
  # non-zero from tripping bats' errexit.
  local err_log="${BATS_TEST_TMPDIR}/err.log"
  local rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" \
    2>"${err_log}" || rc=$?

  # apply reports failure (the gate G failed).
  [ "${rc}" -ne 0 ]

  # G ran and failed.
  grep -qx 'g' "${ran_log}"

  # X and Y (both behind the failed gate) were NEVER applied — a failed gate skips
  # every entry after it, each logged with the gate as cause.
  ! grep -qx 'x' "${ran_log}"
  ! grep -qx 'y' "${ran_log}"
  grep -q "skipping 'x' — a dependency failed (g)" "${err_log}"
  grep -q "skipping 'y' — a dependency failed (g)" "${err_log}"

  # No surviving background jobs (no orphan left behind on the gate-failure path).
  jobs -p > "${BATS_TEST_TMPDIR}/jobs_after"
  [ ! -s "${BATS_TEST_TMPDIR}/jobs_after" ]

  # The scheduler's rc temp dir was removed.
  local rcdir; rcdir="$(cat "${rcdir_marker}")"
  [ -n "${rcdir}" ]
  [ ! -d "${rcdir}" ]
}

# --- bootstrap::dispatch (standalone `lo bootstrap` path) ---------------------
# Regression: the standalone `lo bootstrap <domain>` path (main::bootstrap →
# bootstrap::dispatch) must source the driver and run driver::export BEFORE
# bootstrap::apply, so spec.bootstrap addons' ${LOK8S_SPEC_*} refs substitute.
# Before the fix it went straight to bootstrap::apply with driver::export never
# called, leaving LOK8S_SPEC_* unset (the friction log entry).

# Write a fake Lo driver whose driver::export exports LOK8S_SPEC_CLUSTER_DOMAIN —
# the value the friction reports as empty on the standalone path. The driver
# contract calls driver::export "<domain>", so it reads $1 (mirrors the real
# drivers, minus the argsh :args parsing unavailable in the bats harness).
_write_fake_driver_with_export() {
  local driver_dir="${PATH_LOK8S}/drivers/lo"
  mkdir -p "${driver_dir}"
  cat > "${driver_dir}/main" <<'SCRIPT'
driver::export() { export LOK8S_SPEC_CLUSTER_DOMAIN="$1"; }
SCRIPT
}

@test "bootstrap::dispatch runs driver::export before apply → LOK8S_SPEC_* is set" {
  write_cluster_spec "testcni"
  _write_fake_driver_with_export

  # Capture the spec env AS SEEN BY bootstrap::apply — proves driver::export ran
  # first and the value is non-empty by the time apply is reached.
  bootstrap::apply() { echo "apply spec_domain=[${LOK8S_SPEC_CLUSTER_DOMAIN:-EMPTY}]"; }

  run bootstrap::dispatch "test.lok8s.dev"
  assert_success
  assert_output --partial "apply spec_domain=[test.lok8s.dev]"
  # The empty-render regression must NOT recur.
  [[ "${output}" != *"spec_domain=[EMPTY]"* ]]
}

@test "bootstrap::dispatch tolerates a driver without driver::export (still applies)" {
  write_cluster_spec "testcni"
  # Driver implements the contract minimally — no optional driver::export.
  local driver_dir="${PATH_LOK8S}/drivers/lo"
  mkdir -p "${driver_dir}"
  cat > "${driver_dir}/main" <<'SCRIPT'
driver::provision() { :; }
SCRIPT

  bootstrap::apply() { echo "apply reached: $1"; }

  run bootstrap::dispatch "test.lok8s.dev"
  assert_success
  assert_output --partial "apply reached: test.lok8s.dev"
}

@test "bootstrap::dispatch fails on an unknown driver kind (missing driver script)" {
  # kind=Bogus → no drivers/bogus/main → hard error, apply never reached.
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Bogus
metadata:
  name: e2e-test
spec:
  bootstrap:
    - testcni
YAML

  bootstrap::apply() { echo "apply must not run"; }

  run bootstrap::dispatch "test.lok8s.dev"
  assert_failure
  assert_output --partial "Unknown cluster kind: bogus"
  [[ "${output}" != *"apply must not run"* ]]
}

# The one-document rule (domain::spec_check): a spec file with a second
# document stops `lo bootstrap` before the driver export and the apply, with one
# [error] line that names the lines to remove. Go twin:
# TestDispatchRefusesASecondDocument (internal/bootstrap).
@test "bootstrap::dispatch refuses a spec file with a second document" {
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  bootstrap:
    - testcni
---
kind: KubeOne
YAML
  _write_fake_driver_with_export
  bootstrap::apply() { echo "apply must not run"; }

  run bootstrap::dispatch "test.lok8s.dev"
  assert_failure
  assert_output "[error] ${CLUSTER_YAML} holds more than one YAML document. Remove lines 8 to 9 (the '---' at line 8 and everything after it)."
}

@test "bootstrap::dispatch rejects a path-traversal domain (no filesystem access)" {
  run bootstrap::dispatch "../../../etc"
  assert_failure
  assert_output --partial "invalid domain name"
}

@test "bootstrap::dispatch rejects a malicious cluster kind (path traversal in .kind)" {
  # A crafted spec whose kind traverses out of drivers/ must be rejected BEFORE
  # the source — it must never source an arbitrary file.
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: ../../../tmp/evil
metadata:
  name: e2e-test
YAML

  bootstrap::apply() { echo "apply must not run"; }

  run bootstrap::dispatch "test.lok8s.dev"
  assert_failure
  assert_output --partial "invalid cluster kind"
  [[ "${output}" != *"apply must not run"* ]]
}

# --- bootstrap::_restore_d (pre-bootstrap DR restore) ---

@test "restore_d: no restore.d dir is a silent no-op" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev"
  kubectl() { echo "kubectl-called" >> "${BATS_TEST_TMPDIR}/kubectl.log"; }
  run bootstrap::_restore_d "test.lok8s.dev" "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "$status" -eq 0 ]
  [ ! -f "${BATS_TEST_TMPDIR}/kubectl.log" ]
}

@test "restore_d: plaintext store working copy wins (no sops needed)" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  # Cross-domain-skew regression: a stale PATH_SECRETS (another domain's store,
  # e.g. exported by an earlier build) must be IGNORED — the store resolves
  # from the domain argument.
  export PATH_SECRETS="${BATS_TEST_TMPDIR}/clusters/other.lok8s.dev/secrets"
  mkdir -p "${PATH_SECRETS}/restore.d"
  echo "kind: WrongDomainSecret" > "${PATH_SECRETS}/restore.d/tls-a.yaml"
  local d="${PATH_CLUSTERS}/test.lok8s.dev"
  mkdir -p "${d}/restore.d" "${d}/secrets/restore.d"
  echo "cipher" > "${d}/restore.d/tls-a.sops.yaml"
  echo "kind: Secret" > "${d}/secrets/restore.d/tls-a.yaml"
  kubectl() { echo "$*" >> "${BATS_TEST_TMPDIR}/kubectl.log"; }
  sops() { echo "sops-called" >> "${BATS_TEST_TMPDIR}/sops.log"; return 1; }
  run bootstrap::_restore_d "test.lok8s.dev" "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "$status" -eq 0 ]
  [[ "$output" == *"restore.d — 1 applied, 0 skipped"* ]]
  # Pin the args: right kubeconfig + THIS domain's store file (not PATH_SECRETS's)
  grep -q -- "--kubeconfig ${BATS_TEST_TMPDIR}/kubeconfig" "${BATS_TEST_TMPDIR}/kubectl.log"
  grep -q "test.lok8s.dev/secrets/restore.d/tls-a.yaml" "${BATS_TEST_TMPDIR}/kubectl.log"
  [ ! -f "${BATS_TEST_TMPDIR}/sops.log" ]
  # Negative assertion via run (a bare `! grep` is inert under bats set-e)
  run grep -q "other.lok8s.dev" "${BATS_TEST_TMPDIR}/kubectl.log"
  [ "$status" -ne 0 ]
}

@test "restore_d: sops fallback when no plaintext working copy" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  unset PATH_SECRETS
  local d="${PATH_CLUSTERS}/test.lok8s.dev"
  mkdir -p "${d}/restore.d"
  echo "cipher" > "${d}/restore.d/tls-b.sops.yaml"
  kubectl() { [[ " $* " != *" -f - "* ]] || cat > /dev/null; echo "$*" >> "${BATS_TEST_TMPDIR}/kubectl.log"; }
  sops() { echo "$*" >> "${BATS_TEST_TMPDIR}/sops.log"; echo "kind: Secret"; }
  run bootstrap::_restore_d "test.lok8s.dev" "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "$status" -eq 0 ]
  # Pin the args: sops decrypts THE file; kubectl applies stdin to the right kubeconfig
  grep -q -- "-d ${d}/restore.d/tls-b.sops.yaml" "${BATS_TEST_TMPDIR}/sops.log"
  grep -q -- "--kubeconfig ${BATS_TEST_TMPDIR}/kubeconfig" "${BATS_TEST_TMPDIR}/kubectl.log"
  grep -q -- "-f -" "${BATS_TEST_TMPDIR}/kubectl.log"
  [[ "$output" == *"restore.d — 1 applied, 0 skipped"* ]]
}

@test "restore_d: sops stderr is separated — never piped into kubectl stdin" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  unset PATH_SECRETS
  local d="${PATH_CLUSTERS}/test.lok8s.dev"
  mkdir -p "${d}/restore.d"
  echo "cipher" > "${d}/restore.d/tls-w.sops.yaml"
  # sops succeeds but emits a WARNING on stderr; kubectl must receive ONLY the
  # payload (round-1 regression: `sops 2>&1 | kubectl` mixed the warning into
  # the YAML stream).
  kubectl() { cat > "${BATS_TEST_TMPDIR}/kubectl.stdin"; }
  sops() { echo "WARN: deprecated keygroup" >&2; echo "kind: Secret"; }
  run bootstrap::_restore_d "test.lok8s.dev" "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "$status" -eq 0 ]
  [[ "$output" == *"restore.d — 1 applied, 0 skipped"* ]]
  [ "$(cat "${BATS_TEST_TMPDIR}/kubectl.stdin")" = "kind: Secret" ]
}

@test "restore_d: decrypt failure warns and continues (never fatal)" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  unset PATH_SECRETS
  local d="${PATH_CLUSTERS}/test.lok8s.dev"
  mkdir -p "${d}/restore.d"
  echo "cipher" > "${d}/restore.d/tls-c.sops.yaml"
  kubectl() { return 1; }
  sops() { return 1; }
  run bootstrap::_restore_d "test.lok8s.dev" "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "$status" -eq 0 ]
  [[ "$output" == *"could not decrypt/apply tls-c"* ]]
  [[ "$output" == *"restore.d — 0 applied, 1 skipped"* ]]
}

# ── Buffered parallel output → de-interleaved collapsed blocks ────────────────

@test "bootstrap::apply flushes each parallel entry as one collapsed block (no raw interleaving)" {
  export LOK8S_BOOTSTRAP_PARALLEL=8
  # Stub the per-entry apply to emit kubectl-style lines on stdout — the launch
  # site buffers them per job; the reap must render collapsed blocks instead.
  bootstrap::_apply_one() {
    local name="$1"
    printf 'configmap/%s-one serverside-applied\n' "${name}"
    printf 'deployment.apps/%s-two serverside-applied\n' "${name}"
    sleep 0.1
    return 0
  }
  local n
  for n in a b; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  write_cluster_spec "a" "b"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  [ "$status" -eq 0 ]
  # each entry is one collapsed block…
  [[ "$output" == *"a "*"· 2 resources"* ]]
  [[ "$output" == *"b "*"· 2 resources"* ]]
  # …and the raw per-object lines never reach the terminal
  [[ "$output" != *"serverside-applied"* ]]
}

@test "bootstrap::apply: a failed entry's block is ✗-marked with its errors surfaced" {
  export LOK8S_BOOTSTRAP_PARALLEL=8
  bootstrap::_apply_one() {
    local name="$1"
    printf 'secret/%s serverside-applied\n' "${name}"
    if [[ "${name}" == "b" ]]; then
      echo 'Error from server: admission webhook denied'
      echo 'Error from server: admission webhook denied'
      return 1
    fi
    return 0
  }
  local n
  for n in a b; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  write_cluster_spec "a" "b"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  [ "$status" -eq 1 ]
  [[ "$output" == *"✗"*"b"*"· 1 resource"* ]]
  [[ "$output" == *"admission webhook denied"* ]]
  [[ "$output" == *"×2"* ]]        # duplicate error lines deduped with a count
  [[ "$output" == *"✓"*"a"* ]]     # the unrelated entry still applied + rendered
}

@test "ticker: inert off-tty (never leaves a background pid behind)" {
  # Under bats stdout is not a terminal, so the animation must not start — the
  # safety property that keeps CI/piped logs free of spinner frames.
  _BS_TICKER_PID=""
  _BS_RUNIDX=(); _BS_DONE=0
  local n=0 _names=()
  run bootstrap::_ticker_start
  [ "$status" -eq 0 ]
  bootstrap::_ticker_start
  [ -z "${_BS_TICKER_PID}" ]
  # stop is a no-op when nothing started
  bootstrap::_ticker_stop
}

@test "bootstrap::apply: DEBUG flushes verbatim (de-interleaved, never collapsed)" {
  # kapply's contract: verbose (lo -v) prints everything, no aggregation — the
  # buffered-block flush must honor it too.
  export LOK8S_BOOTSTRAP_PARALLEL=8 DEBUG=1
  bootstrap::_apply_one() {
    printf 'configmap/%s-one serverside-applied\n' "$1"
    return 0
  }
  mkdir -p "${PATH_LOK8S}/addons/a"
  write_cluster_spec "a"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  [ "$status" -eq 0 ]
  [[ "$output" == *"configmap/a-one serverside-applied"* ]]
  [[ "$output" != *"· 1 resource"* ]]
}

# ── hosted platform-owned skip (cilium/ccm never bootstrap onto hosted) ──────

@test "hosted: _apply_one skips cilium unconditionally when LOK8S_BOOTSTRAP_HOSTED=true" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export LOK8S_BOOTSTRAP_HOSTED=true
  run bootstrap::_apply_one "cilium" "/nonexistent/addons/cilium" "kubeone" "hetzner" "/kc" "" "" ""
  assert_success
  assert_output --partial "platform-owned on a hosted cluster"
}

@test "hosted: _apply_one skips ccm unconditionally when LOK8S_BOOTSTRAP_HOSTED=true" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export LOK8S_BOOTSTRAP_HOSTED=true
  run bootstrap::_apply_one "ccm" "/nonexistent/addons/ccm" "capi" "hetzner" "/kc" "" "" ""
  assert_success
  assert_output --partial "platform-owned on a hosted cluster"
}

@test "hosted: a NAME-renamed cilium entry still skips (anchored on the addon dir)" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export LOK8S_BOOTSTRAP_HOSTED=true
  run bootstrap::_apply_one "my-cni" "/nonexistent/addons/cilium" "kubeone" "hetzner" "/kc" "" "" ""
  assert_success
  assert_output --partial "platform-owned on a hosted cluster"
}

@test "self-hosted: LOK8S_BOOTSTRAP_HOSTED=false does not trigger the hosted skip" {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export LOK8S_BOOTSTRAP_HOSTED=false
  # Non-kubeone kind so the driver skip does not fire either; prove the call
  # REACHES the render step (the guard did not swallow it) by stubbing the
  # render to fail loudly.
  addons::render() { echo "RENDER_CALLED" >&2; return 1; }
  run bootstrap::_apply_one "cilium" "/nonexistent/addons/cilium" "capi" "hetzner" "/kc" "" "" ""
  assert_failure
  assert_output --partial "RENDER_CALLED"
  refute_output --partial "platform-owned"
}

# ── hosted gates in bootstrap::apply (reachability vs no-workers) ────────────

_hosted_apply_harness() {
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}/h.test"
  cat > "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: h }
spec:
  kubehz: { hosting: hosted }
  bootstrap: [cert-manager]
YAML
  echo "kc" > "${BATS_TEST_TMPDIR}/kc.yaml"
}

@test "hosted apply: unreachable cluster warns about the OIDC kubeconfig and skips (rc 0)" {
  _hosted_apply_harness
  kubectl() { echo "error: You must be logged in" >&2; return 1; }
  run bootstrap::apply "h.test" "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_success
  assert_output --partial "cannot reach the cluster"
  assert_output --partial "kubelogin"
  refute_output --partial "no Ready workers"
}

#  A hosted cluster a dry run cannot reach (or that has no Ready workers
# yet) is an INCONCLUSIVE assessment, never "nothing would change" — that
# must exit 2, not the clean 0 a real run gets for the identical miss.
@test "hosted apply --dry-run: unreachable cluster is exit 2, not 0" {
  _hosted_apply_harness
  kubectl() { echo "error: You must be logged in" >&2; return 1; }
  local bootstrap_dry_run=1 rc=0
  run bootstrap::apply "h.test" "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_failure 2
  assert_output --partial "assessment not done"
}

@test "hosted apply --dry-run: zero Ready workers is exit 2, not 0" {
  _hosted_apply_harness
  kubectl() { printf 'cp1 NotReady control-plane 1d v1.33\n'; }
  local bootstrap_dry_run=1
  run bootstrap::apply "h.test" "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_failure 2
}

@test "hosted apply: zero Ready workers skips with the workers notice (rc 0)" {
  _hosted_apply_harness
  kubectl() { printf 'cp1 NotReady control-plane 1d v1.33\n'; }
  run bootstrap::apply "h.test" "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_success
  assert_output --partial "no Ready workers yet"
  refute_output --partial "kubelogin"
}

@test "hosted apply: Ready,SchedulingDisabled COUNTS as a worker (prefix match)" {
  _hosted_apply_harness
  kubectl() { printf 'w1 Ready,SchedulingDisabled worker 1d v1.33\n'; }
  # The gate now runs AFTER the entries-resolve (real spec: 1 entry), so it
  # must PRINT the pass-through line; stop the run right after it by failing
  # the entry parse — the assertion pair (worker line printed + rc!=0 from
  # the stub) proves the gate passed and only the stub aborted.
  bootstrap::_parse_entry() { return 1; }
  run bootstrap::apply "h.test" "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_failure
  assert_output --partial "1 Ready worker(s)"
  refute_output --partial "no Ready workers"
}

@test "hosted apply: Ready CONTROL-PLANE nodes do not count as workers" {
  _hosted_apply_harness
  kubectl() { printf 'cp1 Ready control-plane 1d v1.33\ncp2 Ready master 1d v1.33\n'; }
  run bootstrap::apply "h.test" "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_success
  assert_output --partial "no Ready workers yet"
}

@test "hosted apply: an EMPTY bootstrap never probes the cluster" {
  _hosted_apply_harness
  # Overwrite the spec with no bootstrap entries; a kubectl call would fail loudly.
  cat > "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: h }
spec:
  kubehz: { hosting: hosted }
YAML
  kubectl() { echo "PROBE_MUST_NOT_RUN" >&2; return 1; }
  run bootstrap::apply "h.test" "${PATH_CLUSTERS}/h.test/cluster.lok8s.yaml" "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_success
  refute_output --partial "PROBE_MUST_NOT_RUN"
}

# --- KubeOne cilium/ccm reconcile gate: path-based, NOT "does the DaemonSet exist" --
# A full provision (LOK8S_BOOTSTRAP_ONLY=0, set by provision::dispatch) defers
# cilium/ccm to the driver — `kubeone apply` just applied them, so re-applying here
# would race for SSA field ownership. `lo provision --bootstrap` AND standalone
# `lo bootstrap` (both =1) are the sole applier and MUST reconcile from spec.

# Stub an addon named $1 + a kind=KubeOne spec that explicitly lists it, so
# _apply_one resolves the entry and reaches the gate.
_setup_kubeone_addon() {
  local name="${1}" dir="${PATH_LOK8S}/addons/${1}"
  mkdir -p "${dir}"
  cat > "${dir}/chart.yaml" <<YAML
apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: ${name}
valueFiles:
  - values.yaml
YAML
  echo "marker: ${name}" > "${dir}/values.yaml"
  cat > "${CLUSTER_YAML}" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - ${name}
YAML
}

@test "bootstrap: KubeOne defers cilium to the driver on a full provision (=0)" {
  _setup_kubeone_addon cilium
  export LOK8S_BOOTSTRAP_ONLY=0   # provision::dispatch sets this on a full provision

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_output --partial "applied by the KubeOne driver on a full provision"
  # Skip fires BEFORE render → kustomize never ran → no merged values produced.
  [ ! -f "${BATS_TEST_TMPDIR}/last_merged.yaml" ]
}

@test "bootstrap: KubeOne defers ccm to the driver on a full provision (=0)" {
  _setup_kubeone_addon ccm
  export LOK8S_BOOTSTRAP_ONLY=0

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_output --partial "applied by the KubeOne driver on a full provision"
  [ ! -f "${BATS_TEST_TMPDIR}/last_merged.yaml" ]
}

@test "bootstrap: KubeOne reconciles cilium on --bootstrap (LOK8S_BOOTSTRAP_ONLY=1)" {
  _setup_kubeone_addon cilium
  export LOK8S_BOOTSTRAP_ONLY=1

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  refute_output --partial "applied by the KubeOne driver on a full provision"
  # No skip → the render path runs → merged values file is produced (cilium/ccm now
  # reconcile through the same background path as every other addon → normal ✓ · N).
  [ -f "${BATS_TEST_TMPDIR}/last_merged.yaml" ]
}

@test "bootstrap: unset gate defaults to defer (safe fallback, never a spurious re-apply)" {
  _setup_kubeone_addon cilium
  unset LOK8S_BOOTSTRAP_ONLY   # no dispatch set it → bootstrap::apply defaults to 0

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_output --partial "applied by the KubeOne driver on a full provision"
  [ ! -f "${BATS_TEST_TMPDIR}/last_merged.yaml" ]
}

@test "bootstrap: standalone lo bootstrap (dispatch) runs in reconcile mode (=1) on KubeOne" {
  # Regression guard: the gate flag must be set on the bootstrap::dispatch path
  # too, not only provision::dispatch — else `lo bootstrap` silently skips cilium.
  local dom="kotest"
  mkdir -p "${PATH_CLUSTERS}/${dom}" "${PATH_LOK8S}/drivers/kubeone"
  : > "${PATH_LOK8S}/drivers/kubeone/main"   # sourceable no-op driver
  cat > "${PATH_CLUSTERS}/${dom}/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: kotest
spec:
  provider:
    name: hetzner
  bootstrap:
    - cilium
YAML
  # Capture the gate value dispatch hands to apply (stub replaces the real apply).
  bootstrap::apply() { echo "GATE=${LOK8S_BOOTSTRAP_ONLY:-unset}"; }
  unset LOK8S_BOOTSTRAP_ONLY

  run bootstrap::dispatch "${dom}"
  assert_success
  assert_output --partial "GATE=1"
}

# --- --dry-run / --render-dir / --no-diff / --only -----------------------------
# The run modes reach bootstrap::apply through dynamic scoping
# (bootstrap_dry_run, bootstrap_render_dir, bootstrap_no_diff,
# bootstrap_select — set by main::bootstrap / main::provision). A dry run
# renders every entry like the real run and applies NOTHING: the kubectl stub
# logs every argv (KLOG) and answers `diff` from a script; `apply` must never
# appear in the log. The Go twin is internal/bootstrap/dryrun_test.go and the
# piped lines are diffed across both by hack/parity-orchestrate.sh.

# The two-object `kubectl diff` fixture (GNU diff -u -N over LIVE/MERGED):
# one object modified, two the live side lacks (asymmetric on purpose: a
# swapped changed/new count must be visible).
_dry_canned_diff() {
  cat <<'DIFF'
diff -u -N /tmp/LIVE-1/v1.ConfigMap.default.testcni /tmp/MERGED-1/v1.ConfigMap.default.testcni
--- /tmp/LIVE-1/v1.ConfigMap.default.testcni	2026-10-12 10:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.default.testcni	2026-10-12 10:00:00.000000000 +0000
@@ -3,4 +3,5 @@
 kind: ConfigMap
 metadata:
   name: testcni
+  foo: bar
diff -u -N /tmp/LIVE-1/v1.ConfigMap.default.extra /tmp/MERGED-1/v1.ConfigMap.default.extra
--- /tmp/LIVE-1/v1.ConfigMap.default.extra	1970-01-01 00:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.default.extra	2026-10-12 10:00:00.000000000 +0000
@@ -0,0 +1,4 @@
+apiVersion: v1
+kind: ConfigMap
+metadata:
+  name: extra
diff -u -N /tmp/LIVE-1/v1.ConfigMap.default.other /tmp/MERGED-1/v1.ConfigMap.default.other
--- /tmp/LIVE-1/v1.ConfigMap.default.other	1970-01-01 00:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.default.other	2026-10-12 10:00:00.000000000 +0000
@@ -0,0 +1,4 @@
+apiVersion: v1
+kind: ConfigMap
+metadata:
+  name: other
DIFF
}

# _dry_kubectl_stub <rc>... — a kubectl that logs its argv to KLOG and answers
# the N-th `diff` with the N-th rc (1 → the canned diff on stdout, >1 → an
# error on stderr). A dry run may ONLY ever diff — any other verb (apply,
# replace, patch, …) is a hard test failure, not a silent no-op: a stub that
# accepts everything can't catch a dry run that actually writes.
_dry_kubectl_stub() {
  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"; : > "${KLOG}"
  export DRY_RCS="$*" DRY_CALLS="${BATS_TEST_TMPDIR}/diff.calls"; : > "${DRY_CALLS}"
  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    # Drain the manifest — only a `-f -` call has one: a stub that reads
    # stdin unconditionally hangs on a terminal (or an open pipe).
    [[ " $* " != *" -f - "* ]] || cat >/dev/null
    if [[ " $* " != *" diff "* ]]; then
      echo "parity stub: kubectl $* must not be reached by a dry run" >&2
      return 9
    fi
    echo x >> "${DRY_CALLS}"
    local n rc=0; n="$(wc -l < "${DRY_CALLS}")"
    set -- ${DRY_RCS}
    (( n <= $# )) && rc="${!n}"
    case "${rc}" in
      0) return 0 ;;
      1) _dry_canned_diff; return 1 ;;
      *) echo "error: the server could not be reached" >&2; return "${rc}" ;;
    esac
  }
  export -f kubectl _dry_canned_diff
}

@test "bootstrap::apply --dry-run diffs every entry server-side and applies nothing" {
  mkdir -p "${PATH_LOK8S}/addons/second"
  write_cluster_spec "testcni" "second: {dependsOn: [testcni]}"
  _dry_kubectl_stub 0 1
  local bootstrap_dry_run=1 rc=0
  local out="${BATS_TEST_TMPDIR}/dry.out" err="${BATS_TEST_TMPDIR}/dry.err"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2> "${err}" || rc=$?
  [ "${rc}" -eq 1 ]   # something would change
  run cat "${out}"
  assert_line --index 0 "bootstrap dry-run: testcni: unchanged"
  assert_line --index 1 "bootstrap dry-run: second: differs changed=1 new=2"
  assert_line --index 2 "bootstrap dry-run: entries=2 unchanged=1 differs=1 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=0"
  [ "${#lines[@]}" -eq 3 ]
  # Nothing applied, nothing healed: only the two read-only diffs.
  ! grep -qE ' (apply|replace|patch|delete) ' "${KLOG}"
  [ "$(grep -c -- "kubectl --kubeconfig ${KUBECONFIG_FILE} diff --server-side --force-conflicts -f -" "${KLOG}")" -eq 2 ]
}

@test "bootstrap::apply --dry-run --render-dir writes NN-<entry>.yaml and .diff" {
  mkdir -p "${PATH_LOK8S}/addons/second"
  write_cluster_spec "testcni" "second"
  _dry_kubectl_stub 0 1
  local bootstrap_dry_run=1 bootstrap_render_dir="${BATS_TEST_TMPDIR}/out/nested" rc=0
  local err="${BATS_TEST_TMPDIR}/dry.err"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > /dev/null 2> "${err}" || rc=$?
  [ "${rc}" -eq 1 ]
  run ls "${bootstrap_render_dir}"
  assert_output "01-testcni.diff
01-testcni.yaml
02-second.diff
02-second.yaml"
  grep -q 'kind: ConfigMap' "${bootstrap_render_dir}/01-testcni.yaml"
  [ "$(tail -c 1 "${bootstrap_render_dir}/01-testcni.yaml" | od -An -c | tr -d ' ')" = '\n' ]
  [ ! -s "${bootstrap_render_dir}/01-testcni.diff" ]          # unchanged → empty
  diff <(_dry_canned_diff) "${bootstrap_render_dir}/02-second.diff"
  grep -q "bootstrap dry-run: files under ${bootstrap_render_dir}" "${err}"
}

@test "bootstrap::apply --dry-run exit codes: 0 unchanged, 1 differs, 2 error" {
  write_cluster_spec "testcni"
  local bootstrap_dry_run=1 want rc
  for want in 0 1 2; do
    _dry_kubectl_stub "${want}"
    rc=0
    bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" \
      > "${BATS_TEST_TMPDIR}/rc${want}.out" 2> "${BATS_TEST_TMPDIR}/rc${want}.err" || rc=$?
    [ "${rc}" -eq "${want}" ]
  done
  grep -q 'testcni: unchanged$' "${BATS_TEST_TMPDIR}/rc0.out"
  grep -q 'testcni: differs changed=1 new=2$' "${BATS_TEST_TMPDIR}/rc1.out"
  grep -q 'testcni: error$' "${BATS_TEST_TMPDIR}/rc2.out"
  grep -q 'error: the server could not be reached' "${BATS_TEST_TMPDIR}/rc2.err"
  grep -q 'bootstrap: kubectl diff failed for testcni (rc=2)' "${BATS_TEST_TMPDIR}/rc2.err"
}

# kubectl diff's rc 1 means "differences found" by convention, but a
# generic failure can land on the same rc — the race-class pattern (the
# same regex bootstrap::_apply_one's CRD/webhook retry uses), at ANY exit
# code, classifies as inconclusive (needs-crd) — an earlier entry's CRD
# hasn't landed in THIS read-only probe yet — never a false "differs",
# and review round 2, item F: inconclusive is NOT counted as differs, and
# forces the run's exit to 2 (an assessment that could not be completed),
# not 1.
@test "bootstrap::apply --dry-run: a CRD-not-installed rc 1 is inconclusive, not differs" {
  write_cluster_spec "testcni"
  local bootstrap_dry_run=1 rc=0
  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"; : > "${KLOG}"
  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    [[ " $* " != *" -f - "* ]] || cat >/dev/null
    echo 'error: ensure CRDs are installed first' >&2
    return 1
  }
  export -f kubectl
  local out="${BATS_TEST_TMPDIR}/dry.out"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2>/dev/null || rc=$?
  [ "${rc}" -eq 2 ]
  grep -q '^bootstrap dry-run: testcni: inconclusive (needs-crd)$' "${out}"
  grep -q '^bootstrap dry-run: entries=1 unchanged=0 differs=0 rendered=0 skipped=0 inconclusive=1 errors=0 nondeterministic=0$' "${out}"
}

# The SAME race pattern at a HIGHER exit code (kubectl's own ">1 = genuine
# failure" convention) still classifies as inconclusive, not error — the
# round-2 review subagent's mutation found this exact case untested.
@test "bootstrap::apply --dry-run: the race pattern at rc>1 is inconclusive too" {
  write_cluster_spec "testcni"
  local bootstrap_dry_run=1 rc=0
  kubectl() {
    [[ " $* " != *" -f - "* ]] || cat >/dev/null
    echo 'error: failed calling webhook "x.example.com"' >&2
    return 2
  }
  export -f kubectl
  local out="${BATS_TEST_TMPDIR}/dry2.out"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2>/dev/null || rc=$?
  [ "${rc}" -eq 2 ]
  grep -q '^bootstrap dry-run: testcni: inconclusive (needs-crd)$' "${out}"
}

# KUBECTL_EXTERNAL_DIFF (e.g. `diff -q`) exits 1 on a real difference with
# NO "--- " header at all — the old heuristic misclassified this as an
# error; it must still be "differs".
@test "bootstrap::apply --dry-run: an external diff tool with no header still differs" {
  write_cluster_spec "testcni"
  local bootstrap_dry_run=1 rc=0
  kubectl() {
    [[ " $* " != *" -f - "* ]] || cat >/dev/null
    echo 'Files LIVE and MERGED differ'
    return 1
  }
  export -f kubectl
  local out="${BATS_TEST_TMPDIR}/dry3.out"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2>/dev/null || rc=$?
  [ "${rc}" -eq 1 ]
  grep -q '^bootstrap dry-run: testcni: differs$' "${out}"
}

@test "bootstrap::apply --dry-run --no-diff renders only and never calls kubectl" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub
  local bootstrap_dry_run=1 bootstrap_no_diff=1 bootstrap_render_dir="${BATS_TEST_TMPDIR}/out"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line --index 0 "bootstrap dry-run: ${bootstrap_render_dir} holds the rendered manifests in plaintext, Secrets included"
  assert_line --index 1 "bootstrap dry-run: testcni: rendered"
  assert_line --index 2 "bootstrap dry-run: entries=1 unchanged=0 differs=0 rendered=1 skipped=0 inconclusive=0 errors=0 nondeterministic=0"
  [ ! -s "${KLOG}" ]
  [ -f "${bootstrap_render_dir}/01-testcni.yaml" ]
  [ ! -e "${bootstrap_render_dir}/01-testcni.diff" ]
}

@test "bootstrap::apply --dry-run ignores LOK8S_FORCE_RECREATE (diff only, no heal)" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub 1
  export LOK8S_FORCE_RECREATE=1
  local bootstrap_dry_run=1 rc=0
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > /dev/null 2>&1 || rc=$?
  [ "${rc}" -eq 1 ]
  ! grep -qE ' (apply|replace|patch|delete) ' "${KLOG}"
  grep -q ' diff ' "${KLOG}"
}

@test "bootstrap::apply --dry-run reports the real run's skips (KubeOne full provision defers cilium)" {
  mkdir -p "${PATH_LOK8S}/addons/cilium"
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
    - cilium
YAML
  export LOK8S_BOOTSTRAP_ONLY=0
  _dry_kubectl_stub
  local bootstrap_dry_run=1 rc=0
  local out="${BATS_TEST_TMPDIR}/dry.out" err="${BATS_TEST_TMPDIR}/dry.err"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2> "${err}" || rc=$?
  [ "${rc}" -eq 0 ]
  grep -q '^bootstrap dry-run: cilium: skipped$' "${out}"
  grep -q 'skipped=1' "${out}"
  grep -q 'cilium is applied by the KubeOne driver on a full provision' "${err}"
  [ ! -s "${KLOG}" ]
}

# A dry run NEVER applies restore.d — but it DOES decrypt it in memory
# (read-only, same as any other sops decrypt in this codebase) so it can
# report what a real run would restore, as its own entry ahead of the
# addons (lok8s #268 review).
@test "bootstrap::apply --dry-run diffs restore.d as its own entry (never applies it)" {
  write_cluster_spec "testcni"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  echo 'sops: {}' > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/tls.sops.yaml"
  local decrypted="${BATS_TEST_TMPDIR}/decrypted"
  sops() {
    : > "${decrypted}"
    printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls\n'
  }
  export -f sops
  _dry_kubectl_stub 1 0   # restore.d differs, testcni unchanged
  local bootstrap_dry_run=1 rc=0
  local out="${BATS_TEST_TMPDIR}/dry.out"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2>/dev/null || rc=$?
  [ "${rc}" -eq 1 ]
  [ -f "${decrypted}" ]
  grep -q '^bootstrap dry-run: restore.d: differs' "${out}"
  grep -q '^bootstrap dry-run: testcni: unchanged$' "${out}"
  grep -q '^bootstrap dry-run: entries=2 unchanged=1 differs=1 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=0$' "${out}"
  ! grep -qE ' (apply|replace|patch) ' "${KLOG}"
}

# --only takes restore.d out of scope entirely — never applied, never
# diffed — and says so.
@test "bootstrap::apply --dry-run --only skips restore.d (not applied, not diffed)" {
  write_cluster_spec "testcni"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  echo 'sops: {}' > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/tls.sops.yaml"
  sops() { echo "sops must not run with --only" >&2; return 1; }
  export -f sops
  _dry_kubectl_stub 0
  local bootstrap_dry_run=1 bootstrap_select="testcni" rc=0
  local out="${BATS_TEST_TMPDIR}/dry.out" err="${BATS_TEST_TMPDIR}/dry.err"
  bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" > "${out}" 2> "${err}" || rc=$?
  [ "${rc}" -eq 0 ]
  ! grep -q 'restore.d' "${out}"
  grep -q '^\[bootstrap\] --only: restore.d skipped (not applied) — 1 file(s) under clusters/test.lok8s.dev/restore.d$' "${err}"
  ! grep -q 'sops must not run' "${err}"
}

# An unknown --only name is validated BEFORE any writer runs — restore.d
# included. A typo must never apply (or decrypt) restore.d and only THEN
# fail.
@test "bootstrap::apply --only typo never touches restore.d" {
  local n; for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  write_cluster_spec "a" "b" "c"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  echo 'sops: {}' > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/tls.sops.yaml"
  sops() { echo "sops must not run before --only is validated" >&2; return 1; }
  export -f sops
  bootstrap::_apply_one() { echo "$1" >> "${BATS_TEST_TMPDIR}/applied"; return 0; }
  kubectl() { echo "kubectl $*" >> "${KLOG:-${BATS_TEST_TMPDIR}/kubectl.log}"; return 0; }
  export -f kubectl
  local bootstrap_select="a,nope"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "bootstrap: --only: unknown entry 'nope'"
  [ ! -f "${BATS_TEST_TMPDIR}/applied" ]
  ! grep -q 'sops must not run' <<<"${output}"
}

@test "bootstrap::apply --only applies the named entries; dependsOn is not expanded" {
  local log="${BATS_TEST_TMPDIR}/only.log"; : > "${log}"
  bootstrap::_apply_one() { printf '%s wait=%s\n' "$1" "$7" >> "${log}"; return 0; }
  local n; for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  write_cluster_spec "a" "b: {dependsOn: [a]}" "c"
  local bootstrap_select="b"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  run cat "${log}"
  assert_output "b wait="
}

@test "bootstrap::apply --only keeps the edges among the selected entries" {
  local log="${BATS_TEST_TMPDIR}/only.log"; : > "${log}"
  bootstrap::_apply_one() { printf '%s wait=%s\n' "$1" "$7" >> "${log}"; return 0; }
  local n; for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  write_cluster_spec "a" "b: {dependsOn: [a]}" "c"
  local bootstrap_select="b, a"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  run cat "${log}"
  assert_output "a wait=wait
b wait="
}

@test "bootstrap::apply --only matches the addon name of a renamed entry" {
  local log="${BATS_TEST_TMPDIR}/only.log"; : > "${log}"
  bootstrap::_apply_one() { echo "$1" >> "${log}"; return 0; }
  local n; for n in cilium c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  write_cluster_spec "cilium: {name: my-cni}" "c"
  local bootstrap_select="cilium"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  run cat "${log}"
  assert_output "my-cni"
}

@test "bootstrap::apply --only with an unknown name lists the valid names and applies nothing" {
  local log="${BATS_TEST_TMPDIR}/only.log"; : > "${log}"
  bootstrap::_apply_one() { echo "$1" >> "${log}"; return 0; }
  local n; for n in a b c; do mkdir -p "${PATH_LOK8S}/addons/${n}"; done
  write_cluster_spec "a" "b: {name: bee}" "c"
  local bootstrap_select="a,nope"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "bootstrap: --only: unknown entry 'nope' (valid: a, bee, c)"
  [ ! -s "${log}" ]
}

@test "bootstrap::apply --only --dry-run numbers the selection's files from 01" {
  mkdir -p "${PATH_LOK8S}/addons/second"
  write_cluster_spec "testcni" "second"
  _dry_kubectl_stub 0
  local dir="${BATS_TEST_TMPDIR}/render"
  local bootstrap_dry_run=1 bootstrap_select="second" bootstrap_render_dir="${dir}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_line "bootstrap dry-run: second: unchanged"
  assert_line "bootstrap dry-run: entries=1 unchanged=1 differs=0 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=0"
  [ -s "${dir}/01-second.yaml" ]
}

# The kubehz-cluster spec writes `- ./targets/flux: { dependsOn: [fluxcd] }`.
# `--only ./targets/flux` (the key as written) selects that entry, the same
# as its name `flux`.
@test "bootstrap::apply --only accepts the key as written in spec.bootstrap" {
  local log="${BATS_TEST_TMPDIR}/only.log"; : > "${log}"
  bootstrap::_apply_one() { echo "$1" >> "${log}"; return 0; }
  mkdir -p "${PATH_LOK8S}/addons/fluxcd" "${PATH_CLUSTERS}/test.lok8s.dev/targets/flux"
  write_cluster_spec "fluxcd" "./targets/flux: {dependsOn: [fluxcd]}"
  local bootstrap_select="./targets/flux"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_output --partial "bootstrap: --only: flux depends on fluxcd, which is not selected"
  run cat "${log}"
  assert_output "flux"
}

# The key tells apart a framework addon and a cluster target with the same
# last path element; the name and the directory name match both.
@test "bootstrap::apply --only: the key selects the target, not the addon of the same name" {
  local log="${BATS_TEST_TMPDIR}/only.log"; : > "${log}"
  bootstrap::_apply_one() { echo "$1 $2" >> "${log}"; return 0; }
  mkdir -p "${PATH_LOK8S}/addons/rook-ceph" "${PATH_CLUSTERS}/test.lok8s.dev/targets/rook-ceph"
  write_cluster_spec "rook-ceph" "./targets/rook-ceph"
  local bootstrap_select="./targets/rook-ceph"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  run cat "${log}"
  assert_output "rook-ceph ${PATH_CLUSTERS}/test.lok8s.dev/./targets/rook-ceph"
}

# A bare value also matches the directory name of an addon or a target, so
# --only rook-ceph selects ./targets/rook-ceph named rook-ceph-cluster too
# (the --help text says so).
@test "bootstrap::apply --only: a bare name also matches a target directory" {
  local log="${BATS_TEST_TMPDIR}/only.log"; : > "${log}"
  bootstrap::_apply_one() { echo "$1" >> "${log}"; return 0; }
  mkdir -p "${PATH_LOK8S}/addons/rook-ceph" "${PATH_CLUSTERS}/test.lok8s.dev/targets/rook-ceph"
  write_cluster_spec "rook-ceph" "./targets/rook-ceph: {name: rook-ceph-cluster}"
  local bootstrap_select="rook-ceph"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  run sort "${log}"
  assert_output "rook-ceph
rook-ceph-cluster"
}

# Two entries may share a name (a basename collision nothing depends on).
# Their report lines add the key as written, so the two lines differ and each
# names the --only value that selects that one entry.
@test "bootstrap::apply --dry-run adds the key when two entries share a name" {
  mkdir -p "${PATH_LOK8S}/addons/second" "${PATH_CLUSTERS}/test.lok8s.dev/targets/second"
  printf 'resources: []\n' > "${PATH_CLUSTERS}/test.lok8s.dev/targets/second/kustomization.yaml"
  write_cluster_spec "second" "./targets/second"
  _dry_kubectl_stub 0 1
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  [ "${status}" -eq 1 ]
  assert_line "bootstrap dry-run: second (second): unchanged"
  assert_line "bootstrap dry-run: second (./targets/second): differs changed=1 new=2"
  _dry_kubectl_stub 0
  local bootstrap_select="./targets/second"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_line "bootstrap dry-run: second: unchanged"
}

# #281: a dry-run report line names the entry by a name --only accepts (the
# entry name, here a name: override), never by the NN- file stem.
@test "bootstrap::apply --dry-run names each entry the way --only takes it" {
  mkdir -p "${PATH_LOK8S}/addons/second"
  write_cluster_spec "testcni" "second: {name: zweite}"
  _dry_kubectl_stub 0 0
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_line --index 1 "bootstrap dry-run: zweite: unchanged"
  local name="${lines[1]#bootstrap dry-run: }"
  name="${name%%:*}"
  _dry_kubectl_stub 0
  local bootstrap_select="${name}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_line --index 0 "bootstrap dry-run: zweite: unchanged"
  assert_line --index 1 --partial "bootstrap dry-run: entries=1 "
}

# ── Review round 2 items G/H/I/B/C ──────────────────────────────────────

@test "bootstrap::apply --render-dir refuses a foreign non-empty dir without the marker" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub 0
  local dir="${BATS_TEST_TMPDIR}/foreign"
  mkdir -p "${dir}"
  echo "not mine" > "${dir}/README.md"
  local bootstrap_dry_run=1 bootstrap_render_dir="${dir}" rc=0
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial ".lo-dry-run"
  [ ! -e "${dir}/01-testcni.yaml" ]
}

@test "bootstrap::apply --render-dir reuses a marked dir and clears only owned files" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub 0
  local dir="${BATS_TEST_TMPDIR}/out"
  local bootstrap_dry_run=1 bootstrap_render_dir="${dir}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  [ -f "${dir}/.lo-dry-run" ]
  echo stale > "${dir}/01-stale.yaml"
  echo stale > "${dir}/01-stale.diff"
  _dry_kubectl_stub 0
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  [ ! -e "${dir}/01-stale.yaml" ]
  [ ! -e "${dir}/01-stale.diff" ]
  [ -f "${dir}/01-testcni.yaml" ]
}

# Review round 2, item H: --only applies WITHOUT restoring restore.d on a
# REAL (non-dry) run too, with a LOUD warning naming the file(s) — not
# just the dry-run's plain one-liner, since this is a DR/renewal-posture
# decision on a live cluster.
@test "bootstrap::apply --only (real run) skips restore.d and warns loudly" {
  write_cluster_spec "testcni"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  echo 'sops: {}' > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/tls-a.sops.yaml"
  sops() { echo "sops must not run with --only" >&2; return 1; }
  export -f sops
  kubectl() { echo "kubectl $*" >> "${KLOG:-${BATS_TEST_TMPDIR}/kubectl.log}"; return 0; }
  export -f kubectl
  local bootstrap_select="testcni"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  assert_output --partial "WITHOUT restoring restore.d"
  assert_output --partial "tls-a.sops.yaml"
  assert_output --partial "--with-restore"
  ! grep -q 'sops must not run' <<<"${output}"
}

@test "bootstrap::apply --only --with-restore applies restore.d before the selected entry" {
  write_cluster_spec "testcni"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  echo 'sops: {}' > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/tls-a.sops.yaml"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/secrets/restore.d"
  echo 'kind: Secret' > "${PATH_CLUSTERS}/test.lok8s.dev/secrets/restore.d/tls-a.yaml"
  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"; : > "${KLOG}"
  kubectl() { echo "kubectl $*" >> "${KLOG}"; [[ " $* " != *" -f - "* ]] || cat >/dev/null; return 0; }
  export -f kubectl
  local bootstrap_select="testcni" bootstrap_with_restore=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success
  refute_output --partial "WITHOUT restoring restore.d"
  grep -q "restore.d/tls-a.yaml" "${KLOG}"
}

# --- Review round 3: the in-place dry run -------------------------------------
# A dry run renders in place, exactly as a real run does. lo's own writes are
# suppressed at their source (the secrets plugin reports what it generated in
# memory to the boundary's report file), and executable addon code is refused
# at the boundary (the plugin home, no --enable-exec, the refusing docker and
# git). The Go twin is internal/bootstrap/dryrun_round3_test.go and
# internal/addons/dryrun_*_test.go.

# The secrets plugin reports a value it generated in memory: the entry is
# marked non-repeatable, and the next entry starts from an empty report.
@test "bootstrap::apply --dry-run marks an entry non-repeatable from the plugin's report" {
  mkdir -p "${PATH_LOK8S}/addons/second"
  write_cluster_spec "testcni" "second"
  _dry_kubectl_stub 0 0
  export REPORTED="${BATS_TEST_TMPDIR}/reported"
  kustomize() {
    if [[ ! -e "${REPORTED}" ]]; then
      : > "${REPORTED}"
      echo "secret Secret.app.ns.PASSWORD" >> "${LOK8S_BOOTSTRAP_DRYRUN_REPORT}"
    fi
    echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: testcni"
  }
  export -f kustomize
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line --index 0 "bootstrap dry-run: testcni: unchanged (non-repeatable)"
  assert_line --index 1 "bootstrap dry-run: second: unchanged"
}

# An entry's env: may not set lo's own variables — checked BEFORE anything
# of the entry is exported, an entry error on a dry run and a real run. The
# round-2 escapes: LOK8S_BOOTSTRAP_DRYRUN=0, BOOTSTRAP_PROTECTED_ENV_NAMES="",
# bootstrap_render_dir=… (the old check ran after the export).
@test "an entry's env: may not set lo's own variables (dry run and real run)" {
  local name
  for name in LOK8S_BOOTSTRAP_DRYRUN LOK8S_BOOTSTRAP_ALLOW_GENERATORS LOK8S_BOOTSTRAP_DRYRUN_REPORT \
              BOOTSTRAP_PROTECTED_ENV_NAMES bootstrap_render_dir bootstrap_dry_run \
              PATH_SECRETS KUBECONFIG KUSTOMIZE_PLUGIN_HOME HELM_PLUGINS; do
    write_cluster_spec "testcni:
      env:
        LOK8S_USER_OK: fine
        ${name}: \"0\""
    export KCALLED="${BATS_TEST_TMPDIR}/kustomize.${name}"
    kustomize() { : > "${KCALLED}"; echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: x"; }
    export -f kustomize

    _dry_kubectl_stub 0
    local bootstrap_dry_run=1
    run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
    [ "${status}" -eq 2 ] || { echo "dry ${name}: rc=${status}"; return 1; }
    assert_output --partial "bootstrap: testcni: env: may not set ${name} (reserved for lo)"
    assert_output --partial "testcni: error"
    [ ! -e "${KCALLED}" ] || { echo "dry ${name}: the render ran"; return 1; }
    ! grep -q ' apply ' "${KLOG}"

    bootstrap_dry_run=0
    kubectl() { echo "kubectl $*" >> "${KLOG}"; [[ " $* " != *" -f - "* ]] || cat >/dev/null; return 0; }
    export -f kubectl
    run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
    [ "${status}" -eq 1 ] || { echo "real ${name}: rc=${status}"; return 1; }
    assert_output --partial "bootstrap: testcni: env: may not set ${name} (reserved for lo)"
    [ ! -e "${KCALLED}" ] || { echo "real ${name}: the render ran"; return 1; }
  done
}

# The dry run's render reaches kustomize with the boundary: the plugin home
# that links only the built-ins, no --enable-exec, the refusing docker and
# git first on PATH, the sentinel and the report file. --allow-generators
# keeps the real run's plugin home, flags and PATH.
@test "addons::render --dry-run renders with the boundary; --allow-generators with the real run's" {
  export KARGS="${BATS_TEST_TMPDIR}/kargs" KENV="${BATS_TEST_TMPDIR}/kenv"
  kustomize() {
    echo "$*" > "${KARGS}"
    printf 'home=%s\npath=%s\ndry=%s\nreport=%s\nallow=%s\nhelm=%s\ntmp=%s\nstore=%s\n' "${KUSTOMIZE_PLUGIN_HOME:-}" "${PATH%%:*}" \
      "${LOK8S_BOOTSTRAP_DRYRUN:-}" "${LOK8S_BOOTSTRAP_DRYRUN_REPORT:-}" "${LOK8S_BOOTSTRAP_ALLOW_GENERATORS:-}" \
      "${HELM_PLUGINS:-}" "${TMPDIR:-}" "${LOK8S_BOOTSTRAP_DRYRUN_STORE:-}" > "${KENV}"
    echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: x"
  }
  export -f kustomize
  local bootstrap_dry_boundary="${BATS_TEST_TMPDIR}/boundary"
  mkdir -p "${bootstrap_dry_boundary}"
  run addons::dry_run_boundary "${bootstrap_dry_boundary}"
  assert_success
  local bootstrap_dry_run=1
  run addons::render "${ADDON_DIR}" "lo" "hetzner" ""
  assert_success
  run cat "${KARGS}"
  refute_output --partial "--enable-exec"
  assert_output --partial "--enable-alpha-plugins"
  run cat "${KENV}"
  assert_line "home=${bootstrap_dry_boundary}/plugins"
  assert_line "path=${bootstrap_dry_boundary}/bin"
  assert_line "dry=1"
  assert_line "report=${bootstrap_dry_boundary}/report"
  assert_line "allow="
  assert_line "helm=${bootstrap_dry_boundary}/helm-plugins"
  assert_line "tmp=${bootstrap_dry_boundary}/tmp"
  assert_line "store=${bootstrap_dry_boundary}/store"
  local d
  for d in plugins bin helm-plugins store tmp; do
    [ "$(stat -c %a "${bootstrap_dry_boundary}/${d}")" = "700" ] || { echo "${d} is not 0700"; return 1; }
  done
  [ -z "$(ls -A "${bootstrap_dry_boundary}/helm-plugins")" ]
  run "${bootstrap_dry_boundary}/bin/docker" run x
  assert_failure
  assert_output "lok8s dry-run: a KRM container function is not run in a dry run (pass --allow-generators to run it)"
  run "${bootstrap_dry_boundary}/bin/git" clone x
  assert_failure
  assert_output "lok8s dry-run: a remote base is not fetched in a dry run (pass --allow-generators to fetch it)"

  local bootstrap_allow_generators=1
  run addons::render "${ADDON_DIR}" "lo" "hetzner" ""
  assert_success
  run cat "${KARGS}"
  assert_output --partial "--enable-exec"
  run cat "${KENV}"
  assert_line "home=${KUSTOMIZE_PLUGIN_HOME}"
  assert_line "allow=1"
  assert_line "helm=${HELM_PLUGINS:-}"
  # The run store and TMPDIR stay the boundary's with --allow-generators too.
  assert_line "tmp=${bootstrap_dry_boundary}/tmp"
  assert_line "store=${bootstrap_dry_boundary}/store"
}

# Review round 4, item A1: kustomize's helmCharts pulls a chart that
# chartHome lacks into the project with `helm pull`. The dry run's helm
# refuses the pull; anything else (helm version, helm template of a vendored
# chart) runs the next helm on PATH with the same argv.
@test "the dry run's helm refuses a pull and runs the real helm otherwise" {
  local b="${BATS_TEST_TMPDIR}/boundary" real="${BATS_TEST_TMPDIR}/realbin"
  mkdir -p "${b}" "${real}"
  addons::dry_run_boundary "${b}"
  printf '#!/bin/sh\necho "real-helm $*"\n' > "${real}/helm"; chmod +x "${real}/helm"
  PATH="${b}/bin:${real}:${PATH}" run "${b}/bin/helm" pull oci://charts.example/app --untar --untardir charts
  assert_failure
  assert_output "lok8s dry-run: helmCharts needs --allow-generators in a dry run (helm pull writes the chart into the project)"
  PATH="${b}/bin:${real}:${PATH}" run "${b}/bin/helm" version --short
  assert_success
  assert_output "real-helm version --short"
  PATH="${b}/bin:${real}:${PATH}" run "${b}/bin/helm" template app charts/app-1.0.0/app --values v.yaml
  assert_success
  assert_output "real-helm template app charts/app-1.0.0/app --values v.yaml"
}

# A Secret plugin that predates the dry run (no `--capabilities`) would write
# the secrets store: the dry run refuses it before anything renders (rc 2).
@test "bootstrap::apply --dry-run refuses a Secret plugin without the dry-run capability" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub 0
  local plugin="${KUSTOMIZE_PLUGIN_HOME}/secrets.lok8s.dev/v1/secret/Secret"
  mkdir -p "$(dirname "${plugin}")"
  printf '#!/bin/sh\nexit 1\n' > "${plugin}"; chmod +x "${plugin}"
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ]
  assert_output --partial "does not support a dry run (it would write the secrets store)"
  refute_output --partial "01-testcni"
  printf '#!/bin/sh\necho dry-run\n' > "${plugin}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_output --partial "testcni: unchanged"
}

# _fake_helm: a helm v3 stand-in first on PATH (the boundary's helm stub
# execs it). Its answers come from files under ${FAKE_HELM}: version,
# deplist (the rows of `helm dependency list`), deplist.rc, lint (a line
# `helm lint` prints). It never reads stdin.
_fake_helm() {
  export FAKE_HELM="${BATS_TEST_TMPDIR}/fakehelm"
  mkdir -p "${FAKE_HELM}/bin"
  printf 'v3.21.2+g0000000\n' > "${FAKE_HELM}/version"
  printf 'common\t0.1.0  \thttps://charts.example\tok      \n' > "${FAKE_HELM}/deplist"
  : > "${FAKE_HELM}/lint"
  cat > "${FAKE_HELM}/bin/helm" <<'SH'
#!/usr/bin/env bash
# The fake answers like helm v3: `dependency list` logs to stderr (the
# loader's lines about a symlinked subchart and a v2 requirements.yaml) and
# prints its table, then the warnings about extra charts, on stdout.
# ${FAKE_HELM}/table, when it exists, replaces the table.
case "${1:-}" in
  version) cat "${FAKE_HELM}/version" ;;
  dependency)
    echo "walk.go:75: found symbolic link in path: ${3:-}/charts/common resolves to /src/common. Contents of linked file included and used" >&2
    echo 'load.go:120: Warning: Dependencies are handled in Chart.yaml since apiVersion "v2". We recommend migrating dependencies to Chart.yaml.' >&2
    if [[ -s "${FAKE_HELM}/deplist.rc" ]]; then echo "Error: the chart does not load" >&2; exit 1; fi
    if [[ -e "${FAKE_HELM}/table" ]]; then cat "${FAKE_HELM}/table"; exit 0; fi
    printf 'NAME  \tVERSION\tREPOSITORY            \tSTATUS\n'; cat "${FAKE_HELM}/deplist"; echo
    echo "WARNING: \"${3:-}/charts/extra\" is not in Chart.yaml." ;;
  lint)
    echo "==> Linting ${2:-}"
    [[ ! -s "${FAKE_HELM}/lint" ]] || cat "${FAKE_HELM}/lint"
    echo "1 chart(s) linted, 0 chart(s) failed" ;;
  *) echo "fake helm: unexpected $*" >&2; exit 9 ;;
esac
SH
  chmod +x "${FAKE_HELM}/bin/helm"
  export PATH="${FAKE_HELM}/bin:${PATH}"
}

# Review rounds 4 and 5: khelm builds a LOCAL chart's dependencies into the
# chart directory (Chart.lock, charts/, tmpcharts/) when Helm's dependency
# check fails on the chart as Helm's loader loads it, or one is a file://
# chart. The boundary's ChartRenderer is a gate in place of the project's:
# it asks helm (`helm dependency list`: every row ok or unpacked; `helm
# lint`: no dependency the loaded chart lacks), refuses that render and
# reports the chart; a vendored chart, or a remote one, runs the project's
# ChartRenderer with the same argv. What it cannot check is refused.
@test "the dry run's ChartRenderer gate asks helm and refuses what it cannot clear" {
  _fake_helm
  local chart_plugin="khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer"
  mkdir -p "$(dirname "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}")"
  printf '#!/bin/sh\necho "real-chartrenderer $*"\n' > "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}"
  chmod +x "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}"
  local b="${BATS_TEST_TMPDIR}/boundary"
  mkdir -p "${b}"
  addons::dry_run_boundary "${b}"
  local gate="${b}/plugins/${chart_plugin}"
  [ -x "${gate}" ]
  [ ! -L "${gate}" ]
  local root="${BATS_TEST_TMPDIR}/root"
  mkdir -p "${root}/charts/app/charts"
  printf 'apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: common\n    version: 0.1.0\n    repository: https://charts.example\n' > "${root}/charts/app/Chart.yaml"
  _gate() { ( cd "${root}" && PATH="${b}/bin:${PATH}" KUSTOMIZE_PLUGIN_CONFIG_STRING="${1}" LOK8S_BOOTSTRAP_DRYRUN_REPORT="${b}/report" "${gate}" cfg </dev/null ); }
  # Each refusal names its cause and the next action; only a dependency
  # that helm does not report as vendored gets the vendor advice (review
  # round 7, items 1 and 2).
  local p="lok8s dry-run: chart charts/app:"
  local advice="khelm may build the dependencies into the chart directory. Vendor them into charts/, or pass --allow-generators."
  local needs="helm v3 (3.3 or a later 3.x) is required for the dependency check."
  local use_go="Install helm v3, or use the Go implementation (lok8s.yaml: spec.implementation)."

  # helm says ok (an archive), or unpacked (a directory): the chart renders.
  # Its log lines on stderr and the warning about an extra chart on stdout
  # are not rows of the table (review round 6, item 2).
  : > "${b}/report"
  run _gate "chart: charts/app"
  assert_success
  assert_output "real-chartrenderer cfg"
  [ ! -s "${b}/report" ]
  printf 'common\t0.1.0\thttps://charts.example\tunpacked\n' > "${FAKE_HELM}/deplist"
  run _gate "chart: ${root}/charts/app"
  assert_success

  # Every other status of `helm dependency list` is refused and reported.
  local st
  for st in missing misnamed "wrong version" "invalid version" "too many matches" corrupt; do
    printf 'common\t0.1.0\thttps://charts.example\t%s   \n' "${st}" > "${FAKE_HELM}/deplist"
    : > "${b}/report"
    run _gate "chart: charts/app"
    assert_failure
    assert_output "${p} dependency common: ${st} (helm dependency list). ${advice}"
    [ "$(cat "${b}/report")" = "chart-deps charts/app" ] || { echo "status ${st}: report $(cat "${b}/report")"; return 1; }
  done

  # ok to `helm dependency list`, but the loaded chart lacks it (an archive
  # that .helmignore hides): `helm lint` names it — refused.
  printf 'common\t0.1.0\thttps://charts.example\tok\n' > "${FAKE_HELM}/deplist"
  printf '[WARNING] %s: chart directory is missing these dependencies: common,extra\n' "${root}/charts/app" > "${FAKE_HELM}/lint"
  run _gate "chart: charts/app"
  assert_failure
  assert_output "${p} dependency common,extra: not in the chart that helm loads (helm lint). ${advice}"
  : > "${FAKE_HELM}/lint"

  # What the gate cannot check is refused, each with its own message and
  # next action, never the vendor advice: a chart helm cannot load (the
  # last line of helm's stderr says why), no table, no temp file, a helm
  # that is not v3.3+, no helm at all. helm's own "no dependencies" answer
  # has no table and renders: the loaded chart has nothing to build.
  echo 1 > "${FAKE_HELM}/deplist.rc"
  : > "${b}/report"
  run _gate "chart: charts/app"
  assert_failure
  assert_output "${p} helm cannot load the chart: the chart does not load. Fix the chart so that helm can load it."
  [ "$(cat "${b}/report")" = "chart-deps charts/app" ]
  : > "${FAKE_HELM}/deplist.rc"
  : > "${FAKE_HELM}/table"
  : > "${b}/report"
  run _gate "chart: charts/app"
  assert_failure
  assert_output "${p} helm dependency list printed no dependency table (helm v3.21.2+g0000000). Use helm v3.21.2, or use the Go implementation (lok8s.yaml: spec.implementation)."
  [ "$(cat "${b}/report")" = "chart-deps charts/app" ]
  rm "${FAKE_HELM}/table"
  : > "${b}/report"
  run env TMPDIR="${BATS_TEST_TMPDIR}/no-such-dir" bash -c 'cd "$1" && PATH="$2/bin:${PATH}" KUSTOMIZE_PLUGIN_CONFIG_STRING="chart: charts/app" LOK8S_BOOTSTRAP_DRYRUN_REPORT="$2/report" "$3" cfg </dev/null' _ "${root}" "${b}" "${gate}"
  assert_failure
  assert_output "${p} cannot make a temporary file for the dependency check in ${BATS_TEST_TMPDIR}/no-such-dir. Make sure that this directory exists and is writable."
  [ "$(cat "${b}/report")" = "chart-deps charts/app" ]
  : > "${FAKE_HELM}/table"
  printf 'WARNING: no dependencies at %s/charts/app/charts\n' "${root}" > "${FAKE_HELM}/table"
  run _gate "chart: charts/app"
  assert_success
  assert_output "real-chartrenderer cfg"
  rm "${FAKE_HELM}/table"
  printf 'v4.0.0+g0000000\n' > "${FAKE_HELM}/version"
  run _gate "chart: charts/app"
  assert_failure
  assert_output "${p} ${needs} Found: v4.0.0+g0000000. ${use_go}"
  # helm lint checks the dependencies since helm v3.3.0 (an older one would
  # pass an archive that .helmignore hides).
  printf 'v3.2.4+g0000000\n' > "${FAKE_HELM}/version"
  run _gate "chart: charts/app"
  assert_failure
  assert_output "${p} ${needs} Found: v3.2.4+g0000000. ${use_go}"
  printf 'v3.21.2+g0000000\n' > "${FAKE_HELM}/version"
  # No helm at all: a PATH with yq and the system tools only.
  mkdir -p "${FAKE_HELM}/nohelm"
  ln -s "$(command -v yq)" "${FAKE_HELM}/nohelm/yq"
  run env PATH="${b}/bin:${FAKE_HELM}/nohelm:/usr/bin:/bin" bash -c '[[ -z "$(command -v helm)" || "$(command -v helm)" == "'"${b}"'/bin/helm" ]]'
  assert_success
  run env -C "${root}" PATH="${b}/bin:${FAKE_HELM}/nohelm:/usr/bin:/bin" KUSTOMIZE_PLUGIN_CONFIG_STRING="chart: charts/app" \
    LOK8S_BOOTSTRAP_DRYRUN_REPORT="${b}/report" "${gate}" cfg </dev/null
  assert_failure
  assert_output "${p} ${needs} Found: no helm on PATH. ${use_go}"

  # A file:// dependency is always built: refused, vendored or not, without
  # asking helm (requirements.yaml counts too).
  printf 'dependencies:\n  - name: common\n    version: 0.1.0\n    repository: file://../common\n' > "${root}/charts/app/requirements.yaml"
  printf 'v0.0.0-not-asked\n' > "${FAKE_HELM}/version"
  run _gate "chart: charts/app"
  assert_failure
  assert_output "${p} dependency common is a file:// chart. khelm builds it into the chart directory on each render. Pass --allow-generators to render it."
  printf 'v3.21.2+g0000000\n' > "${FAKE_HELM}/version"

  # A chart without dependencies, and a remote chart (a repository: khelm
  # pulls it into its cache), run. The config file argument stands in when
  # the environment has none.
  run _gate $'repository: https://charts.example\nchart: app'
  assert_success
  printf 'chart: charts/app\n' > "${root}/cfg"
  run _gate ""
  assert_failure
  rm "${root}/charts/app/requirements.yaml"
  printf 'apiVersion: v2\nname: app\nversion: 0.1.0\n' > "${root}/charts/app/Chart.yaml"
  run _gate "chart: charts/app"
  assert_success
  [ ! -e "${root}/charts/app/Chart.lock" ]
}

# The same gate against the real helm v3 on real charts (Helm's loader:
# .helmignore honoured, a subchart named by its metadata). Skips without
# helm v3.3+ locally. CI runs it natively with a pinned helm v3 (ci.yml, Bats
# Unit Tests: "Real-helm bats tests"), where CI=true makes a missing helm v3
# a failure: the argsh container that runs the rest of the suite has no
# helm. The fixtures and their verdicts are the ONE table both suites read
# (internal/render/testdata/chartgate-fixtures.txt; the Go twin is
# TestChartRefusal), so the two cannot drift: a row this test cannot build
# fails it. The cause column pins each refusal's own message (review round
# 7, items 2 and 4).
# bats test_tags=real-helm
@test "the dry run's ChartRenderer gate decides with the real helm on real charts" {
  local version minor
  version="$(helm version --short 2>/dev/null)" || version=""
  minor="${version#v3.}"
  minor="${minor%%.*}"
  # The gate's own requirement: helm v3.3 or a later 3.x.
  if [[ "${version}" != v3.* || ! "${minor}" =~ ^[0-9]+$ ]] || (( 10#${minor} < 3 )); then
    [[ -z "${CI:-}" ]] || { echo "CI has no helm v3.3+ (found: ${version:-none})"; return 1; }
    skip "helm v3.3+ not installed (found: ${version:-none})"
  fi
  export HELM_CACHE_HOME="${BATS_TEST_TMPDIR}/helm/cache" HELM_CONFIG_HOME="${BATS_TEST_TMPDIR}/helm/config" HELM_DATA_HOME="${BATS_TEST_TMPDIR}/helm/data"
  local chart_plugin="khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer"
  mkdir -p "$(dirname "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}")"
  printf '#!/bin/sh\necho real-chartrenderer\n' > "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}"
  chmod +x "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}"
  local b="${BATS_TEST_TMPDIR}/boundary"
  mkdir -p "${b}"
  addons::dry_run_boundary "${b}"
  local gate="${b}/plugins/${chart_plugin}" root="${BATS_TEST_TMPDIR}/root" src="${BATS_TEST_TMPDIR}/src"
  local table="${_PROJECT_ROOT}/internal/render/testdata/chartgate-fixtures.txt"
  [ -f "${table}" ]
  _chart() {  # <dir> <name> [version]: a chart (no version line when empty)
    mkdir -p "${1}/templates"
    printf 'apiVersion: v2\nname: %s\n' "${2}" > "${1}/Chart.yaml"
    [[ -z "${3:-}" ]] || printf 'version: %s\n' "${3}" >> "${1}/Chart.yaml"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n' "${2}" > "${1}/templates/cm.yaml"
  }
  _dep() {  # <file> [version]: append a redis dependency (no version line when empty)
    printf 'dependencies:\n  - name: redis\n' >> "${1}"
    [[ -z "${2:-}" ]] || printf '    version: "%s"\n' "${2}" >> "${1}"
    printf '    repository: https://charts.example.test\n' >> "${1}"
  }
  _parent() {  # <name> [dependency version|-none]: a chart that needs redis
    _chart "${root}/${1}" "${1}" 0.1.0
    mkdir -p "${root}/${1}/charts"
    case "${2:-}" in
      "") ;;
      -none) _dep "${root}/${1}/Chart.yaml" ;;
      *) _dep "${root}/${1}/Chart.yaml" "${2}" ;;
    esac
  }
  _tgz() { tar -C "$(dirname "${1}")" -czf "${root}/${2}/charts/${3}" "$(basename "${1}")"; }  # <chart dir> <fixture> <archive>
  _sub() { _chart "${root}/${1}/charts/${2}" redis "${3:-}"; }                                  # <fixture> <dir> [version]
  _v1() {
    mkdir -p "${root}/${1}/charts"
    printf 'apiVersion: v1\nname: %s\nversion: 0.1.0\n' "${1}" > "${root}/${1}/Chart.yaml"
    _dep "${root}/${1}/requirements.yaml" 1.0.0
  }
  _filedep() {
    _parent "${1}"
    printf 'dependencies:\n  - name: common\n    version: 0.1.0\n    repository: file://../common\n' >> "${root}/${1}/Chart.yaml"
  }
  _build() {  # <fixture>: build one row of the table; an unknown name fails
    case "${1}" in
      plain) _parent plain ;;
      vendored-tgz) _parent "${1}" 1.0.0; _tgz "${src}/redis/redis" "${1}" redis-1.0.0.tgz ;;
      vendored-dir) _parent "${1}" 1.0.0; _sub "${1}" redis 1.0.0 ;;
      range) _parent "${1}" "~1.0.0"; _tgz "${src}/redis/redis" "${1}" redis-1.0.0.tgz ;;
      v1-vendored) _v1 "${1}"; _sub "${1}" redis 1.0.0 ;;
      missing) _parent "${1}" 1.0.0 ;;
      missing-no-version) _parent "${1}" -none ;;
      helmignored) _parent "${1}" 1.0.0; _tgz "${src}/redis/redis" "${1}" redis-1.0.0.tgz; echo '*.tgz' > "${root}/${1}/.helmignore" ;;
      misnamed) _parent "${1}" 1.0.0; _tgz "${src}/other/redis" "${1}" redis-1.0.0.tgz ;;
      wrong-version) _parent "${1}" 2.0.0; _tgz "${src}/redis/redis" "${1}" redis-1.0.0.tgz ;;
      two-archives) _parent "${1}" ">=1.0.0"; _tgz "${src}/redis/redis" "${1}" redis-1.0.0.tgz; _tgz "${src}/v11/redis" "${1}" redis-1.1.0.tgz ;;
      corrupt) _parent "${1}" 1.0.0; echo x > "${root}/${1}/charts/redis-1.0.0.tgz" ;;
      filedep) _filedep "${1}" ;;
      filedep-vendored) _filedep "${1}"; _chart "${root}/${1}/charts/common" common 0.1.0 ;;
      v1) _v1 "${1}" ;;
      symlinked) _parent "${1}" 1.0.0; ln -s "${src}/redis/redis" "${root}/${1}/charts/redis" ;;
      v2-req) _parent "${1}"; _dep "${root}/${1}/requirements.yaml" 1.0.0; _sub "${1}" redis 1.0.0 ;;
      v2-req-missing) _parent "${1}"; _dep "${root}/${1}/requirements.yaml" 1.0.0 ;;
      dir-and-foo-bar) _parent "${1}" 1.0.0; _sub "${1}" redis 1.0.0; _tgz "${src}/rb/redis-bar" "${1}" redis-bar-2.0.0.tgz ;;
      tgz-and-dir) _parent "${1}" 1.0.0; _tgz "${src}/redis/redis" "${1}" redis-1.0.0.tgz; _sub "${1}" redis 1.0.0 ;;
      two-dirs) _parent "${1}" 1.0.0; _sub "${1}" redis 1.0.0; _sub "${1}" redis2 1.0.0 ;;
      parent-no-version) _chart "${root}/${1}" "${1}"; _dep "${root}/${1}/Chart.yaml" 1.0.0; _sub "${1}" redis 1.0.0 ;;
      sub-no-version) _parent "${1}" 1.0.0; _sub "${1}" redis ;;
      dep-no-version) _parent "${1}" -none; _sub "${1}" redis 1.0.0 ;;
      *) echo "chartgate-fixtures.txt: no bats builder for fixture ${1}"; return 1 ;;
    esac
  }
  _chart "${src}/redis/redis" redis 1.0.0
  _chart "${src}/other/redis" other 1.0.0
  _chart "${src}/v11/redis" redis 1.1.0
  _chart "${src}/rb/redis-bar" redis-bar 2.0.0
  _chart "${root}/common" common 0.1.0   # the file:// target
  _gate() { ( cd "${root}" && PATH="${b}/bin:${PATH}" KUSTOMIZE_PLUGIN_CONFIG_STRING="chart: ./${1}" LOK8S_BOOTSTRAP_DRYRUN_REPORT="${b}/report" "${gate}" cfg </dev/null ); }
  local -A cause_text=(
    [deps]="Vendor them into charts/, or pass --allow-generators."
    [file]="is a file:// chart. khelm builds it into the chart directory on each render. Pass --allow-generators to render it."
    [load]="Fix the chart so that helm can load it."
  )
  local name verdict cause rc n=0 out="${BATS_TEST_TMPDIR}/gate.out" err="${BATS_TEST_TMPDIR}/gate.err"
  while read -r name verdict cause; do
    [[ -n "${name}" && "${name}" != \#* ]] || continue
    n=$((n + 1))
    _build "${name}" || return 1
    : > "${b}/report"
    rc=0
    _gate "${name}" >"${out}" 2>"${err}" || rc=$?
    if [[ "${verdict}" == render ]]; then
      [ "${rc}" -eq 0 ] || { echo "${name}: rc=${rc}: $(cat "${out}" "${err}")"; return 1; }
      [ "$(cat "${out}")" = "real-chartrenderer" ] || { echo "${name}: stdout $(cat "${out}")"; return 1; }
      # helm's log lines (the symlink, the v2 requirements.yaml) are not shown.
      [ ! -s "${err}" ] || { echo "${name}: stderr $(cat "${err}")"; return 1; }
      [ ! -s "${b}/report" ] || { echo "${name}: report $(cat "${b}/report")"; return 1; }
    else
      [ "${rc}" -eq 1 ] || { echo "${name}: rc=${rc}: $(cat "${out}" "${err}")"; return 1; }
      [ "$(cat "${b}/report")" = "chart-deps ./${name}" ] || { echo "${name}: report $(cat "${b}/report")"; return 1; }
      grep -qF -- "${cause_text[${cause}]}" "${err}" || { echo "${name}: want the ${cause} message, stderr: $(cat "${err}")"; return 1; }
      if [[ "${cause}" != deps ]] && grep -qF -- "${cause_text[deps]}" "${err}"; then
        echo "${name}: the vendor advice for a ${cause} refusal: $(cat "${err}")"; return 1
      fi
    fi
    [ ! -e "${root}/${name}/Chart.lock" ] || { echo "${name}: the gate wrote Chart.lock"; return 1; }
  done < "${table}"
  [ "${n}" -ge 20 ] || { echo "only ${n} fixtures read from ${table}"; return 1; }
}

# The gate's refusal through a whole dry run: the entry is inconclusive (not
# an error), the run exits 2, nothing is written into the chart; vendored,
# the same entry renders through the project's ChartRenderer.
@test "bootstrap::apply --dry-run: unvendored chart dependencies are inconclusive" {
  _fake_helm
  local chart_plugin="khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer"
  mkdir -p "$(dirname "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}")"
  export REAL_RAN="${BATS_TEST_TMPDIR}/real-ran"
  printf '#!/bin/sh\n: > "%s"\n' "${REAL_RAN}" > "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}"
  chmod +x "${KUSTOMIZE_PLUGIN_HOME}/${chart_plugin}"
  mkdir -p "${PATH_BASE}/charts/app/charts"
  printf 'apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: common\n    version: 0.1.0\n    repository: https://charts.example\n' > "${PATH_BASE}/charts/app/Chart.yaml"
  printf 'common\t0.1.0\thttps://charts.example\tmissing\n' > "${FAKE_HELM}/deplist"
  local dir="${PATH_LOK8S}/addons/app"
  mkdir -p "${dir}"
  printf 'apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: app\nchart: %s\n' "${PATH_BASE}/charts/app" > "${dir}/chart.yaml"
  # kustomize starts the ChartRenderer of its plugin home in the
  # kustomization root, with the generator config.
  kustomize() {
    local d="${@: -1}"
    ( cd "${d}" && KUSTOMIZE_PLUGIN_CONFIG_STRING="$(cat chart.yaml)" \
        "${KUSTOMIZE_PLUGIN_HOME}/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer" cfg </dev/null ) || return 1
    echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: app"
  }
  export -f kustomize
  write_cluster_spec "app"
  _dry_kubectl_stub 0
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ] || { echo "rc=${status}: ${output}"; return 1; }
  assert_line "bootstrap dry-run: app: inconclusive (chart dependencies not vendored)"
  assert_line "bootstrap dry-run: entries=1 unchanged=0 differs=0 rendered=0 skipped=0 inconclusive=1 errors=0 nondeterministic=0"
  [ ! -e "${REAL_RAN}" ]
  [ -z "$(ls -A "${PATH_BASE}/charts/app/charts")" ]

  printf 'common\t0.1.0\thttps://charts.example\tunpacked\n' > "${FAKE_HELM}/deplist"
  _dry_kubectl_stub 0
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: app: unchanged"
  [ -e "${REAL_RAN}" ]
}

# Review round 4, item D: the dry run stacks values in a copy in the boundary
# that keeps symlinks the way the real run's copy does (`cp -r`), a dangling
# one included; the copy is gone after the render.
@test "addons::render --dry-run stacks values in a boundary copy that keeps symlinks like the real run" {
  echo "linked: true" > "${BATS_TEST_TMPDIR}/outside.yaml"
  ln -s "${BATS_TEST_TMPDIR}/outside.yaml" "${ADDON_DIR}/linked.yaml"
  ln -s /nonexistent/lok8s-dangling "${ADDON_DIR}/dangling.yaml"
  export SEEN="${BATS_TEST_TMPDIR}/seen"
  kustomize() {
    local d="${@: -1}"
    { [[ -L "${d}/linked.yaml" ]] && echo linked; [[ -L "${d}/dangling.yaml" ]] && echo dangling; echo "dir=${d}"; } > "${SEEN}"
    echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: x"
  }
  export -f kustomize
  local b="${BATS_TEST_TMPDIR}/boundary"
  mkdir -p "${b}"
  addons::dry_run_boundary "${b}"
  local bootstrap_dry_boundary="${b}" bootstrap_dry_run=1
  run addons::render "${ADDON_DIR}" "lo" "hetzner" ""
  assert_success
  run cat "${SEEN}"
  assert_line linked
  assert_line dangling
  assert_line --regexp "^dir=${b}/tmp/lok8s-addon-"
  [ -z "$(ls -A "${b}/tmp")" ]
  bootstrap_dry_run=0
  run addons::render "${ADDON_DIR}" "lo" "hetzner" ""
  assert_success
  run cat "${SEEN}"
  assert_line linked
  assert_line dangling
}

# A restore.d file that does not decrypt is an error (rc 2), never a warning
# and exit 0: the preview of restore.d would be incomplete.
@test "bootstrap::apply --dry-run: a restore.d decrypt failure is an error (rc 2)" {
  write_cluster_spec "testcni"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  echo 'sops: {}' > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/wildcard-tls.sops.yaml"
  sops() { echo "Failed to get the data key required to decrypt the SOPS file." >&2; return 128; }
  export -f sops
  _dry_kubectl_stub 0
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ]
  assert_line "bootstrap dry-run: restore.d: error"
  assert_output --partial "[error] bootstrap: dry-run: restore.d: cannot decrypt wildcard-tls.sops.yaml"
}

# The same with REAL sops material when the toolchain has sops, ssh-keygen
# and ssh-to-age: the normal key discovery (SOPS_AGE_KEY) decrypts and the
# file is diffed; another key fails and the run exits 2.
@test "bootstrap::apply --dry-run: restore.d with real sops (right key diffs, wrong key rc 2)" {
  local t
  for t in sops ssh-keygen ssh-to-age; do
    command -v "${t}" >/dev/null || skip "${t} not installed"
  done
  unset -f sops
  write_cluster_spec "testcni"
  local k="${BATS_TEST_TMPDIR}/k1" k2="${BATS_TEST_TMPDIR}/k2"
  ssh-keygen -q -t ed25519 -N '' -f "${k}" </dev/null
  ssh-keygen -q -t ed25519 -N '' -f "${k2}" </dev/null
  local recipient identity other
  recipient="$(ssh-to-age < "${k}.pub")"
  identity="$(ssh-to-age -private-key -i "${k}")"
  other="$(ssh-to-age -private-key -i "${k2}")"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: wildcard-tls\nstringData:\n  tls.key: restored\n' > "${BATS_TEST_TMPDIR}/tls.yaml"
  ( cd "${BATS_TEST_TMPDIR}" && sops --encrypt --age "${recipient}" --input-type yaml --output-type yaml tls.yaml ) \
    > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/wildcard-tls.sops.yaml" </dev/null
  _dry_kubectl_stub 0 0
  local bootstrap_dry_run=1
  SOPS_AGE_KEY="${identity}" run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: restore.d: unchanged"
  _dry_kubectl_stub 0 0
  SOPS_AGE_KEY="${other}" run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ]
  assert_line "bootstrap dry-run: restore.d: error"
}

# --render-dir: a symlinked dir or path component is refused, a fresh dir is
# 0700, the marker must be a regular file, and the run says once that the
# directory holds plaintext Secrets (no ANSI on piped stderr).
@test "bootstrap::apply --dry-run --render-dir hardening" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub 0
  local bootstrap_dry_run=1 target="${BATS_TEST_TMPDIR}/target"
  mkdir -p "${target}"
  ln -s "${target}" "${BATS_TEST_TMPDIR}/link"
  local bootstrap_render_dir="${BATS_TEST_TMPDIR}/link"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ]; assert_output --partial "${BATS_TEST_TMPDIR}/link is a symlink"
  bootstrap_render_dir="${BATS_TEST_TMPDIR}/link/out"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ]; [ ! -e "${target}/out" ]
  [ -z "$(ls -A "${target}")" ]

  bootstrap_render_dir="${BATS_TEST_TMPDIR}/fresh/out"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  [ "$(stat -c %a "${bootstrap_render_dir}")" = "700" ]
  [ "$(grep -c "bootstrap dry-run: ${bootstrap_render_dir} holds the rendered manifests in plaintext, Secrets included" <<< "${output}")" -eq 1 ]
  ! grep -q $'\033\[' <<< "${output}"

  local d="${BATS_TEST_TMPDIR}/marked"
  mkdir -p "${d}/.lo-dry-run"; echo x > "${d}/01-old.yaml"
  bootstrap_render_dir="${d}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ]; assert_output --partial "as a regular file"
  [ -f "${d}/01-old.yaml" ]
}

# A dry run leaves no temp file behind: not when it finishes, not when it
# is killed (SIGTERM) in the middle of a render. And it CHAINS the caller's
# EXIT trap instead of replacing it.
@test "bootstrap::apply --dry-run leaves no temp files (finished or SIGTERM) and chains the EXIT trap" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub 0
  local tmp="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${tmp}"
  ( export TMPDIR="${tmp}"; bootstrap_dry_run=1
    trap 'echo caller-trap-ran > "${BATS_TEST_TMPDIR}/caller-trap"' EXIT
    bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null >/dev/null 2>&1 ) || true
  [ -z "$(ls -A "${tmp}")" ] || { ls -la "${tmp}"; return 1; }

  # The main-shell path: the caller's EXIT trap is still there afterwards.
  export _PROJECT_ROOT
  bash -c '
    source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap" 2>/dev/null
    trap "echo caller" EXIT
    bootstrap::_dry_run_trap /nonexistent/x
    trap -p EXIT
    bootstrap::_dry_run_untrap /nonexistent/x
    trap -p EXIT' > "${BATS_TEST_TMPDIR}/traps" 2>&1 || true
  # The chained trap while the run lasts, the caller's own trap afterwards,
  # and the caller's trap running at exit.
  run cat "${BATS_TEST_TMPDIR}/traps"
  assert_line --index 0 "trap -- 'rm -rf -- /nonexistent/x; echo caller' EXIT"
  assert_line --index 1 "trap -- 'echo caller' EXIT"
  assert_line --index 2 "caller"

  export SLEPT="${BATS_TEST_TMPDIR}/slept"
  kustomize() { sleep 2; : > "${SLEPT}"; echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: x"; }
  export -f kustomize
  ( export TMPDIR="${tmp}"; bootstrap_dry_run=1
    exec bash -c 'source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"; source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"; source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"; source "${_PROJECT_ROOT}/.lok8s/libs/addons"; source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"; source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"; bootstrap_dry_run=1 bootstrap::apply test.lok8s.dev "'"${CLUSTER_YAML}"'" "'"${KUBECONFIG_FILE}"'"' \
      </dev/null >/dev/null 2>&1 ) &
  local pid=$!
  local i
  for (( i = 0; i < 50; i++ )); do
    [[ -n "$(ls -A "${tmp}")" ]] && break
    sleep 0.1
  done
  [ -n "$(ls -A "${tmp}")" ]          # the boundary exists mid-run
  kill -TERM "${pid}"
  wait "${pid}" || true
  for (( i = 0; i < 40 && ! -e "${SLEPT}"; i++ )); do sleep 0.1; done
  sleep 0.5
  [ -z "$(ls -A "${tmp}")" ] || { ls -laR "${tmp}"; return 1; }
}

@test "bootstrap::mode_check: the flag dependencies (the lines the Go cli prints)" {
  local dry_run=0 render_dir="" no_diff=0 only="" allow_generators=0 with_restore=0 ignore_nondeterministic=0
  run bootstrap::mode_check 1 0
  assert_success
  dry_run=1; run bootstrap::mode_check 0 0
  assert_failure; assert_output --partial "--dry-run requires --bootstrap"
  dry_run=0 only="cilium"; run bootstrap::mode_check 0 1
  assert_failure; assert_output --partial "--only requires --bootstrap"
  only="" render_dir="x"; run bootstrap::mode_check 1 0
  assert_failure; assert_output --partial "--render-dir requires --dry-run"
  render_dir="" no_diff=1; run bootstrap::mode_check 1 0
  assert_failure; assert_output --partial "--no-diff requires --dry-run"
  no_diff=0 allow_generators=1; run bootstrap::mode_check 0 0
  assert_failure; assert_output --partial "--allow-generators requires --bootstrap"
  run bootstrap::mode_check 1 0
  assert_failure; assert_output --partial "--allow-generators requires --dry-run"
  allow_generators=0 dry_run=1; run bootstrap::mode_check 1 0
  assert_success
  # Review round 2, item H's flag: --with-restore needs --bootstrap AND --only.
  dry_run=0 with_restore=1; run bootstrap::mode_check 0 0
  assert_failure; assert_output --partial "--with-restore requires --bootstrap"
  run bootstrap::mode_check 1 0
  assert_failure; assert_output --partial "--with-restore requires --only"
  with_restore=0 only="cilium"; run bootstrap::mode_check 1 1
  assert_success
  # Review round 4: --ignore-nondeterministic needs --bootstrap AND --dry-run.
  only="" dry_run=0 ignore_nondeterministic=1; run bootstrap::mode_check 0 0
  assert_failure; assert_output --partial "--ignore-nondeterministic requires --bootstrap"
  run bootstrap::mode_check 1 0
  assert_failure; assert_output --partial "--ignore-nondeterministic requires --dry-run"
  dry_run=1; run bootstrap::mode_check 1 0
  assert_success
}

# --only GIVEN but blank (every comma-separated value whitespace/empty, OR
# literally "") is an error, never "no filter" — the latter is what an
# ABSENT --only means, and the two must not silently collapse into the
# same full, unfiltered apply (lok8s #268). only_given (the 2nd arg) is
# what main::bootstrap/main::provision derive from the RAW argv
# (bootstrap::_flag_given) — review round 2, item E: a bare `only=""`
# alone can no longer be told apart from "never given" by mode_check, on
# purpose, matching the Go cli's cmd.Flags().Changed("only") fix.
@test "bootstrap::mode_check: --only blank is an error, not 'no filter'" {
  local dry_run=0 render_dir="" no_diff=0 allow_generators=0 with_restore=0
  local only=""
  run bootstrap::mode_check 1 1
  assert_failure; assert_output --partial "--only: no entry names given"
  only=" "
  run bootstrap::mode_check 1 1
  assert_failure; assert_output --partial "--only: no entry names given"
  only=","
  run bootstrap::mode_check 1 1
  assert_failure; assert_output --partial "--only: no entry names given"
  only="cilium"
  run bootstrap::mode_check 1 1
  assert_success
  # only_given=0 (the flag was never on the argv at all) skips the blank
  # check entirely, whatever only happens to hold — that is the ABSENT case.
  only=""
  run bootstrap::mode_check 1 0
  assert_success
}

@test "bootstrap::dispatch --dry-run skips inventory::publish" {
  mkdir -p "${PATH_LOK8S}/drivers/lo"
  printf 'driver::export() { :; }\n' > "${PATH_LOK8S}/drivers/lo/main"
  write_cluster_spec "testcni"
  _dry_kubectl_stub 0
  inventory::publish() { echo "inventory_published"; }
  local bootstrap_dry_run=1
  run bootstrap::dispatch "test.lok8s.dev"
  assert_success
  refute_output --partial "inventory_published"
  assert_output --partial "testcni: unchanged"
}

# --- Review round 4: accidental side effects, not a security boundary --------
# The Go twin is internal/bootstrap/dryrun_round4_test.go and
# internal/addons/dryrun_*_test.go.

# Item A4 (Astra): an entry's env: reaches only the render's processes.
# Exported into the orchestration shell, `env: {stem: ../outside}` redirected
# the render dir's files; addon_dir and kubeconfig were shadowable the same
# way (a dry run and a real run).
@test "bootstrap::apply: an entry's env: cannot shadow the orchestration's variables" {
  write_cluster_spec "testcni:
      env:
        stem: ../outside
        addon_dir: /nonexistent/addon
        kubeconfig: /nonexistent/kubeconfig
        rendered: shadowed"
  _dry_kubectl_stub 0
  local dir="${BATS_TEST_TMPDIR}/out" bootstrap_dry_run=1
  local bootstrap_render_dir="${dir}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: testcni: unchanged"
  [ ! -e "${BATS_TEST_TMPDIR}/outside.yaml" ]
  [ ! -e "${BATS_TEST_TMPDIR}/outside.diff" ]
  grep -q 'name: testcni' "${dir}/01-testcni.yaml"
  [ -f "${dir}/01-testcni.diff" ]
  grep -qF -- "kubectl --kubeconfig ${KUBECONFIG_FILE} diff " "${KLOG}"
  ! grep -q nonexistent "${KLOG}"

  # The real run applies with the cluster's kubeconfig.
  bootstrap_dry_run=0 bootstrap_render_dir=""
  export KLOG="${BATS_TEST_TMPDIR}/kubectl.real.log"; : > "${KLOG}"
  kubectl() { echo "kubectl $*" >> "${KLOG}"; [[ " $* " != *" -f - "* ]] || cat >/dev/null; return 0; }
  export -f kubectl
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  grep -qF -- "--kubeconfig ${KUBECONFIG_FILE}" "${KLOG}"
  ! grep -q nonexistent "${KLOG}"
}

# Item A8: the entry renders twice; an object whose content differs between
# the renders (a chart that mints TLS material per render, as Cilium does) is
# listed as regenerated on every apply. The verdict stays honest (it differs
# on the cluster: exit 1); --ignore-nondeterministic judges the entry
# without it (exit 0 when only it differs), and still lists it. The render
# dir keeps the full render either way.
@test "bootstrap::apply --dry-run lists the objects a chart regenerates on every apply" {
  write_cluster_spec "testcni"
  export RENDERS="${BATS_TEST_TMPDIR}/renders" DIFFIN="${BATS_TEST_TMPDIR}/diff.in"
  : > "${RENDERS}"
  kustomize() {
    echo x >> "${RENDERS}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stable\n  namespace: kube-system\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: cilium-ca\n  namespace: kube-system\ndata:\n  ca.crt: Q0Et%d\n' "$(wc -l < "${RENDERS}")"
  }
  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"; : > "${KLOG}"
  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    local in=""
    [[ " $* " != *" -f - "* ]] || in="$(cat)"
    printf '%s\n' "${in}" > "${DIFFIN}"
    if [[ "${in}" == *cilium-ca* ]]; then _dry_canned_diff; return 1; fi
    return 0
  }
  export -f kustomize kubectl _dry_canned_diff
  local dir="${BATS_TEST_TMPDIR}/out" bootstrap_dry_run=1
  local bootstrap_render_dir="${dir}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 1 ] || { echo "rc=${status}: ${output}"; return 1; }
  [ "$(wc -l < "${RENDERS}")" -eq 2 ]
  assert_line "bootstrap dry-run: testcni: differs changed=1 new=2 (non-deterministic: 1)"
  assert_line "bootstrap dry-run: testcni: non-deterministic (regenerated on every apply): v1 Secret kube-system/cilium-ca"
  assert_line "bootstrap dry-run: entries=1 unchanged=0 differs=1 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=1"
  grep -q cilium-ca "${dir}/01-testcni.yaml"

  : > "${RENDERS}"
  local bootstrap_ignore_nondet=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: testcni: unchanged (non-deterministic: 1)"
  assert_line "bootstrap dry-run: testcni: non-deterministic (regenerated on every apply): v1 Secret kube-system/cilium-ca"
  assert_line "bootstrap dry-run: entries=1 unchanged=1 differs=0 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=1"
  ! grep -q cilium-ca "${DIFFIN}"
  grep -q 'name: stable' "${DIFFIN}"
  grep -q cilium-ca "${dir}/01-testcni.yaml"
}

# A deterministic render lists nothing; when --ignore-nondeterministic leaves
# nothing to diff, the entry is unchanged and kubectl is never asked.
@test "bootstrap::apply --dry-run --ignore-nondeterministic: nothing left to diff is unchanged" {
  write_cluster_spec "testcni"
  export RENDERS="${BATS_TEST_TMPDIR}/renders"; : > "${RENDERS}"
  kustomize() {
    echo x >> "${RENDERS}"
    printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: only\ndata:\n  k: %d\n' "$(wc -l < "${RENDERS}")"
  }
  export -f kustomize
  _dry_kubectl_stub 1
  local dir="${BATS_TEST_TMPDIR}/out" bootstrap_dry_run=1 bootstrap_ignore_nondet=1
  local bootstrap_render_dir="${dir}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: testcni: unchanged (non-deterministic: 1)"
  assert_line "bootstrap dry-run: testcni: non-deterministic (regenerated on every apply): v1 Secret only"
  [ ! -s "${KLOG}" ]
  [ -f "${dir}/01-testcni.diff" ]
  [ ! -s "${dir}/01-testcni.diff" ]
}

# Item D: an entry whose YAML declares a KRM function is marked partial
# (without --enable-exec kustomize skips an exec function silently);
# --allow-generators runs it, so there is no mark then.
@test "bootstrap::apply --dry-run marks an entry that declares a KRM function partial" {
  write_cluster_spec "testcni"
  printf 'metadata:\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: ./fn\n' > "${ADDON_DIR}/fn.yaml"
  _dry_kubectl_stub 0
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: testcni: unchanged (partial: KRM function skipped)"
  _dry_kubectl_stub 0
  local bootstrap_allow_generators=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: testcni: unchanged"
}

# Item A5: every render of a run shares ONE run store (0700, in the
# boundary) and the boundary's TMPDIR; a value the plugin updated in the
# store marks the entry non-repeatable like a generated one.
@test "bootstrap::apply --dry-run: the renders share one run store and the boundary's TMPDIR" {
  mkdir -p "${PATH_LOK8S}/addons/second"
  write_cluster_spec "testcni" "second"
  _dry_kubectl_stub 0 0
  export STORES="${BATS_TEST_TMPDIR}/stores"; : > "${STORES}"
  kustomize() {
    printf '%s %s %s\n' "${LOK8S_BOOTSTRAP_DRYRUN_STORE:-}" \
      "$(stat -c %a "${LOK8S_BOOTSTRAP_DRYRUN_STORE:-/nonexistent}" 2>/dev/null)" "${TMPDIR:-}" >> "${STORES}"
    [[ "${@: -1}" != */second ]] || echo "update Secret.app.ns.PASSWORD" >> "${LOK8S_BOOTSTRAP_DRYRUN_REPORT}"
    echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: x"
  }
  export -f kustomize
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: testcni: unchanged"
  assert_line "bootstrap dry-run: second: unchanged (non-repeatable)"
  [ "$(wc -l < "${STORES}")" -eq 4 ]
  [ "$(sort -u "${STORES}" | wc -l)" -eq 1 ]
  run cat "${STORES}"
  assert_line --index 0 --regexp "^/.*/lok8s-dryrun-[A-Za-z0-9]+/store 700 /.*/lok8s-dryrun-[A-Za-z0-9]+/tmp$"
}

# Item A9: an existing --render-dir that group or others may read is
# tightened to 0700 (stderr says so); the directory is created only after
# the boundary is built, so a refusal there leaves nothing behind.
@test "bootstrap::apply --dry-run tightens an open --render-dir and creates none after a boundary refusal" {
  write_cluster_spec "testcni"
  _dry_kubectl_stub 0
  local dir="${BATS_TEST_TMPDIR}/open" bootstrap_dry_run=1
  mkdir -m 755 "${dir}"
  local bootstrap_render_dir="${dir}"
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  [ "$(stat -c %a "${dir}")" = "700" ]
  assert_line "bootstrap dry-run: ${dir} was 0755: tightened to 0700"

  local plugin="${KUSTOMIZE_PLUGIN_HOME}/secrets.lok8s.dev/v1/secret/Secret"
  mkdir -p "$(dirname "${plugin}")"
  printf '#!/bin/sh\nexit 1\n' > "${plugin}"; chmod +x "${plugin}"
  bootstrap_render_dir="${BATS_TEST_TMPDIR}/fresh"
  _dry_kubectl_stub 0
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 2 ]
  assert_output --partial "does not support a dry run"
  [ ! -e "${bootstrap_render_dir}" ]
}

# Item D: the real run's skips come first, then the reserved-env check: an
# entry the run skips (a platform-owned addon on a hosted cluster) is
# skipped, not an env error, dry or real.
@test "an entry's env: is checked after the real run's skips (dry run and real run)" {
  export LOK8S_BOOTSTRAP_HOSTED=true
  run bootstrap::_apply_one "cilium" "/nonexistent/addons/cilium" "kubeone" "hetzner" "/kc" "" "" "KUBECONFIG=/tmp/evil"
  assert_success
  assert_output --partial "platform-owned on a hosted cluster"
  refute_output --partial "may not set"
  local bootstrap_dry_run=1 bootstrap_dry_boundary="${BATS_TEST_TMPDIR}/boundary"
  mkdir -p "${bootstrap_dry_boundary}"
  run bootstrap::_dry_run_one "cilium" "/nonexistent/addons/cilium" "kubeone" "hetzner" "/kc" "" "KUBECONFIG=/tmp/evil" "01-cilium"
  assert_success
  assert_line "skipped"
  refute_output --partial "may not set"
}

# --- Review round 5 -----------------------------------------------------------
# The Go twin is internal/bootstrap/dryrun_round5_test.go,
# internal/addons/render_test.go and internal/render/chartgate_test.go.

# Item 2: the staging copy keeps symlinks, and the files it rewrites
# (chart.yaml, values.inline.yaml, values.merged.yaml) may be links in the
# addon: a shared ChartRenderer config, shared values. Each is written as a
# new private regular file in the copy; the linked originals stay
# byte-identical — in a dry run and in a real run (the same staging path).
@test "addons::render: the staging never writes through a symlink (dry run and real run)" {
  local shared="${BATS_TEST_TMPDIR}/shared" f
  mkdir -p "${shared}"
  printf 'apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: shared\nvalueFiles:\n  - values.yaml\n' > "${shared}/chart.yaml"
  printf 'shared_inline: true\n' > "${shared}/values.inline.yaml"
  printf 'shared_merged: true\n' > "${shared}/values.merged.yaml"
  for f in chart.yaml values.inline.yaml values.merged.yaml; do
    rm -f "${ADDON_DIR}/${f}"
    ln -s "${shared}/${f}" "${ADDON_DIR}/${f}"
  done
  ( cd "${shared}" && sha256sum chart.yaml values.inline.yaml values.merged.yaml ) > "${BATS_TEST_TMPDIR}/before"
  export SEEN="${BATS_TEST_TMPDIR}/seen"
  kustomize() {
    local d="${@: -1}" g
    : > "${SEEN}"
    for g in chart.yaml values.inline.yaml values.merged.yaml; do
      if [[ -f "${d}/${g}" && ! -L "${d}/${g}" ]]; then
        echo "${g} $(stat -c %a "${d}/${g}")" >> "${SEEN}"
      else
        echo "${g} not-a-regular-file" >> "${SEEN}"
      fi
    done
    grep -q 'values.merged.yaml' "${d}/chart.yaml" && grep -q 'name: shared' "${d}/chart.yaml" && echo "chart.yaml staged" >> "${SEEN}"
    echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: x"
  }
  export -f kustomize
  local b="${BATS_TEST_TMPDIR}/boundary" dry
  mkdir -p "${b}"
  addons::dry_run_boundary "${b}"
  for dry in 1 0; do
    local bootstrap_dry_boundary="${b}" bootstrap_dry_run="${dry}"
    run addons::render "${ADDON_DIR}" "lo" "hetzner" "inline_marker: inline"
    assert_success
    run cat "${SEEN}"
    assert_line "chart.yaml 600"
    assert_line "values.inline.yaml 600"
    assert_line "values.merged.yaml 600"
    assert_line "chart.yaml staged"
    ( cd "${shared}" && sha256sum -c --quiet "${BATS_TEST_TMPDIR}/before" ) || { echo "dry=${dry}: a shared file changed"; return 1; }
    for f in chart.yaml values.inline.yaml values.merged.yaml; do
      [ -L "${ADDON_DIR}/${f}" ] || { echo "dry=${dry}: ${f} is no longer a symlink"; return 1; }
    done
  done
}

# Review round 6, item 1: a keep-chomped block scalar (`|+`) keeps its
# trailing newlines as part of the value. As the last value of the merged
# values or of chart.yaml, the staged files keep every one of them (a
# command substitution drops them) — dry run and real run. The bytes are the
# pinned yq's, and the Go twin pins the same ones
# (internal/addons/render_test.go, TestRenderStagingKeepsKeepChompedTrailingNewlines).
@test "addons::render: the staging keeps the trailing newlines of a keep-chomped last value" {
  local dir="${PATH_LOK8S}/addons/kept" want="${BATS_TEST_TMPDIR}/want" dry
  mkdir -p "${dir}" "${want}"
  printf 'apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: kept\nvalueFiles:\n  - values.yaml\nnote: |+\n  beta\n\n' > "${dir}/chart.yaml"
  printf 'base: 1\n' > "${dir}/values.yaml"
  printf 'driver: 2\nkeep: |+\n  alpha\n\n\n' > "${dir}/values.lo.yaml"
  printf 'apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: kept\nvalueFiles:\n  - values.merged.yaml\nnote: |+\n  beta\n\n' > "${want}/chart.yaml"
  printf 'base: 1\ndriver: 2\nkeep: |+\n  alpha\n\n\n' > "${want}/values.merged.yaml"
  export STAGED="${BATS_TEST_TMPDIR}/staged"
  kustomize() {
    local d="${@: -1}"
    rm -rf "${STAGED}"; mkdir -p "${STAGED}"
    cp "${d}/chart.yaml" "${d}/values.merged.yaml" "${STAGED}/"
    echo "apiVersion: v1"; echo "kind: ConfigMap"; echo "metadata:"; echo "  name: x"
  }
  export -f kustomize
  local b="${BATS_TEST_TMPDIR}/boundary"
  mkdir -p "${b}"
  addons::dry_run_boundary "${b}"
  for dry in 1 0; do
    local bootstrap_dry_boundary="${b}" bootstrap_dry_run="${dry}"
    run addons::render "${dir}" "lo" "" ""
    assert_success
    cmp "${want}/values.merged.yaml" "${STAGED}/values.merged.yaml" || { echo "dry=${dry}: $(od -c "${STAGED}/values.merged.yaml")"; return 1; }
    cmp "${want}/chart.yaml" "${STAGED}/chart.yaml" || { echo "dry=${dry}: $(od -c "${STAGED}/chart.yaml")"; return 1; }
  done
}

# Item 3: an object that only one of the two renders has is
# non-deterministic too (the union of both renders' objects).
@test "bootstrap::apply --dry-run lists an object only the second render has" {
  write_cluster_spec "testcni"
  export RENDERS="${BATS_TEST_TMPDIR}/renders" DIFFIN="${BATS_TEST_TMPDIR}/diff.in"
  : > "${RENDERS}"
  kustomize() {
    echo x >> "${RENDERS}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stable\n  namespace: kube-system\n'
    if [[ "$(wc -l < "${RENDERS}")" -eq 2 ]]; then
      printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: late\n  namespace: kube-system\n'
    fi
  }
  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"; : > "${KLOG}"
  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    local in=""
    [[ " $* " != *" -f - "* ]] || in="$(cat)"
    printf '%s\n' "${in}" > "${DIFFIN}"
    return 0
  }
  export -f kustomize kubectl
  local bootstrap_dry_run=1
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: testcni: unchanged (non-deterministic: 1)"
  assert_line "bootstrap dry-run: testcni: non-deterministic (regenerated on every apply): v1 Secret kube-system/late"
  assert_line --regexp " nondeterministic=1$"
  ! grep -q late "${DIFFIN}"
}

# Item 4: the KRM hint follows the kustomization graph within the project:
# a function in a base the entry references (../base), in a component, or
# in a transformer file outside the entry's directory marks the entry
# partial; a reference outside the project, or a remote one, is not read
# and (review round 6, item 5) marks the entry partial too: the hint cannot
# rule a function out there.
@test "bootstrap::apply --dry-run: the KRM hint follows the kustomization graph" {
  local fn=$'apiVersion: example.test/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: ./fn\n'
  local entry="${PATH_LOK8S}/addons/testcni" out
  out="$(mktemp -d)"   # outside the project (PATH_BASE is BATS_TEST_TMPDIR)
  _krm_case() {  # <want: function|unread|none> <kustomization of the entry>
    printf '%s' "${2}" > "${entry}/kustomization.yaml"
    _dry_kubectl_stub 0
    local bootstrap_dry_run=1
    run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
    assert_success
    case "${1}" in
      function) assert_line "bootstrap dry-run: testcni: unchanged (partial: KRM function skipped)" ;;
      unread) assert_line "bootstrap dry-run: testcni: unchanged (partial: a reference outside the project was not inspected)" ;;
      none) assert_line "bootstrap dry-run: testcni: unchanged" ;;
    esac
  }
  write_cluster_spec "testcni"
  mkdir -p "${PATH_BASE}/shared/base" "${PATH_BASE}/shared/comp" "${PATH_BASE}/fns" "${PATH_BASE}/plain" "${out}/base"
  printf 'transformers:\n- fn.yaml\n' > "${PATH_BASE}/shared/base/kustomization.yaml"
  printf '%s' "${fn}" > "${PATH_BASE}/shared/base/fn.yaml"
  printf 'kind: Component\ntransformers:\n- fn.yaml\n' > "${PATH_BASE}/shared/comp/kustomization.yaml"
  printf '%s' "${fn}" > "${PATH_BASE}/shared/comp/fn.yaml"
  printf '%s' "${fn}" > "${PATH_BASE}/fns/fn.yaml"
  printf 'resources:\n- cm.yaml\n' > "${PATH_BASE}/plain/kustomization.yaml"
  printf 'transformers:\n- fn.yaml\n' > "${out}/base/kustomization.yaml"
  printf '%s' "${fn}" > "${out}/base/fn.yaml"
  _krm_case function "resources:"$'\n'"- ${PATH_BASE}/shared/base"$'\n'
  _krm_case function "resources:"$'\n'"- ../../../shared/base"$'\n'
  _krm_case function "components:"$'\n'"- ../../../shared/comp"$'\n'
  _krm_case function "transformers:"$'\n'"- ../../../fns/fn.yaml"$'\n'
  _krm_case none "resources:"$'\n'"- ../../../plain"$'\n'
  # Review round 7, item 3: what the hint did not read has its own label.
  _krm_case unread "resources:"$'\n'"- ../../../plain"$'\n'"- https://github.com/example/repo//base?ref=v1"$'\n'
  _krm_case unread "resources:"$'\n'"- ${out}/base"$'\n'
  rm -rf "${out}"
}

# Review round 6, items 4 and 5 — the same cases as the Go twin
# (internal/addons/krm_test.go): the hint reads the kustomization file
# itself (an inline function in an extensionless Kustomization, in the
# entry or in a base), reads no file outside the project (grep -r follows
# no link below the entry), reads regular files only (a FIFO does not block
# it), and a reference or a kustomization file outside the project, or a
# remote reference, marks the entry. An entry outside the project (a peeked
# framework addon) reads its own tree.
#
# Review round 7: an unread reference is its own kind (`unread`, its own
# label), a function anywhere wins over it (item 3); a referenced DIRECTORY
# link is followed into the project and marks the entry by what the target
# holds, or as unread when it leaves the project (item 5); a reference is
# cleaned lexically before its links resolve, as kustomize does (item 6).
@test "bootstrap::_krm_declared: inline functions, links and references outside the project" {
  local fn=$'apiVersion: example.test/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: ./fn\n'
  local inline=$'transformers:\n- |-\n  apiVersion: example.test/v1\n  kind: Fn\n  metadata:\n    name: fn\n    annotations:\n      config.kubernetes.io/function: |\n        exec:\n          path: ./fn\n'
  local cm=$'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'
  local out n=0 c=""
  out="$(mktemp -d)"   # outside the project (PATH_BASE is BATS_TEST_TMPDIR)
  printf '%s' "${fn}" > "${out}/fn.yaml"
  printf 'resources:\n- cm.yaml\n' > "${out}/kustomization.yaml"
  mkdir -p "${out}/base"
  printf 'resources:\n- cm.yaml\n' > "${out}/base/kustomization.yaml"
  _case() {  # a fresh case directory c (in the project) with an entry/
    n=$((n + 1)); c="${PATH_BASE}/krm${n}"
    mkdir -p "${c}/entry"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: entry\n' > "${c}/entry/cm.yaml"
  }
  _base() {  # <dir> <fn|plain>: a base with a KRM function, or a plain one
    mkdir -p "${1}"
    if [[ "${2}" == fn ]]; then
      printf 'transformers:\n- fn.yaml\n' > "${1}/kustomization.yaml"; printf '%s' "${fn}" > "${1}/fn.yaml"
    else
      printf 'resources:\n- cm.yaml\n' > "${1}/kustomization.yaml"; printf '%s' "${cm}" > "${1}/cm.yaml"
    fi
  }
  _want() {  # <function|unread|none> <label>
    local rc=0 got
    got="$(bootstrap::_krm_declared "${c}/entry")" || rc=$?
    if [[ "${1}" == none ]]; then
      [ "${rc}" -ne 0 ] && [ -z "${got}" ] || { echo "${2}: marked ${got}"; return 1; }
    else
      [ "${rc}" -eq 0 ] && [ "${got}" = "${1}" ] || { echo "${2}: got '${got}' (rc ${rc}), want ${1}"; return 1; }
    fi
  }
  _case; printf '%s' "${inline}" > "${c}/entry/Kustomization"
  _want function "inline function in an extensionless Kustomization"
  _case; mkdir -p "${c}/shared/base"; printf '%s' "${inline}" > "${c}/shared/base/Kustomization"
  printf 'resources:\n- ../shared/base\n' > "${c}/entry/kustomization.yaml"
  _want function "inline function in a base's extensionless Kustomization"
  _case; printf 'resources:\n- %s/base\n' "${out}" > "${c}/entry/kustomization.yaml"
  _want unread "a base outside the project"
  _case; printf 'resources:\n- https://example.test/raw/cm.yaml\n' > "${c}/entry/kustomization.yaml"
  _want unread "a remote URL"
  _case; printf 'resources:\n- github.com/example/repo/base?ref=v1\n' > "${c}/entry/kustomization.yaml"
  _want unread "a remote GitHub base"
  _case; printf 'resources:\n- git@github.com:example/repo.git//base\n' > "${c}/entry/kustomization.yaml"
  _want unread "a remote git base"
  _case; mkdir -p "${c}/shared/base"; printf '%s' "${inline}" > "${c}/shared/base/Kustomization"
  printf 'resources:\n- https://example.test/raw/cm.yaml\n- ../shared/base\n' > "${c}/entry/kustomization.yaml"
  _want function "a remote base and a function: the function wins"
  _case; printf 'resources:\n- cm.yaml\n' > "${c}/entry/kustomization.yaml"; ln -s "${out}/fn.yaml" "${c}/entry/fn.yaml"
  _want none "an unreferenced .yaml link to a file outside"
  _case; printf 'transformers:\n- fn.yaml\n' > "${c}/entry/kustomization.yaml"; ln -s "${out}/fn.yaml" "${c}/entry/fn.yaml"
  _want unread "a referenced .yaml link to a file outside"
  _case; mkdir -p "${c}/fns"; printf '%s' "${fn}" > "${c}/fns/fn.yaml"
  printf 'transformers:\n- fn.yaml\n' > "${c}/entry/kustomization.yaml"; ln -s "${c}/fns/fn.yaml" "${c}/entry/fn.yaml"
  _want function "a referenced .yaml link to a file inside"
  _case; mkdir -p "${c}/fns"; printf '%s' "${fn}" > "${c}/fns/fn.yaml"
  printf 'resources:\n- cm.yaml\n' > "${c}/entry/kustomization.yaml"; ln -s "${c}/fns/fn.yaml" "${c}/entry/fn.yaml"
  _want none "an unreferenced .yaml link to a file inside"
  _case; ln -s "${out}/kustomization.yaml" "${c}/entry/kustomization.yaml"
  _want unread "a Kustomization that links outside"
  # Item 5: referenced directory links.
  _case; _base "${c}/shared/fnbase" fn; ln -s "${c}/shared/fnbase" "${c}/entry/linked"
  printf 'resources:\n- linked\n' > "${c}/entry/kustomization.yaml"
  _want function "a referenced directory link to a base with a function"
  _case; _base "${c}/shared/plain" plain; ln -s "${c}/shared/plain" "${c}/entry/linked"
  printf 'resources:\n- linked\n' > "${c}/entry/kustomization.yaml"
  _want none "a referenced directory link to a plain base"
  _case; ln -s "${out}/base" "${c}/entry/linked"
  printf 'resources:\n- linked\n' > "${c}/entry/kustomization.yaml"
  _want unread "a referenced directory link out of the project"
  # Item 6: `l/../fns` is the fns next to the link l, not next to its target.
  _case; mkdir -p "${c}/deep/a"; printf '%s' "${cm}" > "${c}/deep/a/cm.yaml"; ln -s "${c}/deep/a" "${c}/l"
  _base "${c}/fns" fn; _base "${c}/deep/fns" plain
  printf 'resources:\n- ../l/../fns\n' > "${c}/entry/kustomization.yaml"
  _want function "link/../x is cleaned before the link resolves (relative)"
  printf 'resources:\n- %s/l/../fns\n' "${c}" > "${c}/entry/kustomization.yaml"
  _want function "link/../x is cleaned before the link resolves (absolute)"
  _case; mkdir -p "${c}/deep/a"; printf '%s' "${cm}" > "${c}/deep/a/cm.yaml"; ln -s "${c}/deep/a" "${c}/l"
  _base "${c}/fns" plain; _base "${c}/deep/fns" fn
  printf 'resources:\n- ../l/../fns\n' > "${c}/entry/kustomization.yaml"
  _want none "link/../x does not reach the target's sibling"
  _case; printf 'resources:\n- ../nowhere\n' > "${c}/entry/kustomization.yaml"
  _want none "a missing reference"
  _case; _base "${c}/shared/base" plain
  printf 'resources:\n- ../shared/base\n' > "${c}/entry/kustomization.yaml"
  _want none "plain"

  # An entry outside the project reads its own tree.
  local own
  own="$(mktemp -d)"
  mkdir -p "${own}/sub"
  printf 'resources:\n- sub\n' > "${own}/kustomization.yaml"
  printf 'resources:\n- cm.yaml\n' > "${own}/sub/kustomization.yaml"
  printf '%s' "${cm}" > "${own}/sub/cm.yaml"
  ! bootstrap::_krm_declared "${own}" >/dev/null || { echo "a reference into the entry's own directory counted as outside"; return 1; }

  # A FIFO named like YAML, below the entry and referenced: never read.
  _case; printf 'resources:\n- cm.yaml\ntransformers:\n- pipe.yaml\n' > "${c}/entry/kustomization.yaml"
  mkfifo "${c}/entry/pipe.yaml"
  ( rc=0; bootstrap::_krm_declared "${c}/entry" >/dev/null || rc=$?; echo "${rc}" > "${BATS_TEST_TMPDIR}/fifo.rc" ) &
  local pid=$! i
  for (( i = 0; i < 100; i++ )); do [[ ! -s "${BATS_TEST_TMPDIR}/fifo.rc" ]] || break; sleep 0.1; done
  if [[ ! -s "${BATS_TEST_TMPDIR}/fifo.rc" ]]; then
    timeout 2 bash -c ': > "${1}"' _ "${c}/entry/pipe.yaml" || true   # unblock the reader
    wait "${pid}" || true
    echo "the hint blocked on a FIFO"; return 1
  fi
  wait "${pid}"
  [ "$(cat "${BATS_TEST_TMPDIR}/fifo.rc")" -ne 0 ] || { echo "a FIFO marked the entry partial"; return 1; }
  rm -rf "${out}" "${own}"
}

# Item 5 (`reuse <name>`: an entry that reads a value another entry of the
# run generated is non-repeatable too) has no bats test here. The `reuse`
# line comes from the Secret plugin, which both implementations run as the
# same binary; a bats stub that writes the line would only repeat the
# report parsing that "the renders share one run store" already covers (an
# `update` line), and passes without the plugin change (review round 6,
# item 8). The plugin's real path is covered in Go:
# kustomize/pkg/cache/dryrun_test.go (TestRunStoreCarriesValuesAcrossPluginProcesses)
# and internal/addons/dryrun_exec_test.go
# (TestDryRunReadingAnotherEntrysValueIsReported: the real kustomize and
# plugin, a producer entry and a consumer entry of one run).

# --- Review round 8 -----------------------------------------------------------
# The Go twin is internal/bootstrap/dryrun_round8_test.go.

# --render-dir together with restore.d: the render dir is prepared (created,
# or a reused dry-run dir cleared of its owned files) BEFORE the restore.d
# preview, which writes restore.d.{yaml,diff} into it. (a) A dir that does
# not exist yet: restore.d's files land in it and its status is the diff's,
# not an error. (b) A reused dry-run dir with an old restore.d.yaml/.diff
# and a stale entry file: restore.d's files are this run's, the stale entry
# file is gone.
@test "bootstrap::apply --dry-run --render-dir with restore.d (new and reused dir)" {
  write_cluster_spec "testcni"
  mkdir -p "${PATH_CLUSTERS}/test.lok8s.dev/restore.d"
  echo 'sops: {}' > "${PATH_CLUSTERS}/test.lok8s.dev/restore.d/tls.sops.yaml"
  sops() { printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls\n'; }
  export -f sops
  local bootstrap_dry_run=1 bootstrap_render_dir="${BATS_TEST_TMPDIR}/fresh/out"

  # (a) a new dir
  _dry_kubectl_stub 1 0   # restore.d differs, testcni unchanged
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  [ "${status}" -eq 1 ] || { echo "rc=${status}: ${output}"; return 1; }
  assert_line "bootstrap dry-run: restore.d: differs changed=1 new=2"
  grep -q 'name: tls' "${bootstrap_render_dir}/restore.d.yaml"
  grep -q '^--- /tmp/LIVE-1/' "${bootstrap_render_dir}/restore.d.diff"

  # (b) a reused dry-run dir with the files of an earlier run
  local d="${BATS_TEST_TMPDIR}/reused"
  mkdir -m 700 "${d}"
  printf 'lok8s dry-run output directory — safe to delete\n' > "${d}/.lo-dry-run"
  echo 'old restore.d manifest' > "${d}/restore.d.yaml"
  echo 'old restore.d diff' > "${d}/restore.d.diff"
  echo 'old entry' > "${d}/05-stale.yaml"
  echo 'old entry diff' > "${d}/05-stale.diff"
  bootstrap_render_dir="${d}"
  _dry_kubectl_stub 0 0   # both unchanged
  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}" </dev/null
  assert_success
  assert_line "bootstrap dry-run: restore.d: unchanged"
  grep -q 'name: tls' "${d}/restore.d.yaml"
  ! grep -q 'old' "${d}/restore.d.yaml"
  [ -f "${d}/restore.d.diff" ] && [ ! -s "${d}/restore.d.diff" ]
  [ ! -e "${d}/05-stale.yaml" ] && [ ! -e "${d}/05-stale.diff" ]
  [ -f "${d}/01-testcni.yaml" ]
}

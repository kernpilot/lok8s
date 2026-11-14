#!/usr/bin/env bats
# build_test.bats — unit tests for .lok8s/libs/build
#
# Domain-based build: `lo build` renders the DOMAIN kustomization
# (clusters/<domain>/kustomization.yaml, which composes the targets) into ONE
# clusters/<domain>/artifacts.yaml. There is no per-target loop and no
# artifacts/<target>/ output.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  # The plugin home of the project with a Secret plugin that has the
  # keys-only run: --keys-only refuses a project without one (kustomize
  # would look for plugins elsewhere).
  unset KUSTOMIZE_PLUGIN_HOME
  _secret_plugin 'dry-run\nkeys-only\n'

  # Stub import
  import() { :; }
  export -f import

  # Source build.sh (and its deps that argsh `import` pulls in at runtime)
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/targets.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/keysonly.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"

  # build::artifacts calls template::envsubst_whitelist for its envsubst pass. In
  # production it arrives via argsh `import` (stubbed to a no-op above), so stub it
  # here too — otherwise the build path hits "command not found" (status 127).
  template::envsubst_whitelist() { echo ""; }
  export -f template::envsubst_whitelist

  # Create the domain directory: targets + a domain kustomization composing them.
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${domain_dir}/targets/networking"
  mkdir -p "${domain_dir}/targets/platform"

  cp "${FIXTURES_DIR}/targets/networking/kustomization.yaml" \
    "${domain_dir}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/networking/namespace.yaml" \
    "${domain_dir}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/platform/kustomization.yaml" \
    "${domain_dir}/targets/platform/"
  cp "${FIXTURES_DIR}/targets/platform/deployment.yaml" \
    "${domain_dir}/targets/platform/"

  cat > "${domain_dir}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./targets/networking
  - ./targets/platform
YAML
}

teardown() {
  teardown_tmpdir
}

# --- build::artifacts ---

@test "build::artifacts writes ONE artifacts.yaml from the domain kustomization" {
  kustomize() {
    echo "apiVersion: v1"
    echo "kind: ConfigMap"
    echo "metadata:"
    echo "  name: test-cm"
  }
  export -f kustomize

  build::artifacts "test.lok8s.dev"

  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  # ONE artifact at the domain root — not per-target.
  [ -f "${domain_dir}/artifacts.yaml" ]
  [ ! -d "${domain_dir}/artifacts/networking" ]
  [ ! -d "${domain_dir}/artifacts/platform" ]
  run cat "${domain_dir}/artifacts.yaml"
  assert_output --partial "kind: ConfigMap"
}

# The Secret plugin's keys-only setting is internal to `lo build --keys-only`:
# a value the shell exported never reaches the render of a plain build or a
# --no-secrets build (Go: render.Build).
@test "build::artifacts pins the Secret plugin's keys-only setting to 0" {
  export KENV="${BATS_TEST_TMPDIR}/kenv"
  kustomize() {
    printf 'keys-only=%s disable=%s\n' "${LOK8S_INTERNAL_SECRETS_KEYS_ONLY-<unset>}" "${LOK8S_SECRETS_DISABLE-<unset>}" > "${KENV}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cm\n'
  }
  export -f kustomize
  export LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1 LOK8S_SECRETS_DISABLE=1

  build::artifacts "test.lok8s.dev"
  [ "$(cat "${KENV}")" = "keys-only=0 disable=0" ]

  rm -f "${KENV}"
  LOK8S_BUILD_NO_SECRETS=1 build::artifacts "test.lok8s.dev"
  [ "$(cat "${KENV}")" = "keys-only=0 disable=1" ]
}

# _secret_plugin <capabilities> [rc]: install a stub Secret plugin in the
# project's plugin home. The stub answers `--capabilities` with the text
# <capabilities> (printf %b) and exit status [rc], and logs what it got:
# its arguments, its working directory and the entries in it, the store
# path, the two plugin settings and its stdin.
_secret_plugin() {
  local home="${BATS_TEST_TMPDIR}/.kustomize"
  mkdir -p "${home}/secrets.lok8s.dev/v1/secret"
  export PROBE_LOG="${BATS_TEST_TMPDIR}/probe.log"
  cat > "${home}/secrets.lok8s.dev/v1/secret/Secret" <<EOF
#!/usr/bin/env bash
{
  echo "args=\$*"
  echo "cwd=\${PWD}"
  echo "entries=\$(ls -A)"
  echo "secrets=\${PATH_SECRETS-<unset>}"
  echo "disable=\${LOK8S_SECRETS_DISABLE-<unset>}"
  echo "keys-only=\${LOK8S_INTERNAL_SECRETS_KEYS_ONLY-<unset>}"
  echo "stdin=\$(cat)"
} >> "\${PROBE_LOG}"
printf '%b' '${1}'
exit ${2:-0}
EOF
  chmod +x "${home}/secrets.lok8s.dev/v1/secret/Secret"
}

# lo build --keys-only (build::keys_only): the render runs with the keys-only
# run and the store-free switch on, goes to stdout, and nothing is written to
# the domain dir: a prior artifacts.yaml and a pre-domain per-target dir stay.
# The plugin probe leaves nothing in the temp dir. KUSTOMIZE_PLUGIN_HOME is
# unset, so the probe and kustomize both get <project>/.kustomize.
@test "build::keys_only prints the keys-only render on stdout and writes no file" {
  export KENV="${BATS_TEST_TMPDIR}/kenv"
  kustomize() {
    printf 'keys-only=%s disable=%s home=%s\n' "${LOK8S_INTERNAL_SECRETS_KEYS_ONLY-<unset>}" "${LOK8S_SECRETS_DISABLE-<unset>}" "${KUSTOMIZE_PLUGIN_HOME-<unset>}" > "${KENV}"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cm\n'
  }
  export -f kustomize
  export LOK8S_INTERNAL_SECRETS_KEYS_ONLY=0 LOK8S_SECRETS_DISABLE=0
  unset KUSTOMIZE_PLUGIN_HOME
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  _secret_plugin 'dry-run\nkeys-only\n'
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  echo "kind: Prior" > "${domain_dir}/artifacts.yaml"
  mkdir -p "${domain_dir}/artifacts/old"
  echo "kind: Old" > "${domain_dir}/artifacts/old/artifacts.yaml"

  build::keys_only "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err"

  [ "$(cat "${KENV}")" = "keys-only=1 disable=1 home=${BATS_TEST_TMPDIR}/.kustomize" ]
  [ "$(cat "${BATS_TEST_TMPDIR}/out")" = "$(printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cm')" ]
  run cat "${BATS_TEST_TMPDIR}/err"
  assert_output --partial "lo build: test.lok8s.dev rendered 1 document(s) -> stdout (keys only)"
  [ "$(cat "${domain_dir}/artifacts.yaml")" = "kind: Prior" ]
  [ "$(cat "${domain_dir}/artifacts/old/artifacts.yaml")" = "kind: Old" ]
  [ -s "${PROBE_LOG}" ]
  run ls -A "${TMPDIR}"
  assert_output ""
  run bash -c "ls -A '${domain_dir}' | grep -c '^tmp\.'"
  assert_output "0"
}

# The contract of --keys-only: it prints no Secret value. Every Secret in
# the final stream has its values emptied, keeps its key names and gets
# the keys-only marker: a
# plugin Secret that a patch gave a value, a plain Secret, a List item, a
# SecretList item without a kind and a Secret whose data is an alias. A
# ConfigMap keeps its values, and a reference to a Secret stays as it is
# (no data key is added to it). Go: TestKeysOnlyBlanksEverySecret.
@test "build::keys_only empties every Secret value in the final stream" {
  cat > "${BATS_TEST_TMPDIR}/render.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  annotations:
    secrets.lok8s.dev/keys-only: "true"
  name: patched
data:
  USER: YWxpY2U=
stringData:
  PASSWORD: patched-plain
type: Opaque
---
apiVersion: v1
kind: Secret
metadata:
  name: plain
data:
  PLAIN: cGxhaW4tdmFsdWU=
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: listed
  stringData:
    KEY: listed-plain
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: listed-cm
  data:
    VISIBLE: list-config
---
apiVersion: v1
kind: SecretList
items:
- metadata:
    name: kindless
  data:
    NESTED: a2luZGxlc3M=
---
apiVersion: v1
kind: Secret
metadata:
  name: aliased
  labels: &values
    ALIASED: alias-plain
data: *values
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
data:
  VISIBLE: plain-config
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: gw
spec:
  listeners:
  - name: https
    tls:
      certificateRefs:
      - kind: Secret
        name: tls
YAML
  export RENDER="${BATS_TEST_TMPDIR}/render.yaml"
  kustomize() { cat "${RENDER}"; }
  export -f kustomize

  build::keys_only "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/out"

  local out value
  out=$(cat "${BATS_TEST_TMPDIR}/out")
  for value in YWxpY2U= patched-plain cGxhaW4tdmFsdWU= listed-plain a2luZGxlc3M= alias-plain; do
    [[ "${out}" != *"${value}"* ]] || { echo "stdout holds the Secret value ${value}: ${out}"; return 1; }
  done
  for value in 'USER: ""' 'PASSWORD: ""' 'PLAIN: ""' 'KEY: ""' 'NESTED: ""' 'ALIASED: ""' \
               'VISIBLE: list-config' 'VISIBLE: plain-config' '- kind: Secret' 'name: tls'; do
    [[ "${out}" == *"${value}"* ]] || { echo "stdout lacks ${value}: ${out}"; return 1; }
  done
  run grep -cE '^[[:blank:]]*(- )?(data|stringData):' "${BATS_TEST_TMPDIR}/out"
  assert_output "8"
  # patched, plain, listed, kindless and aliased; not the reference.
  run grep -c 'secrets.lok8s.dev/keys-only: "true"' "${BATS_TEST_TMPDIR}/out"
  assert_output "5"
}

# Label and annotation values and comments of a Secret are emptied too:
# the keys stay, the marker is "true", no comment is left. A ConfigMap
# prints as it is rendered. Go: TestKeysOnlyEmptiesSecretMetadataAndComments.
@test "build::keys_only empties the labels, annotations and comments of every Secret" {
  cat > "${BATS_TEST_TMPDIR}/render.yaml" <<'YAML'
# head-canary-doc
apiVersion: v1
kind: Secret
metadata:
  name: meta # line-canary-name
  labels:
    copied: label-canary
    nested: {deep: nested-canary}
  annotations:
    note: annotation-canary
stringData:
  # head-canary-key
  KEY: string-canary # line-canary-value
  # foot-canary-key
---
apiVersion: v1
kind: List
items:
# head-canary-item
- apiVersion: v1
  kind: Secret
  metadata:
    name: listed
    annotations: {copied: item-canary}
  data:
    K: bGlzdGVk
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cm
    labels: {visible: label-kept} # comment-kept
  data:
    V: kept
YAML
  export RENDER="${BATS_TEST_TMPDIR}/render.yaml"
  kustomize() { cat "${RENDER}"; }
  export -f kustomize
  _keys_only_to_file() { build::keys_only "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/out"; }
  run _keys_only_to_file
  assert_success
  refute_output --partial "canary"
  local out value
  out=$(cat "${BATS_TEST_TMPDIR}/out")
  [[ "${out}" != *canary* && "${out}" != *bGlzdGVk* ]] || { echo "stdout holds a Secret value or comment: ${out}"; return 1; }
  for value in 'copied: ""' 'nested: {deep: ""}' 'note: ""' 'KEY: ""' 'K: ""' \
               'labels: {visible: label-kept} # comment-kept' 'V: kept'; do
    [[ "${out}" == *"${value}"* ]] || { echo "stdout lacks ${value}: ${out}"; return 1; }
  done
  run grep -o 'secrets.lok8s.dev/keys-only: "true"' "${BATS_TEST_TMPDIR}/out"
  assert_equal "${#lines[@]}" 2
}

# The keys-only output of a domain with only a plain Secret carries the
# marker: the build guard and lo deploy refuse it. Go:
# TestKeysOnlyOutputIsRefusedByTheGuard, TestApplyRefusesKeysOnlyOutput.
@test "build::keys_only output of a plain Secret is refused by the guard and by deploy::apply" {
  kustomize() { printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: only\n  namespace: one\ndata:\n  PLAIN: cGxhaW4tdmFsdWU=\n'; }
  export -f kustomize
  build::keys_only "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/keys.yaml"
  grep -q 'secrets.lok8s.dev/keys-only: "true"' "${BATS_TEST_TMPDIR}/keys.yaml"

  export KEYS="${BATS_TEST_TMPDIR}/keys.yaml"
  kustomize() { cat "${KEYS}"; }
  export -f kustomize
  run build::artifacts "test.lok8s.dev"
  assert_failure
  assert_output --partial "it holds a keys-only Secret (secrets.lok8s.dev/keys-only)"
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  [[ ! -e "${artifact}" ]]

  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/deploy"
  kubectl() { echo "kubectl $*"; }
  export -f kubectl
  cp "${KEYS}" "${artifact}"
  run deploy::apply "test.lok8s.dev"
  assert_failure
  assert_output --partial "refusing to apply test.lok8s.dev: ${artifact} holds a keys-only Secret (secrets.lok8s.dev/keys-only)"
  refute_output --partial "kubectl"
}

# The keys-only run empties the value of each Secret annotation, but a
# Secret with the last-applied annotation (kubectl apply writes it; it can
# hold the values) is refused all the same: the output must not look
# complete when it is not.
# A Secret document, one without a namespace (flow style, a quoted key), a
# List item, a SecretList item without a kind and a merge key; not a
# ConfigMap, not another annotation. Go:
# TestKeysOnlyRefusesTheLastAppliedAnnotation.
@test "build::keys_only refuses a Secret with the last-applied annotation" {
  cat > "${BATS_TEST_TMPDIR}/render.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: app
  namespace: one
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","kind":"Secret","stringData":{"PLAIN":"applied-canary"}}
stringData:
  PLAIN: data-canary
---
apiVersion: v1
kind: Secret
metadata:
  name: nons
  annotations: {"kubectl.kubernetes.io/last-applied-configuration": "{}"}
data:
  PLAIN: ZGF0YS1jYW5hcnk=
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: listed
    namespace: two
    annotations:
      "kubectl.kubernetes.io/last-applied-configuration": "{}"
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cm
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: "{}"
---
apiVersion: v1
kind: SecretList
items:
- metadata:
    name: kindless
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: "{}"
---
apiVersion: v1
kind: Secret
metadata:
  name: merged
  labels: &ann
    kubectl.kubernetes.io/last-applied-configuration: "{}"
  annotations:
    <<: *ann
---
apiVersion: v1
kind: Secret
metadata:
  name: plain
  annotations:
    other: "{}"
YAML
  export RENDER="${BATS_TEST_TMPDIR}/render.yaml"
  kustomize() { cat "${RENDER}"; }
  export -f kustomize
  _keys_only_to_file() { build::keys_only "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/out"; }
  run _keys_only_to_file
  assert_failure
  [[ ! -s "${BATS_TEST_TMPDIR}/out" ]] || { echo "stdout: $(cat "${BATS_TEST_TMPDIR}/out")"; return 1; }
  local name named=()
  for name in one/app nons two/listed kindless merged; do
    assert_output --partial "[error] lo build --keys-only: refusing test.lok8s.dev's render: Secret ${name} has the annotation kubectl.kubernetes.io/last-applied-configuration, which can hold the Secret's values"
  done
  while read -r _ _ _ _ _ _ _ _ name _; do named+=("${name}"); done <<<"${output}"
  [[ "${named[*]}" == "one/app nons two/listed kindless merged" ]] || { echo "refused: ${named[*]}"; return 1; }
  for name in applied-canary data-canary ZGF0YS1jYW5hcnk= "rendered"; do
    refute_output --partial "${name}"
  done
}

# A failed check refuses too. Go: TestKeysOnlyLastAppliedCheckFails.
# The stderr of that yq is withheld too (it can quote the render). Go:
# TestKeysOnlyLastAppliedCheckFails.
@test "build::keys_only refuses the render when the last-applied check fails" {
  local withheld="in keys-only mode lo does not print its stderr, which can quote a value: run the build without --keys-only to see it"
  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  yq() { [[ "${1}" != -N ]] || { echo "Error: a value Zq7-yq-canary" >&2; return 1; }; command yq "$@"; }
  export -f kustomize yq
  _keys_only_to_file() { build::keys_only "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/out"; }
  run _keys_only_to_file
  assert_failure
  [[ ! -s "${BATS_TEST_TMPDIR}/out" ]] || { echo "stdout: $(cat "${BATS_TEST_TMPDIR}/out")"; return 1; }
  assert_output --partial "[error] lo build --keys-only: yq could not check test.lok8s.dev's render for the annotation kubectl.kubernetes.io/last-applied-configuration (${withheld})"
  refute_output --partial "Zq7-yq-canary"
  refute_output --partial "rendered"
}

# build::keys_only holds the render with tracing off (the rules at the top
# of utils/keysonly.sh): under bash -x, and with BASH_XTRACEFD on a file,
# no Secret value of the render is in stdout, stderr or the trace. The
# output shows that the Secret was in the render. A domain without a
# kustomization stops it: under errexit and errtrace, it returns that
# failure to its caller (trace_fail of assert_no_trace_leak).
@test "build::keys_only traces no Secret value of the render" {
  kustomize() { printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\nstringData:\n  PASSWORD: xtrace-canary-keys-only\n'; }
  export -f kustomize
  local -a trace_fail=(build::keys_only "missing.lok8s.dev")
  assert_no_trace_leak xtrace-canary-keys-only build::keys_only "test.lok8s.dev"
  run build::keys_only "test.lok8s.dev"
  assert_success
  assert_output --partial 'PASSWORD: ""'
}

# The stderr of the render and of yq is withheld in keys-only mode: it can
# quote an input value. A failure gets one fixed line. Go:
# TestKeysOnlyWithholdsTheRenderStderr, TestKeysOnlyUnparsedRenderPrintsNothing.
@test "build::keys_only withholds the stderr of the render and of yq" {
  local withheld="in keys-only mode lo does not print its stderr, which can quote a value: run the build without --keys-only to see it"
  kustomize() { echo "Error: a value Zq7-stderr-canary" >&2; return "${STUB_RC}"; }
  export -f kustomize
  _keys_only_quiet() { build::keys_only "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/out"; }
  STUB_RC=1 run _keys_only_quiet
  assert_failure
  refute_output --partial "Zq7-stderr-canary"
  assert_output --partial "[error] kustomize build failed for test.lok8s.dev (${withheld})"
  STUB_RC=0 run _keys_only_quiet
  assert_success
  refute_output --partial "Zq7-stderr-canary"
  kustomize() { printf 'a: [b\n'; }
  export -f kustomize
  run _keys_only_quiet
  assert_failure
  assert_output --partial "[error] lo build --keys-only: yq could not empty the Secret values of test.lok8s.dev's render (${withheld})"
  refute_output --partial "Error:"
}

@test "build::keys_only prints nothing on stdout when the render fails" {
  # Under the real errexit+pipefail of the lo entry.
  run bash -c '
    set -euo pipefail
    import() { :; }
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/verbose.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/domain.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/targets.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/keysonly.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/libs/build"
    export PATH_BASE="'"${BATS_TEST_TMPDIR}"'"
    export PATH_CLUSTERS="'"${BATS_TEST_TMPDIR}"'/clusters"
    template::envsubst_whitelist() { echo ""; }
    kustomize() { echo "apiVersion: v1"; echo "kind: ConfigMap"; return 1; }   # a partial render, then a failure
    export -f template::envsubst_whitelist kustomize
    build::keys_only "test.lok8s.dev"
  '
  assert_failure
  refute_output --partial "kind: ConfigMap"
  assert_output --partial "kustomize build failed for test.lok8s.dev"
}

# A failed write of the render to stdout gets its own message, not the
# render's. Go: TestKeysOnlyStdoutWriteFails.
@test "build::keys_only reports a failed write to stdout" {
  [ -w /dev/full ] || skip "no /dev/full"
  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize
  _keys_only_to_full() { build::keys_only "test.lok8s.dev" > /dev/full; }
  run _keys_only_to_full
  assert_failure
  assert_output --partial "lo build --keys-only: writing test.lok8s.dev's render to stdout failed"
  refute_output --partial "kustomize build failed"
  refute_output --partial "rendered 1 document(s) -> stdout"
}

# A Secret plugin without the keys-only run is refused before the render:
# an older plugin does not know the keys-only setting (and one before
# v0.2.0 not the store-free switch either). The probe is
# `Secret --capabilities`; only a plugin that exits 0 and prints the line
# keys-only passes. Go: TestKeysOnlyProbesThePlugin.
@test "build::keys_only refuses a Secret plugin without the keys-only run" {
  export KENV="${BATS_TEST_TMPDIR}/kenv"
  kustomize() { echo "ran" > "${KENV}"; printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize
  export KUSTOMIZE_PLUGIN_HOME="${BATS_TEST_TMPDIR}/.kustomize"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  local tc caps rc
  for tc in 'dry-run\n|0' '|0' '|1' 'keys-only\n|1' 'keys-only-not\ndry-run\n|0'; do
    caps="${tc%|*}" rc="${tc#*|}"
    _secret_plugin "${caps}" "${rc}"
    rm -f "${KENV}"
    run build::keys_only "test.lok8s.dev"
    assert_failure
    assert_output --partial "the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the keys-only run (an older plugin would read the secrets store) — run 'lo toolchain install' to install the plugin of this lo"
    refute_output --partial "kind: ConfigMap"
    [ ! -e "${KENV}" ] || { echo "kustomize ran for the plugin answer '${tc}'"; return 1; }
  done
  for caps in 'dry-run\nkeys-only\n' 'keys-only'; do
    _secret_plugin "${caps}" 0
    run build::keys_only "test.lok8s.dev"
    assert_success
    assert_output --partial "kind: ConfigMap"
  done
  run ls -A "${TMPDIR}"
  assert_output ""
}

# kustomize takes the first plugin home that exists, so an absent home
# would let it load a plugin from XDG_CONFIG_HOME or HOME that lo did not
# check. --keys-only refuses an absent home before the render; the old
# plugin there would write a canary and never runs. kustomize gets the home
# that lo checked. Go: TestKeysOnlyRefusesAnAbsentPluginHome,
# TestKeysOnlyGivesKustomizeTheCheckedHome.
@test "build::keys_only refuses an absent plugin home" {
  export KENV="${BATS_TEST_TMPDIR}/kenv"
  kustomize() { echo "home=${KUSTOMIZE_PLUGIN_HOME-<unset>}" > "${KENV}"; printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize
  rm -rf "${BATS_TEST_TMPDIR}/.kustomize"
  export XDG_CONFIG_HOME="${BATS_TEST_TMPDIR}/xdg"
  local old="${XDG_CONFIG_HOME}/kustomize/plugin/secrets.lok8s.dev/v1/secret/Secret"
  mkdir -p "${old%/*}"
  printf '#!/usr/bin/env bash\ntouch "%s"\necho keys-only\n' "${BATS_TEST_TMPDIR}/old-plugin-ran" > "${old}"
  chmod +x "${old}"
  run build::keys_only "test.lok8s.dev"
  assert_failure
  assert_output --partial "[error] lo build --keys-only: the kustomize plugin home .kustomize does not exist, so kustomize would load plugins from other directories that lo does not check — run 'lo toolchain install' to install the plugin of this lo"
  refute_output --partial "kind: ConfigMap"
  [[ ! -e "${KENV}" && ! -e "${BATS_TEST_TMPDIR}/old-plugin-ran" ]] || { echo "the render or the old plugin ran"; return 1; }

  _secret_plugin 'keys-only\n'
  run build::keys_only "test.lok8s.dev"
  assert_success
  [ "$(cat "${KENV}")" = "home=${BATS_TEST_TMPDIR}/.kustomize" ]
}

# A plugin home without the Secret executable is refused before the
# render: a kustomize built with Go plugin support would load the Go plugin
# of the same generator (Secret.so), which the probe cannot check. Go:
# TestKeysOnlyRefusesAHomeWithoutTheSecretExecutable.
@test "build::keys_only refuses a plugin home without the Secret executable" {
  export KENV="${BATS_TEST_TMPDIR}/kenv"
  kustomize() { echo "ran" > "${KENV}"; printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize
  local dir="${BATS_TEST_TMPDIR}/.kustomize/secrets.lok8s.dev/v1/secret"
  rm -f "${dir}/Secret"
  echo "a Go plugin" > "${dir}/Secret.so"
  run build::keys_only "test.lok8s.dev"
  assert_failure
  assert_output --partial "[error] lo build --keys-only: the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not exist, so kustomize could load another form of the plugin (a Go plugin, Secret.so) that lo does not check — run 'lo toolchain install' to install the plugin of this lo"
  refute_output --partial "kind: ConfigMap"
  [ ! -e "${KENV}" ] || { echo "kustomize ran"; return 1; }
}

# A probe that cannot start or that a signal stops has its own message: it
# is not a plugin without the keys-only run. An exit status above the
# signal range (255) is the plugin's own: a plugin without the run. Go:
# TestKeysOnlyProbeFailureMessages.
@test "build::keys_only names a probe that cannot start or is interrupted" {
  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize
  _secret_plugin 'keys-only\n' 0
  TMPDIR="${BATS_TEST_TMPDIR}/no-such-dir" run build::keys_only "test.lok8s.dev"
  assert_failure
  assert_output --partial "[error] lo build --keys-only: cannot make a temporary directory for the check of the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret"
  refute_output --partial "does not have the keys-only run"
  refute_output --partial "kind: ConfigMap"

  printf '#!/usr/bin/env bash\nkill -TERM $$\n' > "${BATS_TEST_TMPDIR}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  run build::keys_only "test.lok8s.dev"
  assert_failure
  assert_output --partial "[error] lo build --keys-only: the check of the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret was interrupted"
  refute_output --partial "does not have the keys-only run"
  refute_output --partial "kind: ConfigMap"

  _secret_plugin '' 255
  run build::keys_only "test.lok8s.dev"
  assert_failure
  assert_output --partial "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the keys-only run"
  refute_output --partial "was interrupted"
}

# The probe can do nothing with any plugin version: it runs in an empty
# temp dir (removed after), with an empty stdin, PATH_SECRETS at a path in
# that dir that does not exist, the store-free switch on and the keys-only
# setting off, whatever the caller exported. Go: TestKeysOnlyProbesThePlugin.
@test "build::keys_only probes the plugin in an empty dir with no store and no input" {
  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize
  export KUSTOMIZE_PLUGIN_HOME="${BATS_TEST_TMPDIR}/.kustomize"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}" "${BATS_TEST_TMPDIR}/store"
  export PATH_SECRETS="${BATS_TEST_TMPDIR}/store" LOK8S_SECRETS_DISABLE=0 LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1
  _secret_plugin 'keys-only\n'

  build::keys_only "test.lok8s.dev" <<<"input on the caller's stdin" >/dev/null

  run cat "${PROBE_LOG}"
  [ "${#lines[@]}" -eq 7 ] || { echo "want one probe (7 lines), got: ${output}"; return 1; }
  local cwd="${lines[1]#cwd=}"
  [[ "${cwd}" == "${TMPDIR}/lok8s-keys-only-"* ]] || { echo "probe cwd ${cwd}"; return 1; }
  assert_line --index 0 "args=--capabilities"
  assert_line --index 2 "entries="
  assert_line --index 3 "secrets=${cwd}/store"
  assert_line --index 4 "disable=1"
  assert_line --index 5 "keys-only=0"
  assert_line --index 6 "stdin="
  [ ! -e "${cwd}" ]
  run ls -A "${TMPDIR}"
  assert_output ""
}

# An interrupt during the probe leaves no temp dir: the probe's subshell
# removes it on exit, and SIGINT ends that subshell through its exit trap.
# Job control (set -m) gives the probe its own process group with the
# default SIGINT action, as a terminal's Ctrl-C finds it.
@test "build::keys_only leaves no probe dir after a SIGINT" {
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp" PROBE_PID="${BATS_TEST_TMPDIR}/probe.pid"
  mkdir -p "${TMPDIR}" "${BATS_TEST_TMPDIR}/.kustomize/secrets.lok8s.dev/v1/secret"
  printf '#!/usr/bin/env bash\necho $$ > "${PROBE_PID}"\nexec sleep 30\n' > "${BATS_TEST_TMPDIR}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
  chmod +x "${BATS_TEST_TMPDIR}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
  run bash -c '
    set -m
    import() { :; }
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/verbose.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/libs/build"
    export PATH_BASE="'"${BATS_TEST_TMPDIR}"'"
    unset KUSTOMIZE_PLUGIN_HOME
    build::_keys_only_capable "${PATH_BASE}/.kustomize" &
    job=$!
    for _ in $(seq 100); do [[ -s "${PROBE_PID}" ]] && break; sleep 0.1; done
    [[ -s "${PROBE_PID}" ]] || { echo "the probe did not start"; exit 1; }
    ls -A "${TMPDIR}"
    kill -INT -- "-${job}"
    wait "${job}"
    echo "rc=$?"
  '
  assert_success
  assert_line --index 0 --regexp '^lok8s-keys-only-'
  refute_line "rc=0"
  run ls -A "${TMPDIR}"
  assert_output ""
}

# The output guard: a render that holds an object with the keys-only marker
# is never written into the domain's artifacts (Go: hasKeysOnlyMarker). The
# prior artifacts.yaml stays, no split is made, no temp file is left.
@test "build::artifacts refuses a render that holds the keys-only marker" {
  kustomize() {
    printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\n  name: app\n'
  }
  export -f kustomize
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  local split
  for split in 0 1; do
    echo "kind: Prior" > "${domain_dir}/artifacts.yaml"
    LOK8S_BUILD_SPLIT="${split}" LOK8S_BUILD_NO_SECRETS=1 run build::artifacts "test.lok8s.dev"
    assert_failure
    assert_output --partial "refusing to write test.lok8s.dev's render to ${domain_dir}/artifacts.yaml: it holds a keys-only Secret (secrets.lok8s.dev/keys-only); keys-only output goes to stdout only (lo build --keys-only)"
    [ "$(cat "${domain_dir}/artifacts.yaml")" = "kind: Prior" ]
    [ ! -d "${domain_dir}/artifacts" ]
    run bash -c "ls '${domain_dir}' | grep -c '^tmp\.'"
    assert_output "0"
  done
}

# The guard parses the final stream, after envsubst: a marker key that
# envsubst writes quoted, a flow-style annotations map and a List item are
# the same marker. A line rule finds none of the three. Go:
# TestArtifactsGuardParsesTheFinalStream.
@test "build::artifacts finds the keys-only marker in the parsed final stream" {
  template::envsubst_whitelist() { echo '${LOK8S_USER_MARKER}'; }
  export -f template::envsubst_whitelist
  export LOK8S_USER_MARKER='"secrets.lok8s.dev/keys-only"'
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  local render
  for render in \
    'apiVersion: v1\nkind: Secret\nmetadata:\n  annotations:\n    ${LOK8S_USER_MARKER}: "true"\n  name: app\n' \
    'apiVersion: v1\nkind: Secret\nmetadata: {name: app, annotations: {secrets.lok8s.dev/keys-only: "true"}}\n' \
    'apiVersion: v1\nkind: List\nitems:\n- kind: Secret\n  metadata:\n    name: app\n    annotations:\n      secrets.lok8s.dev/keys-only: "true"\n'; do
    export RENDER="${render}"
    kustomize() { printf '%b' "${RENDER}"; }
    export -f kustomize
    echo "kind: Prior" > "${domain_dir}/artifacts.yaml"
    run build::artifacts "test.lok8s.dev"
    assert_failure
    assert_output --partial "it holds a keys-only Secret (secrets.lok8s.dev/keys-only)"
    [ "$(cat "${domain_dir}/artifacts.yaml")" = "kind: Prior" ]
  done
}

# A render that does not parse as YAML cannot be checked for the marker,
# so the guard refuses it too.
@test "build::artifacts refuses a render that does not parse as YAML" {
  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\ndata: [x\n'; }
  export -f kustomize
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  echo "kind: Prior" > "${domain_dir}/artifacts.yaml"
  run build::artifacts "test.lok8s.dev"
  assert_failure
  assert_output --partial "refusing to write test.lok8s.dev's render to ${domain_dir}/artifacts.yaml: it does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)"
  [ "$(cat "${domain_dir}/artifacts.yaml")" = "kind: Prior" ]
  run bash -c "ls -A '${domain_dir}' | grep -c '^tmp\.'"
  assert_output "0"
}

# The prune of the pre-domain per-target dirs runs after every check: a
# render that the guard refuses, an empty render over a prior artifact and
# a failed render keep artifacts/old/; a good render removes it. Go:
# TestArtifactsPrunesOnlyAfterTheChecks.
@test "build::artifacts prunes the per-target dirs only after the checks" {
  set -o pipefail
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  local tc
  for tc in \
    'apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\n|0|kept' \
    '|0|kept' \
    'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n|1|kept' \
    'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n|0|pruned'; do
    export RENDER="${tc%%|*}" RENDER_RC
    RENDER_RC="${tc#*|}" RENDER_RC="${RENDER_RC%|*}"
    kustomize() { printf '%b' "${RENDER}"; return "${RENDER_RC}"; }
    export -f kustomize
    echo "kind: Prior" > "${domain_dir}/artifacts.yaml"
    mkdir -p "${domain_dir}/artifacts/old"
    echo "kind: Old" > "${domain_dir}/artifacts/old/artifacts.yaml"
    run build::artifacts "test.lok8s.dev"
    if [[ "${tc##*|}" == kept ]]; then
      assert_failure
      [ -f "${domain_dir}/artifacts/old/artifacts.yaml" ] || { echo "the case '${tc}' removed artifacts/old"; return 1; }
    else
      assert_success
      [ ! -e "${domain_dir}/artifacts/old" ] || { echo "a good render kept artifacts/old"; return 1; }
    fi
  done
}

@test "build::artifacts defaults to \$DOMAIN_NAME when no domain arg is given" {
  export DOMAIN_NAME="test.lok8s.dev"
  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize

  build::artifacts

  [ -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml" ]
}

@test "build::artifacts errors when the domain has no kustomization.yaml" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/kustomization.yaml"

  run build::artifacts "test.lok8s.dev"
  assert_failure
  assert_output --partial "has no kustomization.yaml"
}

@test "build::artifacts errors for a nonexistent domain" {
  run build::artifacts "nope.lok8s.dev"
  assert_failure
  assert_output --partial "has no kustomization.yaml"
}

@test "build::artifacts cleans stale per-target artifact dirs from the pre-domain era" {
  # A leftover artifacts/<target>/artifacts.yaml from the old per-target build
  # must be removed so status/hooks don't read phantom output.
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${domain_dir}/artifacts/stale"
  echo "kind: ConfigMap" > "${domain_dir}/artifacts/stale/artifacts.yaml"
  # A non-target file directly under artifacts/ must be PRESERVED.
  echo "queued" > "${domain_dir}/artifacts/.cache-queue"

  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize

  build::artifacts "test.lok8s.dev"

  [ ! -d "${domain_dir}/artifacts/stale" ]
  [ -f "${domain_dir}/artifacts/.cache-queue" ]
}

@test "build::artifacts survives a deploy domain (no cluster.lok8s.yaml) under set -e" {
  # Regression: a DEPLOY domain (deploy.lok8s.yaml + clusterRef) has NO
  # cluster.lok8s.yaml, and its KUBECONFIG resolves to a secret kubeconfig that
  # may not be on disk yet. build::_resolve_api must guard the missing-file read
  # so it does not abort the whole build under `set -euo pipefail`. bats does not
  # run under errexit, so this case runs the build inside an explicit
  # `set -euo pipefail` subshell to mirror the real runtime.
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy.lok8s.dev"
  mkdir -p "${domain_dir}/targets/app"
  cp "${FIXTURES_DIR}/targets/platform/kustomization.yaml" "${domain_dir}/targets/app/"
  cp "${FIXTURES_DIR}/targets/platform/deployment.yaml" "${domain_dir}/targets/app/"
  cat > "${domain_dir}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./targets/app
YAML
  # Deliberately NO cluster.lok8s.yaml here — that is what makes it a deploy domain.

  run bash -c '
    set -euo pipefail
    import() { :; }
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/verbose.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/domain.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/targets.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/keysonly.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/libs/build"
    export PATH_BASE="'"${BATS_TEST_TMPDIR}"'"
    export PATH_CLUSTERS="'"${BATS_TEST_TMPDIR}"'/clusters"
    # KUBECONFIG points at a not-yet-fetched secret kubeconfig (does not exist).
    export KUBECONFIG="'"${BATS_TEST_TMPDIR}"'/.kubeconfig/secret.deploy.lok8s.dev.yaml"
    # Keep the test focused on the kubeconfig-resolution crash, not envsubst.
    template::envsubst_whitelist() { echo ""; }
    kustomize() { printf "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"; }
    export -f template::envsubst_whitelist kustomize
    build::artifacts "deploy.lok8s.dev"
  '
  assert_success
  [ -f "${domain_dir}/artifacts.yaml" ]
}

@test "build::artifacts preserves a prior artifact when the build fails" {
  # Regression: a direct `> artifacts.yaml` truncates the target BEFORE the
  # pipeline runs, so a kustomize failure (under set -o pipefail) would clobber a
  # prior good artifact with an empty/partial one. The build must render to a
  # temp file and promote only on success. Run under the real errexit+pipefail.
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: good\n' > "${domain_dir}/artifacts.yaml"

  run bash -c '
    set -euo pipefail
    import() { :; }
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/verbose.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/domain.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/targets.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/template.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/keysonly.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/libs/build"
    export PATH_BASE="'"${BATS_TEST_TMPDIR}"'"
    export PATH_CLUSTERS="'"${BATS_TEST_TMPDIR}"'/clusters"
    template::envsubst_whitelist() { echo ""; }
    kustomize() { echo "boom" >&2; return 1; }   # build failure
    export -f template::envsubst_whitelist kustomize
    build::artifacts "test.lok8s.dev"
  '
  assert_failure
  assert_output --partial "kustomize build failed"
  # The prior good artifact is untouched (not truncated/clobbered).
  run cat "${domain_dir}/artifacts.yaml"
  assert_output --partial "name: good"
}

# --- build::_export_secrets_path (per-instance secret isolation) ---

@test "build::_export_secrets_path redirects PATH_SECRETS to a domain's own store" {
  local dd="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"

  # No per-domain store: must be a clean no-op AND exit 0 — it's the helper's
  # last command, so a non-zero would abort the build under set -e.
  run build::_export_secrets_path "${dd}"
  assert_success

  # Store present: the secrets plugin is pointed at it (never the flat store).
  export PATH_SECRETS="${BATS_TEST_TMPDIR}/.secrets"
  mkdir -p "${dd}/secrets"
  build::_export_secrets_path "${dd}"
  [ "${PATH_SECRETS}" = "${dd}/secrets" ]
}

# --- unchanged render must not touch the artifact (mtime IS the watch signal) ---

@test "build::artifacts leaves a byte-identical artifacts.yaml untouched (no mtime churn)" {
  kustomize() {
    echo "apiVersion: v1"
    echo "kind: ConfigMap"
    echo "metadata:"
    echo "  name: stable-cm"
  }
  export -f kustomize

  build::artifacts "test.lok8s.dev"
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  [ -f "${domain_dir}/artifacts.yaml" ]

  # Age the artifact so a rewrite is detectable, then rebuild IDENTICAL content.
  touch -d '2020-01-01 00:00:00' "${domain_dir}/artifacts.yaml"
  local before
  before=$(stat -c '%Y' "${domain_dir}/artifacts.yaml")

  build::artifacts "test.lok8s.dev"

  local after
  after=$(stat -c '%Y' "${domain_dir}/artifacts.yaml")
  # An unchanged render must be a filesystem no-op: file watchers (the
  # Tiltfile reads this file during eval, and the eval runs the build) treat
  # mtime as change, and an unconditional rewrite self-triggers the watcher
  # forever — measured live 2026-08-07 as an eval loop that starved every
  # queued Tilt build for hours.
  [ "${before}" = "${after}" ]

  # ...and a CHANGED render still promotes.
  kustomize() {
    echo "apiVersion: v1"
    echo "kind: ConfigMap"
    echo "metadata:"
    echo "  name: changed-cm"
  }
  export -f kustomize
  build::artifacts "test.lok8s.dev"
  run cat "${domain_dir}/artifacts.yaml"
  assert_output --partial "changed-cm"
}

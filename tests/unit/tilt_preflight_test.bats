#!/usr/bin/env bats
# tilt_preflight_test.bats — gating tests for `lo tilt preflight`.
#
# kapply::preflight (the sweep itself) is covered in kapply_test.bats; here
# we pin the GATE in tilt::preflight: the kill switch, the non-kind driver
# refusal, the LOK8S_FORCE_CLEAR_TERMINATING override, and the --age
# passthrough. kapply::preflight is stubbed to a recorder — these tests never
# touch kubectl.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v yq &>/dev/null || skip "yq required (domain::driver reads the cluster spec)"

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"

  # Minimal :args shim (same rationale as tilt_ci_test.bats: argsh builtins
  # are not loaded when a lib is sourced in bats): populate age/crds/
  # crd_allow from their --flag <v> | --flag=<v> | -short <v> forms.
  :args() {
    shift
    while (( $# )); do
      case "$1" in
        --age=*|--age|-a)
          if [[ "$1" == *=* ]]; then age="${1#*=}"; else age="${2:-}"; shift; fi ;;
        --crds=*|--crds|-c)
          if [[ "$1" == *=* ]]; then crds="${1#*=}"; else crds="${2:-}"; shift; fi ;;
        --crd-allow=*|--crd-allow)
          if [[ "$1" == *=* ]]; then crd_allow="${1#*=}"; else crd_allow="${2:-}"; shift; fi ;;
      esac
      shift
    done
  }
  export -f :args
  source "${_PROJECT_ROOT}/.lok8s/libs/tilt"

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}/dev.test" "${PATH_CLUSTERS}/prod.test"
  echo 'kind: Lo' > "${PATH_CLUSTERS}/dev.test/cluster.lok8s.yaml"
  echo 'kind: KubeOne' > "${PATH_CLUSTERS}/prod.test/cluster.lok8s.yaml"

  SWEEP_LOG="${BATS_TEST_TMPDIR}/sweep.log"; : > "${SWEEP_LOG}"
  kapply::preflight() { echo "kapply::preflight $*" >> "${SWEEP_LOG}"; cat >/dev/null; }
  export -f kapply::preflight
  unset LOK8S_PREFLIGHT LOK8S_FORCE_CLEAR_TERMINATING DOMAIN_NAME
}

teardown() { teardown_tmpdir; }

@test "preflight gate: LOK8S_PREFLIGHT=0 disables the sweep entirely" {
  export DOMAIN_NAME=dev.test LOK8S_PREFLIGHT=0
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  [[ ! -s "${SWEEP_LOG}" ]]
}

@test "preflight gate: LOK8S_PREFLIGHT=false disables too (not only literal 0)" {
  export DOMAIN_NAME=dev.test LOK8S_PREFLIGHT=false
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  [[ ! -s "${SWEEP_LOG}" ]]
}

@test "preflight gate: non-kind driver refuses without the override" {
  export DOMAIN_NAME=prod.test
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  assert_output --partial 'not force-clearing'
  [[ ! -s "${SWEEP_LOG}" ]]
}

@test "preflight gate: LOK8S_FORCE_CLEAR_TERMINATING=1 overrides the driver refusal" {
  export DOMAIN_NAME=prod.test LOK8S_FORCE_CLEAR_TERMINATING=1
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  run cat "${SWEEP_LOG}"
  assert_output --partial 'kapply::preflight'
}

@test "preflight gate: lo driver sweeps and --age passes through" {
  export DOMAIN_NAME=dev.test
  run tilt::preflight --age 120 <<< 'kind: ConfigMap'
  assert_success
  run cat "${SWEEP_LOG}"
  assert_output --partial 'kapply::preflight --age 120'
}

@test "preflight spec: no spec section → no flags passed (kapply owns the defaults)" {
  export DOMAIN_NAME=dev.test
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  run cat "${SWEEP_LOG}"
  assert_output --partial 'kapply::preflight'
  refute_output --partial ' --'
}

@test "preflight spec: spec.tilt.preflight.enabled=false disables the sweep" {
  export DOMAIN_NAME=dev.test
  cat > "${PATH_CLUSTERS}/dev.test/cluster.lok8s.yaml" <<'YAML'
kind: Lo
spec:
  tilt:
    preflight:
      enabled: false
YAML
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  [[ ! -s "${SWEEP_LOG}" ]]
}

@test "preflight spec: scalar shorthand 'preflight: false' disables too" {
  export DOMAIN_NAME=dev.test
  cat > "${PATH_CLUSTERS}/dev.test/cluster.lok8s.yaml" <<'YAML'
kind: Lo
spec:
  tilt:
    preflight: false
YAML
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  [[ ! -s "${SWEEP_LOG}" ]]
}

@test "preflight spec: age/crds/crdForceAllow flow through from the cluster spec" {
  export DOMAIN_NAME=dev.test
  cat > "${PATH_CLUSTERS}/dev.test/cluster.lok8s.yaml" <<'YAML'
kind: Lo
spec:
  tilt:
    preflight:
      age: 60
      crds: force
      crdForceAllow:
        - kubehzclusters.kubehz.dev
        - kubehzmigrations.kubehz.dev
YAML
  run tilt::preflight <<< 'kind: ConfigMap'
  assert_success
  run cat "${SWEEP_LOG}"
  assert_output --partial '--age 60'
  assert_output --partial '--crds force'
  assert_output --partial '--crd-allow kubehzclusters.kubehz.dev,kubehzmigrations.kubehz.dev'
}

@test "preflight spec: an explicit CLI flag outranks the cluster spec" {
  export DOMAIN_NAME=dev.test
  cat > "${PATH_CLUSTERS}/dev.test/cluster.lok8s.yaml" <<'YAML'
kind: Lo
spec:
  tilt:
    preflight:
      crds: skip
YAML
  run tilt::preflight --crds drain <<< 'kind: ConfigMap'
  assert_success
  run cat "${SWEEP_LOG}"
  assert_output --partial '--crds drain'
  refute_output --partial '--crds skip'
}

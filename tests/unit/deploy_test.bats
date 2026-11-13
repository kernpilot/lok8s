#!/usr/bin/env bats
# deploy_test.bats — unit tests for .lok8s/libs/deploy
#
# Domain-based deploy: `lo deploy` applies the SINGLE
# clusters/<domain>/artifacts.yaml — CRDs first (wait Established), then the
# rest (wait ready). `-l/--label key=value` applies only the matching subset.
# Real yq drives CRD extraction + label filtering; kubectl is mocked (no cluster).

setup() {
  load "../test_helper"
  setup_tmpdir
  export LOK8S_NONINTERACTIVE=1

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/targets.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/keysonly.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/deploy"

  # These tests exercise apply/filter logic, not readiness polling — stub the
  # scoped wait so it doesn't loop on the fake kubectl.
  kapply::wait_ready() { :; }
  export -f kapply::wait_ready

  # Mock kubectl: consume the piped manifest, record verbs on stdout.
  kubectl() {
    case "$1" in
      apply) cat >/dev/null 2>&1 || true; echo "applied" ;;
      wait)  echo "waited: $*" ;;
      *)     echo "kubectl $*" ;;
    esac
  }
  export -f kubectl

  # The CRD extraction + label filter use real yq expressions; skip if absent.
  if ! command -v yq &>/dev/null; then
    skip "yq not available"
  fi

  # Build a domain with a single composed artifact: one CRD, one system
  # Namespace, one platform Deployment.
  local domain="test.lok8s.dev"
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/${domain}"
  mkdir -p "${domain_dir}"

  cat > "${domain_dir}/artifacts.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.test.lok8s.dev
---
apiVersion: v1
kind: Namespace
metadata:
  name: networking
  labels:
    lok8s.dev/type: system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: default
  labels:
    lok8s.dev/type: platform
YAML
}

teardown() {
  teardown_tmpdir
}

# main::deploy is gated by argsh's `:args` builtin (absent when a lib is sourced
# in bats) and calls _resolve_kubeconfig_for_domain. Shim `:args` to populate
# `label` from -l/--label exactly as real argsh does (verified: `'label|l'`
# parses `-l v`, `--label v`, `--label=v` identically), stub the kubeconfig
# resolver, and stub the two apply entrypoints so the tests assert ROUTING +
# the key=value guard, not a real deploy.
_shim_main_deploy() {
  :args() {
    shift  # drop the description (first positional)
    while (( $# )); do
      case "$1" in
        -l|--label)     label="${2:-}"; shift ;;
        -l=*|--label=*) label="${1#*=}" ;;
      esac
      shift
    done
  }
  _resolve_kubeconfig_for_domain() { :; }
  deploy::apply()          { echo "route=apply domain=${1}"; }
  deploy::apply_filtered() { echo "route=filtered domain=${1} key=${2} val=${3}"; }
  export -f :args _resolve_kubeconfig_for_domain deploy::apply deploy::apply_filtered
  export DOMAIN_NAME="test.lok8s.dev"
}

# --- deploy::apply ---

@test "deploy::apply applies the single domain artifact" {
  run deploy::apply "test.lok8s.dev"
  assert_success
  assert_output --partial "applied"
}

@test "deploy::apply applies CRDs first (waits for Established)" {
  run deploy::apply "test.lok8s.dev"
  assert_success
  # The CRD in the artifact is applied + waited on before the rest.
  assert_output --partial "crd/widgets.test.lok8s.dev"
}

@test "deploy::apply errors when the artifact is missing (build not run)" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  run deploy::apply "test.lok8s.dev"
  assert_failure
  assert_output --partial "run 'lo build' first"
}

@test "deploy::apply is a graceful no-op on an artifact with no objects" {
  printf '# just a comment\n---\n' > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  run deploy::apply "test.lok8s.dev"
  assert_success
  refute_output --partial "applied"
}

# lo deploy refuses an artifact with the keys-only marker (a Secret with
# key names and empty values, from lo build --keys-only): applied, it would
# empty each value in the cluster. The check parses the artifact, so a
# quoted marker key is the same marker, and an artifact that does not parse
# is refused too. Nothing reaches kubectl. Go: TestApplyRefusesAKeysOnlyArtifact.
@test "deploy::apply and deploy::apply_filtered refuse a keys-only artifact" {
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  cat >> "${artifact}" <<'YAML'
---
apiVersion: v1
kind: Secret
metadata:
  name: app
  labels:
    lok8s.dev/type: platform
  annotations:
    "secrets.lok8s.dev/keys-only": "true"
data:
  TOKEN: ""
YAML
  run deploy::apply "test.lok8s.dev"
  assert_failure
  assert_output --partial "refusing to apply test.lok8s.dev: ${artifact} holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one — run 'lo build' again"
  refute_output --partial "applied"
  refute_output --partial "waited"
  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
  assert_failure
  assert_output --partial "holds a keys-only Secret (secrets.lok8s.dev/keys-only)"
  refute_output --partial "applied"

  printf 'apiVersion: v1\nkind: ConfigMap\ndata: [x\n' > "${artifact}"
  run deploy::apply "test.lok8s.dev"
  assert_failure
  assert_output --partial "refusing to apply test.lok8s.dev: ${artifact} does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)"
  refute_output --partial "applied"
  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
  assert_failure
  assert_output --partial "does not parse as YAML"
  refute_output --partial "applied"
}

# An artifact that cannot be read, and a yq that cannot run the check (one
# before v4.47.1 does not know --yaml-fix-merge-anchor-to-spec, or no yq),
# each get their own message: none is an artifact that does not parse. Go:
# TestApplyRefusesAKeysOnlyArtifact (the unreadable case; the Go check runs
# without yq).
@test "deploy::apply names an unreadable artifact, an old yq and no yq, not a parse error" {
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  if (( EUID != 0 )); then
    chmod 000 "${artifact}"
    run deploy::apply "test.lok8s.dev"
    chmod 644 "${artifact}"
    assert_failure
    assert_output --partial "refusing to apply test.lok8s.dev: ${artifact} cannot be read, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)"
    refute_output --partial "does not parse"
    refute_output --partial "applied"
  fi

  yq() {
    local a
    for a in "$@"; do
      if [[ "${a}" == --yaml-fix-merge-anchor-to-spec ]]; then
        echo "Error: unknown flag: --yaml-fix-merge-anchor-to-spec" >&2
        return 1
      fi
    done
    command yq "$@"
  }
  run deploy::apply "test.lok8s.dev"
  assert_failure
  assert_output --partial "refusing to apply test.lok8s.dev: ${artifact} cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only): the installed yq does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.47.1 or later (b install installs the pinned yq)."
  refute_output --partial "does not parse"
  refute_output --partial "applied"

  # No yq on the PATH: the message names that, not an old yq. The PATH
  # keeps cat, which reads the artifact.
  unset -f yq
  mkdir -p "${BATS_TEST_TMPDIR}/no-yq"
  ln -s "$(command -v cat)" "${BATS_TEST_TMPDIR}/no-yq/cat"
  _deploy_without_yq() { local PATH="${BATS_TEST_TMPDIR}/no-yq"; deploy::apply "$@"; }
  run _deploy_without_yq "test.lok8s.dev"
  assert_failure
  assert_output --partial "refusing to apply test.lok8s.dev: ${artifact} cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only): yq is not installed. Install yq v4.47.1 or later (b install installs the pinned yq)."
  refute_output --partial "does not know the flag"
  refute_output --partial "applied"
}

# lo deploy reads the artifact once, checks that copy and applies that copy,
# on both routes. The yq spy writes a keys-only artifact over the file at the
# first yq call of the check, after the read: a second read of the file (for
# the apply) would get it. kubectl logs every call; a marked artifact and one
# that does not parse get no kubectl call at all. Go:
# TestApplyAppliesTheCopyThatPassedTheCheck, TestApplyRefusesAKeysOnlyArtifact.
@test "deploy::apply and deploy::apply_filtered apply the copy that passed the check" {
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  local clean="${BATS_TEST_TMPDIR}/clean.yaml"
  cp "${artifact}" "${clean}"
  export DEPLOY_LOG="${BATS_TEST_TMPDIR}/kubectl.log" DEPLOY_SWAP=""
  kubectl() {
    echo "kubectl $*" >> "${DEPLOY_LOG}"
    case "$1" in
      apply) cat >> "${DEPLOY_LOG}"; echo "applied" ;;
      wait)  echo "waited" ;;
    esac
  }
  yq() {
    local a
    for a in "$@"; do
      if [[ "${a}" == --yaml-fix-merge-anchor-to-spec && -n "${DEPLOY_SWAP}" && ! -e "${DEPLOY_SWAP}" ]]; then
        : > "${DEPLOY_SWAP}"
        printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: swapped\n  labels:\n    lok8s.dev/type: platform\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\ndata:\n  TOKEN: ""\n' > "${artifact}"
        break
      fi
    done
    command yq "$@"
  }

  local route
  for route in apply filtered; do
    cp "${clean}" "${artifact}"
    rm -f "${DEPLOY_LOG}"
    DEPLOY_SWAP="${BATS_TEST_TMPDIR}/swapped.${route}"
    if [[ "${route}" == apply ]]; then
      run deploy::apply "test.lok8s.dev"
    else
      run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
    fi
    assert_success
    [[ -e "${DEPLOY_SWAP}" ]]
    run cat "${DEPLOY_LOG}"
    assert_output --partial "kubectl apply"
    assert_output --partial "name: test-app"
    refute_output --partial "swapped"
    refute_output --partial "keys-only"
  done
  DEPLOY_SWAP=""

  local bad
  for bad in marker parse; do
    if [[ "${bad}" == marker ]]; then
      { cat "${clean}"; printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\n'; } > "${artifact}"
    else
      printf 'apiVersion: v1\nkind: ConfigMap\ndata: [x\n' > "${artifact}"
    fi
    rm -f "${DEPLOY_LOG}"
    run deploy::apply "test.lok8s.dev"
    assert_failure
    assert_output --partial "refusing to apply test.lok8s.dev"
    [[ ! -e "${DEPLOY_LOG}" ]]
    run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
    assert_failure
    assert_output --partial "refusing to apply test.lok8s.dev"
    [[ ! -e "${DEPLOY_LOG}" ]]
  done
}

# Tracing is off where lo holds the bytes of the artifact (the rules at the
# top of utils/keysonly.sh): under bash -x, and with BASH_XTRACEFD on a
# file, no Secret value of the artifact is in stdout, stderr or the trace.
# The kubectl stub shows that the value was applied.
@test "deploy::apply and deploy::apply_filtered trace no Secret value" {
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  labels:\n    lok8s.dev/type: platform\nstringData:\n  TOKEN: xtrace-canary-deploy\n' >> "${artifact}"
  export DEPLOY_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  kubectl() {
    case "$1" in
      apply) cat >> "${DEPLOY_LOG}"; echo "applied" ;;
      wait)  echo "waited" ;;
    esac
  }
  assert_no_trace_leak xtrace-canary-deploy deploy::apply "test.lok8s.dev"
  grep -qF xtrace-canary-deploy "${DEPLOY_LOG}"
  rm -f "${DEPLOY_LOG}"
  assert_no_trace_leak xtrace-canary-deploy deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
  grep -qF xtrace-canary-deploy "${DEPLOY_LOG}"
}

# The last value of each document is a `|+` block scalar that ends in
# newlines: kubectl gets the CRDs, the artifact and the subset byte for
# byte, with the newlines (Go: TestApplyKeepsALastKeepChompedValue).
@test "deploy::apply and deploy::apply_filtered keep the final newlines of a |+ value" {
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  local crd=$'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.test.lok8s.dev\n  annotations:\n    note: |+\n      x\n\n'
  local cm=$'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n  labels:\n    lok8s.dev/type: platform\ndata:\n  tail: |+\n    y\n\n\n'
  printf '%s---\n%s' "${crd}" "${cm}" > "${artifact}"
  export CALLS="${BATS_TEST_TMPDIR}/apply"
  # kapply::apply runs in a pipeline (a subshell): one file per call.
  kapply::apply() { local n=1; while [[ -e "${CALLS}.${n}" ]]; do n=$(( n + 1 )); done; cat > "${CALLS}.${n}"; }
  deploy::wait_crds() { :; }
  run deploy::apply "test.lok8s.dev"
  assert_success
  cmp "${CALLS}.1" <(printf '%s' "${crd}")
  cmp "${CALLS}.2" "${artifact}"
  [ ! -e "${CALLS}.3" ]
  rm -f "${CALLS}".*
  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
  assert_success
  cmp "${CALLS}.1" <(printf '%s' "${cm}")
  [ ! -e "${CALLS}.2" ]
}

# yq parses the artifact (Go: TestApplySplitsAtCRLFSeparators and
# TestApplyKeepsTheEndOfTheLastDocument). With CRLF line ends, the label
# subset holds the selected document only, and not the documents after it.
# A last `|+` value with no final newline ("abc") keeps its value: no
# newline is added to it.
@test "deploy::apply_filtered splits at CRLF separators and keeps a last value at the end of the file" {
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  export CALLS="${BATS_TEST_TMPDIR}/apply"
  kapply::apply() { local n=1; while [[ -e "${CALLS}.${n}" ]]; do n=$(( n + 1 )); done; cat > "${CALLS}.${n}"; }
  deploy::wait_crds() { :; }
  printf 'apiVersion: v1\r\nkind: ConfigMap\r\nmetadata:\r\n  name: a\r\n  labels:\r\n    lok8s.dev/type: platform\r\n---\r\napiVersion: v1\r\nkind: ConfigMap\r\nmetadata:\r\n  name: b\r\n  labels:\r\n    lok8s.dev/type: other\r\n' > "${artifact}"
  local label want
  for label in platform other; do
    want=a; [[ "${label}" == platform ]] || want=b
    rm -f "${CALLS}".*
    run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "${label}"
    assert_success
    [ "$(yq ea -r '[.metadata.name] | join(",")' "${CALLS}.1")" = "${want}" ]
    [ ! -e "${CALLS}.2" ]
  done
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  labels:\n    lok8s.dev/type: platform\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n  labels:\n    lok8s.dev/type: platform\nstringData:\n  password: |+\n    abc' > "${artifact}"
  rm -f "${CALLS}".*
  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
  assert_success
  [ "$(yq ea -r '[.metadata.name] | join(",")' "${CALLS}.1")" = "a,s" ]
  [ "$(yq -o json 'select(.kind == "Secret") | .stringData.password' "${CALLS}.1")" = '"abc"' ]
  rm -f "${CALLS}".*
  run deploy::apply "test.lok8s.dev"
  assert_success
  cmp "${CALLS}.1" "${artifact}"
}

# lo deploy hands the artifact to yq and kubectl through pipes, not
# here-strings: bash puts a here-string larger than a pipe buffer (64 KiB)
# into a file in TMPDIR, which a kill can leave behind. A deploy of a large
# artifact creates no file in TMPDIR: the mtime of the directory stays.
# The control shows that a here-string of the same artifact changes it.
@test "deploy::apply and deploy::apply_filtered put no copy of a large artifact in TMPDIR" {
  local artifact="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  { printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: big\n  labels:\n    lok8s.dev/type: platform\nstringData:\n  BLOB: '
    head -c 98304 /dev/zero | tr '\0' 'a'
    printf '\n'
  } >> "${artifact}"
  [ "$(stat -c %s "${artifact}")" -gt 98304 ]
  export TMPDIR="${BATS_TEST_TMPDIR}/private-tmp" APPLIED="${BATS_TEST_TMPDIR}/applied"
  mkdir -m 700 "${TMPDIR}"
  # kapply::apply keeps its exit status in a mktemp file: it is not under test.
  kapply::apply() { wc -c >> "${APPLIED}"; }
  deploy::wait_crds() { :; }
  local before after
  before="$(stat -c %y "${TMPDIR}")"
  run deploy::apply "test.lok8s.dev"
  assert_success
  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
  assert_success
  after="$(stat -c %y "${TMPDIR}")"
  [ "${before}" = "${after}" ] || { echo "TMPDIR changed: ${before} -> ${after}"; return 1; }
  [ "$(sort -n "${APPLIED}" | tail -1)" -gt 98304 ]
  # Control: a here-string of the artifact goes through TMPDIR.
  local big; big="$(cat "${artifact}")"
  cat <<< "${big}" > /dev/null
  [ "$(stat -c %y "${TMPDIR}")" != "${after}" ]
}

# --- deploy::apply_filtered ---

@test "deploy::apply_filtered applies only the matching subset (full label key)" {
  # Record exactly which objects reach kubectl apply.
  kubectl() {
    case "$1" in
      apply) local m; m=$(cat); grep -q 'kind: Deployment' <<<"${m}" && echo "applied:deployment"; grep -q 'kind: Namespace' <<<"${m}" && echo "applied:namespace"; grep -q 'kind: CustomResourceDefinition' <<<"${m}" && echo "applied:crd"; return 0 ;;
      wait)  echo "waited" ;;
      *)     return 0 ;;
    esac
  }
  export -f kubectl

  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "platform"
  assert_success
  assert_output --partial "applied:deployment"
  refute_output --partial "applied:namespace"
  refute_output --partial "applied:crd"
}

@test "deploy::apply_filtered warns and exits 0 when nothing matches" {
  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "nonexistent"
  assert_success
  assert_output --partial "no objects match"
}

@test "deploy::apply_filtered errors when the artifact is missing" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml"
  run deploy::apply_filtered "test.lok8s.dev" "lok8s.dev/type" "system"
  assert_failure
  assert_output --partial "run 'lo build' first"
}

@test "deploy::apply_filtered rejects injection in label key" {
  run deploy::apply_filtered "test.lok8s.dev" "key; rm -rf /" "value"
  assert_failure
  assert_output --partial "Invalid label selector"
}

@test "deploy::apply_filtered rejects injection in label value" {
  run deploy::apply_filtered "test.lok8s.dev" "type" "value; echo pwned"
  assert_failure
  assert_output --partial "Invalid label selector"
}

# --- main::deploy -l/--label parsing (routing + key=value guard) ---

@test "main::deploy -l key=value routes to apply_filtered" {
  _shim_main_deploy
  run main::deploy -l lok8s.dev/name=zitadel
  assert_success
  assert_output --partial "route=filtered domain=test.lok8s.dev key=lok8s.dev/name val=zitadel"
}

@test "main::deploy without -l routes to apply (full artifact)" {
  _shim_main_deploy
  run main::deploy
  assert_success
  assert_output --partial "route=apply domain=test.lok8s.dev"
}

@test "main::deploy -l =value (empty key) errors expected key=value" {
  _shim_main_deploy
  run main::deploy -l =value
  assert_failure
  assert_output --partial "expected key=value"
}

@test "main::deploy -l foo (no =) errors expected key=value" {
  _shim_main_deploy
  run main::deploy -l foo
  assert_failure
  assert_output --partial "expected key=value"
}

@test "main::deploy refuses a spec file with a second document" {
  # The two implementations read such a file as two different specs. Go twin:
  # TestDeployRefusesASecondDocument (internal/cli).
  _shim_main_deploy
  local spec
  for spec in cluster.lok8s.yaml deploy.lok8s.yaml; do
    rm -f "${PATH_CLUSTERS}/test.lok8s.dev"/*.lok8s.yaml
    printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: fixture\n---\n' > "${PATH_CLUSTERS}/test.lok8s.dev/${spec}"
    run main::deploy
    assert_failure
    assert_output "[error] ${PATH_CLUSTERS}/test.lok8s.dev/${spec} holds more than one YAML document. Remove line 5 (a '---' line after the spec)."
  done
}

@test "main::deploy -l foo= (empty value) errors expected key=value" {
  _shim_main_deploy
  run main::deploy -l foo=
  assert_failure
  assert_output --partial "expected key=value"
}

# --- deploy::wait_crds ---

@test "deploy::wait_crds waits for CRDs to become established" {
  kubectl() {
    if [[ "$1" == "wait" ]]; then
      echo "condition met"
    fi
  }
  export -f kubectl
  yq() { echo "widgets.test.lok8s.dev"; }
  export -f yq

  run deploy::wait_crds "apiVersion: v1"
  assert_success
}

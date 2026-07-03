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

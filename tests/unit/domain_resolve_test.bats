#!/usr/bin/env bats
# domain_resolve_test.bats — utils/domain.sh: the ONE domain-resolution point.
#
# Precedence contract: --domain flag (explicit arg) > DOMAIN_NAME env >
# clusters/.active > "". Born from `lo registry up` silently running against
# a KubeOne prod spec because `.active` clobbered the DOMAIN_NAME env var
# (the env was dead — only the flag beat `.active`), then dying three layers
# down on a spec field KubeOne clusters never carry.

setup() {
  load "../test_helper"
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"

  _TMP="$(mktemp -d)"
  export PATH_CLUSTERS="${_TMP}/clusters"
  mkdir -p "${PATH_CLUSTERS}/kind-dom" "${PATH_CLUSTERS}/cloud-dom" "${PATH_CLUSTERS}/deploy-dom"
  cat > "${PATH_CLUSTERS}/kind-dom/cluster.lok8s.yaml" <<'EOF'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: kind-dom }
spec: { network: { name: kind-dom, cidr: "10.9.9.0/24" } }
EOF
  cat > "${PATH_CLUSTERS}/cloud-dom/cluster.lok8s.yaml" <<'EOF'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: cloud-dom }
spec: {}
EOF
  touch "${PATH_CLUSTERS}/deploy-dom/deploy.lok8s.yaml"
  unset DOMAIN_NAME
}

teardown() {
  rm -rf "${_TMP}"
}

# ── domain::resolve precedence ─────────────────────────────────────────────────

@test "explicit value wins over everything" {
  export DOMAIN_NAME="cloud-dom"
  echo "kind-dom" > "${PATH_CLUSTERS}/.active"
  run domain::resolve "deploy-dom"
  assert_success
  assert_output "deploy-dom"
}

@test "DOMAIN_NAME env wins over .active (with a notice on STDERR only)" {
  export DOMAIN_NAME="kind-dom"
  echo "cloud-dom" > "${PATH_CLUSTERS}/.active"
  # --separate-stderr: the notice MUST NOT leak into stdout — callers do
  # `domain="$(domain::resolve)"` and a stdout notice would corrupt the value.
  run --separate-stderr domain::resolve
  assert_success
  assert_output "kind-dom"
  # The conflict must be VISIBLE — a silently-dead layer is how the original
  # incident happened (env exported, .active won, nobody knew).
  [[ "${stderr}" == *"notice:"* ]] || fail "expected a conflict notice on stderr, got: ${stderr}"
  [[ "${stderr}" == *"cloud-dom"* ]] || fail "the notice should name the losing .active domain"
}

@test "env == .active resolves quietly" {
  export DOMAIN_NAME="kind-dom"
  echo "kind-dom" > "${PATH_CLUSTERS}/.active"
  run domain::resolve
  assert_success
  assert_output "kind-dom"
}

@test ".active is the default when no env is set" {
  echo "cloud-dom" > "${PATH_CLUSTERS}/.active"
  run domain::resolve
  assert_success
  assert_output "cloud-dom"
}

@test "invalid .active content is warned about, ignored, and falls back to the default" {
  echo '../evil; rm -rf /' > "${PATH_CLUSTERS}/.active"
  run --separate-stderr domain::resolve
  assert_success
  # Not just "no evil": the resolution must land on the terminal default.
  assert_output "lok8s.dev"
  [[ "${stderr}" == *"warning:"* ]] || fail "expected an invalid-.active warning on stderr, got: ${stderr}"
}

@test "nothing set resolves to the framework default (lok8s.dev)" {
  # The terminal default lives at the END of the chain — a script-level
  # `: "${DOMAIN_NAME:=lok8s.dev}"` used to sit BEFORE it and permanently
  # outranked .active (env and baked default were indistinguishable).
  run domain::resolve
  assert_success
  assert_output "lok8s.dev"
}

# ── domain::driver ─────────────────────────────────────────────────────────────

@test "driver is the lowercased spec kind" {
  run domain::driver "kind-dom"
  assert_success
  assert_output "lo"
  run domain::driver "cloud-dom"
  assert_success
  assert_output "kubeone"
}

@test "deploy-only domains report the deploy pseudo-driver" {
  run domain::driver "deploy-dom"
  assert_success
  assert_output "deploy"
}

@test "unknown domain fails" {
  run domain::driver "nope"
  assert_failure
}

# ── domain::require_driver ─────────────────────────────────────────────────────

@test "require_driver passes on a matching driver" {
  run domain::require_driver lo "kind-dom" "registry management"
  assert_success
}

@test "require_driver names BOTH drivers and the operation on mismatch" {
  run domain::require_driver lo "cloud-dom" "registry management"
  assert_failure
  # The whole point: the error must say what's wrong IN DRIVER TERMS —
  # not let the op die later on a missing spec.network field.
  assert_output --partial "kubeone"
  assert_output --partial "registry management"
  assert_output --partial "--domain"
}

@test "require_driver fails usefully on an unknown domain" {
  run domain::require_driver lo "nope" "the image cache"
  assert_failure
  assert_output --partial "not found"
}

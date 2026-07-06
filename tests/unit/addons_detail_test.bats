#!/usr/bin/env bats
# addons_detail_test.bats — unit tests for `lo addons --detail` (installed
# inventory) and the in-repo config-help catalog (addons::_config_hint).

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }; export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/libs/addons"
}

teardown() { teardown_tmpdir; }

_spec() {
  local d="$1"
  mkdir -p "${PATH_CLUSTERS}/${d}"
  cat > "${PATH_CLUSTERS}/${d}/cluster.lok8s.yaml"
}

# =============================================================================
# addons::detail — installed inventory (spec.bootstrap ∩ .lok8s/addons/)
# =============================================================================

@test "detail inventories framework addons with category + type + version + hint" {
  # Use the REAL addon tree so category/version come from the shipped labels.
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  mkdir -p "${PATH_CLUSTERS}/inv/targets/networking"
  _spec inv <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: inv }
spec:
  bootstrap:
    - cilium: { wait: true }
    - cert-manager
    - ./targets/networking: { dependsOn: [cert-manager] }
YAML
  run addons::detail inv
  assert_success
  assert_output --partial "Addons deployed by inv (kind=kubeone)"
  # cilium: framework addon, networking category, khelm, its config hint
  assert_output --partial "cilium"
  assert_output --partial "networking"
  assert_output --partial "policyAuditMode"
  # cert-manager: infrastructure category
  assert_output --partial "cert-manager"
  assert_output --partial "infrastructure"
  # the ./targets entry is listed as a per-cluster target (glue), not an addon
  assert_output --partial "networking"
  assert_output --partial "per-cluster glue in"
}

@test "detail on an empty bootstrap says the cluster deploys no addons" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec empty <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: e }
spec:
  bootstrap: []
YAML
  run addons::detail empty
  assert_success
  assert_output --partial "deploys no addons"
}

@test "detail warns (not errors) for a missing cluster spec" {
  run addons::detail nonexistent-domain
  assert_success
  assert_output --partial "nothing to inventory"
}

# PATH-TRAVERSAL GUARD (Copilot review round 1): addons::detail builds a
# filesystem path from the caller-provided domain, so it must reject a
# traversal/injected name (same guard as bootstrap::dispatch) BEFORE touching
# the path — never inventory (or stat) an escaped location.
@test "detail rejects a path-traversal / absolute / injected domain" {
  local d
  for d in '../etc' '/abs' 'foo/../bar' '../../root' 'a/b' '.hidden'; do
    run addons::detail "${d}"
    assert_success
    assert_output --partial "Invalid domain"
    refute_output --partial "Addons deployed by"
  done
}

@test "detail accepts a valid (dotted) domain name" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec good.example <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: g }
spec:
  bootstrap: []
YAML
  run addons::detail good.example
  assert_success
  assert_output --partial "Addons deployed by good.example"
}

@test "detail resolves a map-form entry as ONE addon (no shattering)" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec mapform <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: m }
spec:
  bootstrap:
    - ccm:
        values:
          env:
            ROBOT_ENABLED: { value: "true" }
        wait: true
YAML
  run addons::detail mapform
  assert_success
  # ccm shows with its hint …
  assert_output --partial "ccm"
  assert_output --partial "hcloud CCM"
  # … and the map is NOT shattered into bogus rows named after the reserved keys.
  refute_line --regexp '^(values|env|wait|dependsOn)[[:space:]]'
}

# =============================================================================
# addons::_category — reads the lok8s.dev/category label
# =============================================================================

@test "_category reads the lok8s.dev/category label" {
  local dir="${PATH_LOK8S}/addons/fake"
  mkdir -p "${dir}"
  cat > "${dir}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
labels:
  - pairs:
      lok8s.dev/name: fake
      lok8s.dev/category: storage
YAML
  assert_equal "$(addons::_category "${dir}")" "storage"
}

@test "_category returns - for an addon with no category label" {
  local dir="${PATH_LOK8S}/addons/bare"
  mkdir -p "${dir}"
  cat > "${dir}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
YAML
  assert_equal "$(addons::_category "${dir}")" "-"
}

# =============================================================================
# config-help catalog PARITY — every shipped addon must have a config hint
# =============================================================================

@test "every .lok8s/addons/ dir has a config-help entry (parity, fails on drift)" {
  local missing="" d n hint
  for d in "${_PROJECT_ROOT}/.lok8s/addons/"*/; do
    [[ -d "${d}" ]] || continue
    n="$(basename "${d}")"
    hint="$(addons::_config_hint "${n}")"
    [[ -n "${hint}" ]] || missing="${missing} ${n}"
  done
  assert_equal "${missing}" ""
}

@test "config hints are one-liners (no embedded newline)" {
  local d n hint
  for d in "${_PROJECT_ROOT}/.lok8s/addons/"*/; do
    [[ -d "${d}" ]] || continue
    n="$(basename "${d}")"
    hint="$(addons::_config_hint "${n}")"
    run bash -c "printf '%s' \"${hint}\" | wc -l | tr -d ' '"
    assert_output "0"
  done
}

@test "an unknown addon name has no curated hint (empty)" {
  assert_equal "$(addons::_config_hint definitely-not-an-addon)" ""
}

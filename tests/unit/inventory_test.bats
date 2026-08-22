#!/usr/bin/env bats
# inventory_test.bats — unit tests for .lok8s/libs/inventory/main (the
# ClusterInventory writer).
#
# Pins the contract: inventory::build_json is PURE metadata (resolved
# spec.bootstrap names + chart versions + categories + the sha256 of the
# cluster spec — never inline values/env), deterministic under
# SOURCE_DATE_EPOCH; inventory::publish is FAIL-SOFT (warns + returns 0 on any
# missing kubeconfig / unreachable cluster / CRD problem).

setup() {
  load "../test_helper"
  setup_tmpdir

  command -v yq &>/dev/null || skip "yq required for inventory tests"
  command -v jq &>/dev/null || skip "jq required for inventory tests"

  import() { :; }; export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/libs/addons"
  source "${_PROJECT_ROOT}/.lok8s/libs/inventory/main"
}

teardown() { teardown_tmpdir; }

_spec() { # _spec <domain> — write clusters/<domain>/cluster.lok8s.yaml from stdin
  local d="$1"
  mkdir -p "${PATH_CLUSTERS}/${d}"
  cat > "${PATH_CLUSTERS}/${d}/cluster.lok8s.yaml"
}

# =============================================================================
# inventory::build_json — pure metadata builder
# =============================================================================

@test "build_json resolves addons with chart version + category from the real tree" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec inv <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: inv }
spec:
  kubernetes: { version: "v1.31.10" }
  provider: { name: hetzner }
  bootstrap:
    - cilium: { wait: true, values: { encryption: { enabled: true } } }
    - cert-manager
YAML
  run inventory::build_json inv "${PATH_CLUSTERS}/inv/cluster.lok8s.yaml"
  assert_success
  local out="${output}"

  # CR envelope: the cluster-scoped singleton named "cluster"
  assert_equal "$(jq -r '.apiVersion' <<<"${out}")" "lok8s.dev/v1alpha1"
  assert_equal "$(jq -r '.kind' <<<"${out}")" "ClusterInventory"
  assert_equal "$(jq -r '.metadata.name' <<<"${out}")" "cluster"
  assert_equal "$(jq -r '.metadata.labels["lok8s.dev/domain"]' <<<"${out}")" "inv"

  # spec metadata
  assert_equal "$(jq -r '.spec.kind' <<<"${out}")" "kubeone"
  assert_equal "$(jq -r '.spec.provider' <<<"${out}")" "hetzner"
  assert_equal "$(jq -r '.spec.kubernetesVersion' <<<"${out}")" "v1.31.10"

  # addons: names + REAL pinned chart versions/categories (read the pins so
  # the test survives deliberate bumps)
  local cilium_v
  cilium_v=$(yq -r '.version' "${_PROJECT_ROOT}/.lok8s/addons/cilium/chart.yaml")
  assert_equal "$(jq -r '.spec.addons | length' <<<"${out}")" "2"
  assert_equal "$(jq -r '.spec.addons[0].name' <<<"${out}")" "cilium"
  assert_equal "$(jq -r '.spec.addons[0].chartVersion' <<<"${out}")" "${cilium_v}"
  assert_equal "$(jq -r '.spec.addons[0].source' <<<"${out}")" "addon"
  assert_equal "$(jq -r '.spec.addons[0].category' <<<"${out}")" "networking"
  assert_equal "$(jq -r '.spec.addons[1].name' <<<"${out}")" "cert-manager"
}

@test "build_json emits STRICTLY metadata — inline values/env never leak" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec leak <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: leak }
spec:
  bootstrap:
    - cilium:
        values: { encryption: { enabled: true }, secretString: "hunter2-not-for-export" }
        env: { LOK8S_USER_SECRET_TOKEN: "tok-abc123" }
YAML
  run inventory::build_json leak "${PATH_CLUSTERS}/leak/cluster.lok8s.yaml"
  assert_success
  refute_output --partial "hunter2-not-for-export"
  refute_output --partial "tok-abc123"
  refute_output --partial "encryption"
  # entries carry ONLY the enumerated metadata keys
  run jq -e 'all(.spec.addons[]; (keys - ["name","chartVersion","appVersion","category","source"]) == [])' <<<"${output}"
  assert_success
}

@test "build_json specHash is the sha256 of the cluster spec bytes and is stable" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec hashme <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: hashme }
spec:
  bootstrap: []
YAML
  local expected
  expected=$(sha256sum "${PATH_CLUSTERS}/hashme/cluster.lok8s.yaml" | awk '{print $1}')

  export SOURCE_DATE_EPOCH=1700000000
  local one two
  one=$(inventory::build_json hashme "${PATH_CLUSTERS}/hashme/cluster.lok8s.yaml")
  two=$(inventory::build_json hashme "${PATH_CLUSTERS}/hashme/cluster.lok8s.yaml")
  assert_equal "${one}" "${two}"                                  # fully deterministic
  assert_equal "$(jq -r '.spec.specHash' <<<"${one}")" "${expected}"
  assert_equal "$(jq -r '.spec.renderedAt' <<<"${one}")" "2023-11-14T22:13:20Z"
}

@test "build_json strips a kindest-node @sha256 digest from kubernetesVersion" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec digest <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: digest }
spec:
  cluster: { domain: digest.lok8s.dev }
  kubernetes: { version: "v1.31.12@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2" }
  bootstrap: []
YAML
  run inventory::build_json digest "${PATH_CLUSTERS}/digest/cluster.lok8s.yaml"
  assert_success
  assert_equal "$(jq -r '.spec.kubernetesVersion' <<<"${output}")" "v1.31.12"
}

@test "build_json includes the per-driver default (cilium on kind) when bootstrap is absent" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec defl <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: defl }
spec:
  cluster: { domain: defl.lok8s.dev }
YAML
  run inventory::build_json defl "${PATH_CLUSTERS}/defl/cluster.lok8s.yaml"
  assert_success
  assert_equal "$(jq -r '.spec.addons | length' <<<"${output}")" "1"
  assert_equal "$(jq -r '.spec.addons[0].name' <<<"${output}")" "cilium"
}

@test "build_json marks ./targets entries as source: target" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec tgt <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: tgt }
spec:
  bootstrap:
    - ./targets/networking
YAML
  mkdir -p "${PATH_CLUSTERS}/tgt/targets/networking"
  run inventory::build_json tgt "${PATH_CLUSTERS}/tgt/cluster.lok8s.yaml"
  assert_success
  assert_equal "$(jq -r '.spec.addons[0].name' <<<"${output}")" "networking"
  assert_equal "$(jq -r '.spec.addons[0].source' <<<"${output}")" "target"
}

@test "build_json reads lok8sVersion from .lok8s/VERSION" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec ver <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: ver }
spec:
  bootstrap: []
YAML
  local expected
  expected="$(<"${_PROJECT_ROOT}/.lok8s/VERSION")"
  run inventory::build_json ver "${PATH_CLUSTERS}/ver/cluster.lok8s.yaml"
  assert_success
  assert_equal "$(jq -r '.spec.lok8sVersion' <<<"${output}")" "${expected}"
}

# =============================================================================
# inventory::publish — FAIL-SOFT contract
# =============================================================================

@test "publish without a kubeconfig warns and returns 0 (never breaks a deploy)" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec soft <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: soft }
spec:
  bootstrap: []
YAML
  run inventory::publish soft "${PATH_CLUSTERS}/soft/cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/.kubeconfig/nonexistent.yaml"
  assert_success
  assert_output --partial "kubeconfig not found"
}

@test "publish skips cleanly when there is no cluster spec (deploy domains)" {
  run inventory::publish dep "${PATH_CLUSTERS}/dep/cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_success
  refute_output --partial "error"
}

@test "publish warns and returns 0 when the cluster is unreachable (kubectl fails)" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec unreach <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: unreach }
spec:
  bootstrap: []
YAML
  touch "${BATS_TEST_TMPDIR}/kc.yaml"
  kubectl() { return 1; }; export -f kubectl
  run inventory::publish unreach "${PATH_CLUSTERS}/unreach/cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_success
  assert_output --partial "skipping publish"
}

@test "publish applies the CRD then the CR via server-side apply (field manager lok8s)" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec happy <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: happy }
spec:
  bootstrap: []
YAML
  touch "${BATS_TEST_TMPDIR}/kc.yaml"
  local log="${BATS_TEST_TMPDIR}/kubectl.log"
  export _KUBECTL_LOG="${log}"
  kubectl() {
    echo "$*" >> "${_KUBECTL_LOG}"
    # swallow the CR piped on stdin so the heredoc'd apply doesn't SIGPIPE
    [[ -t 0 ]] || cat > /dev/null
    return 0
  }
  export -f kubectl
  run inventory::publish happy "${PATH_CLUSTERS}/happy/cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/kc.yaml"
  assert_success
  run cat "${log}"
  # 1st call: the CRD manifest (from the synced .lok8s mirror), server-side
  assert_line --index 0 --partial "apply --server-side --field-manager=lok8s"
  assert_line --index 0 --partial "clusterinventory.crd.yaml"
  # then the Established wait, then the CR itself from stdin
  assert_line --index 1 --partial "wait --for=condition=Established"
  assert_line --index 2 --partial "apply --server-side --field-manager=lok8s -f -"
}

@test "publish uses the .lok8s CRD mirror (consumer repos vendor only .lok8s/**)" {
  # A consumer-repo layout: PATH_BASE has NO operator/ tree, only .lok8s.
  # The mirror inside .lok8s must be enough for publish to find the CRD.
  mkdir -p "${PATH_LOK8S}/libs/inventory/manifests"
  cp "${_PROJECT_ROOT}/.lok8s/libs/inventory/manifests/clusterinventory.crd.yaml" \
     "${PATH_LOK8S}/libs/inventory/manifests/"
  run inventory::_crd_manifest
  assert_success
  assert_output "${PATH_LOK8S}/libs/inventory/manifests/clusterinventory.crd.yaml"
}

@test "build_json refuses a malformed kind instead of defaulting it to lo" {
  # rc 2 from domain::spec_driver is "present but not a bare driver name" and
  # is never defaulted. The inventory publishes `kind` into the
  # ClusterInventory CR, so laundering "../../evil" into "lo" would put a false
  # driver identity on the cluster and in the agent's heartbeat.
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec bad <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: ../../evil
metadata: { name: bad }
spec:
  kubernetes: { version: "v1.31.10" }
YAML
  run inventory::build_json bad "${PATH_CLUSTERS}/bad/cluster.lok8s.yaml"
  assert_failure
  refute_output --partial '"kind": "lo"'
}

@test "build_json still defaults to lo when the spec declares no kind" {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  _spec nokind <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
metadata: { name: nokind }
spec:
  kubernetes: { version: "v1.31.10" }
YAML
  run inventory::build_json nokind "${PATH_CLUSTERS}/nokind/cluster.lok8s.yaml"
  assert_success
  assert_output --partial '"kind": "lo"'
}

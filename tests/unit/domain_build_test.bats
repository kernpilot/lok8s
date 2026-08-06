#!/usr/bin/env bats
# domain_build_test.bats — integration-flavoured test for the domain-based
# build → deploy pipeline, using a small FIXTURE domain whose
# kustomization.yaml composes real targets and the REAL kustomize/yq (no
# mocked renderer). Asserts:
#   (a) `lo build` produces ONE clusters/<domain>/artifacts.yaml,
#   (b) a domain missing its kustomization.yaml errors clearly,
#   (c) `lo deploy -l <key=value>` selects the right subset of that artifact.
# kubectl is the only mock (there is no cluster).

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
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"
  source "${_PROJECT_ROOT}/.lok8s/libs/deploy"

  # Real kustomize + yq drive the composition and the label filter.
  command -v kustomize &>/dev/null || skip "kustomize not available"
  command -v yq &>/dev/null || skip "yq not available"

  # Build path needs the envsubst whitelist helper (normally imported).
  template::envsubst_whitelist() { echo ""; }
  export -f template::envsubst_whitelist

  # No readiness polling against the fake cluster.
  kapply::wait_ready() { :; }
  export -f kapply::wait_ready

  # FIXTURE domain: a kustomization.yaml composing two real targets.
  DOMAIN="fixture.lok8s.dev"
  DOMAIN_DIR="${BATS_TEST_TMPDIR}/clusters/${DOMAIN}"
  mkdir -p "${DOMAIN_DIR}/targets/networking" "${DOMAIN_DIR}/targets/platform"
  cp "${FIXTURES_DIR}/targets/networking/kustomization.yaml" "${DOMAIN_DIR}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/networking/namespace.yaml"     "${DOMAIN_DIR}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/platform/kustomization.yaml"   "${DOMAIN_DIR}/targets/platform/"
  cp "${FIXTURES_DIR}/targets/platform/deployment.yaml"      "${DOMAIN_DIR}/targets/platform/"
  cat > "${DOMAIN_DIR}/kustomization.yaml" <<'YAML'
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

# (a) One artifact, composed from all targets.
@test "lo build composes the domain kustomization into ONE artifacts.yaml" {
  build::artifacts "${DOMAIN}"

  [ -f "${DOMAIN_DIR}/artifacts.yaml" ]
  # No per-target artifact tree.
  [ ! -d "${DOMAIN_DIR}/artifacts" ]
  # Both composed targets landed in the one file.
  run yq -r '.kind' "${DOMAIN_DIR}/artifacts.yaml"
  assert_output --partial "Namespace"
  assert_output --partial "Deployment"
}

# (b) Missing domain kustomization → clear, actionable error.
@test "lo build errors when the domain has no kustomization.yaml" {
  rm -f "${DOMAIN_DIR}/kustomization.yaml"
  run build::artifacts "${DOMAIN}"
  assert_failure
  assert_output --partial "has no kustomization.yaml"
  assert_output --partial "resources:"
}

# (d) --no-secrets (LOK8S_BUILD_NO_SECRETS=1) exports LOK8S_SECRETS_DISABLE=1
# to the kustomize invocation → the secrets.lok8s.dev exec generator emits
# nothing and never reads the store, so the render is store-free. A kustomize
# stub records the env it was invoked with (mirrors the sops-stub argv record).
stub_kustomize_env() {
  mkdir -p "${BATS_TEST_TMPDIR}/bin"
  export KUSTOMIZE_ENV_LOG="${BATS_TEST_TMPDIR}/kustomize.env"
  : > "${KUSTOMIZE_ENV_LOG}"
  cat > "${BATS_TEST_TMPDIR}/bin/kustomize" <<'STUB'
#!/usr/bin/env bash
# record the disable knob the build lib set for this invocation
printf 'LOK8S_SECRETS_DISABLE=%s\n' "${LOK8S_SECRETS_DISABLE-<unset>}" >> "${KUSTOMIZE_ENV_LOG}"
# emit a minimal valid resource so the build pipeline (envsubst > tmp) succeeds
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stub\n'
STUB
  chmod +x "${BATS_TEST_TMPDIR}/bin/kustomize"
  export PATH="${BATS_TEST_TMPDIR}/bin:${PATH}"
}

@test "--no-secrets: build::artifacts exports LOK8S_SECRETS_DISABLE=1 to kustomize" {
  stub_kustomize_env
  LOK8S_BUILD_NO_SECRETS=1 run build::artifacts "${DOMAIN}"
  assert_success
  run cat "${KUSTOMIZE_ENV_LOG}"
  assert_output --partial "LOK8S_SECRETS_DISABLE=1"
}

@test "default build: LOK8S_SECRETS_DISABLE is 0 (secrets rendered normally)" {
  stub_kustomize_env
  run build::artifacts "${DOMAIN}"
  assert_success
  run cat "${KUSTOMIZE_ENV_LOG}"
  assert_output --partial "LOK8S_SECRETS_DISABLE=0"
  refute_output --partial "LOK8S_SECRETS_DISABLE=1"
}

# (c) Selective deploy filters the single artifact by label.
@test "lo deploy -l <key=value> applies only the matching subset" {
  build::artifacts "${DOMAIN}"

  # Record which kinds reach kubectl apply.
  kubectl() {
    case "$1" in
      apply) local m; m=$(cat)
             grep -q 'kind: Namespace'  <<<"${m}" && echo "applied:Namespace"
             grep -q 'kind: Deployment' <<<"${m}" && echo "applied:Deployment"
             return 0 ;;
      *) return 0 ;;
    esac
  }
  export -f kubectl

  run deploy::apply_filtered "${DOMAIN}" "lok8s.dev/type" "system"
  assert_success
  assert_output --partial "applied:Namespace"
  refute_output --partial "applied:Deployment"
}

# ── The domain announcement ───────────────────────────────────────────────
# `lo build` writes clusters/<domain>/artifacts* and reads that domain's secret
# store. A run against the wrong domain renders one cluster's manifests from
# another's secrets — the failure that re-keyed a live ZITADEL masterkey. The
# domain can come from clusters/.active, i.e. state a `lo use` set hours ago,
# so every build says which domain it is acting on before it acts.
#
# domain::resolve already warns when DOMAIN_NAME and .active DISAGREE; these
# cover the silent case where nothing disagrees and the answer is simply not
# what the operator assumed.
#
# main::build is driven directly: :args is the argsh runtime (not available
# here), and it feeds `domain` through dynamic scoping — which is exactly how
# the real `lo` main hands the resolved domain down, so setting it is faithful.

_announce_setup() {
  :args() { :; };                       export -f :args
  _resolve_kubeconfig_for_domain() { :; }
}

@test "lo build announces the resolved domain before rendering" {
  _announce_setup
  local domain="${DOMAIN}"
  run main::build
  assert_success
  assert_output --partial "lo build: domain ${DOMAIN}"
}

@test "lo build announces the domain it acts on, not the active one" {
  # .active says one thing, the resolved domain another: the announcement must
  # name the domain actually built, or it is worse than silence.
  _announce_setup
  mkdir -p "${BATS_TEST_TMPDIR}/clusters"
  echo "some-other.lok8s.dev" > "${BATS_TEST_TMPDIR}/clusters/.active"
  local domain="${DOMAIN}"
  run main::build
  assert_success
  assert_output --partial "lo build: domain ${DOMAIN}"
  refute_output --partial "some-other.lok8s.dev"
}

@test "the announcement precedes the render (it warns BEFORE acting)" {
  # Ordering is the whole point: a line printed after artifacts.yaml is written
  # tells the operator what already happened, not what is about to.
  _announce_setup
  local domain="${DOMAIN}"
  build::artifacts() { echo "RENDERING"; }
  run main::build
  assert_success
  [[ "${lines[0]}" == *"lo build: domain ${DOMAIN}"* ]]
  assert_output --partial "RENDERING"
}

# ── An empty render must not replace a good artifact ──────────────────────
# kustomize exits 0 for an empty result (`resources: []`, a target list that
# lost its entries, a base that stopped resolving). The atomic promote then
# installs a 0-byte artifacts.yaml over the previous good one — and on the prod
# path render.yml COMMITS that and Flux applies it with prune: true, deleting
# every resource the domain managed. Measured before the guard: rc=0, 0
# documents, 0 bytes, silent.

_gut_resources() {
  cat > "${DOMAIN_DIR}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
YAML
}

@test "an empty render is REFUSED when it would replace a non-empty artifact" {
  build::artifacts "${DOMAIN}"
  local before
  before=$(grep -c '^kind:' "${DOMAIN_DIR}/artifacts.yaml")
  [ "${before}" -gt 0 ]

  _gut_resources
  run build::artifacts "${DOMAIN}"
  assert_failure
  assert_output --partial "refusing to overwrite"
  assert_output --partial "prune"
  # The good artifact is still on disk, untouched.
  [ "$(grep -c '^kind:' "${DOMAIN_DIR}/artifacts.yaml")" -eq "${before}" ]
}

@test "an empty render is ALLOWED when there is no artifact to lose" {
  # A new or genuinely empty domain destroys nothing — warn, do not fail.
  _gut_resources
  rm -f "${DOMAIN_DIR}/artifacts.yaml"
  run build::artifacts "${DOMAIN}"
  assert_success
  assert_output --partial "rendered 0 documents"
}

@test "a successful build reports how many documents it produced" {
  # A silent success cannot distinguish a full render from one that quietly
  # lost most of its resources.
  run build::artifacts "${DOMAIN}"
  assert_success
  assert_output --partial "rendered"
  assert_output --partial "document(s)"
}

@test "an empty render is REFUSED when only the SPLIT dir has something to lose" {
  # Split-mode domains keep their committed state in artifacts/, not in
  # artifacts.yaml. Counting only the single file waved the empty render
  # through: artifacts.yaml got zeroed, artifacts/ was left stale disagreeing
  # with it, and the build then failed downstream with "build first" —
  # immediately after a build (AUDIT.md r303).
  mkdir -p "${DOMAIN_DIR}/artifacts"
  cat > "${DOMAIN_DIR}/artifacts/Namespace.keep-me.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata: {name: keep-me}
YAML
  rm -f "${DOMAIN_DIR}/artifacts.yaml"
  _gut_resources

  run build::artifacts "${DOMAIN}"
  assert_failure
  assert_output --partial "refusing to overwrite"
  # The split layout survived, and no empty artifacts.yaml was promoted.
  [ -f "${DOMAIN_DIR}/artifacts/Namespace.keep-me.yaml" ]
  [ ! -f "${DOMAIN_DIR}/artifacts.yaml" ]
}

@test "the split-dir count uses the same ownership rule as the prune" {
  # The swap prunes [A-Z]*.yaml and leaves env-owned lowercase files alone, so
  # a dir holding ONLY kustomization.yaml has nothing generated to lose and an
  # empty render there must still be allowed.
  mkdir -p "${DOMAIN_DIR}/artifacts"
  printf 'resources: []\n' > "${DOMAIN_DIR}/artifacts/kustomization.yaml"
  rm -f "${DOMAIN_DIR}/artifacts.yaml"
  _gut_resources

  run build::artifacts "${DOMAIN}"
  assert_success
  assert_output --partial "rendered 0 documents"
}

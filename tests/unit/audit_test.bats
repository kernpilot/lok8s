#!/usr/bin/env bats
# audit_test.bats — unit tests for libs/audit (lo audit — static security posture)
#
# Each security check is exercised with a fixture spec that SHOULD fail and one
# that passes, plus the scoring/exit contract and the stable --json schema.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }; export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # audit delegates spec.bootstrap resolution to the shared bootstrap parser
  # (same functions the apply path uses) — the `lo` entrypoint loads it first.
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/libs/audit"
}

teardown() { teardown_tmpdir; }

# --- fixture helpers ---------------------------------------------------------

# _spec <domain>  — write a cluster.lok8s.yaml from stdin.
_spec() {
  local d="$1"
  mkdir -p "${PATH_CLUSTERS}/${d}"
  cat > "${PATH_CLUSTERS}/${d}/cluster.lok8s.yaml"
}

# _cilium_values <driver> — write addons/cilium/values.<driver>.yaml from stdin.
_cilium_values() {
  mkdir -p "${PATH_LOK8S}/addons/cilium"
  cat > "${PATH_LOK8S}/addons/cilium/values.${1}.yaml"
}

# _kubeone_encryption <true|false> — write the KubeOne driver core config.
_kubeone_encryption() {
  mkdir -p "${PATH_LOK8S}/drivers/kubeone/cluster/core"
  cat > "${PATH_LOK8S}/drivers/kubeone/cluster/core/kubeone.yaml" <<YAML
apiVersion: kubeone.k8c.io/v1beta2
kind: KubeOneCluster
features:
  encryptionProviders:
    enable: ${1}
YAML
}

# _target <domain> <name> — write a target manifest from stdin.
_target() {
  local d="$1" f="$2"
  mkdir -p "${PATH_CLUSTERS}/${d}/targets/${f%/*}"
  cat > "${PATH_CLUSTERS}/${d}/targets/${f}"
}

# _status_of <id> / _sev_of <id> — read the current findings accumulator.
_status_of() {
  local want="$1" line id t s st d r
  for line in "${_AUDIT_FINDINGS[@]}"; do
    IFS=$'\t' read -r id t s st d r <<< "${line}"
    [[ "${id}" == "${want}" ]] && { echo "${st}"; return 0; }
  done
  echo "MISSING"
}
_sev_of() {
  local want="$1" line id t s st d r
  for line in "${_AUDIT_FINDINGS[@]}"; do
    IFS=$'\t' read -r id t s st d r <<< "${line}"
    [[ "${id}" == "${want}" ]] && { echo "${s}"; return 0; }
  done
  echo "MISSING"
}

# =============================================================================
# Cilium policy enforcement — the headline: audit mode = insecure
# =============================================================================

@test "cilium policyAuditMode: true is a HIGH fail" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec audit-cilium <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  audit::run_domain audit-cilium
  assert_equal "$(_status_of cilium-policy-enforcement)" "fail"
  assert_equal "$(_sev_of cilium-policy-enforcement)" "high"
}

@test "cilium enforcing (no audit mode) passes" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: false
policyEnforcementMode: default
YAML
  _spec enforce-cilium <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  audit::run_domain enforce-cilium
  assert_equal "$(_status_of cilium-policy-enforcement)" "pass"
}

@test "cilium policyEnforcementMode: never is a fail" {
  _cilium_values kubeone <<'YAML'
policyEnforcementMode: never
YAML
  _spec never-cilium <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  audit::run_domain never-cilium
  assert_equal "$(_status_of cilium-policy-enforcement)" "fail"
}

@test "an inline policyAuditMode: false overrides the driver default (pass)" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec override-cilium <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap:
    - cilium: { policyAuditMode: false }
YAML
  audit::run_domain override-cilium
  assert_equal "$(_status_of cilium-policy-enforcement)" "pass"
}

# REGRESSION GUARD: the SHIPPED addons/cilium/values.kubeone.yaml default must
# trip the enforcement rule — so we notice if the insecure default is ever
# flipped. Reads the REAL file (copied in), not a fixture.
@test "the stock cilium values.kubeone.yaml trips the enforcement rule" {
  mkdir -p "${PATH_LOK8S}/addons/cilium"
  cp "${_PROJECT_ROOT}/.lok8s/addons/cilium/"values*.yaml \
    "${PATH_LOK8S}/addons/cilium/" 2>/dev/null || true
  _spec pilot <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: pilot }
spec:
  kubernetes: { version: "v1.35.5" }
  provider: { name: hetzner }
  bootstrap:
    - cilium: { wait: true }
YAML
  audit::run_domain pilot
  assert_equal "$(_status_of cilium-policy-enforcement)" "fail"
}

@test "cilium not in spec.bootstrap → not-applicable pass" {
  _spec no-cilium <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ metallb ]
YAML
  audit::run_domain no-cilium
  assert_equal "$(_status_of cilium-policy-enforcement)" "pass"
}

# =============================================================================
# Encryption at rest
# =============================================================================

@test "kubeone with driver encryption enabled passes" {
  _kubeone_encryption true
  _spec enc-on <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  audit::run_domain enc-on
  assert_equal "$(_status_of encryption-at-rest)" "pass"
}

@test "kubeone with encryption explicitly disabled is a fail" {
  _kubeone_encryption true   # driver default on …
  _spec enc-off <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  features: { encryptionProviders: { enable: false } }
  bootstrap: []
YAML
  audit::run_domain enc-off        # … but the spec override wins
  assert_equal "$(_status_of encryption-at-rest)" "fail"
  assert_equal "$(_sev_of encryption-at-rest)" "high"
}

@test "kind=lo dev cluster: encryption is not-applicable (pass)" {
  _spec dev <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: dev }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  audit::run_domain dev
  assert_equal "$(_status_of encryption-at-rest)" "pass"
}

@test "a rendered EncryptionConfiguration artifact counts as configured" {
  _spec art <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  cat > "${PATH_CLUSTERS}/art/artifacts.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources: []
YAML
  audit::run_domain art
  assert_equal "$(_status_of encryption-at-rest)" "pass"
}

# =============================================================================
# Publicly-exposed endpoints
# =============================================================================

@test "a NodePort service is a fail" {
  _spec np <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target np net/svc.yaml <<'YAML'
apiVersion: v1
kind: Service
metadata: { name: np }
spec:
  type: NodePort
YAML
  audit::run_domain np
  assert_equal "$(_status_of exposed-endpoints)" "fail"
}

@test "a LoadBalancer behind a default-Deny SecurityPolicy passes" {
  _spec lb <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target lb net/gw.yaml <<'YAML'
apiVersion: v1
kind: Service
metadata: { name: envoy }
spec:
  type: LoadBalancer
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: ip-allowlist }
spec:
  authorization:
    defaultAction: Deny
YAML
  audit::run_domain lb
  assert_equal "$(_status_of exposed-endpoints)" "pass"
}

@test "HTTPRoutes with no allowlist carve-out are a warn" {
  _spec routes <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target routes net/route.yaml <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: app }
YAML
  audit::run_domain routes
  assert_equal "$(_status_of exposed-endpoints)" "warn"
}

# =============================================================================
# Kubernetes version support (EOL)
# =============================================================================

@test "a supported k8s minor passes" {
  _spec ver-ok <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  audit::run_domain ver-ok
  assert_equal "$(_status_of k8s-version-support)" "pass"
}

@test "an EOL k8s minor on a prod-intent cluster is a fail" {
  _spec ver-eol <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.31.4" }
  bootstrap: []
YAML
  audit::run_domain ver-eol
  assert_equal "$(_status_of k8s-version-support)" "fail"
}

@test "an EOL k8s minor on a kind/dev cluster is only a warn" {
  _spec ver-dev <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: dev }
spec:
  kubernetes: { version: "v1.31.4" }
  bootstrap: [ cilium ]
YAML
  audit::run_domain ver-dev
  assert_equal "$(_status_of k8s-version-support)" "warn"
}

@test "a newer-than-known k8s minor warns (stale support list)" {
  _spec ver-new <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: n }
spec:
  kubernetes: { version: "v1.99.0" }
  bootstrap: [ cilium ]
YAML
  audit::run_domain ver-new
  assert_equal "$(_status_of k8s-version-support)" "warn"
}

# =============================================================================
# Plaintext endpoints
# =============================================================================

@test "a non-HTTPS OIDC issuer is a fail" {
  _spec oidc-bad <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  oidc: { issuer: "http://insecure.example", clientID: "1" }
  bootstrap: []
YAML
  audit::run_domain oidc-bad
  assert_equal "$(_status_of plaintext-endpoints)" "fail"
}

@test "an HTTPS OIDC issuer passes" {
  _spec oidc-ok <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  oidc: { issuer: "https://id.example.cloud", clientID: "1" }
  bootstrap: []
YAML
  audit::run_domain oidc-ok
  assert_equal "$(_status_of plaintext-endpoints)" "pass"
}

# =============================================================================
# Privileged / host-level workloads
# =============================================================================

@test "a privileged target workload is a warn" {
  _spec priv <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target priv app/pod.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata: { name: p }
spec:
  containers:
    - name: c
      image: busybox
      securityContext:
        privileged: true
YAML
  audit::run_domain priv
  assert_equal "$(_status_of privileged-workloads)" "warn"
}

# =============================================================================
# Scoring, exit code, and the --json schema
# =============================================================================

@test "run_domain populates exactly the six checks + emits a fail count" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec sixchecks <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  audit::run_domain sixchecks
  assert_equal "${#_AUDIT_FINDINGS[@]}" "6"
  # cilium audit-mode → at least one fail
  [ "$(audit::_count_status fail)" -ge 1 ]
}

@test "main::audit exits non-zero when a fail-level finding exists" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec failing <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  _run() { domain="failing" main::audit; }
  run _run
  assert_failure
  assert_output --partial "cilium-policy-enforcement"
}

@test "main::audit exits zero for a clean cluster" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: false
policyEnforcementMode: default
YAML
  _kubeone_encryption true
  _spec clean <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  oidc: { issuer: "https://id.example.cloud", clientID: "1" }
  bootstrap: [ cilium ]
YAML
  _run() { domain="clean" main::audit; }
  run _run
  assert_success
}

@test "--json emits the stable schema (jq-validated)" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec jsondom <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  _json() { audit::run_domain jsondom; audit::render_json jsondom; }
  run _json
  assert_success
  # top-level shape
  echo "${output}" | jq -e 'has("domain") and has("score") and has("grade") and has("summary") and has("checks")'
  echo "${output}" | jq -e '.summary | has("pass") and has("warn") and has("fail") and has("unknown")'
  # every check carries the full contract
  echo "${output}" | jq -e '.checks | length == 6'
  echo "${output}" | jq -e '[.checks[] | has("id") and has("title") and has("severity") and has("status") and has("detail") and has("remediation")] | all'
  # severity/status enums
  echo "${output}" | jq -e '[.checks[].severity] | all(. as $s | ["critical","high","medium","low"] | index($s) != null)'
  echo "${output}" | jq -e '[.checks[].status] | all(. as $s | ["pass","warn","fail","unknown"] | index($s) != null)'
  # the headline finding is present and failing
  echo "${output}" | jq -e '.checks[] | select(.id=="cilium-policy-enforcement") | .status=="fail" and .severity=="high"'
}

@test "score drops below 100 when findings fail" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec scored <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  _json() { audit::run_domain scored; audit::render_json scored; }
  run _json
  local score
  score=$(echo "${output}" | jq -r '.score')
  [ "${score}" -lt 100 ]
  [ "${score}" -ge 0 ]
}

@test "a deploy-only / missing cluster spec yields a fail-soft unknown" {
  mkdir -p "${PATH_CLUSTERS}/deployonly"
  cat > "${PATH_CLUSTERS}/deployonly/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata: { name: d }
YAML
  audit::run_domain deployonly
  assert_equal "$(_status_of cluster-spec)" "unknown"
}

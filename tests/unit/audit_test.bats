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
# _detail_of <id> — the finding's detail text.
# _count_of <id>  — how many findings carry that id. One check emits ONE
# finding: _status_of/_sev_of stop at the first match, so a second finding
# under the same id would hide behind the first and double-count in the score.
_detail_of() {
  local want="$1" line id t s st d r
  for line in "${_AUDIT_FINDINGS[@]}"; do
    IFS=$'\t' read -r id t s st d r <<< "${line}"
    [[ "${id}" == "${want}" ]] && { echo "${d}"; return 0; }
  done
  echo "MISSING"
}
_count_of() {
  local want="$1" line id rest n=0
  for line in "${_AUDIT_FINDINGS[@]}"; do
    IFS=$'\t' read -r id rest <<< "${line}"
    [[ "${id}" == "${want}" ]] && n=$(( n + 1 ))
  done
  echo "${n}"
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

# SECURITY REGRESSION (Copilot review round 2, audit:289): a merge/parse
# failure of the effective cilium values — e.g. an unparseable `values:` string
# in the spec.bootstrap inline override — made `merged` empty and fell through
# to the enforcing-PASS branch. Unreadable values can prove nothing: → unknown.
@test "an unparseable cilium values override → unknown, never a fall-through pass" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  # chart.yaml so the bootstrap parser accepts the `values:` reserved key
  # (values: is helm-only); the override itself is a broken YAML string.
  touch "${PATH_LOK8S}/addons/cilium/chart.yaml"
  _spec badinline <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap:
    - cilium:
        values: "policyAuditMode: [unclosed"
YAML
  audit::run_domain badinline
  assert_equal "$(_status_of cilium-policy-enforcement)" "unknown"
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

# SECURITY REGRESSION: an EncryptionConfiguration that encrypts NOTHING (empty
# resources, or identity as the write provider for `secrets`) must NOT be
# green-lit — the mere presence of `kind: EncryptionConfiguration` is not proof.
@test "an empty EncryptionConfiguration artifact (resources: []) is a fail, not a pass" {
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
  assert_equal "$(_status_of encryption-at-rest)" "fail"
  assert_equal "$(_sev_of encryption-at-rest)" "high"
}

@test "an identity-only EncryptionConfiguration for secrets is a fail (encrypts nothing)" {
  _spec artid <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  cat > "${PATH_CLUSTERS}/artid/artifacts.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - identity: {}
YAML
  audit::run_domain artid
  assert_equal "$(_status_of encryption-at-rest)" "fail"
}

@test "identity listed as the WRITE (first) provider for secrets is a fail" {
  _spec artwrite <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  cat > "${PATH_CLUSTERS}/artwrite/artifacts.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - identity: {}
      - secretbox: { keys: [{ name: k1, secret: c2VjcmV0 }] }
YAML
  audit::run_domain artwrite
  assert_equal "$(_status_of encryption-at-rest)" "fail"
}

@test "a rendered EncryptionConfiguration with a real provider for secrets passes" {
  _spec artok <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  cat > "${PATH_CLUSTERS}/artok/artifacts.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - secretbox:
          keys:
            - name: key1
              secret: c2VjcmV0
      - identity: {}
YAML
  audit::run_domain artok
  assert_equal "$(_status_of encryption-at-rest)" "pass"
}

# SECURITY REGRESSION (review round 2): k8s EncryptionConfiguration precedence —
# the FIRST resources entry matching a resource wins. A `secrets → identity`
# group listed BEFORE a wildcard aescbc group writes Secrets PLAINTEXT; the
# later covering group must never rescue the verdict (the old scan passed if
# ANY covering group had a real write provider).
@test "identity-first covering group is a fail — a later wildcard aescbc group must not rescue it" {
  _spec artprec <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  cat > "${PATH_CLUSTERS}/artprec/artifacts.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - identity: {}
  - resources: ["*.*"]
    providers:
      - aescbc: { keys: [{ name: k1, secret: c2VjcmV0 }] }
YAML
  audit::run_domain artprec
  assert_equal "$(_status_of encryption-at-rest)" "fail"
  assert_equal "$(_sev_of encryption-at-rest)" "high"
}

@test "aescbc secrets group first + identity wildcard later passes (first covering group wins)" {
  _spec artprec2 <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  cat > "${PATH_CLUSTERS}/artprec2/artifacts.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - aescbc: { keys: [{ name: k1, secret: c2VjcmV0 }] }
  - resources: ["*.*"]
    providers:
      - identity: {}
YAML
  audit::run_domain artprec2
  assert_equal "$(_status_of encryption-at-rest)" "pass"
}

# Copilot review round 2 (audit:165): kind=lo is documented n/a for
# encryption-at-rest — the short-circuit must beat EVERY other path. An
# explicit enable: false plus a rendered identity-only EncryptionConfiguration
# used to reach the disabled branch and emit a WARN on a dev cluster.
@test "kind=lo with encryption disabled AND an identity-only artifact is still n/a (pass), never warn" {
  _spec lona <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: dev }
spec:
  kubernetes: { version: "v1.35.5" }
  features: { encryptionProviders: { enable: false } }
  bootstrap: [ cilium ]
YAML
  cat > "${PATH_CLUSTERS}/lona/artifacts.yaml" <<'YAML'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - identity: {}
YAML
  audit::run_domain lona
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

# PORTABLE-BOUNDARY REGRESSION (Copilot review round 1): the exposure patterns
# use a POSIX word-boundary `([^[:alnum:]_]|$)` instead of the GNU-only `\b`.
# These two tests lock BOTH edges of that boundary so a future regex regression
# is caught: it must still MATCH the real value even with trailing content, and
# must NOT over-match a word that merely starts with the value.
@test "exposed-endpoints still fires for NodePort with a trailing comment (boundary matches)" {
  _spec npc <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target npc net/svc.yaml <<'YAML'
apiVersion: v1
kind: Service
metadata: { name: np }
spec:
  type: NodePort  # exposed on every node's IP
YAML
  audit::run_domain npc
  assert_equal "$(_status_of exposed-endpoints)" "fail"
}

@test "exposed-endpoints does NOT treat a NodePort-prefixed word as NodePort (boundary rejects)" {
  _spec npn <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  # 'NodePortless' is not a Service type; dropping the word-boundary would make
  # the NodePort pattern match it (a false FAIL), so this asserts a clean pass.
  _target npn net/cm.yaml <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata: { name: cm }
data:
  type: NodePortless
YAML
  audit::run_domain npn
  assert_equal "$(_status_of exposed-endpoints)" "pass"
}

# SECURITYPOLICY MERGE REGRESSION (subagent review round 1, PR #140): the
# carve-out used to be one `grep 'defaultAction: Deny'` over every rendered
# file. That boolean could not see WHICH object carried the deny, nor that a
# route-level policy without `mergeType` REPLACES it wholesale for the routes
# it selects — so the exact footgun the sso-gate fix is about scored a clean
# pass. The tests below lock both halves — object scoping, and the override —
# plus, since round 3, that the override does not swallow a NodePort found with
# it.
@test "a route-level SecurityPolicy without mergeType cancels the gateway's deny" {
  _spec ovr <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target ovr net/gw.yaml <<'YAML'
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: sso-gate }
spec:
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      matchLabels: { "sso.lok8s.dev/protect": "true" }
  oidc: { clientID: x }
YAML
  audit::run_domain ovr
  assert_equal "$(_status_of exposed-endpoints)" "fail"
  assert_equal "$(_sev_of exposed-endpoints)" "high"
}

# MASKED-FINDING REGRESSION (subagent review round 3, PR #140): the override
# branch above used to `return 0` before the NodePort branch could run, so a
# cluster with BOTH holes was told about one of them. The operator fixed the
# mergeType, re-ran the audit, and only then met the node port. Two independent
# holes must be reported together — and in ONE finding, because the id is the
# report's key.
@test "an override and a NodePort found together are BOTH reported" {
  _spec both <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target both net/gw.yaml <<'YAML'
apiVersion: v1
kind: Service
metadata: { name: envoy }
spec:
  type: LoadBalancer
---
apiVersion: v1
kind: Service
metadata: { name: legacy-admin }
spec:
  type: NodePort
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: ip-allowlist }
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: sso-gate }
spec:
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      matchLabels: { "sso.lok8s.dev/protect": "true" }
  oidc: { clientID: x }
YAML
  audit::run_domain both
  assert_equal "$(_status_of exposed-endpoints)" "fail"
  # The override is the graver of the two, so it sets the severity.
  assert_equal "$(_sev_of exposed-endpoints)" "high"
  # One finding per check id — merging the two must not fork into two rows.
  assert_equal "$(_count_of exposed-endpoints)" "1"

  local detail
  detail="$(_detail_of exposed-endpoints)"
  [[ "${detail}" == *"mergeType"* ]] \
    || fail "the override is missing from the finding: ${detail}"
  [[ "${detail}" == *"NodePort"* ]] \
    || fail "the NodePort is masked by the override — both holes are real and they are fixed by different edits: ${detail}"
}

@test "the same route-level SecurityPolicy WITH mergeType keeps the carve-out" {
  _spec mrg <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target mrg net/gw.yaml <<'YAML'
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: sso-gate }
spec:
  mergeType: StrategicMerge
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      matchLabels: { "sso.lok8s.dev/protect": "true" }
  oidc: { clientID: x }
YAML
  audit::run_domain mrg
  assert_equal "$(_status_of exposed-endpoints)" "pass"
}

@test "a non-SecurityPolicy object quoting defaultAction: Deny is not a carve-out" {
  _spec deco <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  # The old text-only detector matched this ConfigMap and scored the HTTPRoute
  # below as allowlisted. Only a real SecurityPolicy may count.
  _target deco net/route.yaml <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: app }
---
apiVersion: v1
kind: ConfigMap
metadata: { name: docs }
data:
  snippet: |
    authorization:
      defaultAction: Deny
YAML
  audit::run_domain deco
  assert_equal "$(_status_of exposed-endpoints)" "warn"
}

@test "an unmerged route-level policy with no gateway-wide deny is not flagged" {
  _spec solo <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  # Nothing to override: no gateway-wide deny exists, so the missing mergeType
  # costs nothing and must not manufacture a finding.
  _target solo net/route.yaml <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: app }
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: sso-gate }
spec:
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
  oidc: { clientID: x }
YAML
  audit::run_domain solo
  assert_equal "$(_status_of exposed-endpoints)" "warn"
}

# ROUND 2: the mergeType test above only proved a GOOD value passes. The check
# read presence, and the CRD has no enum for this field — its only validation
# is the CEL rule `self != 'Replace'` — so every value below is admitted by the
# apiserver, merges NOTHING, and used to score a clean pass. One case per shape
# of the mistake: empty, plausible-but-wrong, wrong case.
@test "a route-level SecurityPolicy with a non-merging mergeType still cancels the deny" {
  local bad
  for bad in '""' 'Merge' 'strategicmerge' 'Replace'; do
    _spec "badmt" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
    _target "badmt" net/gw.yaml <<YAML
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: sso-gate }
spec:
  mergeType: ${bad}
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
  oidc: { clientID: x }
YAML
    audit::run_domain "badmt"
    assert_equal "mergeType=${bad} → $(_status_of exposed-endpoints)" "mergeType=${bad} → fail"
    assert_equal "mergeType=${bad} → $(_sev_of exposed-endpoints)" "mergeType=${bad} → high"
  done
}

@test "JSONMerge is the other real merge value and keeps the carve-out" {
  _spec jm <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target jm net/gw.yaml <<'YAML'
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: sso-gate }
spec:
  mergeType: JSONMerge
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
  oidc: { clientID: x }
YAML
  audit::run_domain jm
  assert_equal "$(_status_of exposed-endpoints)" "pass"
}

# ROUND 2, false positive: the route policy replaces the gateway's deny with a
# deny of its own. Nothing became reachable that was not reachable before, and
# a security gate that fires on a safe configuration teaches people to ignore
# it. Must not be counted.
@test "a route-level policy that re-denies by itself is not an override finding" {
  _spec redeny <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target redeny net/gw.yaml <<'YAML'
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: route-guard }
spec:
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal: { clientCIDRs: ["10.0.0.0/8"] }
YAML
  audit::run_domain redeny
  assert_equal "$(_status_of exposed-endpoints)" "pass"
}

# The exemption directly above, taken at its word, WAS the next fail-open: it
# honoured the DECLARED `defaultAction` and never read the rules underneath.
# `Deny` plus `Allow 0.0.0.0/0` is a deny-by-default that admits the entire
# internet, and with no mergeType it replaces the gateway's real allowlist with
# exactly that — while the audit printed "external exposure is IP-allowlisted".
# The manifests still read locked down; the cluster is open. Same class as the
# grep this scan replaced, one field deeper.
@test "a route-level Deny that allows every client still cancels the carve-out" {
  _spec dinamo <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target dinamo net/gw.yaml <<'YAML'
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal: { clientCIDRs: ["203.0.113.0/24"] }
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: deny-in-name-only }
spec:
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal: { clientCIDRs: ["0.0.0.0/0"] }
YAML
  audit::run_domain dinamo
  # Before this fix the scan scored `deny=1 open=0` and the check fell through
  # to the LoadBalancer+carve-out branch: status pass, severity low, detail
  # "external exposure is IP-allowlisted".
  assert_equal "$(_status_of exposed-endpoints)" "fail"
  assert_equal "$(_sev_of exposed-endpoints)" "high"
}

# A zero prefix length ignores the address, so `10.0.0.0/0` is every IPv4
# address wearing an RFC1918 costume. It is both the likeliest typo and the
# likeliest deliberate dodge, and matching the literal strings `0.0.0.0/0` and
# `::/0` would miss it.
@test "an allow-all is read from the /0 prefix, not from the address in front of it" {
  local cidr
  for cidr in '0.0.0.0/0' '::/0' '10.0.0.0/0'; do
    _spec zeropfx <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
    _target zeropfx net/gw.yaml <<YAML
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: deny-in-name-only }
spec:
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal: { clientCIDRs: ["${cidr}"] }
YAML
    audit::run_domain zeropfx
    assert_equal "${cidr} → $(_status_of exposed-endpoints)" "${cidr} → fail"
  done
}

# The other half of the same fix, and the half that decides whether anyone
# leaves the check switched on. A principal ANDs its fields, so
# `clientCIDRs: ["0.0.0.0/0"]` next to a `jwt` principal means "any IP, but
# only with this token" — a real guard. So does an `operation` that scopes the
# rule to some methods. Reading either as an allow-all would fire on a correct
# gateway, and a security gate that cries wolf gets muted.
@test "an Allow rule narrowed by JWT or by operation is not an allow-all" {
  local label spec_snippet
  # shellcheck disable=SC2043
  for label in jwt operation; do
    case "${label}" in
      jwt)       spec_snippet='        principal:
          clientCIDRs: ["0.0.0.0/0"]
          jwt: { scopes: ["admin"] }' ;;
      operation) spec_snippet='        operation: { methods: ["GET"] }
        principal: { clientCIDRs: ["0.0.0.0/0"] }' ;;
    esac
    _spec narrowed <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
    _target narrowed net/gw.yaml <<YAML
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata: { name: route-guard }
spec:
  targetSelectors:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
${spec_snippet}
YAML
    audit::run_domain narrowed
    assert_equal "${label} → $(_status_of exposed-endpoints)" "${label} → pass"
  done
}

# One reading of "denies", used for both counts. A gateway-wide Deny that
# admits every client is not a carve-out either, and scoring it as one is the
# same false "external exposure is IP-allowlisted" the route-level case
# produced — there is no allowlist to be behind.
@test "a gateway-wide Deny that allows every client is not a carve-out" {
  _spec gwopen <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target gwopen net/gw.yaml <<'YAML'
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
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: gw
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal: { clientCIDRs: ["0.0.0.0/0"] }
YAML
  audit::run_domain gwopen
  # No real carve-out exists, so the LoadBalancer is simply unguarded: the
  # honest verdict is the no-allowlist warn, never the allowlisted pass.
  assert_equal "$(_status_of exposed-endpoints)" "warn"
  local detail
  detail="$(_detail_of exposed-endpoints)"
  [[ "${detail}" != *"IP-allowlisted"* ]] \
    || fail "the audit still calls an allow-all Deny an IP allowlist: ${detail}"
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

# Copilot review round 2 (audit:636): prod-intent semantics — the doc says
# "everything except kind=lo is prod-intent"; the code treated an EMPTY kind as
# dev. Now they agree, fail-closed: only kind=lo is dev; empty/unknown kinds
# are scored like production.
@test "_prod_intent: kind=lo is dev; kubeone and EMPTY/unknown kinds are prod-intent" {
  run audit::_prod_intent lo
  assert_failure
  run audit::_prod_intent kubeone
  assert_success
  run audit::_prod_intent ""
  assert_success
  run audit::_prod_intent somefuturekind
  assert_success
}

@test "a spec with NO kind is scored prod-intent (fail-closed): EOL k8s is a fail, not a warn" {
  _spec nokind <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
metadata: { name: c }
spec:
  kubernetes: { version: "v1.31.4" }
  bootstrap: []
YAML
  audit::run_domain nokind
  assert_equal "$(_status_of k8s-version-support)" "fail"
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

@test "privileged check fires for hostNetwork/hostPath too (portable boundary)" {
  _spec hn <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: []
YAML
  _target hn app/pod.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata: { name: p }
spec:
  hostNetwork: true  # shares the node netns
  volumes:
    - name: h
      hostPath: { path: /var/run }
YAML
  audit::run_domain hn
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

@test "a high/critical unknown caps the grade — couldn't-check is not a perfect score" {
  # A deploy-only domain yields a single HIGH 'unknown' (cluster-spec); it must
  # NOT read as a perfect A/100 for score-keyed tooling.
  mkdir -p "${PATH_CLUSTERS}/blind"
  cat > "${PATH_CLUSTERS}/blind/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata: { name: d }
YAML
  _json() { audit::run_domain blind; audit::render_json blind; }
  run _json
  assert_success
  local grade score
  grade=$(echo "${output}" | jq -r '.grade')
  score=$(echo "${output}" | jq -r '.score')
  [ "${grade}" != "A" ]
  [ "${grade}" != "B" ]
  [ "${score}" -le 70 ]
}

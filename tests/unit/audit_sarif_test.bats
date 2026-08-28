#!/usr/bin/env bats
# audit_sarif_test.bats — unit tests for `lo audit --sarif` (SARIF 2.1.0 output)
#
# The SARIF document is what CI uploads to GitHub code scanning
# (github/codeql-action/upload-sarif), so the tests pin the envelope, the
# status → level mapping, the pass-exclusion, and the location contract:
# a finding keyed to one spec value carries a physicalLocation (repo-relative
# uri + startLine); an aggregate finding carries no locations at all.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }; export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/libs/audit"
}

teardown() { teardown_tmpdir; }

# --- fixture helpers ---------------------------------------------------------

# _spec <domain> — write a cluster.lok8s.yaml from stdin.
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

# _sarif <domain> — audit the domain and render the SARIF document (the same
# pipeline main::audit's --sarif branch runs for one domain).
_sarif() {
  audit::run_domain "$1"
  audit::render_sarif "$(audit::_sarif_findings "$1")"
}

# =============================================================================
# Envelope
# =============================================================================

@test "--sarif emits a valid SARIF 2.1.0 envelope (version, schema, tool, rules)" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec sarifdom <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  run _sarif sarifdom
  assert_success
  echo "${output}" | jq -e '.version == "2.1.0"'
  echo "${output}" | jq -e '."$schema" | test("sarif-schema-2.1.0")'
  echo "${output}" | jq -e '.runs | length == 1'
  echo "${output}" | jq -e '.runs[0].tool.driver.name == "lo-audit"'
  echo "${output}" | jq -e '.runs[0].tool.driver.informationUri | startswith("https://")'
  # every result references a declared rule, and every rule is referenced
  echo "${output}" | jq -e '
    ([.runs[0].results[].ruleId] | unique) == ([.runs[0].tool.driver.rules[].id] | sort)'
  echo "${output}" | jq -e '[.runs[0].tool.driver.rules[] | has("shortDescription")] | all'
}

# =============================================================================
# Locations
# =============================================================================

@test "a finding keyed to a spec value carries a repo-relative uri + startLine" {
  # v1.31 is EOL on a prod-intent cluster → k8s-version-support fails, and the
  # verdict is keyed to spec.kubernetes.version — line 5 of this fixture.
  _spec eolloc <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.31.4" }
  bootstrap: []
YAML
  run _sarif eolloc
  assert_success
  local result
  result=$(echo "${output}" | jq -c '.runs[0].results[] | select(.ruleId == "k8s-version-support")')
  echo "${result}" | jq -e '.level == "error"'
  # PATH_BASE is the tmpdir here, so the uri must come out repo-RELATIVE.
  echo "${result}" | jq -e '.locations[0].physicalLocation.artifactLocation.uri == "clusters/eolloc/cluster.lok8s.yaml"'
  echo "${result}" | jq -e '.locations[0].physicalLocation.region.startLine == 5'
}

@test "a plaintext OIDC issuer points at the issuer's own line" {
  _spec oidcloc <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  oidc: { issuer: "http://insecure.example", clientID: "1" }
  bootstrap: []
YAML
  run _sarif oidcloc
  assert_success
  local result
  result=$(echo "${output}" | jq -c '.runs[0].results[] | select(.ruleId == "plaintext-endpoints")')
  echo "${result}" | jq -e '.level == "error"'
  echo "${result}" | jq -e '.locations[0].physicalLocation.artifactLocation.uri == "clusters/oidcloc/cluster.lok8s.yaml"'
  echo "${result}" | jq -e '.locations[0].physicalLocation.region.startLine == 6'
}

@test "an aggregate finding falls back to the domain spec, with no line" {
  # The cilium verdict merges base/driver/provider/inline values, so there is
  # no one LINE to point at — but code scanning DROPS a result that carries no
  # location at all, which silently lost most findings. The domain spec is the
  # honest subject of every check, so it is the fallback uri; startLine is
  # omitted rather than fabricated.
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec noloc <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  run _sarif noloc
  assert_success
  echo "${output}" | jq -e '
    .runs[0].results[] | select(.ruleId == "cilium-policy-enforcement")
    | .locations[0].physicalLocation
    | (.artifactLocation.uri | endswith("clusters/noloc/cluster.lok8s.yaml"))
      and (has("region") | not)'
}

@test "every result carries a location — code scanning drops the ones that do not" {
  # The whole-envelope invariant behind the fallback above: not one result may
  # be location-less, or that finding never becomes an alert.
  _spec everyloc <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  run _sarif everyloc
  assert_success
  echo "${output}" | jq -e '
    (.runs[0].results | length) > 0
    and all(.runs[0].results[]; .locations[0].physicalLocation.artifactLocation.uri != null)'
}

@test "rules carry a security-severity from the worst severity, tagged security" {
  # GitHub derives alert severity from the RULE property, not the result bag —
  # without the tag + score every alert lands at the tool default.
  _spec sev <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  run _sarif sev
  assert_success
  echo "${output}" | jq -e '
    all(.runs[0].tool.driver.rules[];
        (.properties.tags | index("security"))
        and (.properties."security-severity" | tonumber) > 0)'
}

# =============================================================================
# Status → level mapping, pass exclusion
# =============================================================================

@test "status maps to level: fail → error, warn → warning, unknown → note" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  # cilium audit-mode → fail; an HTTPRoute with no carve-out → warn; no
  # encryption signal anywhere (no driver file, no artifacts) → unknown.
  _spec levels <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  _target levels net/route.yaml <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: app }
YAML
  run _sarif levels
  assert_success
  echo "${output}" | jq -e '.runs[0].results[] | select(.ruleId == "cilium-policy-enforcement") | .level == "error"'
  echo "${output}" | jq -e '.runs[0].results[] | select(.ruleId == "exposed-endpoints") | .level == "warning"'
  echo "${output}" | jq -e '.runs[0].results[] | select(.ruleId == "encryption-at-rest") | .level == "note"'
  # the finding's own severity/status ride along for downstream tooling
  echo "${output}" | jq -e '[.runs[0].results[] | .properties | has("severity") and has("status") and (.domain == "levels")] | all'
}

@test "pass findings are excluded — a clean cluster uploads zero alerts" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: false
policyEnforcementMode: default
YAML
  _kubeone_encryption true
  _spec cleanest <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  oidc: { issuer: "https://id.example.cloud", clientID: "1" }
  bootstrap: [ cilium ]
YAML
  # a benign target so the exposure scan has something to read (nothing to
  # scan is an honest `unknown`, and unknown is NOT excluded from results)
  _target cleanest app/cm.yaml <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata: { name: cm }
data: { greeting: "hello" }
YAML
  run _sarif cleanest
  assert_success
  # all six checks pass → a VALID envelope with results [] and rules []
  echo "${output}" | jq -e '.version == "2.1.0"'
  echo "${output}" | jq -e '.runs[0].results == []'
  echo "${output}" | jq -e '.runs[0].tool.driver.rules == []'
}

@test "an empty findings accumulator still renders a valid SARIF document" {
  _AUDIT_FINDINGS=()
  run audit::render_sarif "$(audit::_sarif_findings nowhere)"
  assert_success
  echo "${output}" | jq -e '.version == "2.1.0" and (.runs[0].results == [])'
}

# =============================================================================
# Message content and multi-domain accumulation
# =============================================================================

@test "the result message carries the detail and the remediation" {
  _cilium_values kubeone <<'YAML'
policyAuditMode: true
YAML
  _spec msg <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.35.5" }
  bootstrap: [ cilium ]
YAML
  run _sarif msg
  assert_success
  echo "${output}" | jq -e '
    .runs[0].results[] | select(.ruleId == "cilium-policy-enforcement")
    | .message.text | test("policyAuditMode") and test("Remediation:")'
}

@test "findings from several domains share one run, told apart by properties.domain" {
  _spec multi-a <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: a }
spec:
  kubernetes: { version: "v1.31.4" }
  bootstrap: []
YAML
  _spec multi-b <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: b }
spec:
  kubernetes: { version: "v1.31.4" }
  bootstrap: []
YAML
  _multi() {
    local findings d
    findings='[]'
    for d in multi-a multi-b; do
      audit::run_domain "${d}"
      findings=$(jq -cn --argjson acc "${findings}" --argjson add "$(audit::_sarif_findings "${d}")" '$acc + $add')
    done
    audit::render_sarif "${findings}"
  }
  run _multi
  assert_success
  echo "${output}" | jq -e '.runs | length == 1'
  echo "${output}" | jq -e '
    [.runs[0].results[] | select(.ruleId == "k8s-version-support") | .properties.domain]
    | sort == ["multi-a", "multi-b"]'
  # the shared rule is declared once, not per domain
  echo "${output}" | jq -e '[.runs[0].tool.driver.rules[].id] | unique == .'
}

# =============================================================================
# Helpers
# =============================================================================

@test "_rel_uri strips the repo root and leaves outside paths alone" {
  run audit::_rel_uri "${PATH_BASE}/clusters/x/cluster.lok8s.yaml"
  assert_output "clusters/x/cluster.lok8s.yaml"
  run audit::_rel_uri "/somewhere/else/cluster.lok8s.yaml"
  assert_output "/somewhere/else/cluster.lok8s.yaml"
}

@test "the default and --json outputs do not leak the location columns" {
  _spec plainout <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: c }
spec:
  kubernetes: { version: "v1.31.4" }
  bootstrap: []
YAML
  # human output: the remediation line must end at the remediation text — a
  # reader that stops one column short would drag "<TAB>file<TAB>line" along.
  _human() { audit::run_domain plainout; audit::render_human plainout; }
  run _human
  assert_success
  [[ "${output}" != *"cluster.lok8s.yaml"$'\t'* ]] || fail "location columns leaked into the human report"
  [[ "${output}" != *$'\t'* ]] || fail "a raw TAB leaked into the human report"
  # --json: the stable schema keeps exactly its six check fields
  _json() { audit::run_domain plainout; audit::render_json plainout; }
  run _json
  assert_success
  echo "${output}" | jq -e '
    [.checks[] | keys | sort == ["detail","id","remediation","severity","status","title"]] | all'
}

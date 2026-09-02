package audit

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// specWith builds a minimal cluster spec with extra spec-level YAML appended.
func setupDomain(t *testing.T, a *Auditor, name, spec string) string {
	t.Helper()
	dir := filepath.Join(a.Clusters, name)
	writeFileT(t, filepath.Join(dir, "cluster.lok8s.yaml"), spec)
	return dir
}

// ── encryption-at-rest ───────────────────────────────────────────────────────

func TestCheckEncryptionLoShortCircuits(t *testing.T) {
	a := newFixtureAuditor(t)
	// Even an explicit enable: false must not degrade a kind=lo cluster —
	// the short-circuit runs BEFORE any evaluation.
	setupDomain(t, a, "d.dev", "kind: Lo\nspec:\n  features:\n    encryptionProviders:\n      enable: false\n")
	f := findingByID(t, a.RunDomain("d.dev"), "encryption-at-rest")
	if f.Status != "pass" || f.Severity != "high" {
		t.Errorf("kind=lo must pass: %+v", f)
	}
	if !strings.Contains(f.Detail, "kind=lo") {
		t.Errorf("detail = %q", f.Detail)
	}
}

func TestCheckEncryptionSpecFlag(t *testing.T) {
	cases := []struct {
		name, spec, wantStatus, wantSeverity string
	}{
		{"explicit true passes", "kind: KubeOne\nspec:\n  features:\n    encryptionProviders:\n      enable: true\n", "pass", "high"},
		{"explicit false fails on prod intent", "kind: KubeOne\nspec:\n  features:\n    encryptionProviders:\n      enable: false\n", "fail", "high"},
		{"atRest fallback true", "kind: KubeOne\nspec:\n  encryption:\n    atRest: true\n", "pass", "high"},
		{"atRest fallback false", "kind: KubeOne\nspec:\n  encryption:\n    atRest: false\n", "fail", "high"},
		// Only an exact true/false counts — yes/True/1 are NOT booleans to
		// the string compare (yq preserves the literal).
		{"non-boolean literal is not a flag", "kind: Capi\nspec:\n  features:\n    encryptionProviders:\n      enable: yes\n", "unknown", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newFixtureAuditor(t)
			setupDomain(t, a, "d.dev", tc.spec)
			f := findingByID(t, a.RunDomain("d.dev"), "encryption-at-rest")
			if f.Status != tc.wantStatus || f.Severity != tc.wantSeverity {
				t.Errorf("status/severity = %s/%s, want %s/%s", f.Status, f.Severity, tc.wantStatus, tc.wantSeverity)
			}
		})
	}
}

func TestCheckEncryptionDriverDefault(t *testing.T) {
	a := newFixtureAuditor(t)
	setupDomain(t, a, "d.dev", "kind: KubeOne\n")
	writeFileT(t, filepath.Join(a.Lok8s, "drivers", "kubeone", "cluster", "core", "kubeone.yaml"),
		"features:\n  encryptionProviders:\n    enable: true\n")
	f := findingByID(t, a.RunDomain("d.dev"), "encryption-at-rest")
	if f.Status != "pass" {
		t.Errorf("kubeone driver default enable:true must pass, got %s", f.Status)
	}
}

func TestCheckEncryptionArtifacts(t *testing.T) {
	realProvider := `kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - aescbc:
          keys: []
      - identity: {}
`
	identityOnly := `kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - identity: {}
`
	// The apiserver honors the FIRST matching resources group: identity for
	// `secrets` listed before `*.* → aescbc` writes Secrets PLAINTEXT.
	identityFirstGroup := `kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - identity: {}
  - resources: ["*.*"]
    providers:
      - aescbc:
          keys: []
`
	wildcard := `kind: EncryptionConfiguration
resources:
  - resources: ["*.*"]
    providers:
      - secretbox:
          keys: []
`
	cases := []struct {
		name, artifacts, wantStatus string
	}{
		{"real provider proves encryption", realProvider, "pass"},
		{"identity-only present fails", identityOnly, "fail"},
		{"first covering group wins", identityFirstGroup, "fail"},
		{"wildcard group with real provider passes", wildcard, "pass"},
		// Present but unparseable = fail-CLOSED: the presence is readable
		// (grep), only the proof is not.
		{"unparseable config fails closed", "kind: EncryptionConfiguration\nresources: [::bad\n", "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newFixtureAuditor(t)
			dir := setupDomain(t, a, "d.dev", "kind: KubeOne\n")
			writeFileT(t, filepath.Join(dir, "artifacts.yaml"), tc.artifacts)
			f := findingByID(t, a.RunDomain("d.dev"), "encryption-at-rest")
			if f.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s (%s)", f.Status, tc.wantStatus, f.Detail)
			}
		})
	}
}

func TestCheckEncryptionUnknown(t *testing.T) {
	a := newFixtureAuditor(t)
	setupDomain(t, a, "d.dev", "kind: Capi\n")
	f := findingByID(t, a.RunDomain("d.dev"), "encryption-at-rest")
	if f.Status != "unknown" {
		t.Errorf("no signal must be unknown, got %s", f.Status)
	}
	if !strings.Contains(f.Detail, "kind=capi") {
		t.Errorf("detail = %q", f.Detail)
	}
	if !strings.Contains(f.Remediation, "lo build d.dev") {
		t.Errorf("remediation = %q", f.Remediation)
	}
}

// ── cilium-policy-enforcement ────────────────────────────────────────────────

// ciliumAddon writes a chart-shaped cilium addon with the given values files.
func ciliumAddon(t *testing.T, a *Auditor, values map[string]string) {
	t.Helper()
	dir := filepath.Join(a.Lok8s, "addons", "cilium")
	writeFileT(t, filepath.Join(dir, "chart.yaml"), "name: cilium\n")
	for name, content := range values {
		writeFileT(t, filepath.Join(dir, name), content)
	}
}

func TestCheckCiliumNotDeployed(t *testing.T) {
	a := newFixtureAuditor(t)
	// KubeOne with an ABSENT bootstrap key: the per-driver default adds
	// cilium only for kind=lo.
	setupDomain(t, a, "d.dev", "kind: KubeOne\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "pass" || !strings.Contains(f.Detail, "not in spec.bootstrap") {
		t.Errorf("%+v", f)
	}
}

func TestCheckCiliumExplicitEmptyOptOut(t *testing.T) {
	a := newFixtureAuditor(t)
	ciliumAddon(t, a, map[string]string{"values.yaml": "policyAuditMode: true\n"})
	// `bootstrap: []` is the authoritative opt-out even for kind=lo.
	setupDomain(t, a, "d.dev", "kind: Lo\nspec:\n  bootstrap: []\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "pass" || !strings.Contains(f.Detail, "not in spec.bootstrap") {
		t.Errorf("%+v", f)
	}
}

func TestCheckCiliumDefaultForLo(t *testing.T) {
	a := newFixtureAuditor(t)
	ciliumAddon(t, a, map[string]string{
		"values.yaml":    "policyAuditMode: false\n",
		"values.lo.yaml": "policyAuditMode: true\n",
	})
	setupDomain(t, a, "d.dev", "kind: Lo\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	// driver layer (values.lo.yaml) overrides base: audit mode on → FAIL.
	if f.Status != "fail" || !strings.Contains(f.Detail, "policyAuditMode: true") {
		t.Errorf("%+v", f)
	}
}

func TestCheckCiliumInlineOverrideWins(t *testing.T) {
	a := newFixtureAuditor(t)
	ciliumAddon(t, a, map[string]string{"values.yaml": "policyAuditMode: true\n"})
	setupDomain(t, a, "d.dev", `kind: Lo
spec:
  bootstrap:
    - cilium:
        policyAuditMode: false
        policyEnforcementMode: always
`)
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "pass" {
		t.Fatalf("inline override must win the merge: %+v", f)
	}
	if !strings.Contains(f.Detail, "policyEnforcementMode: always") {
		t.Errorf("detail must carry the effective mode: %q", f.Detail)
	}
}

func TestCheckCiliumEnforcementNever(t *testing.T) {
	a := newFixtureAuditor(t)
	ciliumAddon(t, a, map[string]string{"values.yaml": "policyAuditMode: false\npolicyEnforcementMode: never\n"})
	setupDomain(t, a, "d.dev", "kind: Lo\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "fail" || !strings.Contains(f.Detail, "policyEnforcementMode: never") {
		t.Errorf("%+v", f)
	}
}

func TestCheckCiliumProviderLayer(t *testing.T) {
	a := newFixtureAuditor(t)
	ciliumAddon(t, a, map[string]string{
		"values.yaml":         "policyAuditMode: false\n",
		"values.hetzner.yaml": "policyAuditMode: true\n",
	})
	setupDomain(t, a, "d.dev", "kind: Lo\nspec:\n  provider:\n    name: hetzner\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "fail" {
		t.Errorf("provider values layer must stack, got %s", f.Status)
	}
}

func TestCheckCiliumUnreadableValues(t *testing.T) {
	a := newFixtureAuditor(t)
	ciliumAddon(t, a, map[string]string{"values.yaml": "policyAuditMode: [broken\n"})
	setupDomain(t, a, "d.dev", "kind: Lo\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "unknown" {
		t.Errorf("unparseable values must be unknown, never a fall-through pass: %+v", f)
	}
}

func TestCheckCiliumNonBooleanAuditModeIsUnknown(t *testing.T) {
	a := newFixtureAuditor(t)
	// yq preserves the literal: `True` is not `true` to the string compare.
	ciliumAddon(t, a, map[string]string{"values.yaml": "policyAuditMode: True\n"})
	setupDomain(t, a, "d.dev", "kind: Lo\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "unknown" {
		t.Errorf("non-canonical boolean literal must be unknown: %+v", f)
	}
}

func TestCheckCiliumNoValuesAtAll(t *testing.T) {
	a := newFixtureAuditor(t)
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "chart.yaml"), "name: cilium\n")
	setupDomain(t, a, "d.dev", "kind: Lo\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "unknown" || !strings.Contains(f.Detail, "Could not read Cilium values under ") {
		t.Errorf("%+v", f)
	}
}

func TestCheckCiliumMalformedEntrySkipped(t *testing.T) {
	a := newFixtureAuditor(t)
	ciliumAddon(t, a, map[string]string{"values.yaml": "policyAuditMode: true\n"})
	// Multi-key map entry is malformed → skipped silently → cilium "not in
	// spec.bootstrap" (the preserved bash `|| continue` quirk).
	setupDomain(t, a, "d.dev", `kind: Lo
spec:
  bootstrap:
    - cilium: {}
      other: {}
`)
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "pass" || !strings.Contains(f.Detail, "not in spec.bootstrap") {
		t.Errorf("%+v", f)
	}
}

func TestCheckCiliumTargetPathEntry(t *testing.T) {
	a := newFixtureAuditor(t)
	dir := setupDomain(t, a, "d.dev", `kind: KubeOne
spec:
  bootstrap:
    - ./targets/cilium
`)
	writeFileT(t, filepath.Join(dir, "targets", "cilium", "values.yaml"), "policyAuditMode: true\n")
	f := findingByID(t, a.RunDomain("d.dev"), "cilium-policy-enforcement")
	if f.Status != "fail" {
		t.Errorf("path-resolved cilium dir must be audited: %+v", f)
	}
}

// ── exposed-endpoints ────────────────────────────────────────────────────────

func exposedFinding(t *testing.T, targets map[string]string) Finding {
	t.Helper()
	a := newFixtureAuditor(t)
	dir := setupDomain(t, a, "d.dev", "kind: KubeOne\n")
	for name, content := range targets {
		writeFileT(t, filepath.Join(dir, name), content)
	}
	return findingByID(t, a.RunDomain("d.dev"), "exposed-endpoints")
}

func TestCheckExposedNothingToScan(t *testing.T) {
	f := exposedFinding(t, nil)
	if f.Status != "unknown" || f.Severity != "medium" {
		t.Errorf("%+v", f)
	}
}

func TestCheckExposedNodePort(t *testing.T) {
	f := exposedFinding(t, map[string]string{"targets/svc.yaml": "spec:\n  type: NodePort\n"})
	if f.Status != "fail" || f.Severity != "medium" ||
		!strings.Contains(f.Detail, "1 manifest(s) declare a NodePort Service") {
		t.Errorf("%+v", f)
	}
}

func TestCheckExposedWordBoundary(t *testing.T) {
	// NodePortXyz must NOT match — the portable word-boundary contract.
	f := exposedFinding(t, map[string]string{"targets/svc.yaml": "spec:\n  type: NodePortXyz\n"})
	if f.Status != "pass" {
		t.Errorf("NodePortXyz must not count as NodePort: %+v", f)
	}
}

func TestCheckExposedRoutesWithoutDeny(t *testing.T) {
	f := exposedFinding(t, map[string]string{"targets/route.yaml": "kind: HTTPRoute\n"})
	if f.Status != "warn" || !strings.Contains(f.Detail, "1 HTTPRoute(s) exposed with no default-Deny") {
		t.Errorf("%+v", f)
	}
}

func TestCheckExposedLoadBalancer(t *testing.T) {
	f := exposedFinding(t, map[string]string{"targets/svc.yaml": "spec:\n  type: LoadBalancer\n"})
	if f.Status != "warn" || f.Severity != "low" {
		t.Errorf("%+v", f)
	}
}

const gatewayDeny = `kind: SecurityPolicy
spec:
  targetRefs:
    - kind: Gateway
      name: shared
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal:
          clientCIDRs: ["10.0.0.0/8"]
`

func TestCheckExposedLBWithDenyPasses(t *testing.T) {
	f := exposedFinding(t, map[string]string{
		"targets/svc.yaml":    "spec:\n  type: LoadBalancer\n",
		"targets/policy.yaml": gatewayDeny,
	})
	if f.Status != "pass" || !strings.Contains(f.Detail, "1 LoadBalancer + a default-Deny") {
		t.Errorf("%+v", f)
	}
}

func TestCheckExposedRouteOverrideCancelsDeny(t *testing.T) {
	f := exposedFinding(t, map[string]string{
		"targets/policy.yaml": gatewayDeny + `---
kind: SecurityPolicy
spec:
  targetRef:
    kind: HTTPRoute
    name: app
  authorization:
    defaultAction: Allow
`,
	})
	if f.Status != "fail" || f.Severity != "high" {
		t.Fatalf("unmerged route policy must cancel the carve-out: %+v", f)
	}
	if !strings.Contains(f.Detail, "1 route-level SecurityPolicy(ies) carry no usable mergeType") {
		t.Errorf("detail = %q", f.Detail)
	}
	if strings.Contains(f.Detail, "SEPARATELY") {
		t.Errorf("no NodePort → no SEPARATELY clause: %q", f.Detail)
	}
}

func TestCheckExposedOverridePlusNodePortSharesOneFinding(t *testing.T) {
	f := exposedFinding(t, map[string]string{
		"targets/svc.yaml": "spec:\n  type: NodePort\n",
		"targets/policy.yaml": gatewayDeny + `---
kind: SecurityPolicy
spec:
  targetRef:
    kind: HTTPRoute
    name: app
  authorization:
    defaultAction: Allow
`,
	})
	if f.Status != "fail" || f.Severity != "high" {
		t.Fatalf("%+v", f)
	}
	// The NodePort is reported WITH the override, not swallowed by it (one
	// finding per check id is the report's contract).
	if !strings.Contains(f.Detail, "SEPARATELY, 1 manifest(s) declare a NodePort Service") {
		t.Errorf("detail = %q", f.Detail)
	}
	if !strings.Contains(f.Remediation, "Then handle the NodePort Service(s) as their own fix") {
		t.Errorf("remediation = %q", f.Remediation)
	}
}

func TestCheckExposedMergedRoutePolicyKeepsDeny(t *testing.T) {
	f := exposedFinding(t, map[string]string{
		"targets/svc.yaml": "spec:\n  type: LoadBalancer\n",
		"targets/policy.yaml": gatewayDeny + `---
kind: SecurityPolicy
spec:
  targetRef:
    kind: HTTPRoute
    name: app
  mergeType: StrategicMerge
  authorization:
    defaultAction: Allow
`,
	})
	if f.Status != "pass" {
		t.Errorf("StrategicMerge route policy must not cancel the deny: %+v", f)
	}
}

func TestCheckExposedScansArtifactsToo(t *testing.T) {
	a := newFixtureAuditor(t)
	dir := setupDomain(t, a, "d.dev", "kind: KubeOne\n")
	writeFileT(t, filepath.Join(dir, "artifacts.yaml"), "spec:\n  type: NodePort\n")
	f := findingByID(t, a.RunDomain("d.dev"), "exposed-endpoints")
	if f.Status != "fail" {
		t.Errorf("artifacts.yaml must be scanned: %+v", f)
	}
}

// ── k8s-version-support ──────────────────────────────────────────────────────

func k8sFinding(t *testing.T, spec string) Finding {
	t.Helper()
	a := newFixtureAuditor(t)
	setupDomain(t, a, "d.dev", spec)
	return findingByID(t, a.RunDomain("d.dev"), "k8s-version-support")
}

func TestCheckK8sVersion(t *testing.T) {
	cases := []struct {
		name, version, kind, wantStatus, wantSeverity, wantDetail string
	}{
		{"supported passes", "v1.35.4", "KubeOne", "pass", "high", "Kubernetes 1.35 is within the supported window (1.34, 1.35, 1.36)."},
		{"eol fails on prod intent", "v1.33.2", "KubeOne", "fail", "high", "Kubernetes 1.33 is End-of-Life (oldest supported minor: 1.34) — no upstream security patches."},
		{"eol warns on dev", "v1.33.2", "Lo", "warn", "medium", "Kubernetes 1.33 is End-of-Life (oldest supported minor: 1.34) — no upstream security patches."},
		{"newer than known warns", "v1.99.0", "KubeOne", "warn", "low", "Kubernetes 1.99 is newer than the newest minor this audit knows (1.36) — the support list may be stale."},
		{"pinned digest suffix stripped", "v1.34.1@sha256:abc", "KubeOne", "pass", "high", "Kubernetes 1.34 is within the supported window (1.34, 1.35, 1.36)."},
		{"unparseable is unknown", "latest", "KubeOne", "unknown", "medium", "Could not parse spec.kubernetes.version ('latest')."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := k8sFinding(t, fmt.Sprintf("kind: %s\nspec:\n  kubernetes:\n    version: %s\n", tc.kind, tc.version))
			if f.Status != tc.wantStatus || f.Severity != tc.wantSeverity {
				t.Errorf("status/severity = %s/%s, want %s/%s", f.Status, f.Severity, tc.wantStatus, tc.wantSeverity)
			}
			if f.Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", f.Detail, tc.wantDetail)
			}
			// Every verdict is keyed to one spec value → every finding
			// carries its location for SARIF.
			if f.File == "" {
				t.Error("k8s-version finding must carry its file")
			}
			if f.Line != 4 {
				t.Errorf("line = %d, want 4 (the version key's line)", f.Line)
			}
		})
	}
}

func TestCheckK8sVersionAbsent(t *testing.T) {
	f := k8sFinding(t, "kind: KubeOne\n")
	if f.Status != "unknown" || f.Detail != "Could not parse spec.kubernetes.version ('<empty>')." {
		t.Errorf("%+v", f)
	}
	if f.Line != 0 {
		t.Errorf("absent key line = %d, want 0", f.Line)
	}
}

// ── privileged-workloads ─────────────────────────────────────────────────────

func TestCheckPrivileged(t *testing.T) {
	cases := []struct {
		name       string
		targets    map[string]string
		wantStatus string
		wantDetail string
	}{
		{"no targets", nil, "pass", "No per-cluster target manifests to scan (framework addons are vetted separately)."},
		{"clean targets", map[string]string{"targets/a.yaml": "kind: ConfigMap\n"}, "pass",
			"No privileged / hostNetwork / hostPath usage in per-cluster targets."},
		{"elevated access", map[string]string{
			"targets/a.yaml": "privileged: true\nhostNetwork: true\n",
			"targets/b.yml":  "hostPath:\n  path: /\n",
		}, "warn", "Per-cluster targets use elevated access: privileged=1, hostNetwork=1, hostPath=1 manifest(s)."},
		{"privilegedX word boundary", map[string]string{"targets/a.yaml": "privileged: trueX\n"}, "pass",
			"No privileged / hostNetwork / hostPath usage in per-cluster targets."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newFixtureAuditor(t)
			dir := setupDomain(t, a, "d.dev", "kind: KubeOne\n")
			for name, content := range tc.targets {
				writeFileT(t, filepath.Join(dir, name), content)
			}
			f := findingByID(t, a.RunDomain("d.dev"), "privileged-workloads")
			if f.Status != tc.wantStatus || f.Detail != tc.wantDetail {
				t.Errorf("got %s %q, want %s %q", f.Status, f.Detail, tc.wantStatus, tc.wantDetail)
			}
		})
	}
}

func TestCheckPrivilegedIgnoresArtifacts(t *testing.T) {
	a := newFixtureAuditor(t)
	dir := setupDomain(t, a, "d.dev", "kind: KubeOne\n")
	// Scoped to targets/ — the vetted framework addons (rendered into
	// artifacts) legitimately need host access.
	writeFileT(t, filepath.Join(dir, "artifacts.yaml"), "privileged: true\n")
	f := findingByID(t, a.RunDomain("d.dev"), "privileged-workloads")
	if f.Status != "pass" {
		t.Errorf("artifacts must not count for privileged: %+v", f)
	}
}

// ── plaintext-endpoints ──────────────────────────────────────────────────────

func TestCheckPlaintextIssuer(t *testing.T) {
	a := newFixtureAuditor(t)
	setupDomain(t, a, "d.dev", "kind: KubeOne\nspec:\n  oidc:\n    issuer: http://id.example.com\n")
	f := findingByID(t, a.RunDomain("d.dev"), "plaintext-endpoints")
	if f.Status != "fail" || f.Severity != "high" {
		t.Fatalf("%+v", f)
	}
	if f.Detail != "spec.oidc.issuer is not HTTPS ('http://id.example.com') — the apiserver would trust an OIDC token issuer over plaintext." {
		t.Errorf("detail = %q", f.Detail)
	}
	if f.File == "" || f.Line != 4 {
		t.Errorf("issuer finding must carry file+line, got %q:%d", f.File, f.Line)
	}
}

func TestCheckPlaintextTargets(t *testing.T) {
	a := newFixtureAuditor(t)
	dir := setupDomain(t, a, "d.dev", "kind: KubeOne\nspec:\n  oidc:\n    issuer: https://id.example.com\n")
	writeFileT(t, filepath.Join(dir, "targets", "cm.yaml"), strings.Join([]string{
		"a: http://plain.example.com/path",
		"dup: http://plain.example.com/other", // same endpoint → sort -u dedupes
		"b: http://second.example.org",
		"skip1: http://localhost:8080",
		"skip2: http://127.0.0.1/x",
		"skip3: http://www.w3.org/2001/XMLSchema",
		"skip4: http://schemas.example.com/x",
		"skip5: http://svc.ns.svc.cluster.local",
		""}, "\n"))
	f := findingByID(t, a.RunDomain("d.dev"), "plaintext-endpoints")
	if f.Status != "warn" {
		t.Fatalf("%+v", f)
	}
	if f.Detail != "2 plaintext http:// endpoint(s) referenced in per-cluster targets." {
		t.Errorf("detail = %q (exclusions/dedupe broken)", f.Detail)
	}
}

func TestCheckPlaintextClean(t *testing.T) {
	a := newFixtureAuditor(t)
	setupDomain(t, a, "d.dev", "kind: KubeOne\nspec:\n  oidc:\n    issuer: https://id.example.com\n")
	f := findingByID(t, a.RunDomain("d.dev"), "plaintext-endpoints")
	if f.Status != "pass" || f.Detail != "No plaintext OIDC issuer or http:// endpoints found." {
		t.Errorf("%+v", f)
	}
}

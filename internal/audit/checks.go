package audit

// The six audit checks, transcribed 1:1 from .lok8s/libs/audit — every
// message, severity, status, and branch order is load-bearing (the parity
// harness diffs the output byte-for-byte against the bash implementation).

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/assets"
)

var minorRe = regexp.MustCompile(`^([0-9]+)\.([0-9]+)`)

// checkEncryption — is the apiserver told to encrypt Secrets before writing
// them to etcd? For KubeOne the authoritative static signal is the driver's
// features.encryptionProviders.enable; a spec override wins; a rendered
// EncryptionConfiguration in artifacts is accepted as proof ONLY when it uses
// a real provider for the `secrets` resource — an identity-only or empty
// config encrypts NOTHING and is treated as DISABLED, not pass. FAIL if
// explicitly disabled on a prod-intent cluster. kind=lo (local kind/dev)
// short-circuits to n/a (pass) FIRST — the documented contract — so no other
// path (an explicit enable: false, an identity-only config) can degrade a
// throwaway dev cluster to warn.
func (a *Auditor) checkEncryption(r *run, domainName, domainDir, specFile, kind string) {
	const id = "encryption-at-rest"
	const title = "Secret encryption at rest (etcd)"

	if kind == "lo" {
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "pass",
			Detail:      "kind=lo (local kind/dev cluster) — at-rest encryption is not required for a throwaway cluster.",
			Remediation: "For a production cluster (KubeOne), keep features.encryptionProviders.enable: true (the shipped default)."})
		return
	}

	// 1. Spec override wins (a user can force it on or off). Read WITHOUT the
	// `//` default operator — `//` treats a literal `false` as empty and
	// would swallow an explicit enable: false, mis-reporting a disabled
	// cluster as unset. yqRenderNode keeps the raw literal ("null" when
	// absent), so ONLY an exact true/false counts — the same strictness as
	// the bash string compare.
	specDoc := firstDocNode(specFile)
	specEnable := yqRenderNode(lookupPath(specDoc, "spec", "features", "encryptionProviders", "enable"))
	if specEnable == "null" {
		specEnable = yqRenderNode(lookupPath(specDoc, "spec", "encryption", "atRest"))
	}
	if specEnable == "null" {
		specEnable = ""
	}

	// 2. KubeOne driver default — the authoritative static signal for kubeone.
	driverEnable := ""
	koCore, _, _ := assets.Peek(a.paths(), "drivers/kubeone/cluster/core/kubeone.yaml")
	if kind == "kubeone" && isFile(koCore) {
		driverEnable = yqRenderNode(lookupFile(koCore, "features", "encryptionProviders", "enable"))
		if driverEnable == "null" {
			driverEnable = ""
		}
	}

	// 3. Rendered-artifacts fallback (any driver): a REAL
	// EncryptionConfiguration. Presence records that a config exists;
	// encryptionEncryptsSecrets requires a real provider to be the write
	// provider for the `secrets` resource.
	artifacts := artifactFiles(domainDir)
	artifactPresent := artifactsContain(artifacts, "kind: EncryptionConfiguration")
	artifactEncrypts := artifactPresent && encryptionEncryptsSecrets(artifacts)

	enabled := "" // true | false | "" (unknown)
	if specEnable == "true" || specEnable == "false" {
		enabled = specEnable
	} else if driverEnable != "" {
		enabled = driverEnable
	}

	// PASS: an explicit enable, or artifacts that PROVABLY encrypt Secrets.
	if enabled == "true" || artifactEncrypts {
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "pass",
			Detail:      "At-rest encryption is configured — the apiserver encrypts Secrets before writing them to etcd.",
			Remediation: "No action. Rotate the encryption key periodically (KubeOne re-encrypts Secrets on apply)."})
		return
	}
	// FAIL/WARN: explicitly disabled (flag false) OR a rendered
	// EncryptionConfiguration that leaves `secrets` unencrypted (identity-only
	// / empty / identity as the write provider). Both leave Secrets base64 in
	// etcd.
	if enabled == "false" || artifactPresent {
		severity, status := "high", "fail"
		if !prodIntent(kind) {
			severity, status = "medium", "warn"
		}
		detail := "A rendered EncryptionConfiguration does NOT encrypt Secrets — no aescbc/secretbox/kms provider is the write provider for the 'secrets' resource (identity-only or empty) — Secrets are stored base64-only in etcd."
		if enabled == "false" {
			detail = "At-rest encryption is DISABLED (encryptionProviders.enable: false) — Secrets are stored base64-only in etcd."
		}
		r.emit(Finding{ID: id, Title: title, Severity: severity, Status: status,
			Detail:      detail,
			Remediation: "Set features.encryptionProviders.enable: true (KubeOne driver) and re-provision so the apiserver encrypts Secrets with a real provider (aescbc/secretbox/kms), not identity."})
		return
	}
	r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "unknown",
		Detail:      "Could not statically determine at-rest encryption for kind=" + kind + " — no spec/driver flag and no rendered EncryptionConfiguration.",
		Remediation: "Run 'lo build " + domainName + "' to render artifacts, or confirm the control plane sets --encryption-provider-config."})
}

// checkCilium — AUDIT mode logs policy verdicts but enforces NOTHING, so a
// cluster in policyAuditMode: true has NetworkPolicies that block nothing.
// This is a HIGH finding and is the shipped default for KubeOne
// (addons/cilium/values.kubeone.yaml: policyAuditMode: true), so it fires on
// the stock pilot config — by design.
func (a *Auditor) checkCilium(r *run, domainName, specFile, kind, provider string) {
	const id = "cilium-policy-enforcement"
	const title = "Cilium network-policy enforcement"

	// Is cilium deployed? Resolve spec.bootstrap through the SAME parser the
	// apply path uses, then find the entry whose resolved dir basename is
	// 'cilium'. A malformed entry is skipped silently, like the bash
	// `parse_entry … 2>/dev/null || continue`.
	var cil bootstrapEntry
	found := false
	for _, entry := range resolveBootstrapEntries(specFile, kind) {
		e, ok := a.parseBootstrapEntry(domainName, entry)
		if !ok {
			continue
		}
		if filepath.Base(e.dir) == "cilium" {
			cil = e
			found = true
			break
		}
	}
	if !found {
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "pass",
			Detail:      "Cilium is not in spec.bootstrap — network-policy enforcement is provided by another CNI (not audited here).",
			Remediation: "If this cluster uses a different CNI, ensure its NetworkPolicy support is enabled and enforcing."})
		return
	}

	// Effective values: base < driver(kind) < provider < inline (later wins)
	// — the same stacking addons::render uses, but a pure deep merge (no
	// kustomize/network).
	var vfiles []string
	for _, name := range []string{"values.yaml", "values." + kind + ".yaml"} {
		if isFile(filepath.Join(cil.dir, name)) {
			vfiles = append(vfiles, filepath.Join(cil.dir, name))
		}
	}
	if provider != "" && isFile(filepath.Join(cil.dir, "values."+provider+".yaml")) {
		vfiles = append(vfiles, filepath.Join(cil.dir, "values."+provider+".yaml"))
	}
	hasInline := cil.inlineInclude
	if len(vfiles) == 0 && !hasInline {
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "unknown",
			Detail:      "Could not read Cilium values under " + cil.dir + ".",
			Remediation: "Ensure the cilium addon ships values.yaml / values." + kind + ".yaml."})
		return
	}
	var inline = cil.inline
	if !hasInline {
		inline = nil
	}
	auditMode, enforceMode := "", ""
	if merged, err := mergeYAMLDocs(vfiles, inline); err == nil {
		auditMode = altNode(mapValue(merged, "policyAuditMode"), "false")
		enforceMode = altNode(mapValue(merged, "policyEnforcementMode"), "")
	}
	// Merge/parse failure (e.g. an unparseable inline override in
	// spec.bootstrap) → UNKNOWN, never a fall-through pass and never a crash:
	// values we cannot read cannot prove enforcement is on. A successful read
	// always yields "true"/"false" for auditMode, so anything else means
	// unreadable values.
	if auditMode != "true" && auditMode != "false" {
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "unknown",
			Detail:      "Could not parse/merge the effective Cilium values (base/driver/provider/inline override) — the values are unreadable, so the enforcement state cannot be proven.",
			Remediation: "Fix the YAML (addons/cilium/values*.yaml or the spec.bootstrap cilium override), then re-run 'lo audit'."})
		return
	}

	if auditMode == "true" {
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "fail",
			Detail:      "Cilium policyAuditMode: true — NetworkPolicies (incl. host-firewall CCNPs) are LOGGED, not enforced; nothing is actually blocked.",
			Remediation: "Validate the allow-set (hubble observe --verdict AUDIT covers etcd 2379/2380, apiserver 6443, kubelet 10250, vxlan 8472), then set policyAuditMode: false in addons/cilium/values." + kind + ".yaml (or a spec.bootstrap cilium override) to ENFORCE."})
		return
	}
	if enforceMode == "never" {
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "fail",
			Detail:      "Cilium policyEnforcementMode: never — NetworkPolicy enforcement is disabled cluster-wide.",
			Remediation: "Set policyEnforcementMode to 'default' (or 'always') so NetworkPolicies are honored."})
		return
	}
	if enforceMode == "" {
		enforceMode = "default"
	}
	r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "pass",
		Detail:      "Cilium enforces NetworkPolicies (policyAuditMode: false, policyEnforcementMode: " + enforceMode + ").",
		Remediation: "No action — keep policyAuditMode off; add NetworkPolicies to restrict east-west traffic."})
}

// checkExposed — NodePort Services (reachable on every node), LoadBalancer
// Services, and HTTPRoutes without a default-Deny SecurityPolicy
// (IP-allowlist) carve-out, plus route-level policies that silently CANCEL
// such a carve-out (see secpolScan). Scans the cluster's own targets + any
// `lo build` artifacts. Fail-soft: nothing to scan → unknown.
func (a *Auditor) checkExposed(r *run, domainName, domainDir string) {
	const id = "exposed-endpoints"
	const title = "Publicly-exposed endpoints"

	files := append(targetFiles(domainDir), artifactFiles(domainDir)...)
	if len(files) == 0 {
		r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "unknown",
			Detail:      "No rendered targets/artifacts found to scan for exposed endpoints.",
			Remediation: "Run 'lo build " + domainName + "' first for full endpoint coverage."})
		return
	}

	nodeport := grepCountFiles(reNodePort, files)
	lb := grepCountFiles(reLoadBalancer, files)
	routes := grepCountFiles(reHTTPRoute, files)
	denyPolicies, openPolicies := secpolScan(files)
	secpolDeny := denyPolicies > 0

	// The deny is only a carve-out if nothing quietly cancels it. A
	// route-level SecurityPolicy without a usable mergeType REPLACES the
	// gateway-wide one for every route it selects. A NodePort found at the
	// same time is reported WITH the override, not swallowed by it (one
	// finding per check id is the report's contract — the renderers and the
	// score walk a flat list, and duplicate ids would double-count a check).
	if denyPolicies > 0 && openPolicies > 0 {
		detail := fmt.Sprintf("%d route-level SecurityPolicy(ies) carry no usable mergeType, so each REPLACES the gateway-wide default-Deny for the routes it selects — the allowlist survives in the manifests and is gone at runtime. Not correlated by gateway: confirm the flagged policies select routes attached to the Gateway that carries the deny.", openPolicies)
		remedy := "Set 'mergeType: StrategicMerge' (or 'JSONMerge') on every route-level SecurityPolicy (needs Envoy Gateway v1.8+) so it merges into the Gateway's policy instead of replacing it. No other value merges — the CRD has no enum, so a typo is accepted and silently replaces."
		if nodeport > 0 {
			detail += fmt.Sprintf(" SEPARATELY, %d manifest(s) declare a NodePort Service, reachable on EVERY node's IP — that one bypasses the gateway and its allowlist outright, so fixing the mergeType does not close it.", nodeport)
			remedy += " Then handle the NodePort Service(s) as their own fix: route through the shared LoadBalancer/gateway, or restrict the node ports at the firewall."
		}
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "fail", Detail: detail, Remediation: remedy})
		return
	}

	if nodeport > 0 {
		r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "fail",
			Detail:      fmt.Sprintf("%d manifest(s) declare a NodePort Service — a node port is reachable on EVERY node's IP, bypassing the gateway.", nodeport),
			Remediation: "Route through the single shared LoadBalancer/gateway instead; drop NodePort or restrict it at the firewall."})
		return
	}
	if routes > 0 && !secpolDeny {
		r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "warn",
			Detail:      fmt.Sprintf("%d HTTPRoute(s) exposed with no default-Deny SecurityPolicy (IP-allowlist) carve-out.", routes),
			Remediation: "For a pre-prod/locked-down plane attach an Envoy Gateway SecurityPolicy (authorization.defaultAction: Deny + allow CIDRs) to the Gateway."})
		return
	}
	if lb > 0 && secpolDeny {
		r.emit(Finding{ID: id, Title: title, Severity: "low", Status: "pass",
			Detail:      fmt.Sprintf("%d LoadBalancer + a default-Deny SecurityPolicy carve-out — external exposure is IP-allowlisted.", lb),
			Remediation: "No action — keep the allowlist current; widen it only deliberately."})
		return
	}
	if lb > 0 {
		r.emit(Finding{ID: id, Title: title, Severity: "low", Status: "warn",
			Detail:      fmt.Sprintf("%d LoadBalancer Service exposed (expected for the shared gateway); no default-Deny allowlist detected.", lb),
			Remediation: "Fine for a public plane fronted by TLS + per-route auth; add a SecurityPolicy allowlist for a locked-down plane."})
		return
	}
	r.emit(Finding{ID: id, Title: title, Severity: "low", Status: "pass",
		Detail:      "No NodePort/LoadBalancer/HTTPRoute exposure found in the scanned manifests.",
		Remediation: "No action."})
}

// checkK8sVersion — is spec.kubernetes.version still a supported minor? EOL →
// FAIL (prod-intent) / WARN (dev); newer-than-known → WARN (update the list);
// unparseable → unknown. Every verdict here is keyed to ONE spec value, so
// every finding carries its location (file + the version key's line) for the
// SARIF renderer.
func (a *Auditor) checkK8sVersion(r *run, specFile, kind string) {
	const id = "k8s-version-support"
	const title = "Kubernetes version support (EOL)"

	specURI := a.relURI(specFile)
	specLineno := specLine(specFile, "spec", "kubernetes", "version")
	supported := strings.Join(k8sSupportedMinors, ", ")

	raw := altNode(lookupFile(specFile, "spec", "kubernetes", "version"), "")
	if i := strings.Index(raw, "@"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimPrefix(raw, "v")
	m := minorRe.FindStringSubmatch(raw)
	if m == nil {
		display := raw
		if display == "" {
			display = "<empty>"
		}
		r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "unknown",
			Detail:      "Could not parse spec.kubernetes.version ('" + display + "').",
			Remediation: "Set spec.kubernetes.version to a supported release, e.g. v" + k8sLatestMinor + ".x.",
			File:        specURI, Line: specLineno})
		return
	}
	minor := m[1] + "." + m[2]
	rank := minorRank(minor)
	oldest := k8sSupportedMinors[0]

	for _, s := range k8sSupportedMinors {
		if s == minor {
			r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "pass",
				Detail:      "Kubernetes " + minor + " is within the supported window (" + supported + ").",
				Remediation: "Keep upgrading within the window; plan the next minor before this one reaches EOL.",
				File:        specURI, Line: specLineno})
			return
		}
	}
	if rank > minorRank(k8sLatestMinor) {
		r.emit(Finding{ID: id, Title: title, Severity: "low", Status: "warn",
			Detail:      "Kubernetes " + minor + " is newer than the newest minor this audit knows (" + k8sLatestMinor + ") — the support list may be stale.",
			Remediation: "Update _AUDIT_K8S_SUPPORTED_MINORS in .lok8s/libs/audit (see https://kubernetes.io/releases/).",
			File:        specURI, Line: specLineno})
		return
	}
	severity, status := "high", "fail"
	if !prodIntent(kind) {
		severity, status = "medium", "warn"
	}
	r.emit(Finding{ID: id, Title: title, Severity: severity, Status: status,
		Detail:      "Kubernetes " + minor + " is End-of-Life (oldest supported minor: " + oldest + ") — no upstream security patches.",
		Remediation: "Upgrade to a supported minor (" + supported + "); for KubeOne bump spec.kubernetes.version and run 'lo provision'.",
		File:        specURI, Line: specLineno})
}

// minorRank orders "maj.min" numerically (bash: audit::_minor_rank).
func minorRank(v string) int {
	m := minorRe.FindStringSubmatch(v)
	if m == nil {
		return 0
	}
	maj, min := atoi(m[1]), atoi(m[2])
	return maj*1000 + min
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// checkPrivileged — privileged securityContext, hostNetwork, or hostPath in
// the cluster's OWN targets. Scoped to targets/ (not the vetted framework
// addons, which legitimately need host access) to avoid noise. WARN, since
// some per-cluster glue genuinely needs it — the point is to make it visible.
func (a *Auditor) checkPrivileged(r *run, domainDir string) {
	const id = "privileged-workloads"
	const title = "Privileged / host-level workloads"

	files := targetFiles(domainDir)
	if len(files) == 0 {
		r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "pass",
			Detail:      "No per-cluster target manifests to scan (framework addons are vetted separately).",
			Remediation: "No action."})
		return
	}
	priv := grepCountFiles(rePrivileged, files)
	hostNet := grepCountFiles(reHostNetwork, files)
	hostPath := grepCountFiles(reHostPath, files)
	if priv+hostNet+hostPath == 0 {
		r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "pass",
			Detail:      "No privileged / hostNetwork / hostPath usage in per-cluster targets.",
			Remediation: "No action."})
		return
	}
	r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "warn",
		Detail:      fmt.Sprintf("Per-cluster targets use elevated access: privileged=%d, hostNetwork=%d, hostPath=%d manifest(s).", priv, hostNet, hostPath),
		Remediation: "Confirm each is required; drop privileged/hostNetwork/hostPath where possible and prefer a least-privilege securityContext."})
}

// checkPlaintext — a non-HTTPS OIDC issuer is a FAIL (the apiserver would
// trust tokens over cleartext); http:// endpoints referenced in per-cluster
// targets are a WARN. Best-effort scan (excludes localhost / schema URLs /
// in-cluster .svc).
func (a *Auditor) checkPlaintext(r *run, domainDir, specFile string) {
	const id = "plaintext-endpoints"
	const title = "Plaintext (non-HTTPS) endpoints"

	issuer := altNode(lookupFile(specFile, "spec", "oidc", "issuer"), "")
	if issuer != "" && !strings.HasPrefix(issuer, "https://") {
		// This verdict is keyed to one spec value → carry its location for SARIF.
		r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "fail",
			Detail:      "spec.oidc.issuer is not HTTPS ('" + issuer + "') — the apiserver would trust an OIDC token issuer over plaintext.",
			Remediation: "Use an https:// URL for spec.oidc.issuer.",
			File:        a.relURI(specFile), Line: specLine(specFile, "spec", "oidc", "issuer")})
		return
	}

	if hits := plaintextHits(targetFiles(domainDir)); hits > 0 {
		r.emit(Finding{ID: id, Title: title, Severity: "medium", Status: "warn",
			Detail:      fmt.Sprintf("%d plaintext http:// endpoint(s) referenced in per-cluster targets.", hits),
			Remediation: "Prefer https:// for every external endpoint; terminate TLS at the shared Envoy gateway."})
		return
	}
	r.emit(Finding{ID: id, Title: title, Severity: "high", Status: "pass",
		Detail:      "No plaintext OIDC issuer or http:// endpoints found.",
		Remediation: "Keep all external endpoints on HTTPS."})
}

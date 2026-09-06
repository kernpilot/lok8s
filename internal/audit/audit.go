// Package audit is the Go port of the argsh static security-posture audit
// (.lok8s/libs/audit): a linter-style command that reads cluster.lok8s.yaml +
// the rendered addon/kustomize inputs (exactly like `lo lint` — NO live
// cluster needed) and reports security findings with a severity, a
// per-cluster score, and a non-zero exit when any FAIL-level finding is
// present.
//
// Every check is a separate, independently testable function and is
// FAIL-SOFT: an input it cannot read yields an `unknown` finding, never an
// error — the audit never blocks and never touches a cluster. One deliberate
// fail-CLOSED exception: an EncryptionConfiguration that is PRESENT but
// unparseable counts as not-encrypting → FAIL, not unknown (its presence is
// readable; only the proof is not — see encryptionEncryptsSecrets).
//
// Output contracts (all byte-parity with the bash implementation):
//   - human report (RenderHuman), ordered fail → warn → unknown → pass;
//   - `--json` (RenderJSON), the STABLE dashboard schema, hand-rolled key
//     order;
//   - `--sarif` (RenderSarif), SARIF 2.1.0 in jq's pretty-print format for
//     GitHub code scanning.
package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
)

// Supported Kubernetes minors. A static list is intentional (cluster-free).
// UPDATE THIS when new minors release or old ones reach End-of-Life — see
// https://kubernetes.io/releases/ (upstream supports the newest 3 minors; a
// minor is EOL ~14 months after release). Mirrors _AUDIT_K8S_SUPPORTED_MINORS
// in .lok8s/libs/audit — keep the two lists in lockstep.
var k8sSupportedMinors = []string{"1.34", "1.35", "1.36"}

const k8sLatestMinor = "1.36"

// domainNameRe is the same path-traversal guard bootstrap::dispatch /
// provision::resolve_spec use (and domain.NameRe): reject an injected domain
// before it builds any filesystem path.
var domainNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Finding is one audit verdict. File/Line are OPTIONAL: the source location
// of the finding when it HAS a single one (a spec-file key). Aggregate
// findings (a scan over many manifests) leave them empty. Only the SARIF
// renderer reads them (the JSON schema deliberately omits locations).
type Finding struct {
	ID          string
	Title       string
	Severity    string // critical | high | medium | low
	Status      string // pass | warn | fail | unknown
	Detail      string
	Remediation string
	File        string
	Line        int
}

// Auditor carries the resolved project layout the checks read from.
type Auditor struct {
	Base     string // repo root (bash: PATH_BASE) — SARIF uris are relative to it
	Clusters string // clusters/ dir (bash: PATH_CLUSTERS)
	Lok8s    string // framework tree (bash: PATH_LOK8S)
	// Paths is the full layout, for the embedded-asset resolver (the
	// addon dirs and the kubeone core template read through assets.Peek —
	// read-only, so the audit never ejects anything).
	Paths *config.Paths
}

// New builds an Auditor from the resolved project paths.
func New(paths *config.Paths) *Auditor {
	return &Auditor{Base: paths.Base, Clusters: paths.Clusters, Lok8s: paths.Lok8s, Paths: paths}
}

// paths returns the layout for the asset resolver, derived from the three
// strings when an Auditor was built by hand (tests, the bash-shaped
// constructor).
func (a *Auditor) paths() *config.Paths {
	if a.Paths != nil {
		return a.Paths
	}
	return &config.Paths{Base: a.Base, Clusters: a.Clusters, Lok8s: a.Lok8s}
}

// run accumulates findings for one domain (bash: _AUDIT_FINDINGS).
type run struct {
	findings []Finding
}

// emit appends a finding, defensively stripping embedded tabs/newlines from
// the free-text fields exactly like audit::_emit (the bash stores findings as
// TSV lines; the Go port keeps the same normalization so the rendered output
// is byte-identical even for spec-derived text).
func (r *run) emit(f Finding) {
	f.Detail = stripTSV(f.Detail)
	f.Remediation = stripTSV(f.Remediation)
	f.File = stripTSV(f.File)
	r.findings = append(r.findings, f)
}

func stripTSV(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

// RunDomain resolves the domain's spec + kind + provider, then runs every
// check. Findings carry status — a domain that cannot be audited yields
// `unknown` findings, never an error (bash: audit::run_domain).
func (a *Auditor) RunDomain(d string) []Finding {
	r := &run{}

	if !domainNameRe.MatchString(d) {
		r.emit(Finding{ID: "cluster-spec", Title: "Cluster spec", Severity: "high", Status: "unknown",
			Detail:      "Invalid domain name '" + d + "'.",
			Remediation: "Use a domain under clusters/."})
		return r.findings
	}

	domainDir := filepath.Join(a.Clusters, d)
	specFile := filepath.Join(domainDir, "cluster.lok8s.yaml")
	if !isFile(specFile) {
		r.emit(Finding{ID: "cluster-spec", Title: "Cluster spec", Severity: "high", Status: "unknown",
			Detail:      "No cluster.lok8s.yaml under " + domainDir + " (deploy-only or missing domain).",
			Remediation: "Audit the referenced cluster (spec.clusterRef.domain) instead."})
		return r.findings
	}

	kind, err := domain.SpecDriver(specFile, "")
	if err != nil {
		kind = "" // bash: kind=$(domain::spec_driver …) || kind=""
	}
	provider := altNode(lookupFile(specFile, "spec", "provider", "name"), "")

	a.checkEncryption(r, d, domainDir, specFile, kind)
	a.checkCilium(r, d, specFile, kind, provider)
	a.checkExposed(r, d, domainDir)
	a.checkK8sVersion(r, specFile, kind)
	a.checkPrivileged(r, domainDir)
	a.checkPlaintext(r, domainDir, specFile)
	return r.findings
}

// HasFail reports whether any finding carries status fail — the one thing
// that turns the exit code non-zero (bash: audit::_count_status fail).
func HasFail(findings []Finding) bool {
	for _, f := range findings {
		if f.Status == "fail" {
			return true
		}
	}
	return false
}

// prodIntent is true for everything EXCEPT kind=lo. kind=lo is the local
// kind/dev driver; kubeone/capi/kkp are real infra. An EMPTY/unknown kind
// counts as prod-intent too — fail-closed: a cluster we cannot prove to be a
// dev cluster is scored like production (bash: audit::_prod_intent).
func prodIntent(kind string) bool {
	return kind != "lo"
}

// sevRank orders severities for the human report (critical first).
func sevRank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	}
	return 4
}

// weight is the score penalty for one warn/fail finding (bash: audit::_weight).
func weight(severity, kind string) int {
	if kind == "fail" {
		switch severity {
		case "critical":
			return 40
		case "high":
			return 25
		case "medium":
			return 15
		case "low":
			return 5
		}
		return 10
	}
	switch severity {
	case "critical":
		return 15
	case "high":
		return 10
	case "medium":
		return 5
	case "low":
		return 2
	}
	return 5
}

// score computes the domain score and summary counts (bash: audit::_score).
// Start at 100; subtract a severity-weighted penalty per warn/fail; clamp
// [0,100]. A high/critical check we could not evaluate must NOT read as a
// perfect score: "couldn't check" is not "passed" — cap the score at 70
// (grade C at best) so score-keyed tooling can't grade an un-auditable
// cluster A/B.
func score(findings []Finding) (score int, grade string, pass, warn, fail, unknown int) {
	score = 100
	blind := false
	for _, f := range findings {
		switch f.Status {
		case "pass":
			pass++
		case "warn":
			warn++
			score -= weight(f.Severity, "warn")
		case "fail":
			fail++
			score -= weight(f.Severity, "fail")
		case "unknown":
			unknown++
			if f.Severity == "critical" || f.Severity == "high" {
				blind = true
			}
		}
	}
	if score < 0 {
		score = 0
	}
	if blind && score > 70 {
		score = 70
	}
	grade = "F"
	switch {
	case score >= 90:
		grade = "A"
	case score >= 80:
		grade = "B"
	case score >= 70:
		grade = "C"
	case score >= 60:
		grade = "D"
	}
	return score, grade, pass, warn, fail, unknown
}

// relURI maps a path to repo-relative form for SARIF artifactLocation.uri
// (code scanning maps repo-relative uris to alerts). A path outside the repo
// stays as it is (bash: audit::_rel_uri).
func (a *Auditor) relURI(path string) string {
	if a.Base != "" && strings.HasPrefix(path, a.Base+"/") {
		return strings.TrimPrefix(path, a.Base+"/")
	}
	return path
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

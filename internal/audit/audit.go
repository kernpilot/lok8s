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
	"slices"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// k8sMinorEOL is the Kubernetes support table, newest minor first: the date
// (UTC, yyyy-mm-dd) upstream patch support ends for each minor. A static table
// is intentional (the audit is cluster-free and offline). A minor is supported
// while its date is in the future, so a minor reaches End-of-Life on its date
// with no change here. ADD a row when a new minor releases (about every four
// months; https://kubernetes.io/releases/, https://endoflife.date/kubernetes).
// Keep at least one row past its EOL: it names the oldest minor the table
// knows. Mirrors _AUDIT_K8S_MINOR_EOL in .lok8s/libs/audit;
// TestK8sMinorEOLMatchesBash fails when the two differ.
var k8sMinorEOL = []k8sMinorRow{
	{"1.37", "2027-10-28"},
	{"1.36", "2027-06-28"},
	{"1.35", "2027-02-28"},
	{"1.34", "2026-10-27"},
	{"1.33", "2026-06-28"},
}

// k8sMinorRow is one row of k8sMinorEOL.
type k8sMinorRow struct {
	minor string
	eol   string
}

// k8sTodayEnv pins the audit's date (yyyy-mm-dd) for tests, in both
// implementations (bash: the same variable). Unset, the audit uses today (UTC).
const k8sTodayEnv = "_AUDIT_TODAY"

var isoDateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// k8sToday is the date the support table is read against (bash:
// audit::_k8s_today).
func k8sToday() string {
	if v := os.Getenv(k8sTodayEnv); isoDateRe.MatchString(v) {
		return v
	}
	return time.Now().UTC().Format("2006-01-02")
}

// k8sSupportedMinors are the minors whose EOL date is after `today`, oldest
// first (bash: audit::_k8s_supported).
func k8sSupportedMinors(today string) []string {
	var out []string
	for _, row := range slices.Backward(k8sMinorEOL) {
		if row.eol > today {
			out = append(out, row.minor)
		}
	}
	return out
}

// k8sLatestMinor is the newest minor the table knows.
func k8sLatestMinor() string {
	return k8sMinorEOL[0].minor
}

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
	// Paths is the project layout: Base (bash: PATH_BASE; SARIF uris are
	// relative to it), Clusters (PATH_CLUSTERS), Lok8s (PATH_LOK8S), and
	// the rest for the embedded-asset resolver (the addon dirs and the
	// kubeone core template read through assets.Peek, read-only, so the
	// audit never ejects anything).
	Paths *config.Paths
}

// New builds an Auditor from the resolved project paths.
func New(paths *config.Paths) *Auditor {
	return &Auditor{Paths: paths}
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

	domainDir := filepath.Join(a.Paths.Clusters, d)
	specFile := filepath.Join(domainDir, "cluster.lok8s.yaml")
	if !fsutil.FileExists(specFile) {
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
	if a.Paths.Base != "" && strings.HasPrefix(path, a.Paths.Base+"/") {
		return strings.TrimPrefix(path, a.Paths.Base+"/")
	}
	return path
}

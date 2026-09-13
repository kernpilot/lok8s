package audit

// Renderers for the human report and the stable JSON schema (bash:
// audit::render_human / audit::render_json). Byte-parity is the contract.

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// RenderHuman writes the human-readable report for one domain's findings.
// Ordered fail → warn → unknown → pass, and within each group by severity
// (critical first) — the most actionable first. Colorless for deterministic
// output/piping.
func RenderHuman(w io.Writer, domainName string, findings []Finding) {
	sc, grade, p, wa, f, u := score(findings)
	total := p + wa + f + u

	fmt.Fprintf(w, "\n  audit · %s   score %d/100  (grade %s)\n", domainName, sc, grade)
	fmt.Fprintf(w, "  %d checks · %d fail · %d warn · %d unknown · %d pass\n\n", total, f, wa, u, p)

	for _, status := range []string{"fail", "warn", "unknown", "pass"} {
		renderStatusGroup(w, findings, status)
	}
}

func renderStatusGroup(w io.Writer, findings []Finding, want string) {
	var rows []Finding
	for _, f := range findings {
		if f.Status == want {
			rows = append(rows, f)
		}
	}
	if len(rows) == 0 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return sevRank(rows[i].Severity) < sevRank(rows[j].Severity)
	})
	for _, f := range rows {
		fmt.Fprintf(w, "  %-4s [%-8s] %s — %s\n", statusLabel(f.Status), f.Severity, f.ID, f.Title)
		fmt.Fprintf(w, "         %s\n", f.Detail)
		if f.Status != "pass" {
			fmt.Fprintf(w, "         remediation: %s\n", f.Remediation)
		}
	}
	fmt.Fprintf(w, "\n")
}

func statusLabel(status string) string {
	switch status {
	case "fail":
		return "FAIL"
	case "warn":
		return "WARN"
	case "pass":
		return "PASS"
	}
	return "UNKN"
}

// RenderJSON writes the machine-readable object — the STABLE schema the
// dashboard consumes. Hand-rolled with explicit key order and the same
// minimal escaping set as the bash (audit::_json_escape); locations are
// deliberately NOT emitted (they are SARIF's).
func RenderJSON(w io.Writer, domainName string, findings []Finding) {
	sc, grade, p, wa, f, u := score(findings)

	var b strings.Builder
	b.WriteString("{")
	fmt.Fprintf(&b, `"domain":"%s",`, jsonEscape(domainName))
	fmt.Fprintf(&b, `"score":%d,`, sc)
	fmt.Fprintf(&b, `"grade":"%s",`, grade)
	fmt.Fprintf(&b, `"summary":{"pass":%d,"warn":%d,"fail":%d,"unknown":%d},`, p, wa, f, u)
	b.WriteString(`"checks":[`)
	for i, fd := range findings {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"%s","title":"%s","severity":"%s","status":"%s","detail":"%s","remediation":"%s"}`,
			jsonEscape(fd.ID), jsonEscape(fd.Title),
			jsonEscape(fd.Severity), jsonEscape(fd.Status),
			jsonEscape(fd.Detail), jsonEscape(fd.Remediation))
	}
	b.WriteString("]}\n")
	io.WriteString(w, b.String())
}

// jsonEscape mirrors audit::_json_escape: backslash, quote, and the three
// common control characters — nothing more (the emitted text never carries
// other controls; matching the bash exactly beats gold-plating here).
func jsonEscape(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\t", `\t`,
		"\n", `\n`,
		"\r", `\r`,
	)
	return r.Replace(s)
}

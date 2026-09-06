package audit

// SARIF 2.1.0 renderer for GitHub code scanning (bash: audit::render_sarif).
// One run, tool lo-audit. Non-pass findings become results — fail → error,
// warn → warning, unknown → note (unknown IS surfaced: "couldn't check" is a
// finding, not a confirmation). Pass findings are omitted: code scanning
// tracks problems, and an all-pass audit must upload as zero alerts, not six.
// rules[] carries exactly the ruleIds the results reference, each tagged
// security with a security-severity derived from the audit severity (the
// WORST per id) — that rule property is what GitHub turns into alert
// severity; a result-level property bag is ignored. EVERY result carries a
// location: the finding's own spec key when it has one (uri + startLine),
// otherwise the domain's cluster.lok8s.yaml (uri only) — code scanning drops
// a location-less result, so an aggregate finding without this fallback
// never becomes an alert.
//
// The bash builds this document with jq, so the Go port reproduces jq's
// output format exactly: 2-space indent, insertion-order object keys,
// `[]`/`{}` inline for empty containers, no HTML escaping. group_by(.id)
// SORTS the rules by id; results keep finding order.

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// SarifFinding is one finding tagged with the domain it belongs to and the
// domain's fallback location (bash: audit::_sarif_findings). One SARIF run
// may span several domains — each result names its domain in properties.
type SarifFinding struct {
	Finding
	Domain     string
	DefaultURI string
}

// SarifFindings tags a domain's findings for the SARIF run. Every finding is
// ABOUT this domain's spec, so that file is the honest fallback location for
// the aggregate checks (a scan over many manifests has no single line, but it
// does have a subject).
func (a *Auditor) SarifFindings(domainName string, findings []Finding) []SarifFinding {
	defaultURI := a.relURI(filepath.Join(a.Clusters, domainName, "cluster.lok8s.yaml"))
	out := make([]SarifFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, SarifFinding{Finding: f, Domain: domainName, DefaultURI: defaultURI})
	}
	return out
}

// securitySeverity maps an audit severity to GitHub's security-severity
// scale. The strings match jq's number→string rendering of the literals in
// the bash program (9.5 / 8.0 / 5.0 / 2.0 — jq preserves the literal form).
func securitySeverity(severity string) (rank float64, repr string) {
	switch severity {
	case "critical":
		return 9.5, "9.5"
	case "high":
		return 8.0, "8.0"
	case "medium":
		return 5.0, "5.0"
	}
	return 2.0, "2.0"
}

// RenderSarif writes the SARIF 2.1.0 document for the accumulated findings.
func RenderSarif(w io.Writer, findings []SarifFinding) {
	var results []SarifFinding
	for _, f := range findings {
		if f.Status != "pass" {
			results = append(results, f)
		}
	}

	// rules: one entry per referenced ruleId, sorted (jq group_by sorts by
	// the grouping key); shortDescription from the FIRST finding with that
	// id, security-severity the WORST across the group.
	type ruleInfo struct {
		title string
		rank  float64
		repr  string
	}
	rules := map[string]*ruleInfo{}
	var ruleIDs []string
	for _, f := range results {
		rank, repr := securitySeverity(f.Severity)
		if ri, seen := rules[f.ID]; seen {
			if rank > ri.rank {
				ri.rank, ri.repr = rank, repr
			}
			continue
		}
		rules[f.ID] = &ruleInfo{title: f.Title, rank: rank, repr: repr}
		ruleIDs = append(ruleIDs, f.ID)
	}
	sort.Strings(ruleIDs)

	rulesArr := jArr{}
	for _, id := range ruleIDs {
		ri := rules[id]
		rulesArr = append(rulesArr, jObj{
			{"id", id},
			{"shortDescription", jObj{{"text", ri.title}}},
			{"properties", jObj{
				{"security-severity", ri.repr},
				{"tags", jArr{"security"}},
			}},
		})
	}

	resultsArr := jArr{}
	for _, f := range results {
		level := "note"
		switch f.Status {
		case "fail":
			level = "error"
		case "warn":
			level = "warning"
		}
		text := f.Detail
		if f.Remediation != "" {
			text = f.Detail + " Remediation: " + f.Remediation
		}
		uri := f.File
		if uri == "" {
			uri = f.DefaultURI
		}
		physical := jObj{{"artifactLocation", jObj{{"uri", uri}}}}
		if f.File != "" && f.Line > 0 {
			physical = append(physical, jPair{"region", jObj{{"startLine", f.Line}}})
		}
		resultsArr = append(resultsArr, jObj{
			{"ruleId", f.ID},
			{"level", level},
			{"message", jObj{{"text", text}}},
			{"properties", jObj{
				{"domain", f.Domain},
				{"severity", f.Severity},
				{"status", f.Status},
			}},
			{"locations", jArr{jObj{{"physicalLocation", physical}}}},
		})
	}

	doc := jObj{
		{"$schema", "https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json"},
		{"version", "2.1.0"},
		{"runs", jArr{jObj{
			{"tool", jObj{{"driver", jObj{
				{"name", "lo-audit"},
				{"informationUri", "https://lok8s.io/guide/audit"},
				{"rules", rulesArr},
			}}}},
			{"results", resultsArr},
		}}},
	}

	var b strings.Builder
	writeJQ(&b, doc, 0)
	b.WriteString("\n")
	io.WriteString(w, b.String())
}

// ── jq-format JSON writer ────────────────────────────────────────────────────

// jObj is an insertion-ordered JSON object; jArr a JSON array. Values may be
// string, int, jObj, or jArr.
type jPair struct {
	k string
	v any
}
type jObj []jPair
type jArr []any

// writeJQ renders a value the way `jq .` pretty-prints: 2-space indent,
// `"key": value`, empty containers inline.
func writeJQ(b *strings.Builder, v any, depth int) {
	switch t := v.(type) {
	case jObj:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, p := range t {
			writeIndent(b, depth+1)
			writeJQString(b, p.k)
			b.WriteString(": ")
			writeJQ(b, p.v, depth+1)
			if i < len(t)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		writeIndent(b, depth)
		b.WriteString("}")
	case jArr:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range t {
			writeIndent(b, depth+1)
			writeJQ(b, e, depth+1)
			if i < len(t)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		writeIndent(b, depth)
		b.WriteString("]")
	case string:
		writeJQString(b, t)
	case int:
		fmt.Fprintf(b, "%d", t)
	default:
		// The renderer only ever passes the types above; anything else is a
		// programming error made loud, not silent JSON corruption.
		panic(fmt.Sprintf("writeJQ: unsupported type %T", v))
	}
}

func writeIndent(b *strings.Builder, depth int) {
	for range depth {
		b.WriteString("  ")
	}
}

// writeJQString escapes like jq: mandatory escapes plus \uXXXX for other
// control characters, raw UTF-8 for everything else (no HTML escaping).
func writeJQString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

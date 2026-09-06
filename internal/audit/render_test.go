package audit

import (
	"strings"
	"testing"
)

func sampleFindings() []Finding {
	return []Finding{
		{ID: "encryption-at-rest", Title: "Secret encryption at rest (etcd)", Severity: "high", Status: "pass",
			Detail: "ok", Remediation: "none"},
		{ID: "cilium-policy-enforcement", Title: "Cilium network-policy enforcement", Severity: "high", Status: "fail",
			Detail: "audit mode", Remediation: "enforce"},
		{ID: "exposed-endpoints", Title: "Publicly-exposed endpoints", Severity: "low", Status: "warn",
			Detail: "1 LoadBalancer", Remediation: "allowlist"},
		{ID: "k8s-version-support", Title: "Kubernetes version support (EOL)", Severity: "critical", Status: "fail",
			Detail: "eol", Remediation: "upgrade", File: "clusters/d.dev/cluster.lok8s.yaml", Line: 4},
		{ID: "privileged-workloads", Title: "Privileged / host-level workloads", Severity: "medium", Status: "unknown",
			Detail: "cannot read", Remediation: "look"},
	}
}

func TestRenderHumanGolden(t *testing.T) {
	var b strings.Builder
	RenderHuman(&b, "d.dev", sampleFindings())
	want := "\n" +
		"  audit · d.dev   score 33/100  (grade F)\n" +
		"  5 checks · 2 fail · 1 warn · 1 unknown · 1 pass\n" +
		"\n" +
		"  FAIL [critical] k8s-version-support — Kubernetes version support (EOL)\n" +
		"         eol\n" +
		"         remediation: upgrade\n" +
		"  FAIL [high    ] cilium-policy-enforcement — Cilium network-policy enforcement\n" +
		"         audit mode\n" +
		"         remediation: enforce\n" +
		"\n" +
		"  WARN [low     ] exposed-endpoints — Publicly-exposed endpoints\n" +
		"         1 LoadBalancer\n" +
		"         remediation: allowlist\n" +
		"\n" +
		"  UNKN [medium  ] privileged-workloads — Privileged / host-level workloads\n" +
		"         cannot read\n" +
		"         remediation: look\n" +
		"\n" +
		"  PASS [high    ] encryption-at-rest — Secret encryption at rest (etcd)\n" +
		"         ok\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("human render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderHumanSkipsEmptyGroups(t *testing.T) {
	var b strings.Builder
	RenderHuman(&b, "d.dev", []Finding{{ID: "x", Title: "X", Severity: "low", Status: "pass", Detail: "d"}})
	got := b.String()
	// Only the pass group prints; no stray blank lines from empty groups.
	if strings.Count(got, "PASS") != 1 || strings.Contains(got, "FAIL") {
		t.Errorf("got:\n%s", got)
	}
	if !strings.HasSuffix(got, "         d\n\n") {
		t.Errorf("group must end with exactly one blank line:\n%q", got)
	}
}

func TestRenderJSONGolden(t *testing.T) {
	var b strings.Builder
	RenderJSON(&b, "d.dev", []Finding{
		{ID: "a", Title: "A \"quoted\"", Severity: "high", Status: "fail",
			Detail: `back\slash`, Remediation: "r"},
		{ID: "b", Title: "B", Severity: "low", Status: "pass", Detail: "d", Remediation: "n"},
	})
	want := `{"domain":"d.dev","score":75,"grade":"C","summary":{"pass":1,"warn":0,"fail":1,"unknown":0},` +
		`"checks":[{"id":"a","title":"A \"quoted\"","severity":"high","status":"fail","detail":"back\\slash","remediation":"r"},` +
		`{"id":"b","title":"B","severity":"low","status":"pass","detail":"d","remediation":"n"}]}` + "\n"
	if got := b.String(); got != want {
		t.Errorf("json mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestRenderJSONChecksKeepRunOrder(t *testing.T) {
	// JSON emits findings in RUN order (the human report re-sorts; the JSON
	// schema does not).
	var b strings.Builder
	RenderJSON(&b, "d", []Finding{
		{ID: "z", Status: "pass"}, {ID: "a", Status: "fail"},
	})
	out := b.String()
	if strings.Index(out, `"id":"z"`) > strings.Index(out, `"id":"a"`) {
		t.Errorf("checks order must be run order: %s", out)
	}
}

func TestJSONEscape(t *testing.T) {
	if got := jsonEscape("a\\b\"c\td\ne\rf"); got != `a\\b\"c\td\ne\rf` {
		t.Errorf("jsonEscape = %q", got)
	}
}

func TestRenderSarifGolden(t *testing.T) {
	findings := []SarifFinding{
		{Finding: Finding{ID: "k8s-version-support", Title: "Kubernetes version support (EOL)",
			Severity: "high", Status: "fail", Detail: "eol.", Remediation: "upgrade.",
			File: "clusters/d.dev/cluster.lok8s.yaml", Line: 4},
			Domain: "d.dev", DefaultURI: "clusters/d.dev/cluster.lok8s.yaml"},
		{Finding: Finding{ID: "exposed-endpoints", Title: "Publicly-exposed endpoints",
			Severity: "medium", Status: "warn", Detail: "lb.", Remediation: "allow."},
			Domain: "d.dev", DefaultURI: "clusters/d.dev/cluster.lok8s.yaml"},
		{Finding: Finding{ID: "encryption-at-rest", Title: "Secret encryption at rest (etcd)",
			Severity: "high", Status: "pass", Detail: "ok.", Remediation: "none."},
			Domain: "d.dev", DefaultURI: "clusters/d.dev/cluster.lok8s.yaml"},
	}
	var b strings.Builder
	RenderSarif(&b, findings)
	want := `{
  "$schema": "https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json",
  "version": "2.1.0",
  "runs": [
    {
      "tool": {
        "driver": {
          "name": "lo-audit",
          "informationUri": "https://lok8s.io/guide/audit",
          "rules": [
            {
              "id": "exposed-endpoints",
              "shortDescription": {
                "text": "Publicly-exposed endpoints"
              },
              "properties": {
                "security-severity": "5.0",
                "tags": [
                  "security"
                ]
              }
            },
            {
              "id": "k8s-version-support",
              "shortDescription": {
                "text": "Kubernetes version support (EOL)"
              },
              "properties": {
                "security-severity": "8.0",
                "tags": [
                  "security"
                ]
              }
            }
          ]
        }
      },
      "results": [
        {
          "ruleId": "k8s-version-support",
          "level": "error",
          "message": {
            "text": "eol. Remediation: upgrade."
          },
          "properties": {
            "domain": "d.dev",
            "severity": "high",
            "status": "fail"
          },
          "locations": [
            {
              "physicalLocation": {
                "artifactLocation": {
                  "uri": "clusters/d.dev/cluster.lok8s.yaml"
                },
                "region": {
                  "startLine": 4
                }
              }
            }
          ]
        },
        {
          "ruleId": "exposed-endpoints",
          "level": "warning",
          "message": {
            "text": "lb. Remediation: allow."
          },
          "properties": {
            "domain": "d.dev",
            "severity": "medium",
            "status": "warn"
          },
          "locations": [
            {
              "physicalLocation": {
                "artifactLocation": {
                  "uri": "clusters/d.dev/cluster.lok8s.yaml"
                }
              }
            }
          ]
        }
      ]
    }
  ]
}
`
	if got := b.String(); got != want {
		t.Errorf("sarif mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderSarifAllPass(t *testing.T) {
	// An all-pass audit must upload as ZERO alerts — pass findings are
	// omitted, and the empty containers print inline like jq.
	var b strings.Builder
	RenderSarif(&b, []SarifFinding{
		{Finding: Finding{ID: "a", Status: "pass"}, Domain: "d", DefaultURI: "u"},
	})
	out := b.String()
	if !strings.Contains(out, `"rules": []`) || !strings.Contains(out, `"results": []`) {
		t.Errorf("empty rules/results must render as []:\n%s", out)
	}
}

func TestRenderSarifWorstSeverityPerRule(t *testing.T) {
	// One id emitting different severities on different paths → the rule
	// carries the WORST security-severity.
	findings := []SarifFinding{
		{Finding: Finding{ID: "x", Title: "X", Severity: "medium", Status: "warn", Detail: "a"}, Domain: "d", DefaultURI: "u"},
		{Finding: Finding{ID: "x", Title: "X", Severity: "critical", Status: "fail", Detail: "b"}, Domain: "d", DefaultURI: "u"},
	}
	var b strings.Builder
	RenderSarif(&b, findings)
	if !strings.Contains(b.String(), `"security-severity": "9.5"`) {
		t.Errorf("worst severity must win:\n%s", b.String())
	}
}

func TestRenderSarifUnknownIsNote(t *testing.T) {
	// unknown IS surfaced — "couldn't check" is a finding, not a
	// confirmation — and a finding with no remediation gets the bare detail.
	var b strings.Builder
	RenderSarif(&b, []SarifFinding{
		{Finding: Finding{ID: "x", Title: "X", Severity: "high", Status: "unknown", Detail: "d."}, Domain: "d", DefaultURI: "u"},
	})
	out := b.String()
	if !strings.Contains(out, `"level": "note"`) {
		t.Errorf("unknown must map to note:\n%s", out)
	}
	if !strings.Contains(out, `"text": "d."`) || strings.Contains(out, "Remediation:") {
		t.Errorf("empty remediation must not append the suffix:\n%s", out)
	}
}

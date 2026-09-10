package audit

// sarif_test.go covers the SARIF renderer: the golden, the all-pass run,
// the worst severity per rule and the unknown level.

import (
	"strings"
	"testing"
)

func TestRenderSarifGolden(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

package audit

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFileT writes a fixture file, creating parents.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newFixtureAuditor builds a synthetic project tree: base/clusters/<domain>
// plus base/.lok8s for the framework side.
func newFixtureAuditor(t *testing.T) *Auditor {
	t.Helper()
	base := t.TempDir()
	return &Auditor{
		Base:     base,
		Clusters: filepath.Join(base, "clusters"),
		Lok8s:    filepath.Join(base, ".lok8s"),
	}
}

func findingByID(t *testing.T, findings []Finding, id string) Finding {
	t.Helper()
	for _, f := range findings {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("no finding with id %q in %+v", id, findings)
	return Finding{}
}

func TestScoreWeightsAndGrades(t *testing.T) {
	cases := []struct {
		name      string
		findings  []Finding
		wantScore int
		wantGrade string
	}{
		{"all pass", []Finding{{Status: "pass"}, {Status: "pass"}}, 100, "A"},
		{"high fail", []Finding{{Status: "fail", Severity: "high"}}, 75, "C"},
		{"critical fail", []Finding{{Status: "fail", Severity: "critical"}}, 60, "D"},
		{"medium warn", []Finding{{Status: "warn", Severity: "medium"}}, 95, "A"},
		{"low warn", []Finding{{Status: "warn", Severity: "low"}}, 98, "A"},
		{"clamps at zero", []Finding{
			{Status: "fail", Severity: "critical"}, {Status: "fail", Severity: "critical"},
			{Status: "fail", Severity: "critical"}}, 0, "F"},
		// A high/critical unknown caps the score at 70 (grade C at best):
		// "couldn't check" must not read as a perfect score.
		{"high unknown caps at 70", []Finding{{Status: "unknown", Severity: "high"}, {Status: "pass"}}, 70, "C"},
		{"medium unknown does not cap", []Finding{{Status: "unknown", Severity: "medium"}, {Status: "pass"}}, 100, "A"},
		{"cap does not raise a lower score", []Finding{
			{Status: "unknown", Severity: "high"},
			{Status: "fail", Severity: "critical"}}, 60, "D"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotScore, gotGrade, _, _, _, _ := score(tc.findings)
			if gotScore != tc.wantScore || gotGrade != tc.wantGrade {
				t.Errorf("score = %d/%s, want %d/%s", gotScore, gotGrade, tc.wantScore, tc.wantGrade)
			}
		})
	}
}

func TestHasFail(t *testing.T) {
	if HasFail([]Finding{{Status: "warn"}, {Status: "unknown"}, {Status: "pass"}}) {
		t.Error("warn/unknown/pass must not count as fail (exit-code contract)")
	}
	if !HasFail([]Finding{{Status: "pass"}, {Status: "fail"}}) {
		t.Error("fail finding not detected")
	}
}

func TestRunDomainInvalidName(t *testing.T) {
	a := newFixtureAuditor(t)
	findings := a.RunDomain("../evil")
	if len(findings) != 1 {
		t.Fatalf("want exactly one finding, got %d", len(findings))
	}
	f := findings[0]
	if f.ID != "cluster-spec" || f.Status != "unknown" || f.Severity != "high" {
		t.Errorf("unexpected finding: %+v", f)
	}
	if f.Detail != "Invalid domain name '../evil'." {
		t.Errorf("detail = %q", f.Detail)
	}
	// The path-traversal guard fires BEFORE any filesystem path is built —
	// and an unknown finding must not flip the exit code.
	if HasFail(findings) {
		t.Error("invalid domain must be unknown (rc 0), not fail")
	}
}

func TestRunDomainMissingSpec(t *testing.T) {
	a := newFixtureAuditor(t)
	findings := a.RunDomain("ghost.dev")
	if len(findings) != 1 || findings[0].ID != "cluster-spec" || findings[0].Status != "unknown" {
		t.Fatalf("want single cluster-spec unknown, got %+v", findings)
	}
	wantDetail := "No cluster.lok8s.yaml under " + filepath.Join(a.Clusters, "ghost.dev") +
		" (deploy-only or missing domain)."
	if findings[0].Detail != wantDetail {
		t.Errorf("detail = %q, want %q", findings[0].Detail, wantDetail)
	}
}

func TestRunDomainOrderAndCount(t *testing.T) {
	a := newFixtureAuditor(t)
	writeFileT(t, filepath.Join(a.Clusters, "d.dev", "cluster.lok8s.yaml"),
		"kind: KubeOne\nspec:\n  kubernetes:\n    version: v1.35.0\n")
	findings := a.RunDomain("d.dev")
	wantOrder := []string{
		"encryption-at-rest", "cilium-policy-enforcement", "exposed-endpoints",
		"k8s-version-support", "privileged-workloads", "plaintext-endpoints",
	}
	if len(findings) != len(wantOrder) {
		t.Fatalf("want %d findings, got %d", len(wantOrder), len(findings))
	}
	for i, id := range wantOrder {
		if findings[i].ID != id {
			t.Errorf("finding[%d].ID = %s, want %s (check run order is the JSON contract)", i, findings[i].ID, id)
		}
	}
}

func TestEmitStripsTabsAndNewlines(t *testing.T) {
	r := &run{}
	r.emit(Finding{ID: "x", Detail: "a\tb\nc", Remediation: "d\ne", File: "f\tg"})
	f := r.findings[0]
	if f.Detail != "a b c" || f.Remediation != "d e" || f.File != "f g" {
		t.Errorf("TSV strip failed: %+v", f)
	}
}

func TestRelURI(t *testing.T) {
	a := &Auditor{Base: "/repo"}
	if got := a.relURI("/repo/clusters/x/cluster.lok8s.yaml"); got != "clusters/x/cluster.lok8s.yaml" {
		t.Errorf("relURI inside repo = %q", got)
	}
	if got := a.relURI("/elsewhere/spec.yaml"); got != "/elsewhere/spec.yaml" {
		t.Errorf("relURI outside repo = %q", got)
	}
}

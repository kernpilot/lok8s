package audit

import (
	"path/filepath"
	"strings"
	"testing"
)

func entriesFor(t *testing.T, a *Auditor, spec, kind string) []bootstrapEntry {
	t.Helper()
	specFile := filepath.Join(a.Clusters, "d.dev", "cluster.lok8s.yaml")
	writeFileT(t, specFile, spec)
	var out []bootstrapEntry
	for _, n := range resolveBootstrapEntries(specFile, kind) {
		if e, ok := a.parseBootstrapEntry("d.dev", n); ok {
			out = append(out, e)
		}
	}
	return out
}

func TestBootstrapEntryResolution(t *testing.T) {
	a := newFixtureAuditor(t)
	entries := entriesFor(t, a, `kind: Lo
spec:
  bootstrap:
    - cilium
    - ./targets/x
    - /abs/y
`, "lo")
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[0].dir != a.Lok8s+"/addons/cilium" {
		t.Errorf("bare name dir = %s", entries[0].dir)
	}
	if entries[1].dir != a.Clusters+"/d.dev/./targets/x" {
		t.Errorf("relative dir = %s (must resolve against the cluster dir, uncleaned)", entries[1].dir)
	}
	// Absolute entries CONCATENATE onto the base — the bash
	// `${PATH_BASE}${_raw}` contract, not filepath.Join.
	if entries[2].dir != a.Base+"/abs/y" {
		t.Errorf("absolute dir = %s", entries[2].dir)
	}
}

func TestBootstrapDefaultOnlyForLo(t *testing.T) {
	a := newFixtureAuditor(t)
	if got := entriesFor(t, a, "kind: KubeOne\n", "kubeone"); len(got) != 0 {
		t.Errorf("kubeone must not default to cilium: %+v", got)
	}
	got := entriesFor(t, a, "kind: Lo\n", "lo")
	if len(got) != 1 || filepath.Base(got[0].dir) != "cilium" {
		t.Errorf("lo must default to [cilium]: %+v", got)
	}
	if got := entriesFor(t, a, "kind: Lo\nspec:\n  bootstrap: []\n", "lo"); len(got) != 0 {
		t.Errorf("explicit empty list is an authoritative opt-out: %+v", got)
	}
}

func TestBootstrapLegacyInline(t *testing.T) {
	a := newFixtureAuditor(t)
	entries := entriesFor(t, a, `kind: Lo
spec:
  bootstrap:
    - cilium:
        encryption:
          enabled: true
`, "lo")
	if len(entries) != 1 || !entries[0].inlineInclude {
		t.Fatalf("%+v", entries)
	}
	if v := yqRenderNode(lookupPath(entries[0].inline, "encryption", "enabled")); v != "true" {
		t.Errorf("legacy inline values lost: %q", v)
	}
}

func TestBootstrapNewSchemaValues(t *testing.T) {
	a := newFixtureAuditor(t)
	// values: against a chart addon works…
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "chart.yaml"), "name: cilium\n")
	entries := entriesFor(t, a, `kind: Lo
spec:
  bootstrap:
    - cilium:
        values:
          policyAuditMode: false
        wait: true
`, "lo")
	if len(entries) != 1 || !entries[0].inlineInclude {
		t.Fatalf("%+v", entries)
	}
	if v := yqRenderNode(lookupPath(entries[0].inline, "policyAuditMode")); v != "false" {
		t.Errorf("values not extracted: %q", v)
	}
}

func TestBootstrapValuesOnKustomizeTargetSkipped(t *testing.T) {
	a := newFixtureAuditor(t)
	// The dir EXISTS but has no chart.yaml → `values:` is helm-only → the
	// entry errors in bash and is SKIPPED by the audit's `|| continue`.
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "kustomization.yaml"), "resources: []\n")
	entries := entriesFor(t, a, `kind: Lo
spec:
  bootstrap:
    - cilium:
        values:
          x: 1
`, "lo")
	if len(entries) != 0 {
		t.Errorf("values: on a non-chart target must skip the entry: %+v", entries)
	}
}

func TestBootstrapValueFilesMerge(t *testing.T) {
	a := newFixtureAuditor(t)
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "chart.yaml"), "name: cilium\n")
	writeFileT(t, filepath.Join(a.Clusters, "d.dev", "vf.yaml"), "policyAuditMode: true\nkeep: 1\n")
	entries := entriesFor(t, a, `kind: Lo
spec:
  bootstrap:
    - cilium:
        valueFiles:
          - ./vf.yaml
        values:
          policyAuditMode: false
`, "lo")
	if len(entries) != 1 {
		t.Fatalf("%+v", entries)
	}
	// Files pre-merge in list order with inline values: ON TOP.
	if v := yqRenderNode(lookupPath(entries[0].inline, "policyAuditMode")); v != "false" {
		t.Errorf("inline must override valueFiles: %q", v)
	}
	if v := yqRenderNode(lookupPath(entries[0].inline, "keep")); v != "1" {
		t.Errorf("valueFiles content lost: %q", v)
	}
}

func TestBootstrapValueFilesMissingFileSkips(t *testing.T) {
	a := newFixtureAuditor(t)
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "chart.yaml"), "name: cilium\n")
	entries := entriesFor(t, a, `kind: Lo
spec:
  bootstrap:
    - cilium:
        valueFiles:
          - ./nope.yaml
`, "lo")
	if len(entries) != 0 {
		t.Errorf("missing valueFiles file must skip the entry: %+v", entries)
	}
}

func TestBootstrapValidationRejects(t *testing.T) {
	a := newFixtureAuditor(t)
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "chart.yaml"), "name: cilium\n")
	cases := []struct{ name, entry string }{
		{"non-map non-null value", "    - cilium: true\n"},
		{"non-boolean wait", "    - cilium:\n        wait: yes\n"},
		{"unquoted numeric name", "    - cilium:\n        name: 123\n"},
		{"bad name charset", "    - cilium:\n        name: \"a b\"\n"},
		{"env sequence container", "    - cilium:\n        env: [a]\n"},
		{"env map value", "    - cilium:\n        env:\n          K:\n            nested: 1\n"},
		{"env bad key", "    - cilium:\n        env:\n          bad-key: v\n"},
		{"dependsOn scalar container", "    - cilium:\n        dependsOn: x\n"},
		{"dependsOn null element", "    - cilium:\n        dependsOn: [~]\n"},
		{"valueFiles scalar container", "    - cilium:\n        valueFiles: ./x.yaml\n"},
		{"valueFiles empty element", "    - cilium:\n        valueFiles: [\"\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := entriesFor(t, a, "kind: Lo\nspec:\n  bootstrap:\n"+tc.entry, "lo")
			if len(entries) != 0 {
				t.Errorf("malformed entry must be skipped: %+v", entries)
			}
		})
	}
}

func TestBootstrapWaitStringTrueAccepted(t *testing.T) {
	a := newFixtureAuditor(t)
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "chart.yaml"), "name: cilium\n")
	// Preserved quirk: a QUOTED "true" renders "true" and passes the bash
	// string compare even though the schema says boolean.
	entries := entriesFor(t, a, "kind: Lo\nspec:\n  bootstrap:\n    - cilium:\n        wait: \"true\"\n", "lo")
	if len(entries) != 1 {
		t.Errorf(`wait: "true" must be accepted (string compare): %+v`, entries)
	}
}

func TestInlineIncludedGate(t *testing.T) {
	a := newFixtureAuditor(t)
	// values: "" and values: null render "" / "null" → excluded; {} → "{}"
	// → included (the bash `-n && != null` gate on the rendered string).
	writeFileT(t, filepath.Join(a.Lok8s, "addons", "cilium", "chart.yaml"), "name: cilium\n")
	empty := entriesFor(t, a, "kind: Lo\nspec:\n  bootstrap:\n    - cilium:\n        values: \"\"\n", "lo")
	if len(empty) != 1 || empty[0].inlineInclude {
		t.Errorf("empty-string values must not join the stack: %+v", empty)
	}
	braces := entriesFor(t, a, "kind: Lo\nspec:\n  bootstrap:\n    - cilium:\n        values: {}\n", "lo")
	if len(braces) != 1 || !braces[0].inlineInclude {
		t.Errorf("{} values must join the stack: %+v", braces)
	}
}

func TestMergeYAMLDocsSemantics(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "1.yaml")
	f2 := filepath.Join(dir, "2.yaml")
	writeFileT(t, f1, "a: true\nlist: [a, b]\nm:\n  x: 1\n")
	writeFileT(t, f2, "a: false\nlist: [c]\nm:\n  y: 2\n")
	merged, err := mergeYAMLDocs([]string{f1, f2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := yqRenderNode(lookupPath(merged, "a")); v != "false" {
		t.Errorf("scalar replace: %q", v)
	}
	if v := yqRenderNode(lookupPath(merged, "m", "x")); v != "1" {
		t.Errorf("maps must deep-merge: %q", v)
	}
	if v := yqRenderNode(lookupPath(merged, "m", "y")); v != "2" {
		t.Errorf("maps must deep-merge: %q", v)
	}
	if n := lookupPath(merged, "list"); n == nil || len(n.Content) != 1 {
		t.Errorf("lists must REPLACE, not concatenate")
	}

	// A scalar document is a merge ERROR (yq: cannot multiply !!map with
	// !!str) — the caller renders unknown, never a fall-through pass.
	f3 := filepath.Join(dir, "3.yaml")
	writeFileT(t, f3, "just a string\n")
	if _, err := mergeYAMLDocs([]string{f1, f3}, nil); err == nil {
		t.Error("scalar doc must fail the merge")
	}

	// A null document is a no-op.
	f4 := filepath.Join(dir, "4.yaml")
	writeFileT(t, f4, "null\n")
	merged, err = mergeYAMLDocs([]string{f1, f4}, nil)
	if err != nil || yqRenderNode(lookupPath(merged, "a")) != "true" {
		t.Errorf("null doc must be a no-op (err=%v)", err)
	}
}

func TestYqRenderPreservesLiterals(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s.yaml")
	writeFileT(t, f, "a: ~\nb: True\nc: \"true\"\nd: 1.0\n")
	doc := firstDocNode(f)
	for k, want := range map[string]string{"a": "~", "b": "True", "c": "true", "d": "1.0"} {
		if got := yqRenderNode(lookupPath(doc, k)); got != want {
			t.Errorf("%s renders %q, want %q (yq preserves scalar style)", k, got, want)
		}
	}
	if got := yqRenderNode(lookupPath(doc, "missing")); got != "null" {
		t.Errorf("absent key renders %q, want null", got)
	}
	if strings.TrimSpace(altNode(lookupPath(doc, "a"), "def")) != "def" {
		t.Errorf("`//` must treat null as empty")
	}
}

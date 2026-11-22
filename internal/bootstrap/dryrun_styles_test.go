package bootstrap

// dryrun_styles_test.go — the diff input of --ignore-nondeterministic keeps
// the scalar styles of the render. The bash twin (bootstrap::_without_objects
// in libs/bootstrap) does the same; tests/unit/bootstrap_diff_input_test.bats
// is its test.

import (
	"strings"
	"testing"
)

// kubectl reads the diff input as YAML 1.1, as it reads the real apply. So
// each object that stays keeps the style that the render gave each scalar:
// a quoted "yes" stays a string, and a plain yes stays plain.
func TestWithoutObjectsKeepsTheSourceStyles(t *testing.T) {
	render := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kept\n  namespace: ns\n" +
		"  annotations:\n    \"on\": a\ndata:\n" +
		"  quotedYes: \"yes\"\n  quotedTime: \"1:20\"\n  quotedDate: \"2026-11-20\"\n  quotedEquals: \"=\"\n" +
		"  single: 'off'\n  plainYes: yes\n  plainWord: hello\n" +
		"---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: dropped\n  namespace: ns\n"
	got, err := withoutObjects(render, []string{"v1 Secret ns/dropped"})
	if err != nil {
		t.Fatalf("withoutObjects: %v", err)
	}
	for _, line := range []string{
		`    "on": a`, `  quotedYes: "yes"`, `  quotedTime: "1:20"`, `  quotedDate: "2026-11-20"`,
		`  quotedEquals: "="`, `  single: 'off'`, `  plainYes: yes`, `  plainWord: hello`,
	} {
		if !strings.Contains("\n"+got, "\n"+line+"\n") {
			t.Errorf("the diff input has no line %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "dropped") {
		t.Errorf("the diff input holds the object that it leaves out:\n%s", got)
	}
}

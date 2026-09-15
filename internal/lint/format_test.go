package lint

import (
	"bytes"
	"io"
	"testing"
)

func TestFormatWriterRewritesFindings(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	in := "Validating: alpha.dev\n" +
		"\033[0;31m[error]\033[0m   Missing required field: kind\n" +
		"\033[0;33m[warn]\033[0m   clusters/alpha.dev/targets/app/kustomization.yaml: missing resource ./deploy.yaml\n" +
		"[note] clusters/alpha.dev/cluster.lok8s.yaml: spec.runtime equals the default (kind); you can drop it\n" +
		"\033[0;31m[error]\033[0m 2 validation error(s)\n" +
		"  OK\n"
	cases := map[string]string{
		FormatEditor: "Validating: alpha.dev\n" +
			"clusters/alpha.dev/cluster.lok8s.yaml:1: [error] Missing required field: kind\n" +
			"clusters/alpha.dev/targets/app/kustomization.yaml:1: [warn] clusters/alpha.dev/targets/app/kustomization.yaml: missing resource ./deploy.yaml\n" +
			"clusters/alpha.dev/cluster.lok8s.yaml:1: [note] clusters/alpha.dev/cluster.lok8s.yaml: spec.runtime equals the default (kind); you can drop it\n" +
			"clusters/alpha.dev/cluster.lok8s.yaml:1: [error] 2 validation error(s)\n" +
			"  OK\n",
		FormatGitHub: "Validating: alpha.dev\n" +
			"::error file=clusters/alpha.dev/cluster.lok8s.yaml,line=1::Missing required field: kind\n" +
			"::warning file=clusters/alpha.dev/targets/app/kustomization.yaml,line=1::clusters/alpha.dev/targets/app/kustomization.yaml: missing resource ./deploy.yaml\n" +
			"::notice file=clusters/alpha.dev/cluster.lok8s.yaml,line=1::clusters/alpha.dev/cluster.lok8s.yaml: spec.runtime equals the default (kind); you can drop it\n" +
			"::error file=clusters/alpha.dev/cluster.lok8s.yaml,line=1::2 validation error(s)\n" +
			"  OK\n",
	}
	for format, want := range cases {
		var out bytes.Buffer
		w := NewFormatWriter(&out, format, "clusters/alpha.dev/cluster.lok8s.yaml")
		// Written in two chunks across a line boundary: the writer buffers.
		io.WriteString(w, in[:40])
		io.WriteString(w, in[40:])
		if out.String() != want {
			t.Errorf("%s:\n--- got ---\n%s--- want ---\n%s", format, out.String(), want)
		}
	}
	var out bytes.Buffer
	if w := NewFormatWriter(&out, FormatText, "x"); w != &out {
		t.Errorf("text must return the writer itself")
	}
}

func TestFormatWriterLineNumberAndFlush(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	var out bytes.Buffer
	w := NewFormatWriter(&out, FormatEditor, "spec.yaml").(*FormatWriter)
	io.WriteString(w, "[warn] services/api/lok8s.yaml: line 12: unknown key build.contxt")
	if out.Len() != 0 {
		t.Fatalf("a partial line was written early: %q", out.String())
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "services/api/lok8s.yaml:12: [warn] services/api/lok8s.yaml: line 12: unknown key build.contxt\n"; out.String() != want {
		t.Errorf("flush:\n--- got ---\n%s--- want ---\n%s", out.String(), want)
	}
}

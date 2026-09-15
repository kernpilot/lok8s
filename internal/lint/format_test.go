package lint

import (
	"bytes"
	"io"
	"strings"
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

// TestFormatWriterEscapesWorkflowCommands: a key or a path in a finding
// cannot end the workflow command or start another one.
func TestFormatWriterEscapesWorkflowCommands(t *testing.T) {
	var out bytes.Buffer
	w := NewFormatWriter(&out, FormatGitHub, "clusters/a:b,c.dev/cluster.lok8s.yaml")
	io.WriteString(w, "[error] unknown key 'x%0A::error title=pwned::y'\rZ\n[warn] services/a,b.yaml: 100% line 3\n")
	want := "::error file=clusters/a%3Ab%2Cc.dev/cluster.lok8s.yaml,line=1::unknown key 'x%250A::error title=pwned::y'%0DZ\n" +
		"::warning file=services/a%2Cb.yaml,line=3::services/a,b.yaml: 100%25 line 3\n"
	if out.String() != want {
		t.Errorf("escaping:\n--- got ---\n%s--- want ---\n%s", out.String(), want)
	}
	if strings.Contains(out.String(), "\n::error title=") {
		t.Errorf("an injected command survived")
	}
}

// failingWriter accepts nothing.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestFormatWriterReportsConsumedBytesOnError: the bytes went into the
// buffer before the sink failed, so Write returns len(p) with the error.
func TestFormatWriterReportsConsumedBytesOnError(t *testing.T) {
	w := NewFormatWriter(failingWriter{}, FormatEditor, "spec.yaml")
	p := []byte("[error] x\n")
	n, err := w.Write(p)
	if n != len(p) || err == nil {
		t.Errorf("Write = (%d, %v), want (%d, an error)", n, err, len(p))
	}
}

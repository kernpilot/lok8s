package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderInline(t *testing.T) {
	cases := []struct {
		in       string
		contains []string // must appear in output
		absent   []string // markers that must be consumed
	}{
		{"a **bold** b", []string{"bold", "a ", " b"}, []string{"**"}},
		{"see `lo up` now", []string{"lo up"}, []string{"`"}},
		{"an *emphasized* word", []string{"emphasized"}, []string{"*"}},
		{"math 2 * 3 = 6", []string{"2 * 3"}, nil},                    // lone * with spaces stays literal
		{"keep snake_case_name", []string{"snake_case_name"}, nil},    // underscores untouched
		{"`code with ** stars`", []string{"code with ** stars"}, nil}, // code not re-parsed
	}
	for _, c := range cases {
		got := renderInline(c.in)
		for _, want := range c.contains {
			if !strings.Contains(got, want) {
				t.Errorf("renderInline(%q) = %q, missing %q", c.in, got, want)
			}
		}
		for _, bad := range c.absent {
			if strings.Contains(got, bad) {
				t.Errorf("renderInline(%q) = %q, should not contain marker %q", c.in, got, bad)
			}
		}
	}
}

func TestHeaderAndBullets(t *testing.T) {
	var f bool
	if got := renderLine("## Title", &f); !strings.Contains(got, "Title") || strings.Contains(got, "#") {
		t.Errorf("header not stripped/styled: %q", got)
	}
	if got := renderLine("- item", &f); !strings.Contains(got, "•") || !strings.Contains(got, "item") {
		t.Errorf("bullet not rendered: %q", got)
	}
	if got := renderLine("3. third", &f); !strings.Contains(got, "3.") || !strings.Contains(got, "third") {
		t.Errorf("numbered list not rendered: %q", got)
	}
	if got := renderLine("not_a_header text", &f); !strings.Contains(got, "not_a_header") {
		t.Errorf("plain line mangled: %q", got)
	}
}

func TestMdStreamLineBuffering(t *testing.T) {
	var buf bytes.Buffer
	md := &mdStream{w: &buf}
	// tokens split mid-marker and mid-line
	for _, tok := range []string{"# Hi", "\n- ", "**a", "b**\n", "tail no newline"} {
		md.feed(tok)
	}
	md.flush()
	out := buf.String()
	if strings.Count(out, "\n") != 3 { // two full lines + the flushed tail
		t.Errorf("expected 3 lines, got %d:\n%q", strings.Count(out, "\n"), out)
	}
	for _, want := range []string{"Hi", "ab", "tail no newline"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%q", want, out)
		}
	}
	if strings.Contains(out, "**") {
		t.Errorf("bold markers leaked:\n%q", out)
	}
}

func TestMdStreamFences(t *testing.T) {
	var buf bytes.Buffer
	md := &mdStream{w: &buf}
	for _, tok := range []string{"```yaml\n", "kind: Lo\n", "name: **keep**\n", "```\n"} {
		md.feed(tok)
	}
	out := buf.String()
	// inside a fence, content is verbatim — bold markers must survive
	if !strings.Contains(out, "name: **keep**") {
		t.Errorf("fenced content should be verbatim:\n%q", out)
	}
	if !strings.Contains(out, "kind: Lo") {
		t.Errorf("fenced code missing:\n%q", out)
	}
}

func TestNextPosture(t *testing.T) {
	if got := nextPosture("read-only"); got != "confirm" {
		t.Errorf("read-only -> %q, want confirm", got)
	}
	if got := nextPosture("open"); got != "read-only" {
		t.Errorf("open -> %q, want read-only (wrap)", got)
	}
	if got := nextPosture("bogus"); got != "read-only" {
		t.Errorf("unknown -> %q, want read-only", got)
	}
}

type fakeBE struct {
	label string
	avail bool
}

func (f *fakeBE) Label() string                              { return f.label }
func (f *fakeBE) Available() bool                            { return f.avail }
func (f *fakeBE) Agentic() bool                              { return true }
func (f *fakeBE) Complete([]Msg) (string, error)             { return "", nil }
func (f *fakeBE) Stream([]Msg) (<-chan string, <-chan error) { return nil, nil }

func TestCycleBackendSkipsUnavailable(t *testing.T) {
	a := &fakeBE{"a", true}
	b := &fakeBE{"b", true}
	cc := &fakeBE{"c", false} // not installed
	backends := map[string]Backend{"a": a, "b": b, "c": cc}
	c := &Conductor{backend: a}

	cycleBackend(c, backends) // a -> b
	if c.backend != Backend(b) {
		t.Fatalf("expected cycle a->b, got %s", c.backend.Label())
	}
	cycleBackend(c, backends) // b -> a (c is unavailable, skipped; wraps)
	if c.backend != Backend(a) {
		t.Fatalf("expected cycle b->a (skip unavailable c), got %s", c.backend.Label())
	}
}

func TestCycleBackendSingleAvailableStaysPut(t *testing.T) {
	a := &fakeBE{"a", true}
	b := &fakeBE{"b", false}
	backends := map[string]Backend{"a": a, "b": b}
	c := &Conductor{backend: a}
	cycleBackend(c, backends)
	if c.backend != Backend(a) {
		t.Fatalf("only one available: should stay on a, got %s", c.backend.Label())
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- shared test fakes ----

// scriptBackend returns scripted routing JSON, then a canned streamed answer.
type scriptBackend struct {
	routes  []string
	i       int
	answer  string
	agentic bool
}

func (s *scriptBackend) Label() string   { return "fake" }
func (s *scriptBackend) Available() bool { return true }
func (s *scriptBackend) Agentic() bool   { return s.agentic }
func (s *scriptBackend) Complete(_ []Msg) (string, error) {
	if s.i >= len(s.routes) {
		return `{"tool":null}`, nil
	}
	r := s.routes[s.i]
	s.i++
	return r, nil
}
func (s *scriptBackend) Stream(_ []Msg) (<-chan string, <-chan error) {
	toks := make(chan string)
	errc := make(chan error, 1)
	go func() {
		defer close(toks)
		defer close(errc)
		for _, w := range strings.Fields(s.answer) {
			toks <- w + " "
		}
	}()
	return toks, errc
}

type fakeTools struct{ out map[string]string }

func (f fakeTools) CallTool(name string, _ map[string]any) string {
	if v, ok := f.out[name]; ok {
		return v
	}
	return "(no output)"
}

func collect(ch <-chan Event) []Event {
	var es []Event
	for e := range ch {
		es = append(es, e)
	}
	return es
}

func has(es []Event, typ string, pred func(Event) bool) bool {
	for _, e := range es {
		if e.Type == typ && (pred == nil || pred(e)) {
			return true
		}
	}
	return false
}

func minimalConductor(posture string, cat *Catalog, mcp toolCaller, be Backend) *Conductor {
	return newConductor(&Config{Posture: posture, MaxToolSteps: 3}, be, mcp, cat)
}

// ---- unit tests ----

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		in   string
		tool string
		ok   bool
	}{
		{`{"tool":"lo_status"}`, "lo_status", true},
		{"sure: {\"tool\":\"lo_up\"} done", "lo_up", true},
		{"```json\n{\"tool\":null}\n```", "", true},
		{"no json here", "", false},
	}
	for _, c := range cases {
		j := extractJSON(c.in)
		if c.ok != (j != nil) {
			t.Errorf("extractJSON(%q): got nil=%v want ok=%v", c.in, j == nil, c.ok)
			continue
		}
		if c.ok {
			if tool, _ := j["tool"].(string); tool != c.tool {
				t.Errorf("extractJSON(%q) tool = %q want %q", c.in, tool, c.tool)
			}
		}
	}
}

func TestAllowedPosture(t *testing.T) {
	cat := newCatalog([]Tool{{Name: "lo_status"}, {Name: "lo_destroy"}},
		Injection{Tiers: map[string][]string{"readonly": {"lo_status"}}})
	ro := minimalConductor("read-only", cat, fakeTools{}, &scriptBackend{})
	if !ro.allowed("lo_status") {
		t.Error("read tool should be allowed in read-only")
	}
	if ro.allowed("lo_destroy") {
		t.Error("write tool should be blocked in read-only")
	}
	if !minimalConductor("open", cat, fakeTools{}, &scriptBackend{}).allowed("lo_destroy") {
		t.Error("write tool should be allowed in open posture")
	}
}

func TestSchemaInjection(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s.md")
	os.WriteFile(f, []byte("SCHEMA-BODY"), 0o644)
	c := newConductor(&Config{SchemaFiles: []string{f}}, &scriptBackend{}, fakeTools{}, newCatalog(nil, Injection{}))
	if got := c.schema("please author a cluster.lok8s.yaml"); !strings.Contains(got, "SCHEMA-BODY") {
		t.Errorf("authoring intent should inject schema, got %q", got)
	}
	if got := c.schema("what is the cluster status?"); got != "" {
		t.Errorf("non-authoring intent should not inject schema, got %q", got)
	}
}

func TestConductorReadToolTurn(t *testing.T) {
	cat := newCatalog([]Tool{{Name: "lo_status", Description: "status"}},
		Injection{Tiers: map[string][]string{"readonly": {"lo_status"}}})
	be := &scriptBackend{routes: []string{`{"tool":"lo_status"}`}, answer: "all good", agentic: true}
	c := minimalConductor("read-only", cat, fakeTools{out: map[string]string{"lo_status": "Running 12 pods"}}, be)
	es := collect(c.Respond("status?"))
	if !has(es, "route", func(e Event) bool { return e.Tool == "lo_status" }) {
		t.Error("expected a route event to lo_status")
	}
	if !has(es, "tool", func(e Event) bool { return strings.Contains(e.Output, "Running 12 pods") }) {
		t.Error("expected the tool output surfaced as a tool event")
	}
	if !has(es, "token", nil) {
		t.Error("expected streamed answer tokens")
	}
	if !has(es, "answer_done", nil) {
		t.Error("expected answer_done")
	}
}

type blockTools struct{ called *bool }

func (b blockTools) CallTool(_ string, _ map[string]any) string {
	*b.called = true
	return "SHOULD NOT RUN"
}

func TestConductorGateBlocksWrite(t *testing.T) {
	cat := newCatalog([]Tool{{Name: "lo_destroy", Description: "destroy"}}, Injection{}) // no tier => mutating
	called := false
	be := &scriptBackend{routes: []string{`{"tool":"lo_destroy"}`}, answer: "needs confirmation", agentic: true}
	c := minimalConductor("read-only", cat, blockTools{called: &called}, be)
	es := collect(c.Respond("tear it down"))
	if !has(es, "gate", func(e Event) bool { return e.Decision == "blocked" && e.Tool == "lo_destroy" }) {
		t.Error("write tool must be gate-blocked in read-only")
	}
	if has(es, "tool", nil) {
		t.Error("a blocked tool must not produce a tool (execution) event")
	}
	if called {
		t.Error("CallTool must never run for a gate-blocked tool")
	}
}

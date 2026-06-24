package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type Event struct {
	Type     string // route|gate|tool|handoff|answer_start|token|answer_done|error
	Tool     string
	Args     map[string]any
	Output   string
	Decision string
	Reason   string
	Text     string
	Error    string
	Backend  string
}

// toolCaller is what the conductor needs to execute a tool — satisfied by *MCP
// and by fakes in tests.
type toolCaller interface {
	CallTool(name string, args map[string]any) string
}

type Conductor struct {
	cfg      *Config
	backend  Backend
	catalog  *Catalog
	mcp      toolCaller
	posture  string
	history  []Msg
	maxSteps int
}

func newConductor(cfg *Config, b Backend, mcp toolCaller, cat *Catalog) *Conductor {
	return &Conductor{cfg: cfg, backend: b, mcp: mcp, catalog: cat,
		posture: cfg.Posture, maxSteps: cfg.MaxToolSteps}
}

func (c *Conductor) setBackend(b Backend) { c.backend = b }

func (c *Conductor) allowed(tool string) bool {
	if !c.catalog.exposed(tool) {
		return false // drop/deny-filtered tools never run, regardless of posture
	}
	return c.posture == "open" || c.catalog.tag(tool) == "read"
}

// extractJSON returns the first balanced top-level {...} object in s (tracking
// string literals + escapes), so a model that wraps its JSON in prose — or emits
// a trailing second object — doesn't corrupt the parse the way a greedy
// first-brace-to-last-brace regex would.
func extractJSON(s string) map[string]any {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var v map[string]any
				if json.Unmarshal([]byte(s[start:i+1]), &v) != nil {
					return nil
				}
				return v
			}
		}
	}
	return nil
}

var authorRe = regexp.MustCompile(`(?i)author|write|create|cluster\.lok8s|deploy\.lok8s|addon|chart|secret|yaml`)

func (c *Conductor) schema(userMsg string) string {
	if !authorRe.MatchString(userMsg) {
		return ""
	}
	var parts []string
	for _, f := range c.cfg.SchemaFiles {
		if b, err := os.ReadFile(f); err == nil {
			parts = append(parts, string(b))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\nAUTHORITATIVE lok8s SCHEMA (follow exactly):\n" + strings.Join(parts, "\n")
}

const routeSys = `You are the lok8s assistant (%s mode). To answer the user you may run lo tools to gather facts. Respond with ONE JSON object per step:
  {"tool": "<name>", "args": {...}}   to run a tool, or
  {"tool": null}                       when you have enough to answer.
%s Available tools:
%s`

const answerSys = "You are the lok8s assistant. Answer the user clearly and concisely from the tool outputs. If they asked you to author lok8s config, output the complete, valid YAML in a fenced block. Surface the exact `lo ...` command when relevant. Don't invent tool output you didn't see.%s"

func (c *Conductor) Respond(userMsg string) <-chan Event {
	ev := make(chan Event)
	go func() {
		defer close(ev)
		c.history = append(c.history, Msg{Role: "user", Content: userMsg})

		// frontier CLI handoff — it brings its own tools.
		if !c.backend.Agentic() {
			ev <- Event{Type: "handoff", Backend: c.backend.Label()}
			msgs := append([]Msg{{Role: "system",
				Content: "You are assisting in a lok8s `lo chat` session (cluster ops). Answer the user; you may use your own tools."}}, c.history...)
			ev <- Event{Type: "answer_start"}
			ans := c.stream(msgs, ev)
			c.history = append(c.history, Msg{Role: "assistant", Content: ans})
			ev <- Event{Type: "answer_done"}
			return
		}

		constraint := "Only [read] tools may run; [write] tools are blocked."
		if c.posture == "open" {
			constraint = "All tools may run, including [write] tools."
		}
		routeMsgs := append([]Msg{{Role: "system",
			Content: fmt.Sprintf(routeSys, c.posture, constraint, c.catalog.menu())}}, c.history...)
		type kv struct{ tool, out string }
		var toolCtx []kv
		for i := 0; i < c.maxSteps; i++ {
			raw, err := c.backend.Complete(routeMsgs)
			if err != nil {
				ev <- Event{Type: "error", Error: "routing: " + err.Error()}
				return
			}
			j := extractJSON(raw)
			tool, _ := j["tool"].(string)
			if tool == "" {
				break
			}
			args, _ := j["args"].(map[string]any)
			ev <- Event{Type: "route", Tool: tool, Args: args}
			if !c.catalog.isTool(tool) {
				ev <- Event{Type: "gate", Tool: tool, Decision: "unknown"}
				routeMsgs = append(routeMsgs, Msg{Role: "assistant", Content: raw},
					Msg{Role: "user", Content: tool + " is not a valid tool; pick from the menu or answer."})
				continue
			}
			if !c.allowed(tool) {
				reason := c.posture + ": '" + tool + "' is a write tool"
				reprompt := tool + " is a write tool, blocked in " + c.posture +
					" mode. Tell the user it needs `--posture open`, or use a read tool."
				if !c.catalog.exposed(tool) {
					reason = "'" + tool + "' is not available in lo chat"
					reprompt = tool + " is not available here (off the menu). Pick a tool from the menu or answer directly."
				}
				ev <- Event{Type: "gate", Tool: tool, Decision: "blocked", Reason: reason}
				routeMsgs = append(routeMsgs, Msg{Role: "assistant", Content: raw},
					Msg{Role: "user", Content: reprompt})
				continue
			}
			out := c.mcp.CallTool(tool, args)
			if len(out) > 2000 {
				out = out[:2000]
			}
			toolCtx = append(toolCtx, kv{tool, out})
			ev <- Event{Type: "tool", Tool: tool, Args: args, Output: out}
			routeMsgs = append(routeMsgs, Msg{Role: "assistant", Content: raw},
				Msg{Role: "user", Content: "Output of " + tool + ":\n" + out})
		}

		var ctx strings.Builder
		for _, t := range toolCtx {
			ctx.WriteString("$ lo " + t.tool + "\n" + t.out + "\n\n")
		}
		if ctx.Len() == 0 {
			ctx.WriteString("(no tools were run)")
		}
		ansMsgs := append([]Msg{{Role: "system", Content: fmt.Sprintf(answerSys, c.schema(userMsg))}}, c.history...)
		ansMsgs = append(ansMsgs, Msg{Role: "user", Content: "Tool outputs:\n" + ctx.String() + "\nNow answer my request above."})
		ev <- Event{Type: "answer_start"}
		ans := c.stream(ansMsgs, ev)
		c.history = append(c.history, Msg{Role: "assistant", Content: ans})
		ev <- Event{Type: "answer_done"}
	}()
	return ev
}

func (c *Conductor) stream(msgs []Msg, ev chan<- Event) string {
	toks, errc := c.backend.Stream(msgs)
	var sb strings.Builder
	for t := range toks {
		sb.WriteString(t)
		ev <- Event{Type: "token", Text: t}
	}
	if e := <-errc; e != nil {
		ev <- Event{Type: "error", Error: e.Error()}
	}
	return sb.String()
}

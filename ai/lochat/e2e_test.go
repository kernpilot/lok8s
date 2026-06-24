package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestMCPHelperProcess is not a real test — when GO_MCP_HELPER=1 it impersonates
// a `lo mcp` server (newline-delimited JSON-RPC over stdio). TestE2E_RealMCP
// spawns the test binary itself with that env to get a genuine subprocess +
// protocol round-trip without a live cluster.
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_MCP_HELPER") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		if req.ID == nil {
			continue // notification (e.g. notifications/initialized)
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": "lo_status", "description": "Cluster status"},
				{"name": "lo_destroy", "description": "Destroy the cluster"},
			}}
		case "tools/call":
			text := "unknown tool"
			if req.Params.Name == "lo_status" {
				text = "Running 12 pods; gateway OK"
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		default:
			result = map[string]any{}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		w.Write(resp)
		w.WriteByte('\n')
		w.Flush()
	}
}

func TestE2E_RealMCP(t *testing.T) {
	mcp := newMCP(MCPConfig{
		Command: []string{os.Args[0], "-test.run=TestMCPHelperProcess"},
		Env:     map[string]string{"GO_MCP_HELPER": "1"},
		Timeout: 10,
	})
	if err := mcp.Start(); err != nil {
		t.Fatalf("mcp start (handshake): %v", err)
	}
	defer mcp.Close()

	tools, err := mcp.ListTools()
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools/list returned %d tools, want 2", len(tools))
	}

	// real tools/call round-trip over stdio
	if out := mcp.CallTool("lo_status", nil); !strings.Contains(out, "Running 12 pods") {
		t.Fatalf("CallTool(lo_status) = %q", out)
	}

	// full conductor turn through the REAL stdio MCP + a scripted backend:
	// route -> posture-gate -> execute (real subprocess) -> stream answer.
	cat := newCatalog(tools, Injection{Tiers: map[string][]string{"readonly": {"lo_status"}}})
	be := &scriptBackend{routes: []string{`{"tool":"lo_status"}`}, answer: "Cluster healthy.", agentic: true}
	c := newConductor(&Config{Posture: "read-only", MaxToolSteps: 3}, be, mcp, cat)
	es := collect(c.Respond("how's my cluster?"))
	if !has(es, "tool", func(e Event) bool { return strings.Contains(e.Output, "Running 12 pods") }) {
		t.Error("e2e: real-MCP tool output not surfaced to the conductor")
	}
	if !has(es, "answer_done", nil) {
		t.Error("e2e: turn did not complete (no answer_done)")
	}
}

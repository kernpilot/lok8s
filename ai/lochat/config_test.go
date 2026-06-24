package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(
		`{"endpoint":"http://x/v1","conductor":"a","backends":{"a":{"api":"ollama","model":"m"}}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "http://x/v1" {
		t.Errorf("endpoint = %q", c.Endpoint)
	}
	if c.Conductor != "a" {
		t.Errorf("conductor = %q", c.Conductor)
	}
	if c.Posture != "read-only" {
		t.Errorf("default posture = %q (want read-only)", c.Posture)
	}
	if c.MaxToolSteps != 4 {
		t.Errorf("default max_tool_steps = %d (want 4)", c.MaxToolSteps)
	}
	if c.MCP.Timeout != 120 {
		t.Errorf("default mcp timeout = %d (want 120)", c.MCP.Timeout)
	}
	if c.Backends["a"].Model != "m" {
		t.Error("backend model not parsed")
	}
}

func TestLoadConfigMissing(t *testing.T) {
	if _, err := loadConfig("/no/such/file.json"); err == nil {
		t.Error("expected error for a missing config file")
	}
}

package main

import (
	"encoding/json"
	"os"
)

// MCPConfig: how to launch `lo mcp` (stdio JSON-RPC).
type MCPConfig struct {
	Command   []string          `json:"command"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	ExtraPath []string          `json:"extra_path"`
	Timeout   int               `json:"timeout"`
}

// BackendConfig: a conductor brain — local HTTP endpoint or an external CLI.
type BackendConfig struct {
	Type        string   `json:"type"` // "" / "http" => HTTP; "cli" => external CLI
	BaseURL     string   `json:"base_url"`
	API         string   `json:"api"` // "ollama" | "openai"
	Model       string   `json:"model"`
	APIKey      string   `json:"api_key"`
	NumCtx      int      `json:"num_ctx"`
	Temperature float64  `json:"temperature"`
	Think       *bool    `json:"think"`
	Command     []string `json:"command"` // cli
	Detect      string   `json:"detect"`  // cli
	Timeout     int      `json:"timeout"`
}

type Injection struct {
	Drop  []string            `json:"drop"`
	Deny  []string            `json:"deny"`
	Tiers map[string][]string `json:"tiers"`
}

type Config struct {
	MCP          MCPConfig                 `json:"mcp"`
	Endpoint     string                    `json:"endpoint"` // injected as base_url into http backends
	Conductor    string                    `json:"conductor"`
	Posture      string                    `json:"posture"`
	MaxToolSteps int                       `json:"max_tool_steps"`
	SchemaFiles  []string                  `json:"schema_files"`
	Backends     map[string]*BackendConfig `json:"backends"`
	Injection    Injection                 `json:"injection"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Posture == "" {
		c.Posture = "read-only"
	}
	if c.MaxToolSteps == 0 {
		c.MaxToolSteps = 4
	}
	if c.MCP.Timeout == 0 {
		c.MCP.Timeout = 120
	}
	return &c, nil
}

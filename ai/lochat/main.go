// lo chat — a fully-local, transparent, streaming assistant over `lo mcp`.
// Single static binary (stdlib only). Config arrives as JSON (the argsh shim
// converts the YAML chat config via yq), so there are no Go module deps.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	cfgPath := flag.String("config", "", "path to JSON config (required)")
	prompt := flag.String("p", "", "single-shot prompt (non-interactive)")
	model := flag.String("model", "", "conductor backend key (chat.backends)")
	posture := flag.String("posture", "", "read-only | confirm | open")
	flag.Parse()

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "lo chat: --config <json> is required")
		os.Exit(2)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		die(err)
	}
	if *posture != "" {
		cfg.Posture = *posture
	}

	backends := map[string]Backend{}
	for name, b := range cfg.Backends {
		backends[name] = makeBackend(name, b)
	}
	name := *model
	if name == "" {
		name = cfg.Conductor
	}
	be, ok := backends[name]
	if !ok {
		die(fmt.Errorf("unknown conductor %q (see chat.backends)", name))
	}

	mcp := newMCP(cfg.MCP)
	if err := mcp.Start(); err != nil {
		die(fmt.Errorf("mcp start: %w", err))
	}
	defer mcp.Close()
	tools, err := mcp.ListTools()
	if err != nil {
		die(fmt.Errorf("mcp tools/list: %w", err))
	}
	cat := newCatalog(tools, cfg.Injection)

	c := newConductor(cfg, be, mcp, cat)
	if *prompt != "" {
		renderTurn(c, *prompt)
		return
	}
	interactive(c, backends)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// lo chat — a fully-local, transparent, streaming assistant over `lo mcp`.
// Single static binary (stdlib only). The argsh shim passes a JSON config plus
// the dynamic bits (--lo, --cwd, --base-dir) from the lo runtime — no yq, no
// Go module deps.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	cfgPath := flag.String("config", "", "path to JSON config (required)")
	prompt := flag.String("p", "", "single-shot prompt (non-interactive)")
	model := flag.String("model", "", "conductor backend key (chat.backends)")
	posture := flag.String("posture", "", "read-only | confirm | open")
	loPath := flag.String("lo", "", "path to the lo CLI (used to launch `lo mcp`)")
	cwd := flag.String("cwd", ".", "working dir for lo mcp (the project root / PATH_BASE)")
	baseDir := flag.String("base-dir", "", "resolve relative schema_files against this (default: --cwd)")
	check := flag.Bool("check", false, "run a system check (lo mcp bridge + local AI runtime) and exit")
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

	// dynamic bits supplied by the shim from the lo runtime (no yq needed):
	if *loPath != "" {
		cfg.MCP.Command = []string{*loPath, "mcp"}
	}
	if *cwd != "" {
		cfg.MCP.Cwd = *cwd
	}
	bd := *baseDir
	if bd == "" {
		bd = *cwd
	}
	for i, f := range cfg.SchemaFiles {
		if !filepath.IsAbs(f) {
			cfg.SchemaFiles[i] = filepath.Join(bd, f)
		}
	}
	for _, b := range cfg.Backends {
		if b.Type != "cli" {
			if b.BaseURL == "" {
				b.BaseURL = cfg.Endpoint
			}
			if b.APIKey == "" {
				b.APIKey = "ollama"
			}
		}
	}
	if len(cfg.MCP.Command) == 0 {
		die(fmt.Errorf("--lo <path> required (to launch `lo mcp`)"))
	}

	backends := map[string]Backend{}
	for name, b := range cfg.Backends {
		backends[name] = makeBackend(name, b)
	}
	name := *model
	if name == "" {
		name = cfg.Conductor
	}

	// `lo chat --check`: report the bridge + AI runtime, then exit (no session).
	if *check {
		os.Exit(runCheck(cfg, backends, name))
	}

	be, ok := backends[name]
	if !ok {
		die(fmt.Errorf("unknown conductor %q (see chat.backends)", name))
	}

	mcp := newMCP(cfg.MCP)
	// Transient "connecting" hint — only when stderr is a terminal, so redirected
	// runs don't get stray cursor-control escapes.
	connecting := stderrTTY()
	if connecting {
		fmt.Fprint(os.Stderr, cDim+"⋯ connecting to lo mcp…"+cReset)
	}
	clearLine := func() {
		if connecting {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
	}
	if err := mcp.Start(); err != nil {
		clearLine()
		die(fmt.Errorf("mcp start: %w", err))
	}
	defer mcp.Close()
	tools, err := mcp.ListTools()
	if err != nil {
		clearLine()
		die(fmt.Errorf("mcp tools/list: %w", err))
	}
	clearLine()
	cat := newCatalog(tools, cfg.Injection)

	// short conductor readiness check (falls back to another local model, or
	// advises) so the first turn doesn't fail with an opaque HTTP error.
	be, name = preflightConductor(name, be, backends)

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

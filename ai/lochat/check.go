package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// llamafile is a single static APE (engine) → a perfect fit for `b`. The asset
// glob ends in a digit so it matches the versioned engine (llamafile-0.10.3) and
// NOT the .zip / -thin / bench / whisperfile / diffusionfile siblings. The repo
// is public, so `b` needs no GITHUB_TOKEN.
const (
	llamafileRepo      = "Mozilla-Ocho/llamafile"
	llamafileAssetGlob = "llamafile-*[0-9]"
)

func bInstallArgs() []string {
	return []string{"install", "--asset", llamafileAssetGlob, llamafileRepo}
}

func bInstallHint() string { return "b " + strings.Join(bInstallArgs(), " ") }

// checkResult is one line of the system check: is this piece ready, and if not,
// what does the user run to fix it.
type checkResult struct {
	ok     bool
	detail string
	fix    []string
}

// modelPresent reports whether an ollama tag list contains the wanted model
// (exact, or `<model>:<tag>` / `<model>:latest`).
func modelPresent(list []string, model string) bool {
	for _, n := range list {
		if n == model || strings.HasPrefix(n, model+":") {
			return true
		}
		if !strings.Contains(model, ":") && n == model+":latest" {
			return true
		}
	}
	return false
}

func summarize(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	if len(list) > 4 {
		return strings.Join(list[:4], ", ") + fmt.Sprintf(", +%d", len(list)-4)
	}
	return strings.Join(list, ", ")
}

// checkBackend determines whether a conductor backend can actually serve now.
// ollama needs the exact model pulled; OpenAI-compatible servers (llamafile,
// vLLM, …) serve whatever weights are loaded, so any model => ready.
func checkBackend(name string, b Backend) checkResult {
	switch be := b.(type) {
	case *cliBackend:
		if be.Available() {
			return checkResult{true, "installed", nil}
		}
		tool := be.detect()
		return checkResult{false, tool + " not on PATH",
			[]string{"install the " + tool + " CLI (optional off-machine handoff)"}}
	case *httpBackend:
		models, err := be.listModels(3 * time.Second)
		if err != nil {
			if be.ollama() {
				return checkResult{false, "ollama unreachable at " + be.cfg.BaseURL,
					[]string{"start/install ollama (see `lo chat --check`)"}}
			}
			return checkResult{false, "no server at " + be.cfg.BaseURL,
				[]string{"start a llamafile (or other OpenAI-compatible server) on that port"}}
		}
		if be.ollama() && !modelPresent(models, be.cfg.Model) {
			return checkResult{false,
				"ollama up; model " + be.cfg.Model + " not pulled (have: " + summarize(models) + ")",
				[]string{"ollama pull " + be.cfg.Model}}
		}
		if len(models) == 0 {
			return checkResult{false, "server up but no model loaded",
				[]string{"load a model into the server at " + be.cfg.BaseURL}}
		}
		return checkResult{true, "ready (" + summarize(models) + ")", nil}
	}
	return checkResult{b.Available(), "", nil}
}

func isLocal(b Backend) bool {
	_, ok := b.(*httpBackend)
	return ok
}

// ---- the `--check` system check / init helper ----

func printItem(ok bool, name, detail string) {
	mark := cGreen + "✓" + cReset
	if !ok {
		mark = cRed + "✗" + cReset
	}
	fmt.Printf("  %s %-24s %s%s%s\n", mark, name, cDim, detail, cReset)
}

func runCheck(cfg *Config, backends map[string]Backend, active string) int {
	fmt.Printf("%s%slo chat — system check%s\n\n", cBold, cGreen, cReset)

	// 1. the lo mcp bridge (argsh.so builtin + tool surface)
	fmt.Printf("%sbridge%s\n", cBold, cReset)
	mcp := newMCP(cfg.MCP)
	mcp.timeout = 8 * time.Second
	if err := mcp.Start(); err != nil {
		printItem(false, "lo mcp", err.Error())
	} else {
		tools, _ := mcp.ListTools()
		printItem(true, "lo mcp", fmt.Sprintf("%d tools available", len(tools)))
	}
	mcp.Close()

	// 2. conductor backends (local models + optional frontier CLIs)
	fmt.Printf("\n%sconductor backends%s\n", cBold, cReset)
	localReady := false
	for _, n := range sortedNames(backends) {
		r := checkBackend(n, backends[n])
		label := n
		if n == active {
			label = n + "  (active)"
		}
		printItem(r.ok, label, r.detail)
		for _, f := range r.fix {
			fmt.Printf("       %s↳ %s%s\n", cDim, f, cReset)
		}
		if r.ok && isLocal(backends[n]) {
			localReady = true
		}
	}

	// 3a. ollama up but the preferred (active) model isn't pulled → offer the pull
	offered := false
	if be, ok := backends[active]; ok {
		if model, need := needsModelPull(be); need {
			fmt.Println()
			offerOllamaPull(model)
			offered = true
		}
	}
	// 3b. nothing local at all → full setup guide + llamafile engine offer
	if !localReady && !offered {
		fmt.Println()
		printSetupGuide()
		offerLlamafileInstall()
	}
	fmt.Println()
	return 0
}

// needsModelPull reports the model to pull when `be` is a *reachable* ollama
// backend whose model isn't pulled yet. (Server down => nothing to pull through;
// non-ollama => weights are managed elsewhere.)
func needsModelPull(be Backend) (string, bool) {
	h, ok := be.(*httpBackend)
	if !ok || !h.ollama() {
		return "", false
	}
	models, err := h.listModels(3 * time.Second)
	if err != nil || modelPresent(models, h.cfg.Model) {
		return "", false
	}
	return h.cfg.Model, true
}

// offerOllamaPull offers to fetch the preferred model from ollama's registry via
// `ollama pull` — which verifies layer digests, so running it on confirm is safe.
// Interactive only.
func offerOllamaPull(model string) {
	ola, err := exec.LookPath("ollama")
	if err != nil || !stdinTTY() {
		fmt.Printf("%sPull the preferred model:%s  ollama pull %s\n", cBold, cReset, model)
		return
	}
	fmt.Printf("%sPull %s now? %s(ollama pull %s — digest-verified)%s [y/N] ", cBold, model, cDim, model, cReset)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.ToLower(strings.TrimSpace(line)) != "y" {
		fmt.Printf("%sskipped — run `ollama pull %s` when ready%s\n", cDim, model, cReset)
		return
	}
	fmt.Printf("%s$ ollama pull %s%s\n", cDim, model, cReset)
	cmd := exec.Command(ola, "pull", model)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Printf("%spull failed: %v%s\n", cRed, err, cReset)
		return
	}
	fmt.Printf("%s✓ %s ready — lo chat will use it.%s\n", cGreen, model, cReset)
}

// offerLlamafileInstall offers to install the llamafile engine via b (single
// static binary, same manager as the rest of the toolchain). Interactive only:
// it prompts y/N when stdin is a TTY, otherwise it just prints the command.
// Never installs silently. The model weights (.gguf) remain a separate step.
func offerLlamafileInstall() {
	bPath, err := exec.LookPath("b")
	if err != nil {
		return // no b on PATH — the guide already covered the options
	}
	if !stdinTTY() {
		fmt.Printf("\n%sInstall the llamafile engine via b:%s  %s%s%s\n", cBold, cReset, cCyan, bInstallHint(), cReset)
		return
	}
	fmt.Printf("\n%sInstall the llamafile engine now via b?%s %s(%s)%s [y/N] ", cBold, cReset, cDim, bInstallHint(), cReset)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.ToLower(strings.TrimSpace(line)) != "y" {
		fmt.Printf("%sskipped — run `%s` when ready%s\n", cDim, bInstallHint(), cReset)
		return
	}
	fmt.Printf("%s$ %s%s\n", cDim, bInstallHint(), cReset)
	cmd := exec.Command(bPath, bInstallArgs()...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Printf("%sinstall failed: %v — install manually from %s releases%s\n", cRed, err, llamafileRepo, cReset)
		return
	}
	fmt.Printf("%s✓ llamafile installed.%s next: give it a model and start the server:\n", cGreen, cReset)
	fmt.Println("    llamafile -m /path/to/model.gguf --server --nobrowser   # serves :8080")
	fmt.Println("    lo chat --model local-llamafile")
}

func printSetupGuide() {
	fmt.Printf("%sNo local AI model is ready — pick a runtime:%s\n\n", cYellow, cReset)

	fmt.Printf("%s  Ollama%s — recommended: its own registry, digest-verified, handles chat templates\n", cBold, cReset)
	fmt.Println("    1. install via your package manager (e.g. pacman -S ollama, brew install ollama)")
	fmt.Println("       or download from https://ollama.com/download — verify, then run (don't pipe to a shell)")
	fmt.Println("    2. ollama pull gemma4:e2b          # fast default, ~8GB VRAM")
	fmt.Println("       ollama pull gemma4:e4b          # mid, ~10GB")
	fmt.Println("       ollama pull qwen2.5-coder:14b   # stronger, ~11GB VRAM")
	fmt.Println("    lo chat targets http://localhost:11434 out of the box.")
	fmt.Println()

	fmt.Printf("%s  Single-binary server%s — no daemon; serves an OpenAI API on :8080 (lo chat --model local-llamafile)\n", cBold, cReset)
	fmt.Println("    · llama-server (llama.cpp) — auto-pulls a GGUF straight from Hugging Face:")
	fmt.Println("         llama-server -hf unsloth/gemma-4-E4B-it-GGUF:UD-Q4_K_XL")
	fmt.Println("    · llamafile — one portable APE via b (the lok8s toolchain manager); bring your own .gguf:")
	fmt.Printf("         %s\n", bInstallHint())
	fmt.Println("         llamafile -m /path/to/model.gguf --server --nobrowser")
	fmt.Printf("    %s(a pre-packaged *.llamafile is a remote executable — download, verify, chmod +x, run yourself)%s\n", cDim, cReset)
	fmt.Println()

	fmt.Printf("%s  Frontier CLI%s — claude / gemini / codex (sends data off-machine; not local)\n", cBold, cReset)
	fmt.Println("    install one, then:  lo chat --model claude")
}

// ---- startup preflight ----

// preflightConductor verifies the active conductor before the first turn. If it
// isn't ready it prints actionable advice and, when possible, falls back to
// another *local* model — never silently to an off-machine CLI. Returns the
// backend (and name) to actually use.
func preflightConductor(name string, be Backend, backends map[string]Backend) (Backend, string) {
	r := checkBackend(name, be)
	if r.ok {
		return be, name
	}
	fmt.Fprintf(os.Stderr, "%s⚠ conductor %q not ready: %s%s\n", cYellow, name, r.detail, cReset)
	for _, f := range r.fix {
		fmt.Fprintf(os.Stderr, "  %s↳ %s%s\n", cDim, f, cReset)
	}
	// prefer another ready LOCAL backend (privacy: don't auto-route off-machine)
	for _, n := range sortedNames(backends) {
		if n == name || !isLocal(backends[n]) {
			continue
		}
		if checkBackend(n, backends[n]).ok {
			fmt.Fprintf(os.Stderr, "%s→ using local model %q instead%s\n", cGreen, n, cReset)
			return backends[n], n
		}
	}
	// otherwise just suggest installed CLIs (let the user opt in to off-machine)
	var cli []string
	for _, n := range sortedNames(backends) {
		if c, ok := backends[n].(*cliBackend); ok && c.Available() {
			cli = append(cli, n)
		}
	}
	if len(cli) > 0 {
		fmt.Fprintf(os.Stderr, "%s  off-machine CLI available: /model %s%s\n", cDim, strings.Join(cli, " · /model "), cReset)
	}
	fmt.Fprintf(os.Stderr, "%s  run `lo chat --check` for setup help%s\n", cDim, cReset)
	return be, name
}

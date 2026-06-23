# lochat — the `lo chat` engine (Go)

A single static, **dependency-free** Go binary (stdlib only → builds offline,
fits `b`'s binary model): the transparent, streaming, read-only lok8s assistant.
Driven by the `lo chat` argsh shim (`.lok8s/libs/chat`), which resolves the lo
runtime, renders the YAML chat config to JSON via `yq`, and execs this binary.

The architecture was chosen by the benchmark (see `../../LOCAL-AI-PLAN.md`):
flat diet tools · schema-in-context authoring · doctor-tree debug · a
**deterministic** read-only posture gate (models can't self-police — proven).

## Build
```bash
go build -C ai/lochat -o "$PATH_BIN/lochat" .   # onto PATH / .bin (b-managed)
```

## Run
```bash
lo chat                          # interactive TUI (binary on PATH + shim wired)
lo chat -p "what addons exist?"  # single-shot
lo chat --model local-14b        # pick a backend; --posture open|confirm|read-only
# dev (bypass shim):
ai/lochat/lochat --config <json> -p "..."
```
In-session: `/model` (switch local ↔ frontier CLI), `/posture`, `/think`, `/tools`, `/clear`.

## Files
| file | role |
|------|------|
| `main.go` | flags + wiring |
| `config.go` | JSON config (the shim builds it from YAML via yq) |
| `mcp.go` | MCP stdio client — `tools/list` + `tools/call` over `lo mcp` |
| `tools.go` | diet catalog + `@readonly`/`@idempotent` tiers |
| `backend.go` | streaming HTTP (ollama/openai) + CLI escalation (`claude -p`/`gemini`/`codex`) |
| `conductor.go` | route → posture-gate → execute READ tools → stream a grounded answer |
| `ui.go` | ANSI transparent UI (route/gate/tool panels, streamed tokens) + REPL |

Config, tools, and knowledge (the skills) are all external — no Go module deps.

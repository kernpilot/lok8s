# lochat — the `lo chat` engine (Go)

A single static, **dependency-free** Go binary (stdlib only → builds offline,
fits `b`'s binary model): the transparent, streaming, read-only lok8s assistant.
Driven by the `lo chat` argsh shim (`.lok8s/libs/chat`), which resolves the lo
runtime and execs this binary with a **JSON config + dynamic flags** (no `yq`,
no transforms). The shim preflights for `argsh.so` (the `lo mcp` builtin) first.

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
lo chat --model local-14b        # pick a backend; --posture open|read-only
lo chat --check                  # system check (bridge + runtime) + guided setup
# dev (bypass shim):
ai/lochat/lochat --config <json> -p "..."
```
In-session (aliases + no-arg cycle): `/model`·`/m`, `/models`, `/posture`·`/p`,
`/think`·`/t`, `/tools`, `/clear`·`/c`, `/quit`·`/q`. Answers render light
markdown on a TTY; piping keeps raw markdown (no escapes).

## Files
| file | role |
|------|------|
| `main.go` | flags + wiring + startup preflight |
| `config.go` | JSON config + defaults (no yq) |
| `mcp.go` | MCP stdio client — `tools/list` + `tools/call`; fail-fast if `lo mcp` dies |
| `tools.go` | diet catalog + `@readonly`/`@idempotent` tiers |
| `backend.go` | streaming HTTP (ollama/openai) + CLI escalation (`claude -p`/`gemini`/`codex`); probes availability |
| `conductor.go` | route → posture-gate → execute READ tools → stream a grounded answer |
| `ui.go` | ANSI transparent UI (panels, streamed tokens, **markdown**) + REPL + slash shortcuts (TTY-aware color) |
| `check.go` | `--check` system check, conductor preflight, setup offers (`b install` llamafile · `ollama pull`) |

Config, tools, and knowledge (the skills) are all external — no Go module deps.

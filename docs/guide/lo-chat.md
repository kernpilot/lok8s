# Local AI — `lo chat`

`lo chat` is a **fully local**, transparent, streaming assistant for your cluster.
It talks to your project through the same `lo mcp` tool surface the editor agents
use, runs **read-only by default**, and streams every step — which tool it calls,
what came back, then the answer. No data leaves your machine unless you explicitly
switch to a frontier CLI.

```bash
lo chat                       # interactive REPL
lo chat -p "what's broken?"   # single-shot
lo chat --check               # system check + guided setup
```

## How it works

The assistant is a small **conductor** model that routes your question through `lo`
tools (a flat, "dieted" subset of the full CLI), gathers facts, then streams an
answer. Three properties keep it safe and useful on small local models:

- **Transparent** — every route, every gate decision, and every tool's output is
  printed as it happens. Nothing is hidden.
- **Deterministic safety gate** — the *posture* (not the model) decides what may
  run. In `read-only` (the default) only `[read]` tools execute; write tools are
  blocked before they reach the cluster. Models can't talk their way past it.
- **Schema-in-context** — when you ask it to author config (a `cluster.lok8s.yaml`,
  an addon chart), the relevant lok8s schema is injected so the output follows the
  real patterns.

## First run: `lo chat --check`

`lo chat --check` reports everything the assistant needs and, when something is
missing, offers to fix it:

```
lo chat — system check

bridge
  ✓ lo mcp                   57 tools available

conductor backends
  ✓ local-e2b  (active)      ready (gemma4:e2b, …)
  ✗ local-llamafile          no server at http://localhost:8080/v1
```

If no local model is ready it prints a setup guide and offers to install the
llamafile engine via `b` (on confirm), or to `ollama pull` your preferred model.

## Choosing a model

`lo chat` needs one local runtime. Pick whichever you prefer — all are local and
private.

### Ollama (recommended)

The default conductor (`local-e2b` → `gemma4:e2b`) talks to Ollama at
`http://localhost:11434`. Ollama has its **own model registry** — digest-verified,
and it handles chat templates for you — so you just pull by name:

```bash
ollama pull gemma4:e2b          # fast default (~8GB VRAM)
ollama pull gemma4:e4b          # mid (~10GB)      → lo chat --model local-e4b
ollama pull qwen2.5-coder:14b   # stronger (~11GB) → lo chat --model local-14b
```

`lo chat --check` will offer to `ollama pull` a configured-but-missing model for
you (digest-verified, so safe to run on confirm). This is the lowest-friction path.

### Single-binary server (no daemon)

Prefer one executable and no background daemon? Run any server that exposes an
**OpenAI-compatible API on `:8080`** and use `lo chat --model local-llamafile`.
Two good options:

- **`llama-server`** (from [llama.cpp](https://github.com/ggml-org/llama.cpp)) —
  **auto-pulls a GGUF straight from Hugging Face** and serves it, in one command.
  This is the simplest way to run an HF model like Gemma 4 E4B:

  ```bash
  llama-server -hf unsloth/gemma-4-E4B-it-GGUF:UD-Q4_K_XL   # downloads + serves :8080
  lo chat --model local-llamafile
  ```

- **[llamafile](https://github.com/Mozilla-Ocho/llamafile)** — a single portable
  APE (the llama.cpp engine), offline, auto-detects CUDA/Metal/ROCm. The engine is
  a perfect fit for `b`, the lok8s toolchain manager; you bring your own `.gguf`:

  ```bash
  b install --asset 'llamafile-*[0-9]' Mozilla-Ocho/llamafile   # the engine (offered by --check)
  llamafile -m /path/to/gemma-4-E4B-it.gguf --server --nobrowser
  lo chat --model local-llamafile
  ```

> **Why these aren't auto-pulled.** `llama-server -hf` does its own HF download, and
> a pre-packaged `*.llamafile` is a **remote executable** — neither is digest-checked
> the way `ollama pull` is. `lo chat` shows the command; you fetch (and verify) it
> yourself. Ollama models *are* digest-verified, so those `lo chat` pulls for you.

> **Which `:8080` server?** `local-llamafile` is just an OpenAI-compatible backend
> pointed at `http://localhost:8080` — `llama-server`, `llamafile`, vLLM, or
> LM Studio all satisfy it.

## Shortcuts (in the REPL)

| Command | Alias | No-arg behavior |
| --- | --- | --- |
| `/model` | `/m` | cycle to the next *available* model |
| `/models` | | list models (● active · ✗ not installed) |
| `/posture` | `/p` | cycle `read-only` → `confirm` → `open` |
| `/think` | `/t` | toggle reasoning (Ollama backends) |
| `/tools` | | list the available tools |
| `/clear` | `/c` | reset the conversation |
| `/help` | `/?` | this help |
| `/quit` | `/q` | exit |

All accept an explicit argument too (`/m local-14b`, `/p open`, `/t off`). The
answer is rendered with light markdown (bold, code, lists, fenced blocks); piping
to a file keeps the raw markdown with no escape codes.

## Posture & safety

| Posture | What runs |
| --- | --- |
| `read-only` (default) | only `[read]` tools — status, doctor, lint, kubeconfig, … |
| `confirm` | same gate as read-only (write tools blocked) |
| `open` | all tools, including writes |

Switch per session with `--posture`, or live with `/posture`. The gate is enforced
in code, independent of the model.

## Frontier handoff (optional)

If you have a frontier CLI installed (`claude`, `gemini`, `codex`), you can hand a
turn to it with `/model claude`. This **sends data off-machine** — `lo chat` keeps
it strictly opt-in and never routes there automatically, even if your configured
local model is unavailable.

## Configuration

`lo chat` reads `lo-chat.json` from the project root (falling back to the shipped
defaults), or the path in `$LO_CHAT_CONFIG`. Backends are conductor brains:

```json
{
  "conductor": "local-e2b",
  "posture": "read-only",
  "backends": {
    "local-e2b": { "api": "ollama", "model": "gemma4:e2b", "num_ctx": 16384 },
    "local-e4b": { "api": "ollama", "model": "gemma4:e4b", "num_ctx": 16384 },
    "local-llamafile": { "api": "openai", "model": "local", "base_url": "http://localhost:8080/v1" },
    "claude": { "type": "cli", "command": ["claude", "-p"] }
  }
}
```

- `api: "ollama"` uses the native Ollama API (supports `num_ctx`, a `think`
  toggle); `model` is an Ollama-registry name.
- `api: "openai"` targets any OpenAI-compatible server (llamafile, vLLM, LM Studio)
  via `base_url`.
- `type: "cli"` is a frontier handoff.

> `lo chat` is a single static Go binary managed by `b`. It requires the `lo mcp`
> bridge, which is an `argsh.so` builtin — if a fresh checkout reports
> *"Invalid command: mcp"*, run `argsh builtins install`.

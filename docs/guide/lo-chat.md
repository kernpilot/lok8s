# Local AI: `lo chat`

`lo chat` is a local, streaming assistant for your cluster. It runs in the
`read-only` posture by default. It shows every step: the tool it calls, the tool
output, then the answer. No data leaves your machine unless you switch to a
frontier CLI.

```bash
lo chat                       # interactive REPL
lo chat -p "what's broken?"   # single-shot
lo chat --check               # system check + guided setup
```

::: warning lo chat needs parts that a default install does not have
`lo chat` starts a separate engine, `lochat`. The engine gets its tools from
the MCP server of the frozen bash implementation (the `mcp` builtin of argsh),
not from the Go [`lo mcp`](/guide/mcp) server. The `lo` binary embeds the
bash tree, but `lo toolchain install` does not install the engine or the argsh
runtime by default. Do the steps in [Set up](#set-up) first.
:::

## Set up

You need three parts: the argsh runtime, the `lochat` engine and a local
model runtime. Run the steps in the project directory.

1. Install the `bash` group of the toolchain: argsh with its `argsh.so`
   builtin, yq, jq and the other tools of the bash tree. This step uses the
   network.

   - If `.bin/b.yaml` exists, `lo toolchain install` does not change it.
     Remove the `# ` in front of the entries of the `bash` group in
     `.bin/b.yaml`, then run `.bin/b install`.
   - If `.bin/b.yaml` does not exist, run this command:

     ```bash
     lo toolchain install --groups core,local,bash
     ```

2. Download the `lochat` engine of the `lo` you run and the checksums of
   its release into a temporary directory, then verify the engine. This
   step uses the network.

   ```bash
   V="v$(lo version -o json | .bin/jq -r .lok8s)"   # the version of your lo
   A=lochat-linux-amd64                              # your platform
   T="$(mktemp -d)"
   curl -fsSL -o "${T}/${A}" "https://github.com/kernpilot/lok8s/releases/download/${V}/${A}"
   curl -fsSL -o "${T}/checksums.txt" "https://github.com/kernpilot/lok8s/releases/download/${V}/checksums.txt"
   (cd "${T}" && sha256sum --ignore-missing -c checksums.txt)   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
   ```

3. Install the engine into the toolchain directory of the project, then
   remove the temporary directory:

   ```bash
   install -m 0755 "${T}/${A}" .bin/lochat
   rm -rf "${T}"
   ```

   `lo chat` looks for `.bin/lochat` first, then for `lochat` on PATH.

4. Install a local model runtime and pull a model (see
   [Choosing a model](#choosing-a-model)).

5. Run the system check:

   ```bash
   lo chat --check
   ```

   `lo ai check` runs the same check and also shows the skill wiring.

## How it works

The assistant is a small **conductor** model. It routes your question through
`lo` tools (a small, flat subset of the CLI), collects facts, then streams an
answer. Three properties keep it safe and useful on small local models:

- **Transparent**: it prints every route, every gate decision, and every tool's
  output as it happens. Nothing is hidden.
- **Deterministic safety gate**: the *posture* (not the model) decides what may
  run. In `read-only` (the default) only `[read]` tools execute; the gate blocks
  write tools before they reach the cluster. Models can't talk their way past it.
- **Schema-in-context**: when you ask it to write config (a `cluster.lok8s.yaml`,
  an addon chart), it adds the lok8s schema to the prompt. It reads the schema
  from the `schema_files` of the config (the lok8s skills under `skills/`) and
  skips a file that does not exist.

## First run: `lo chat --check`

`lo chat --check` reports everything the assistant needs and, when something is
missing, offers to fix it:

```
lo chat — system check

bridge
  ✓ lo mcp                   <n> tools available

conductor backends
  ✓ local-e2b  (active)      ready (gemma4:e2b, …)
  ✗ local-llamafile          no server at http://localhost:8080/v1
```

If no local model is ready it prints a setup guide and offers to install the
llamafile engine via `b` (on confirm), or to `ollama pull` your preferred model.

## Choosing a model

`lo chat` needs one local runtime. Pick whichever you prefer: all are local and
private.

### Ollama (recommended)

The default conductor (`local-e2b` → `gemma4:e2b`) talks to Ollama at
`http://localhost:11434`. Ollama has its **own model registry** (digest-verified,
and it handles chat templates for you), so you just pull by name:

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

- **`llama-server`** (from [llama.cpp](https://github.com/ggml-org/llama.cpp))
  **auto-pulls a GGUF straight from Hugging Face** and serves it, in one command.
  This is the simplest way to run an HF model like Gemma 4 E4B:

  ```bash
  llama-server -hf unsloth/gemma-4-E4B-it-GGUF:UD-Q4_K_XL   # downloads + serves :8080
  lo chat --model local-llamafile
  ```

- **[llamafile](https://github.com/Mozilla-Ocho/llamafile)**: a single portable
  APE (the llama.cpp engine), offline, auto-detects CUDA/Metal/ROCm. `b`, the
  lok8s toolchain manager, can install the engine. You bring your own `.gguf`:

  ```bash
  b install --asset 'llamafile-*[0-9]' Mozilla-Ocho/llamafile   # the engine (offered by --check)
  llamafile -m /path/to/gemma-4-E4B-it.gguf --server --nobrowser
  lo chat --model local-llamafile
  ```

> **Why these aren't auto-pulled.** `llama-server -hf` does its own HF download, and
> a pre-packaged `*.llamafile` is a **remote executable**: neither is digest-checked
> the way `ollama pull` is. `lo chat` shows the command; you fetch (and verify) it
> yourself. Ollama models *are* digest-verified, so `lo chat` will pull those for you.

> **Which `:8080` server?** `local-llamafile` is just an OpenAI-compatible backend
> pointed at `http://localhost:8080`: `llama-server`, `llamafile`, vLLM, or
> LM Studio all satisfy it.

## Shortcuts (in the REPL)

| Command | Alias | No-arg behavior |
| --- | --- | --- |
| `/model` | `/m` | cycle to the next *available* model |
| `/models` | | list models (● active · ✗ not installed) |
| `/posture` | `/p` | toggle `read-only` ↔ `open` |
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
| `read-only` (default) | only `[read]` tools: status, doctor, lint, … |
| `open` | all tools, including writes |

Switch per session with `--posture`, or live with `/posture`. The gate is enforced
in code, independent of the model.

## Frontier handoff (optional)

If you have a frontier CLI installed (`claude`, `gemini`, `codex`), you can hand a
turn to it with `/model claude`. This **sends data off-machine**. `lo chat` keeps
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

> The engine is a separate static Go binary (`lochat`, source in `ai/lochat/`).
> It drives the `mcp` builtin from `argsh.so`. When `lo chat` reports that
> `argsh.so` is missing, install the `bash` group of the toolchain (see
> [Set up](#set-up)) or run `argsh builtins install`.

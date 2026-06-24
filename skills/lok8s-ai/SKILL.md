---
name: lok8s-ai
description: >-
  Use when working with lok8s's own AI features — `lo chat` (the fully-local
  assistant), `lo ai` (skill linking + setup checks), the `lo mcp` tool bridge,
  or configuring conductor backends / posture / skill delivery. Explains how the
  lok8s Agent Skills reach an assistant (native symlink vs. `lo chat` injection)
  and how the read-only safety gate works. Not for authoring cluster artifacts —
  see lok8s-implement / lok8s-service for that.
---

# The lok8s AI surface

lok8s ships three things an assistant uses, plus this management layer:

| Piece | What it is |
|---|---|
| **`lo mcp`** | the tool bridge — `.mcp.json` points an agent at `lo mcp`, which exposes every `lo` subcommand as an MCP tool (`lo_status`, `lo_build`, …). It's an `argsh.so` builtin: if a fresh checkout says *"Invalid command: mcp"*, run `argsh builtins install`. |
| **`skills/*/SKILL.md`** | the *knowledge* layer — schemas, decision trees, playbooks (this file is one). Complements the tools: tools *do*, skills *know*. |
| **`lo chat`** | the local runtime — a small "conductor" model that routes your question over the `lo` tools, gathers facts, and streams an answer. |
| **`lo ai`** | manage the above: check the setup, and link skills into skill-aware assistants. |

## `lo chat` — the local assistant

```bash
lo chat                    # interactive
lo chat -p "what's broken?"  # single-shot
lo chat --check            # system check + guided setup (also: lo ai check)
```

- **Transparent**: prints every route → tool → output before the answer.
- **Read-only by default**: a *deterministic* posture gate (not the model) decides
  what runs. `read-only` allows only `[read]` tools; `open` allows writes. Tools on
  the `deny`/`drop` lists (e.g. `lo_secrets_print`) never run in any posture.
- **Backends** (in `lo-chat.json`, or the shipped defaults): `api: ollama` (local
  Ollama, the default), `api: openai` (any OpenAI server on `:8080` —
  `llama-server`/llamafile/vLLM), or `type: cli` (a frontier handoff: claude /
  gemini / codex — opt-in, off-machine).
- In-session shortcuts: `/model` `/m`, `/posture` `/p`, `/think` `/t`, `/tools`,
  `/clear`, `/quit` (aliases + no-arg cycling); answers render light markdown.

Models are managed outside lok8s (Ollama's registry, or `llama-server -hf` to
pull a GGUF straight from Hugging Face). `lo chat --check` offers an `ollama pull`
for a configured-but-missing model.

## `lo ai` — manage skills + setup

```bash
lo ai check            # the lo mcp bridge + local runtime + skill wiring
lo ai skills           # list the skills and how each assistant gets them
lo ai link claude      # symlink skills/* into .claude/skills/ (--copy to copy)
lo ai unlink claude    # remove them
```

## Two ways skills reach an assistant

- **Link** (`lo ai link claude`) — symlink `skills/*` into the agent's native
  skill dir (`.claude/skills/`). Claude then loads them with progressive
  disclosure (descriptions always; full body on demand). Best for skill-aware
  agents; also benefits interactive Claude Code in the repo. `.claude/` is
  gitignored, so links are per-machine (re-run `lo ai link` after a fresh
  checkout — skills ship to consumers via `b` env-sync, profile `core`).
- **Inject** — `lo chat` (and agents without a skill system) get the relevant
  skill text injected into context for a turn. No filesystem changes.

Rule of thumb: **link** where the agent has a skill system, **inject** otherwise
— never both for the same agent, or you double-feed it.

## Maintaining the skills

The skills cache behavior that lives in the `.lok8s/**` bash readers, drivers,
providers, and the secrets kustomize plugin — **the code is authoritative**. When
a reader changes, a skill can rot; use **lok8s-skill-maintainer** to re-derive a
skill from its sources and patch the drift.

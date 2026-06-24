# lok8s local-AI harness

Benchmark + train a **fully-local** `lo` assistant. Two jobs:

1. **Benchmark** (`bench`) — measure where a stock local model actually fails on
   real lok8s tasks, bucketed so the fix is unambiguous. This is *Step Zero*: it
   decides whether you need a router, a LoRA, or better skills — before you build
   any of them.
2. **Train** (`synth` → `verify` → `train`) — a configurable QLoRA pipeline for a
   format-adherence adapter, with a hard verifier so it only learns YAML that
   compiles.

Everything is config-driven (`config.yaml`): swap endpoints, models, the tool-
injection strategy, or the training backend without touching code. See
[`../LOCAL-AI-PLAN.md`](../LOCAL-AI-PLAN.md) for the architecture and the why.

## Install

```bash
cd ai
python -m venv .venv && . .venv/bin/activate
pip install -r requirements-bench.txt        # tiny: PyYAML (+ optional numpy)
cp config.example.yaml config.yaml            # then edit endpoints/models
# training only, on the gfx1201 box:
#   (install ROCm torch first — see requirements-train.txt) then:
#   pip install -r requirements-train.txt
```

Point `lo.cwd` at a lok8s project root (has `.lok8s/`, `clusters/`) and make sure
its toolchain is installed there (`GITHUB_TOKEN=$(gh auth token) ./.bin/b install`).

## The three buckets (what the benchmark tells you)

| Bucket | Failure | Fix it by |
|--------|---------|-----------|
| **A Routing** | wrong tool picked | building the router (hierarchy / embeddings) |
| **B Format** | invalid YAML / args | training the format LoRA |
| **C Reasoning** | wrong sequence/plan | improving the Markdown decision-tree skills |

`bench` runs each intent under every `eval.configs` strategy (the **A/B**: e.g.
`raw` vs `hierarchy`), `runs_per_intent` times, and prints a comparison table.
The delta between `raw` and `hierarchy` is your evidence for the router; the
`fmt_pass` column is your evidence (or not) for a LoRA.

## Workflow

```bash
python -m lo_ai dump-tools                 # snapshot + sanity-check the lo MCP surface
python -m lo_ai route -q "why is foo down" # see what the injector surfaces (no model needed)
python -m lo_ai bench                       # the benchmark -> results/run-<ts>/
# only if Bucket B fires:
python -m lo_ai synth  -i path/to/schema.md # teacher generates (intent, yaml) pairs
python -m lo_ai verify                      # hard-filter via `lo lint` (+ `lo build`)
python -m lo_ai train                       # QLoRA on the conductor's base
```

## Injection strategies (`injection.strategy`)

- `raw` — every tool. The deliberately-bad baseline.
- `diet` — minus plumbing/CI + the always-denied secret-readers.
- `hierarchy` — verb → (namespace) → tool, never more than `max_tools` on screen.
  Verbs default to the CLI's own `@readonly`/`@idempotent` tiers and double as the
  runtime posture gate. Override with `injection.verbs`.
- `semantic` — top-k tools by embedding similarity (needs `embeddings.enabled`).

## Notes

- The benchmark needs the conductor model reachable at `llm.conductor.base_url`
  (Ollama / llama.cpp server). It does **not** need the training stack.
- Bucket B verification needs a runnable `lo` in `lo.cwd`; if it isn't, those
  runs are scored `skip` rather than failing the sweep.
- The judge for Bucket C must differ from the conductor — never self-judge. Leave
  `llm.judge.model` empty to just record traces for manual review.

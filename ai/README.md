# lok8s local AI

A fully local assistant for `lo`: it knows how lok8s works, reads the
cluster through `lo mcp`, and helps you debug or author specs. All inference
runs on your machine. No data leaves it. That is the same promise lok8s makes
about the platform, applied to the AI layer.

This directory holds two things:

1. **The benchmark and training harness** (`lo_ai`, Python). It measures where
   a local model fails on real lok8s tasks and, only if needed, trains a
   format adapter.
2. **The chat engine** (`lochat/`, Go). It is the `lo chat` command. See
   [`lochat/README.md`](lochat/README.md).

## Architecture

The design follows the measurements, not the priors. The benchmark was the
first thing built, and it removed most of the planned machinery.

- **Conductor.** One stock local model, untrained. It routes the user's intent
  to a `lo` tool, executes read-only tools, and streams a grounded answer.
- **Flat tool exposure ("diet").** The conductor sees the `lo mcp` tools minus
  plumbing, CI helpers and the secret readers. A verb hierarchy on top made
  routing worse at every model size. There is no router.
- **Schema in context.** For authoring, the prompt carries the relevant lok8s skill (cluster spec
  or addon pattern). With it, the models write valid
  YAML. Without it, they do not know the schema. This replaced the planned
  LoRA and the synthetic-data pipeline. No training is needed.
- **Guided debugging.** Free-form agent loops find the right tools but fail to
  conclude. The debug flow follows the `lok8s-doctor` decision tree: symptom,
  cause, fix.
- **Deterministic posture gate.** The harness enforces safety, not
  the model. The CLI's own tiers (`@readonly`, `@idempotent`, untagged is
  mutating) decide what runs, what asks for confirmation, and what is blocked.
  Models do not police themselves reliably. In read-only mode one model
  invoked a mutating tool on 4 of 5 destructive intents.

## Which model

The numbers come from a 16 GB VRAM card with think mode off. The table ranks
only models that fit VRAM. A score is the fraction of intents solved.

| dimension | qwen2.5-coder:14b | gemma4:e2b |
|---|---|---|
| routing | 0.87 | 0.84 |
| argument correctness | 0.80 | 1.00 |
| cluster spec authoring, schema in context | 1.00 | 1.00 |
| addon authoring, schema in context | 1.00 | 0.33 |
| multi-step debug | 0.60 | 0.60 |
| refuses destructive calls in read-only mode | 0.80 | 0.20 |
| latency and VRAM per call | 3.1 s, 10.9 GB | 1.4 s, 7.8 GB |

Recommendation:

- **Default:** `gemma4:e2b`. It ties on routing, spec authoring and debugging
  at half the latency and 70 percent of the VRAM.
- **When addon authoring matters:** `qwen2.5-coder:14b`. The multi-file
  khelm addon pattern needs its capacity.
- **Tiny, routing only:** `qwen2.5-coder:1.5b` (1.5 GB, 0.7 s). It authors
  poorly.

Larger and newer models did not win. `qwen3-coder:30b` spilled past 16 GB and
was slower and less accurate. Think mode took about 73 s per call and
truncated. Dense, coder-tuned, VRAM-resident and think-off wins. The gate
protects you regardless of the model you choose.

## The three buckets

The benchmark puts every intent into one bucket. The bucket names the fix.

| bucket | failure | the fix |
|---|---|---|
| A routing | wrong tool picked | change the tool injection |
| B format | invalid YAML or arguments | schema in context, then a LoRA only if that fails |
| C reasoning | wrong sequence or plan | improve the Markdown decision-tree skills |

`lo lint` scores bucket B. Add `lo build` to `train.verify.commands` to
render as well, as `config.example.yaml` does. There is no `lo --dry-run`.
The build is the render check. Bucket C uses a gold action
trace or a different judge model. The conductor never judges itself.

## Install

```bash
cd ai
python -m venv .venv && . .venv/bin/activate
pip install -r requirements-bench.txt        # PyYAML (+ optional numpy)
cp config.example.yaml config.yaml            # then edit endpoints and models
# training only, on a ROCm or CUDA box:
#   install the matching torch first (see requirements-train.txt), then
#   pip install -r requirements-train.txt
```

Point `lo.cwd` at a lok8s project root and install its toolchain there
(`lo toolchain install`, or `./.bin/b install` in an existing project).

## Workflow

```bash
python -m lo_ai dump-tools                 # snapshot and check the lo MCP surface
python -m lo_ai route -q "why is foo down" # what the injector surfaces (no model)
python -m lo_ai bench                       # the benchmark -> results/run-<ts>/
# only if bucket B fires and schema in context does not close it:
python -m lo_ai synth  -i path/to/schema.md # teacher writes (intent, yaml) pairs
python -m lo_ai verify                      # hard filter: lo lint, then lo build
python -m lo_ai train                       # QLoRA on the conductor's own base
```

`bench` runs each intent under every `eval.configs` strategy, `runs_per_intent`
times, and prints a comparison table. The `fmt_pass` column is the evidence for
or against a LoRA.

The other rows of the model matrix come from the dedicated evals:
`authoreval` (cluster specs), `addoneval` (addons), `agenteval` (multi-step
debug with mocked tools) and `safetyeval` (the posture gate). `ledger` prints
the recorded runs. Each is a subcommand of `python -m lo_ai`.

## Injection strategies (`injection.strategy`)

- `raw`: every tool. This is the deliberately bad baseline.
- `diet`: minus plumbing, CI helpers and the always-denied secret readers. The
  default.
- `hierarchy`: verb, then namespace, then tool, never more than `max_tools` on
  screen. It stays for A/B runs. It lost on every model.
- `semantic`: top-k tools by embedding similarity (needs `embeddings.enabled`).

## Training, if you ever need it

Train the adapter on the conductor's own base model, not on a second model.
A 16 GB card cannot hold two models resident, so a separate worker means a
reload on every mutation. One resident model with a runtime LoRA toggle
(llama.cpp `/lora-adapters`) has zero swap cost. Training runs offline on
synthetic data only, so it never touches private cluster data. The verifier
keeps only pairs that pass `lo lint` and `lo build`. Use a current teacher
model, and regenerate the pairs when the schema changes.

## Notes

- The benchmark needs the conductor reachable at `llm.conductor.base_url`
  (Ollama or a llama.cpp server). It does not need the training stack.
- Bucket B verification needs a runnable `lo` in `lo.cwd`. If it is missing,
  those runs get the score `skip`.
- Leave `llm.judge.model` empty to record traces for manual review instead
  of scoring bucket C.
- Only benchmark models that fit VRAM. The runner records the GPU fraction
  from Ollama; a model that spills stays in history under its old hash.

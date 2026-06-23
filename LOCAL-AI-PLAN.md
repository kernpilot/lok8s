# lok8s Local AI — plan & decisions

A fully-local, privacy-preserving AI assistant for lok8s (`lo chat`): chat with
something that knows how lok8s works, watches the cluster, and helps debug or
bootstrap — **all inference on-device** (EU-sovereign, no lock-in; same promise
lok8s makes about kubehz, applied to the AI layer).

Branch: `feat/local-ai`. Target box: AMD RX 9070 (RDNA 4, gfx1201, 16 GB VRAM) +
128 GB RAM.

## Architecture (capability/schema split, not domain split)

Splitting by *task capability + output schema* avoids the drift coupling that a
per-domain ("cert-manager agent") split would create as the CLI changes.

- **Conductor** — stock ~14B coder model (current pick: Qwen2.5-Coder-14B q4,
  ~9 GB VRAM; revisit for newer). **Untrained.** Conversational UX + walks the
  `lok8s-doctor` decision tree. Intelligence lives in the *harness*, not weights.
- **Router** — **hierarchy first, embeddings second.**
  - Hierarchy is *free*: the CLI's safety annotations are the tiers —
    `@readonly` (15 cmds) / `@idempotent` (e.g. `build`) / untagged=mutating —
    and the `lo_<group>_*` namespaces (`secrets`, `image`, `registry`, `gitops`,
    `tilt`, `kubehz`, `k8s`) are the groups. Routing to a verb both narrows the
    schema set **and** sets the confirm/block posture gate. One mechanism.
  - Embeddings (e.g. `nomic-embed-text`) only as the within-group leaf selector.
    Symptom→tool similarity is weak, so don't lead with it. ~20 dieted tools
    barely need a vector DB — in-process cosine is enough.
- **Worker(s)** — format-adherence LoRA, **only if the eval proves it's needed.**
  - **Train the adapter on the Conductor's own base**, not a separate 7B. On
    16 GB VRAM you can't hold a 2nd model resident, so a separate worker means an
    unload/reload (seconds) on every mutation — which erases the smaller-model
    speed win. Instead toggle the adapter on the resident model at runtime
    (llama.cpp `/lora-adapters`). One resident model, zero swap.
  - If eval Bucket B is low, **no LoRA at all** — the Conductor writes the YAML
    and the entire training pipeline below is unnecessary. Want this outcome.

## Stack (verified 2026-06-23)

- **gfx1201 is officially supported** in ROCm 7.x (7.2 lists RX 9070 XT/GRE);
  PyTorch ROCm wheels cover gfx1201. Not "brand new" — RDNA 4 is ~15 months old.
- **Unsloth has official AMD/ROCm support** (QLoRA on gfx1201, ROCm 6.0+).
  Expect *setup* friction (ensure pip pulls ROCm wheels not CUDA; bitsandbytes-
  on-AMD quirks), not architectural blockers.
- **Inference:** llama.cpp. Prefer the **Vulkan backend** if ROCm inference is
  flaky on a fresh card — Vulkan is GPU-agnostic and sidesteps ROCm entirely.
  Runtime LoRA toggle for the single-base approach.
- **Training is occasional + offline + on synthetic (non-private) data**, so it
  *may* run in the cloud without breaking the local-inference privacy promise.
  Decision: train locally (committed), accept multi-hour/day runs.

## Verify-in-the-loop data pipeline (only if a LoRA is needed)

Teacher generates `(intent, YAML)` pairs → **hard-filter before training**:

1. `lo lint` — schema validation (yq key allow-lists). Cheap, catches bad fields.
2. `lo build` — actually renders kustomize/khelm → `artifacts/`. Catches render/
   envsubst errors `lint` can't. (There is **no `lo --dry-run`** — this is the
   render check.)
3. *(optional)* apply to a throwaway kind cluster for semantic validity.

Caveats: (a) lint+build validate *form, not intent* — a pair can pass and still
implement the wrong thing, so teacher quality matters. (b) Use a **current**
teacher (Sonnet 4.6 bulk / Opus 4.8 hard cases), not a stale one. (c) Re-generate
when the schema changes — this is the same drift loop the `lok8s-skill-maintainer`
skill addresses for the Markdown skills.

## Phased plan

- **P0 — Eval (Step Zero, do before anything else).** Seeded intents through a
  stock 14B over the real `lo mcp` surface. Run **A/B**: raw ~60-tool dump vs
  dieted+hierarchy injection (the delta justifies the router). N runs/intent for
  variance. Auto-score into buckets:
  - **A Routing** (wrong tool) → build the hierarchy/embeddings router.
  - **B Format** (bad YAML/args) → train the format LoRA (on the 14B base).
  - **C Reasoning** (wrong sequence) → improve the Markdown decision trees.
  Scoring: A = tool-name match; B = `lo lint && lo build`; C = gold action-trace
  or a *different* judge model (never self-judge).
- **P1 — Read-only `lo chat`.** Local model + skills as context + read tools.
- **P2 — (conditional) format LoRA + pipeline**, gated on Bucket B.
- **P3 — Guarded mutations** behind the posture gate (confirm + render preview).
- **P4 — Proactive watch** via `operator/hooks/` feeding events to the agent.

**The eval gates everything** — it decides which of P2–P4 even exist. Build it
first; let measurement, not priors, pick the rest.

## Empirical findings (2026-06-23, real run on the RX 9070 / ROCm gfx1201)

Benchmarked qwen2.5-coder 1.5b/14b and qwen3-coder:30b for real. Three planned
assumptions were overturned — the eval earned its keep:

1. **The hierarchy router HURTS at every size** (flat/diet > hierarchy):
   1.5b 0.70 vs 0.34 · 14b **0.88** vs 0.71 · 30b 0.83 vs 0.66. The "60-tool
   overload" fear didn't materialize — the 14b routes 56 flat tools at 0.88.
   → dropped the hierarchy router; default `injection.strategy = diet`.

2. **Bigger isn't better here.** qwen2.5-coder:14b (0.88 routing, 3.4 s/call,
   fully VRAM-resident) beats qwen3-coder:30b (0.83, 7.5 s/call, spills past
   16 GB → 37/63 CPU/GPU) on **both** accuracy and latency. → conductor = **14b**.

3. **A LoRA is (almost certainly) unnecessary.** Clean authoring eval: the 14b
   writes valid lok8s YAML **6/8 unaided, 7/8 with the cluster-spec skill in
   context**, and the lone remaining fail was a verifier false-negative (correct
   Deploy spec; temp env lacked the referenced cluster) → effectively 8/8.
   Schema-in-context (RAG) closes the gap; no training/synthetic-data pipeline.

**Architecture collapses to:** 14b conductor + flat/diet tool exposure +
schema-in-context authoring. No router, no LoRA, no synthetic-data pipeline —
roughly two-thirds of the original build, deleted by measurement.

Caveats: 1 run/intent (temp 0, stability 1.0 → deterministic, so safe); 8
authoring intents (expand + seed referenced clusters in the verifier to pin the
RAG delta precisely); routing gold labels have some ambiguity that understates
true accuracy.

### Model bake-off (think-off, same 59-intent set)

Tested newer/other Qwen — qwen3-coder:30b, qwen3:14b, qwen3.5:4b/9b, qwen3.6:27b.
**None beats qwen2.5-coder:14b as the conductor.** Routing: 14b **0.881** (3.2s) >
30b 0.831 (7.5s) > 27b 0.819 (16.5s, VRAM spill) > qwen3.5:4b 0.814 (2.0s) >
9b 0.746. Authoring +schema: 14b/9b/27b all 0.875; **without** schema the newer
models fail (0–0.375) — they don't know the lok8s schema, so RAG is essential
(reconfirms: no LoRA). qwen3.5:4b is a compelling *routing-only* fast/tiny option
(0.814 @ 2.0s) but authors poorly (0.50). Reasoning mode (think-on) is non-viable:
~73s/call and truncates. **Task-fit > recency: dense + coder-tuned + VRAM-resident
+ no-think wins.** Verdict: **qwen2.5-coder:14b + diet tools + schema-in-context.**

### VRAM-fit ranking (think-off, fits-16GB only — dataset b7224ea9d)

Polished round: deploy-ref verifier false-negative fixed (authoring now tops at
1.0, not 0.875), VRAM captured per run, ranking published for other hardware.

| model | route | author+schema | lat | VRAM |
|---|---|---|---|---|
| qwen2.5-coder:14b | **0.873** | 1.00 | 3.1s | 10.9GB |
| **gemma4:e2b** | 0.841 | 1.00 | **1.4s** | 7.8GB |
| qwen3.5:4b | 0.794 | 0.58 | 2.2s | 6.1GB |
| qwen2.5-coder:1.5b | 0.714 | 0.33 | 0.7s | 1.5GB |
| qwen3.5:9b | 0.714 | 1.00 | 2.5s | 8.9GB |
| gemma4:e4b | 0.714 | 1.00 | 1.8s | 10.7GB |

**Recommendation matrix (lok8s runs on varied hardware):**
- Max accuracy → **qwen2.5-coder:14b** (0.873 route, 1.0 author; 10.9GB, 3.1s).
- Best balance / snappy / smaller cards → **gemma4:e2b** — near-14b routing
  (0.841) + perfect authoring, 2.3× faster, 7.8GB. Surprise standout.
- Tiny / routing-only → **qwen2.5-coder:1.5b** (1.5GB, 0.7s; weak authoring 0.33).

Authoring 1.0 needs schema-in-context for all (RAG; no LoRA — reconfirmed across
families). gemma4:12b untested (needs Ollama > 0.24.0). **Rule going forward:
only benchmark models that fit VRAM (gpu_frac=1.0); spilling models stay in
history under their old hash.**

### Addon/chart authoring (harbor) — RAG works, but needs a capable model

`addoneval`: author a chart addon, scored offline for **pattern conformance**
(khelm `ChartRenderer` + kustomization `generators`/`includeSelectors: false`/the
three `lok8s.dev/*` labels + `host.${LOK8S_SPEC_CLUSTER_DOMAIN}`). With the
`lok8s-addons` skill in context: **qwen2.5-coder:14b 3/3** (harbor/kube-prometheus/
cert-manager all conform); **gemma4:e2b 1/3** (follows the pattern but misses the
domain-var host 2/3). Without schema both 0/3 (neither knows khelm's apiVersion).

So addon authoring is **harder than cluster specs** (gemma4:e2b was 1.0 there,
1/3 here) — RAG suffices but needs the 14b's capacity; no LoRA. **Refines the
conductor pick:** if addon authoring matters, the 14b is the safer conductor;
gemma4:e2b is great for routing + simple specs but weak on the multi-file khelm
pattern. ("whitelisted domain" = the envsubst whitelist; a host reaches a manifest
only via `${LOK8S_SPEC_CLUSTER_DOMAIN}`, the whitelisted token.)

Remaining "cover all" dimensions still to build: argument-correctness scoring,
multi-step agentic debug (mocked tools), and safety/posture gating.

### Multi-step agentic debug — hard for all; must be guided

`agenteval` (mocked tools; chain run → read output → decide until cause+fix or
max_steps). Solve rate: **qwen2.5-coder:14b 3/5, gemma4:e2b 3/5 (ties!),
gemma4:12b 1/5** (bigger wanders more). Far below routing (0.87): the common
failure is running the *right* tools but not synthesizing the cause / not
terminating. **Implication: the debug assistant must be driven by the
`lok8s-doctor` decision tree (symptom → cause → fix), not free-form agentic
looping.** Harness > autonomy, now proven for the debug case.

### Cross-dimension conductor matrix (fits-VRAM, think-off)

| dimension | qwen2.5-coder:14b | gemma4:e2b |
|---|---|---|
| routing | 0.87 | 0.84 |
| spec author +schema | 1.00 | 1.00 |
| addon author +schema | **1.00** | 0.33 |
| agentic debug | 0.60 | 0.60 |
| latency / VRAM | 3.1s / 10.9GB | **1.4s / 7.8GB** |

**Default conductor: gemma4:e2b** — ties on routing/spec/debug at half the
latency and 70% the VRAM. **Use qwen2.5-coder:14b when addon authoring matters**
(0.33 vs 1.0), or scaffold the addon boilerplate deterministically. Multi-step
debug needs the doctor decision tree regardless of model.

Remaining "cover all" dimensions: argument-correctness, safety/posture.

## Open items

- ~~Confirm the current best ~14B local coder model~~ — **done (2026-06-23):**
  qwen2.5-coder:14b won the bake-off vs qwen3-coder:30b / qwen3.5:4b,9b / qwen3.6:27b.
- `b install` the toolchain in this worktree before running `lo`.
- Decide cloud-vs-local for the teacher generation step.

"""Clean Bucket-B (authoring) eval — RAG-vs-LoRA decider.

The routing benchmark's format bucket is confounded: the model often routes to
`lo_provision` instead of authoring. This eval removes that escape — it directly
asks the model to AUTHOR the spec, parses the YAML block, and runs it through the
real verifier. Run it twice (`--no-schema` vs `--schema`) to see whether putting
the authoritative lok8s schema/skill in context fixes the model's schema
hallucination — if it does, no LoRA is needed.
"""
from __future__ import annotations

import json
import re
from datetime import datetime

from lo_ai.config import Config
from lo_ai.llm import LLM

SYS = ("You are the lok8s YAML author. Output ONLY the complete, valid YAML for "
       "the requested lok8s resource — no prose, no explanations, no markdown "
       "fences.")


def extract_yaml(text: str) -> str:
    text = text.strip()
    m = re.search(r"```(?:ya?ml)?\s*(.*?)```", text, re.S)
    if m:
        return m.group(1).strip()
    i = text.find("apiVersion:")
    return text[i:].strip() if i != -1 else text


def _load_schema(cfg) -> str:
    parts = []
    for f in cfg.get("eval.author.schema_files", []) or []:
        p = cfg.resolve(f)
        if p.exists():
            parts.append(f"# {p.name}\n{p.read_text()}")
        else:
            print(f"  ! schema file missing: {p}")
    return "\n\n".join(parts)


def author_bench(cfg: Config, with_schema: bool = False, model: str = None,
                 tag: str = "", limit: int = 0) -> dict:
    from lo_ai.eval.verify import verify_yaml

    spec = dict(cfg["llm"]["conductor"])
    if model:
        spec["model"] = model
    llm = LLM(spec)

    intents = json.loads(cfg.resolve(cfg["eval"]["dataset"]).read_text())
    fmt = [it for it in intents if it.get("bucket") == "format"]
    if limit:
        fmt = fmt[:limit]

    schema = _load_schema(cfg) if with_schema else ""
    system = SYS + (f"\n\nAUTHORITATIVE lok8s SCHEMA (follow it exactly):\n{schema}"
                    if schema else "")

    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    label = f"author-{stamp}-{tag or ('schema' if with_schema else 'noschema')}"
    out_dir = cfg.resolve(cfg["eval"]["out_dir"]) / label
    out_dir.mkdir(parents=True, exist_ok=True)

    counts = {"pass": 0, "fail": 0, "error": 0, "skip": 0}
    records = []
    for i, it in enumerate(fmt):
        r = llm.chat([{"role": "system", "content": system},
                      {"role": "user", "content": it["intent"]}])
        y = extract_yaml(r["content"])
        try:
            status, log = verify_yaml(cfg, y, it.get("verify") or {})
        except Exception as e:
            status, log = "error", str(e)
        counts[status] += 1
        records.append({"intent_id": it["id"], "with_schema": with_schema,
                        "status": status, "yaml": y, "log": log[:600]})
        print(f"[{i+1}/{len(fmt)}] {it['id']:14} {status}", flush=True)

    decided = counts["pass"] + counts["fail"]
    summary = {"model": spec["model"], "with_schema": with_schema, **counts,
               "n": len(fmt),
               "pass_rate": round(counts["pass"] / decided, 3) if decided else None}
    (out_dir / "summary.json").write_text(json.dumps(summary, indent=2))
    (out_dir / "records.jsonl").write_text(
        "\n".join(json.dumps(r) for r in records))
    print(f"\nauthoring [{spec['model']}, schema={with_schema}]: "
          f"{counts['pass']}/{len(fmt)} pass  -> {out_dir}")
    return summary

"""Benchmark runner: route each intent under each injection strategy, N times.

Records the model's tool choice, route path, and any authored YAML so the scorer
can bucket failures into Routing / Format / Reasoning.
"""
from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime
from pathlib import Path

from lo_ai.config import Config
from lo_ai.llm import LLM
from lo_ai.mcp_client import fetch_tools
from lo_ai.tools import Option, ToolCatalog

DEFAULT_SYSTEM = """You are the lok8s assistant. The user wants to operate their \
Kubernetes cluster via the `lo` CLI. You are shown OPTIONS (each a tool or a \
group). Pick exactly ONE option whose name best matches the user's intent. If it \
is a group, you will choose again inside it. If the task requires authoring or \
modifying a cluster spec (a cluster.lok8s.yaml / addon / secret), put the \
COMPLETE YAML in "yaml" (otherwise leave it ""). Respond with ONE JSON object and \
nothing else:
{"choice": "<option name>", "arguments": {<tool args>}, "yaml": "<yaml or empty>", "reason": "<short>"}"""

MAX_DEPTH = 4


def render_options(options: list[Option]) -> str:
    lines = []
    for o in options:
        if o.kind == "tool":
            props = o.parameters.get("properties", {})
            req = set(o.parameters.get("required", []))
            args = ", ".join(
                f"{k}{'*' if k in req else ''}:{v.get('type', 'string')}"
                for k, v in props.items()
            ) or "none"
            lines.append(f"- {o.name} [tool] — {o.description} (args: {args})")
        else:
            lines.append(f"- {o.name} [group] — {o.description}")
    return "\n".join(lines)


def extract_json(text: str) -> dict | None:
    text = text.strip()
    m = re.search(r"```(?:json)?\s*(\{.*?\})\s*```", text, re.S)
    if m:
        text = m.group(1)
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass
    # balanced-brace scan for the first object
    start = text.find("{")
    while start != -1:
        depth = 0
        for i in range(start, len(text)):
            if text[i] == "{":
                depth += 1
            elif text[i] == "}":
                depth -= 1
                if depth == 0:
                    try:
                        return json.loads(text[start:i + 1])
                    except json.JSONDecodeError:
                        break
        start = text.find("{", start + 1)
    return None


def _to_function(o: Option) -> dict:
    params = o.parameters if o.kind == "tool" else {"type": "object", "properties": {}}
    return {"type": "function",
            "function": {"name": o.name, "description": o.description, "parameters": params}}


def ask_step(llm: LLM, system: str, intent: str, options: list[Option],
             path: list[str], tool_mode: str) -> dict:
    if tool_mode == "native":
        messages = [
            {"role": "system", "content": system},
            {"role": "user", "content": f"User request: {intent}\nPath: {path or '(top)'}"},
        ]
        r = llm.chat(messages, tools=[_to_function(o) for o in options],
                     tool_choice="required")
        if r["tool_calls"]:
            tc = r["tool_calls"][0]
            return {"choice": tc["name"], "arguments": tc.get("arguments", {}),
                    "yaml": "", "reason": "", "latency_ms": r["latency_ms"],
                    "content": json.dumps(tc), "error": None}
        return {"choice": None, "arguments": {}, "yaml": "", "reason": "",
                "latency_ms": r["latency_ms"], "content": r["content"],
                "error": "no tool_call returned"}

    # json mode (default)
    user = (f"User request: {intent}\n\nOPTIONS:\n{render_options(options)}\n\n"
            f"Path so far: {path or '(top)'}\nReturn the JSON object.")
    r = llm.chat([{"role": "system", "content": system},
                  {"role": "user", "content": user}])
    parsed = extract_json(r["content"]) or {}
    return {
        "choice": parsed.get("choice"),
        "arguments": parsed.get("arguments") or {},
        "yaml": parsed.get("yaml") or "",
        "reason": parsed.get("reason") or "",
        "latency_ms": r["latency_ms"],
        "content": r["content"][:2000],
        "error": None if parsed else "unparseable model output",
    }


def route(catalog: ToolCatalog, llm: LLM, intent: str, strategy: str,
          tool_mode: str, embedder, system: str) -> dict:
    path: list[str] = []
    steps: list[dict] = []
    chosen_tool = None
    yaml_out, args_out, err = "", {}, None
    for _ in range(MAX_DEPTH):
        options = catalog.present(strategy, query=intent, embedder=embedder, path=path)
        if not options:
            err = "no options at this step"
            break
        step = ask_step(llm, system, intent, options, path, tool_mode)
        steps.append(step)
        choice = step["choice"]
        if step["error"] and not choice:
            err = step["error"]
            break
        if catalog.is_tool(choice):
            chosen_tool = choice
            yaml_out, args_out = step["yaml"], step["arguments"]
            break
        group_names = {o.name for o in options if o.kind == "group"}
        if choice in group_names:
            path.append(choice)
            continue
        err = f"off-menu choice: {choice!r}"
        break
    return {
        "chosen_tool": chosen_tool,
        "route_path": path,
        "yaml": yaml_out,
        "arguments": args_out,
        "latency_ms": sum(s["latency_ms"] for s in steps),
        "steps": steps,
        "error": err,
    }


def run_bench(cfg: Config, tag: str = "", limit: int = 0) -> dict:
    from lo_ai.eval.scorer import score
    from lo_ai.embed import Embedder

    tools = fetch_tools(cfg)
    catalog = ToolCatalog(tools, cfg["injection"])
    llm = LLM(cfg["llm"]["conductor"])
    tool_mode = cfg.get("llm.conductor.tool_mode", "json")
    system = cfg.get("eval.system_prompt") or DEFAULT_SYSTEM
    embedder = (Embedder(cfg["embeddings"])
                if cfg.get("embeddings.enabled") else None)

    ds_path = cfg.resolve(cfg["eval"]["dataset"])
    dataset_sha = hashlib.sha256(ds_path.read_bytes()).hexdigest()[:12]
    intents = json.loads(ds_path.read_text())
    if limit:
        intents = intents[:limit]
    configs = cfg.get("eval.configs", ["raw", "hierarchy"])
    runs_per = int(cfg.get("eval.runs_per_intent", 5))

    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    name = f"run-{stamp}" + (f"-{tag}" if tag else "")
    out_dir = cfg.resolve(cfg["eval"]["out_dir"]) / name
    out_dir.mkdir(parents=True, exist_ok=True)

    total = len(configs) * len(intents) * runs_per
    done = 0
    records: list[dict] = []
    with open(out_dir / "runs.jsonl", "w") as fh:
        for strategy in configs:
            for it in intents:
                for run in range(runs_per):
                    try:
                        res = route(catalog, llm, it["intent"], strategy,
                                    tool_mode, embedder, system)
                        rec = {"intent_id": it["id"], "intent": it["intent"],
                               "bucket": it.get("bucket"), "config": strategy,
                               "run": run, "gold_tool": it.get("gold_tool"),
                               **res}
                    except Exception as e:  # never let one run kill the sweep
                        rec = {"intent_id": it["id"], "intent": it["intent"],
                               "bucket": it.get("bucket"), "config": strategy,
                               "run": run, "gold_tool": it.get("gold_tool"),
                               "chosen_tool": None, "yaml": "", "arguments": {},
                               "route_path": [], "latency_ms": 0, "steps": [],
                               "error": f"exception: {e}"}
                    records.append(rec)
                    fh.write(json.dumps(rec) + "\n")
                    fh.flush()
                    done += 1
                print(f"[{done}/{total}] {strategy:10} {it['id']}", flush=True)

    summary = score(cfg, intents, records)
    (out_dir / "summary.json").write_text(json.dumps(summary, indent=2))
    (out_dir / "meta.json").write_text(json.dumps({
        "stamp": stamp, "configs": configs, "runs_per_intent": runs_per,
        "n_tools": len(tools), "model": llm.model, "tool_mode": tool_mode,
        "dataset_sha": dataset_sha, "n_intents": len(intents),
        "think": cfg.get("llm.conductor.think"), "api": cfg.get("llm.conductor.api"),
    }, indent=2))
    from lo_ai.eval.ledger import build_ledger
    build_ledger(cfg)
    print(f"\nresults -> {out_dir}")
    return summary

"""Multi-step agentic debug eval — the "watch + debug the cluster" loop.

Gives the model a debug situation and a READ-ONLY tool menu, mocks each tool's
output, and lets it CHAIN calls (run -> read mocked output -> decide next) until
it states a cause+fix or hits max_steps. Scores whether it reaches the gold
diagnosis via a sensible path. Mocked tools = deterministic, no live broken
cluster needed. This is the dimension the single-step routing bench can't cover.
"""
from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime

from lo_ai.config import Config
from lo_ai.llm import LLM
from lo_ai.mcp_client import fetch_tools
from lo_ai.tools import ToolCatalog

SYS = """You are the lok8s debugging assistant. The user reports a problem; you
investigate by running READ-ONLY lo tools, one at a time. Each turn respond with
ONE JSON object and nothing else:
{"tool": "<tool name or null>", "args": {}, "done": false, "answer": ""}
You'll see each tool's output before your next turn. When you know the root cause,
set "done": true and put the cause AND the exact fix command in "answer"."""


def _extract(text: str) -> dict:
    m = re.search(r"\{.*\}", text, re.S)
    if not m:
        return {}
    try:
        return json.loads(m.group(0))
    except Exception:
        return {}


def agent_bench(cfg: Config, model: str = None, tag: str = "", limit: int = 0) -> dict:
    spec = dict(cfg["llm"]["conductor"])
    if model:
        spec["model"] = model
    llm = LLM(spec)

    catalog = ToolCatalog(fetch_tools(cfg), cfg["injection"])
    readonly = [n for n in catalog._dieted() if catalog.tier(n) == "readonly"]
    menu = "\n".join(
        f"- {n}: {(catalog.tools[n].description.splitlines() or [''])[0]}"
        for n in readonly)

    ds_path = cfg.resolve(cfg.get("eval.agent_dataset", "lo_ai/data/agent.json"))
    dataset_sha = hashlib.sha256(ds_path.read_bytes()).hexdigest()[:12]
    scenarios = json.loads(ds_path.read_text())
    if limit:
        scenarios = scenarios[:limit]

    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    out_dir = cfg.resolve(cfg["eval"]["out_dir"]) / f"agent-{stamp}-{tag or spec['model']}".replace(":", "-")
    out_dir.mkdir(parents=True, exist_ok=True)

    solved_n, records = 0, []
    for sc in scenarios:
        msgs = [{"role": "system", "content": SYS + "\n\nREAD-ONLY TOOLS:\n" + menu},
                {"role": "user", "content": sc["intent"]}]
        path, answer = [], ""
        for _ in range(sc.get("max_steps", 5)):
            j = _extract(llm.chat(msgs)["content"])
            if j.get("done"):
                answer = j.get("answer", "") or ""
                break
            tool = j.get("tool")
            if not tool:
                answer = j.get("answer", "") or ""
                break
            path.append(tool)
            out = sc.get("mocks", {}).get(tool, f"(no such tool / no output: {tool})")
            msgs.append({"role": "assistant", "content": json.dumps(j)})
            msgs.append({"role": "user", "content": f"Output of {tool}:\n{out}"})

        kws = sc.get("gold_answer_keywords", [])
        reached = bool(answer) and all(k.lower() in answer.lower() for k in kws)
        gp = sc.get("gold_path", [])
        path_ok = (not gp) or (gp[0] in path)
        solved = reached and path_ok
        solved_n += 1 if solved else 0
        records.append({"id": sc["id"], "solved": solved, "reached": reached,
                        "path_ok": path_ok, "path": path, "steps": len(path),
                        "answer": answer[:300]})
        print(f"  {sc['id']:16} {'solved' if solved else 'miss':6} "
              f"path={path} reached={reached}", flush=True)

    summary = {"model": spec["model"], "agent": True, "n": len(scenarios),
               "solved": solved_n, "think": spec.get("think"), "dataset_sha": dataset_sha,
               "solve_rate": round(solved_n / len(scenarios), 3) if scenarios else None}
    (out_dir / "summary.json").write_text(json.dumps(summary, indent=2))
    (out_dir / "records.jsonl").write_text("\n".join(json.dumps(r) for r in records))
    print(f"\nagentic debug [{spec['model']}]: {solved_n}/{len(scenarios)} solved "
          f"-> {out_dir}")
    from lo_ai.eval.ledger import build_ledger
    build_ledger(cfg)
    return summary

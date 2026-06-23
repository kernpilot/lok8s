"""Score benchmark runs into the three buckets and print the A/B comparison.

  A Routing  — chosen_tool == gold_tool  (which fix? router)
  B Format   — authored YAML passes `lo lint`/`lo build`  (which fix? format LoRA)
  C Reasoning— right sequence/plan  (which fix? the Markdown decision trees)
"""
from __future__ import annotations

import statistics
from collections import defaultdict


def _mean(xs):
    return round(statistics.mean(xs), 3) if xs else None


def _routing(records):
    """accuracy + stability over records that have a gold_tool."""
    have_gold = [r for r in records if r.get("gold_tool")]
    if not have_gold:
        return {"n": 0, "acc": None, "stability": None}
    acc = _mean([1.0 if r["chosen_tool"] == r["gold_tool"] else 0.0
                 for r in have_gold])
    by_intent = defaultdict(list)
    for r in have_gold:
        by_intent[r["intent_id"]].append(r["chosen_tool"])
    stabilities = []
    for choices in by_intent.values():
        modal = max(set(choices), key=choices.count)
        stabilities.append(choices.count(modal) / len(choices))
    return {"n": len(have_gold), "acc": acc, "stability": _mean(stabilities)}


def _format(cfg, records, intents_by_id):
    from lo_ai.eval.verify import verify_yaml
    recs = [r for r in records if r.get("bucket") == "format"]
    counts = {"pass": 0, "fail": 0, "error": 0, "skip": 0}
    cache: dict[str, str] = {}
    failures = []
    for r in recs:
        y = r.get("yaml") or ""
        vspec = intents_by_id.get(r["intent_id"], {}).get("verify") or {}
        key = f"{r['intent_id']}::{hash(y)}"
        if key not in cache:
            try:
                status, log = verify_yaml(cfg, y, vspec)
            except Exception as e:
                status, log = "error", str(e)
            cache[key] = status
            if status in ("fail", "error"):
                failures.append({"intent_id": r["intent_id"], "config": r["config"],
                                 "status": status, "log": log[:400]})
        counts[cache[key]] += 1
    n = len(recs)
    decided = counts["pass"] + counts["fail"]
    return {
        "n": n, **counts,
        "pass_rate": round(counts["pass"] / decided, 3) if decided else None,
        "failures": failures[:10],
    }


def _reasoning(cfg, records):
    recs = [r for r in records if r.get("bucket") == "reasoning"]
    judge_model = cfg.get("llm.judge.model") or ""
    if not recs:
        return {"n": 0, "note": "no reasoning intents"}
    if not judge_model:
        return {"n": len(recs), "judged": False,
                "note": "no judge model configured — traces recorded for manual review"}
    # Auto-judge with a DIFFERENT model (never self-judge).
    from lo_ai.llm import LLM
    judge = LLM(cfg["llm"]["judge"])
    passed = 0
    for r in recs:
        prompt = (f"User intent: {r['intent']}\nThe assistant chose tool "
                  f"{r['chosen_tool']} via path {r.get('route_path')}.\n"
                  f"Is this a correct first step? Answer strictly 'YES' or 'NO'.")
        try:
            out = judge.chat([{"role": "user", "content": prompt}])["content"]
            if out.strip().upper().startswith("YES"):
                passed += 1
        except Exception:
            pass
    return {"n": len(recs), "judged": True, "judge_model": judge_model,
            "pass_rate": round(passed / len(recs), 3)}


def score(cfg, intents, records) -> dict:
    intents_by_id = {it["id"]: it for it in intents}
    by_cfg = defaultdict(list)
    for r in records:
        by_cfg[r["config"]].append(r)

    summary = {"configs": {}}
    for strategy, recs in by_cfg.items():
        errs = [r for r in recs if r.get("error")]
        summary["configs"][strategy] = {
            "routing": _routing(recs),
            "format": _format(cfg, recs, intents_by_id),
            "reasoning": _reasoning(cfg, recs),
            "latency_ms_mean": _mean([r["latency_ms"] for r in recs if r["latency_ms"]]),
            "steps_mean": _mean([len(r.get("steps", [])) for r in recs]),
            "error_rate": round(len(errs) / len(recs), 3) if recs else None,
            "routing_failures": [
                {"intent": r["intent"], "chose": r["chosen_tool"],
                 "gold": r["gold_tool"], "config": strategy, "err": r.get("error")}
                for r in recs
                if r.get("gold_tool") and r["chosen_tool"] != r["gold_tool"]
            ][:10],
        }
    _print_table(summary)
    return summary


def _print_table(summary: dict) -> None:
    print("\n=== A/B comparison ==================================================")
    hdr = f"{'config':12} {'route_acc':>9} {'stability':>9} {'fmt_pass':>8} {'reason':>7} {'lat_ms':>8} {'steps':>5} {'err':>5}"
    print(hdr)
    print("-" * len(hdr))
    for strategy, s in summary["configs"].items():
        rt, fm, rs = s["routing"], s["format"], s["reasoning"]
        print(f"{strategy:12} "
              f"{_fmt(rt['acc']):>9} {_fmt(rt['stability']):>9} "
              f"{_fmt(fm.get('pass_rate')):>8} {_fmt(rs.get('pass_rate')):>7} "
              f"{str(s['latency_ms_mean'] or '-'):>8} "
              f"{str(s['steps_mean'] or '-'):>5} {_fmt(s['error_rate']):>5}")
    print("====================================================================")
    print("route_acc -> build the router · fmt_pass -> train the format LoRA · "
          "reason -> improve the decision-tree skills")


def _fmt(x):
    return f"{x:.2f}" if isinstance(x, (int, float)) else "-"

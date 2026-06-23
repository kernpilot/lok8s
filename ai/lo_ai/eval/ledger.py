"""Benchmark history ledger.

Keeps every run's headline metrics so future tweaks can be compared against a
baseline. Rebuilds results/leaderboard.jsonl from all on-disk summaries
(idempotent — safe to run anytime) and prints a comparison table.

Comparability rule: a route_acc is only meaningful against the SAME dataset, so
every row carries dataset_sha. Compare within a dataset_sha; a changed hash means
the test set moved and the numbers aren't directly comparable.
"""
from __future__ import annotations

import glob
import json
from pathlib import Path


def _load(p):
    try:
        return json.load(open(p))
    except Exception:
        return {}


def collect(results_dir: Path) -> list[dict]:
    rows = []
    for sj in sorted(glob.glob(str(results_dir / "*" / "summary.json"))):
        d = Path(sj).parent
        s = _load(sj)
        meta = _load(d / "meta.json")
        name = d.name
        if "configs" in s:                      # routing run
            for cfg, c in s["configs"].items():
                rt, fm = c.get("routing", {}), c.get("format", {})
                rows.append({
                    "run": name, "kind": "routing", "model": meta.get("model"),
                    "config": cfg, "route_acc": rt.get("acc"),
                    "stability": rt.get("stability"), "format_pass": fm.get("pass_rate"),
                    "lat_ms": c.get("latency_ms_mean"), "steps": c.get("steps_mean"),
                    "err": c.get("error_rate"), "n": rt.get("n"),
                    "n_tools": meta.get("n_tools"), "think": meta.get("think"),
                    "dataset_sha": meta.get("dataset_sha", "?"),
                })
        elif "with_schema" in s:                 # authoring run
            rows.append({
                "run": name, "kind": "authoring", "model": s.get("model"),
                "config": "schema" if s.get("with_schema") else "noschema",
                "route_acc": None, "format_pass": s.get("pass_rate"),
                "passes": s.get("pass"), "n": s.get("n"), "think": s.get("think"),
                "dataset_sha": s.get("dataset_sha", "?"),
            })
    return rows


def build_ledger(cfg) -> list[dict]:
    rd = cfg.resolve(cfg["eval"]["out_dir"])
    rd.mkdir(parents=True, exist_ok=True)
    rows = collect(rd)
    (rd / "leaderboard.jsonl").write_text(
        "\n".join(json.dumps(r) for r in rows) + ("\n" if rows else ""))
    return rows


def _f(x):
    return f"{x:.3f}" if isinstance(x, (int, float)) else "-"


def _mode(t):
    return "/think" if t is True else ("/fast" if t is False else "")


def _model(r):
    return (str(r["model"]) + _mode(r.get("think")))[:23]


def print_ledger(rows: list[dict]) -> None:
    routing = [r for r in rows if r["kind"] == "routing"]
    auth = [r for r in rows if r["kind"] == "authoring"]
    if routing:
        print("\n== ROUTING (by dataset, then acc) ==")
        h = f"{'dataset':10}{'model':24}{'cfg':10}{'acc':>7}{'fmt':>6}{'lat_ms':>8}{'err':>6}{'n':>4}"
        print(h); print("-" * len(h))
        for r in sorted(routing, key=lambda x: (str(x["dataset_sha"]), -(x["route_acc"] or 0))):
            print(f"{str(r['dataset_sha'])[:9]:10}{_model(r):24}{r['config']:10}"
                  f"{_f(r['route_acc']):>7}{_f(r['format_pass']):>6}"
                  f"{str(round(r['lat_ms'])) if r['lat_ms'] else '-':>8}{_f(r['err']):>6}{str(r['n']):>4}")
    if auth:
        print("\n== AUTHORING (by dataset, then pass) ==")
        h = f"{'dataset':10}{'model':24}{'mode':10}{'pass':>7}{'n':>4}"
        print(h); print("-" * len(h))
        for r in sorted(auth, key=lambda x: (str(x["dataset_sha"]), -(x["format_pass"] or 0))):
            print(f"{str(r['dataset_sha'])[:9]:10}{_model(r):24}{r['config']:10}"
                  f"{_f(r['format_pass']):>7}{str(r['n']):>4}")
    print(f"\n{len(rows)} rows. Compare within a dataset hash only.")

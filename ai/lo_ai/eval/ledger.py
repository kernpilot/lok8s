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
                    "vram_gb": meta.get("vram_gb"), "size_gb": meta.get("size_gb"),
                    "gpu_frac": meta.get("gpu_frac"),
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


def _ranking(rows):
    """Per-model deployable ranking (think-off) with VRAM + fit — the shareable
    model-selection guide for any card, not just this box."""
    routing = [r for r in rows if r["kind"] == "routing" and r["route_acc"] is not None]
    if not routing:
        return
    ds = max(routing, key=lambda r: r.get("run", "")).get("dataset_sha", "?")  # latest run's dataset
    pool = [r for r in routing if r["dataset_sha"] == ds and r.get("think") is not True
            and (r.get("err") or 0) < 0.99]   # drop all-errored runs (e.g. failed pull)
    best = {}
    for r in pool:
        k = r["model"]
        s = (1 if r["config"] == "diet" else 0, r["route_acc"])
        if k not in best or s > best[k][0]:
            best[k] = (s, r)
    authbest = {}
    for r in rows:
        if r["kind"] == "authoring" and r.get("config") == "schema" and r.get("think") is not True:
            m = r["model"]
            if m not in authbest or (r["format_pass"] or 0) > (authbest[m] or 0):
                authbest[m] = r["format_pass"]
    print(f"\n== RANKING (dataset {ds[:9]}, think-off, by routing acc) ==")
    h = f"{'model':20}{'route':>7}{'auth+sch':>9}{'lat_ms':>8}{'vram_gb':>8}{'fit16':>6}"
    print(h); print("-" * len(h))
    for m, (s, r) in sorted(best.items(), key=lambda x: -x[1][0][1]):
        gf = r.get("gpu_frac")
        fit = "ok" if (gf and gf >= 0.99) else ("?" if gf is None else f"{int(gf * 100)}%")
        print(f"{m[:20]:20}{_f(r['route_acc']):>7}{_f(authbest.get(m)):>9}"
              f"{str(round(r['lat_ms'])) if r['lat_ms'] else '-':>8}"
              f"{str(r.get('vram_gb') or '-'):>8}{fit:>6}")


def print_ledger(rows: list[dict]) -> None:
    _ranking(rows)
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

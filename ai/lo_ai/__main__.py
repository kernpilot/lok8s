"""lok8s local-AI harness CLI.

    python -m lo_ai dump-tools         # snapshot the lo MCP surface
    python -m lo_ai route -q "..."     # show what the injector surfaces (no model)
    python -m lo_ai bench              # run the A/B benchmark
    python -m lo_ai synth -i spec.md   # teacher -> (intent, yaml) pairs
    python -m lo_ai verify             # hard-filter pairs through lo lint/build
    python -m lo_ai train              # QLoRA the format adapter
"""
from __future__ import annotations

import argparse
import json
import sys

from lo_ai.config import load_config


def _catalog(cfg):
    from lo_ai.mcp_client import fetch_tools
    from lo_ai.tools import ToolCatalog
    return ToolCatalog(fetch_tools(cfg), cfg["injection"])


def cmd_dump_tools(cfg, args):
    from lo_ai.mcp_client import fetch_tools
    tools = fetch_tools(cfg)
    out = cfg.resolve(args.out)
    out.write_text(json.dumps(tools, indent=2))
    cat = _catalog(cfg)
    groups = sorted(cat._dynamic_groups())
    ro = [t["name"] for t in tools if cat.tier(t["name"]) == "readonly"]
    print(f"{len(tools)} tools -> {out}")
    print(f"groups: {', '.join(groups)}")
    print(f"readonly: {len(ro)} | dieted surface: {len(cat._dieted())} | "
          f"denied: {len(cat.deny)}")


def cmd_route(cfg, args):
    cat = _catalog(cfg)
    embedder = None
    if cfg.get("embeddings.enabled"):
        from lo_ai.embed import Embedder
        embedder = Embedder(cfg["embeddings"])
    strat = cfg.get("injection.strategy", "hierarchy")
    options = cat.present(strat, query=args.query, embedder=embedder, path=[])
    print(f"strategy={strat}  query={args.query!r}  -> {len(options)} option(s) "
          f"at top level (max_tools={cat.max_tools}):\n")
    for o in options:
        print(f"  {o.name} [{o.kind}] — {o.description}")


def cmd_bench(cfg, args):
    from lo_ai.eval.runner import run_bench
    if args.model:
        cfg["llm"]["conductor"]["model"] = args.model
    if args.runs:
        cfg.raw["eval"]["runs_per_intent"] = args.runs
    if args.configs:
        cfg.raw["eval"]["configs"] = [c.strip() for c in args.configs.split(",")]
    run_bench(cfg, tag=args.tag, limit=args.limit or 0)


def cmd_synth(cfg, args):
    from lo_ai.train.synth import generate_pairs
    spec = sys.stdin.read() if args.input == "-" else cfg.resolve(args.input).read_text()
    generate_pairs(cfg, spec)


def cmd_verify(cfg, args):
    from lo_ai.eval.verify import verify_yaml
    raw = cfg.resolve(cfg.get("train.generate.out", "./data/pairs.raw.jsonl"))
    out = cfg.resolve(cfg.get("train.verify.out", "./data/pairs.verified.jsonl"))
    vspec = {"domain": cfg.get("train.verify.domain", "eval-tmp"),
             "file": cfg.get("train.verify.file", "cluster.lok8s.yaml")}
    counts = {"pass": 0, "fail": 0, "error": 0, "skip": 0}
    out.parent.mkdir(parents=True, exist_ok=True)
    with open(raw) as fin, open(out, "w") as fout:
        for line in fin:
            line = line.strip()
            if not line:
                continue
            pair = json.loads(line)
            status, _ = verify_yaml(cfg, pair.get("yaml", ""), vspec)
            counts[status] += 1
            if status == "pass":
                fout.write(json.dumps(pair) + "\n")
    print(f"verify: {counts} -> kept {counts['pass']} in {out}")


def cmd_train(cfg, args):
    from lo_ai.train.qlora import train
    train(cfg)


def main(argv=None):
    p = argparse.ArgumentParser(prog="lo_ai")
    p.add_argument("--config", default="config.yaml", help="path to config.yaml")
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("dump-tools"); s.add_argument("--out", default="tools.json")
    s.set_defaults(fn=cmd_dump_tools)
    s = sub.add_parser("route"); s.add_argument("-q", "--query", required=True)
    s.set_defaults(fn=cmd_route)
    b = sub.add_parser("bench")
    b.add_argument("--model", help="override llm.conductor.model")
    b.add_argument("--runs", type=int, help="override eval.runs_per_intent")
    b.add_argument("--configs", help="comma list, override eval.configs")
    b.add_argument("--tag", default="", help="label appended to the run dir")
    b.add_argument("--limit", type=int, default=0, help="cap to first N intents")
    b.set_defaults(fn=cmd_bench)
    s = sub.add_parser("synth"); s.add_argument("-i", "--input", required=True,
                                                help="schema/feature file ('-' for stdin)")
    s.set_defaults(fn=cmd_synth)
    sub.add_parser("verify").set_defaults(fn=cmd_verify)
    sub.add_parser("train").set_defaults(fn=cmd_train)

    args = p.parse_args(argv)
    cfg = load_config(args.config)
    args.fn(cfg, args)


if __name__ == "__main__":
    main()

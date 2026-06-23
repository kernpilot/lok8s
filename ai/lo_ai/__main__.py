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


def _apply_llm_overrides(cfg, args):
    c = cfg["llm"]["conductor"]
    if getattr(args, "model", None):
        c["model"] = args.model
    if getattr(args, "api", None):
        c["api"] = args.api
    if getattr(args, "num_ctx", None):
        c["num_ctx"] = args.num_ctx
    if getattr(args, "max_tokens", None):
        c["max_tokens"] = args.max_tokens
    if getattr(args, "think", None) in ("on", "off"):
        c["think"] = (args.think == "on")


def _add_llm_flags(p):
    p.add_argument("--api", choices=["openai", "ollama"])
    p.add_argument("--think", choices=["on", "off"], help="reasoning toggle (ollama api)")
    p.add_argument("--num-ctx", dest="num_ctx", type=int)
    p.add_argument("--max-tokens", dest="max_tokens", type=int)


def cmd_bench(cfg, args):
    from lo_ai.eval.runner import run_bench
    _apply_llm_overrides(cfg, args)
    if args.runs:
        cfg.raw["eval"]["runs_per_intent"] = args.runs
    if args.configs:
        cfg.raw["eval"]["configs"] = [c.strip() for c in args.configs.split(",")]
    run_bench(cfg, tag=args.tag, limit=args.limit or 0)


def cmd_authoreval(cfg, args):
    from lo_ai.eval.author import author_bench
    _apply_llm_overrides(cfg, args)
    author_bench(cfg, with_schema=args.schema, model=None,
                 tag=args.tag, limit=args.limit or 0)


def cmd_addoneval(cfg, args):
    from lo_ai.eval.addon import addon_bench
    _apply_llm_overrides(cfg, args)
    addon_bench(cfg, with_schema=args.schema, model=None,
                tag=args.tag, limit=args.limit or 0)


def cmd_agenteval(cfg, args):
    from lo_ai.eval.agent import agent_bench
    _apply_llm_overrides(cfg, args)
    agent_bench(cfg, model=None, tag=args.tag, limit=args.limit or 0)


def cmd_safetyeval(cfg, args):
    from lo_ai.eval.safety import safety_bench
    _apply_llm_overrides(cfg, args)
    safety_bench(cfg, model=None, tag=args.tag, limit=args.limit or 0)


def cmd_chat(cfg, args):
    from lo_ai.chat.app import run
    run(cfg, prompt=args.prompt, model=args.model, posture=args.posture)


def cmd_ledger(cfg, args):
    from lo_ai.eval.ledger import build_ledger, print_ledger
    print_ledger(build_ledger(cfg))


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
    _add_llm_flags(b)
    b.set_defaults(fn=cmd_bench)

    a = sub.add_parser("authoreval")
    a.add_argument("--schema", dest="schema", action="store_true",
                   help="inject the lok8s schema/skill into the authoring prompt")
    a.add_argument("--no-schema", dest="schema", action="store_false")
    a.set_defaults(schema=False)
    a.add_argument("--model")
    a.add_argument("--tag", default="")
    a.add_argument("--limit", type=int, default=0)
    _add_llm_flags(a)
    a.set_defaults(fn=cmd_authoreval)

    ad = sub.add_parser("addoneval")
    ad.add_argument("--schema", dest="schema", action="store_true")
    ad.add_argument("--no-schema", dest="schema", action="store_false")
    ad.set_defaults(schema=False)
    ad.add_argument("--model")
    ad.add_argument("--tag", default="")
    ad.add_argument("--limit", type=int, default=0)
    _add_llm_flags(ad)
    ad.set_defaults(fn=cmd_addoneval)

    ae = sub.add_parser("agenteval")
    ae.add_argument("--model")
    ae.add_argument("--tag", default="")
    ae.add_argument("--limit", type=int, default=0)
    _add_llm_flags(ae)
    ae.set_defaults(fn=cmd_agenteval)

    se = sub.add_parser("safetyeval")
    se.add_argument("--model")
    se.add_argument("--tag", default="")
    se.add_argument("--limit", type=int, default=0)
    _add_llm_flags(se)
    se.set_defaults(fn=cmd_safetyeval)

    ch = sub.add_parser("chat")
    ch.add_argument("-p", "--prompt", help="single-shot, non-interactive")
    ch.add_argument("--model", help="conductor backend key (see chat.backends)")
    ch.add_argument("--posture", choices=["read-only", "confirm", "open"])
    ch.set_defaults(fn=cmd_chat)

    sub.add_parser("ledger").set_defaults(fn=cmd_ledger)
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

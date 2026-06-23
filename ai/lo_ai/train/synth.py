"""Synthetic data generation (the 'teacher').

Given a schema/feature description, ask a strong current model (Sonnet 4.6 / Opus
4.8, or a local 70B) for many (intent, YAML) pairs. These are then HARD-FILTERED
by `lo lint`/`lo build` (see eval.verify / the `verify` command) before training,
so the LoRA only ever learns YAML that actually compiles.
"""
from __future__ import annotations

import json
import os
import urllib.request

PROMPT = """You generate training data for a tool that writes lok8s \
`cluster.lok8s.yaml` files. Given the schema/feature below, produce {n} DIVERSE, \
realistic (user_request, yaml) pairs: a natural-language request a user might \
type, and the COMPLETE, valid YAML that satisfies it.

Output JSONL — one JSON object per line, no prose, no code fences:
{{"intent": "<user request>", "yaml": "<complete yaml>"}}

SCHEMA / FEATURE:
{spec}"""


def _teacher_complete(cfg, prompt: str) -> str:
    t = cfg["train"]["teacher"]
    key = os.environ.get(t.get("api_key_env", "ANTHROPIC_API_KEY"), "")
    base = t["base_url"].rstrip("/")
    timeout = float(t.get("request_timeout", 180))

    if t.get("style") == "openai":
        url = f"{base}/chat/completions"
        body = {"model": t["model"], "max_tokens": 8192,
                "messages": [{"role": "user", "content": prompt}]}
        headers = {"Content-Type": "application/json"}
        if key:
            headers["Authorization"] = f"Bearer {key}"
    else:  # anthropic
        url = f"{base}/messages"
        body = {"model": t["model"], "max_tokens": 8192,
                "messages": [{"role": "user", "content": prompt}]}
        headers = {"Content-Type": "application/json",
                   "anthropic-version": "2023-06-01"}
        if key:
            headers["x-api-key"] = key

    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 method="POST")
    for k, v in headers.items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        payload = json.loads(resp.read().decode())
    if t.get("style") == "openai":
        return payload["choices"][0]["message"]["content"]
    return "".join(b.get("text", "") for b in payload.get("content", []))


def generate_pairs(cfg, spec_text: str) -> int:
    n = int(cfg.get("train.generate.per_schema", 100))
    out = cfg.resolve(cfg.get("train.generate.out", "./data/pairs.raw.jsonl"))
    out.parent.mkdir(parents=True, exist_ok=True)

    text = _teacher_complete(cfg, PROMPT.format(n=n, spec=spec_text))
    written = 0
    with open(out, "a") as fh:
        for line in text.splitlines():
            line = line.strip().strip("`")
            if not line.startswith("{"):
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                continue
            if "intent" in obj and "yaml" in obj:
                fh.write(json.dumps({"intent": obj["intent"],
                                     "yaml": obj["yaml"]}) + "\n")
                written += 1
    print(f"wrote {written} raw pairs -> {out}")
    return written

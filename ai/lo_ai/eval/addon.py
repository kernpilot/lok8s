"""Addon/chart authoring eval — "add the harbor chart".

Does the model produce a lok8s chart addon that conforms to the framework
pattern: a khelm ChartRenderer chart.yaml + a kustomization.yaml with
generators/includeSelectors:false/the three lok8s.dev labels + values.yaml that
uses ${LOK8S_SPEC_CLUSTER_DOMAIN} for any host. This is an OFFLINE, deterministic
pattern-conformance check (no chart fetch needed) — a faithful "did it follow our
chart pattern" rather than "did a remote chart render".
"""
from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime

import yaml

from lo_ai.config import Config
from lo_ai.llm import LLM

SYS = """You are a lok8s addon author. Add a new addon for the requested Helm chart,
following the lok8s addon pattern EXACTLY. Output ONLY the files, each as:
=== <filename> ===
<file content>

Required: chart.yaml (a khelm ChartRenderer), kustomization.yaml, values.yaml, and
namespace.yaml if the addon owns its namespace. Any external hostname MUST be
written as <name>.${LOK8S_SPEC_CLUSTER_DOMAIN}."""

FILE_RE = re.compile(r"^===\s*([\w.\-/]+)\s*===\s*$", re.M)


def extract_files(text: str) -> dict:
    files, parts = {}, FILE_RE.split(text)
    for i in range(1, len(parts) - 1, 2):
        content = parts[i + 1].strip()
        content = re.sub(r"^```[\w]*\n", "", content)
        content = re.sub(r"\n```$", "", content).strip()
        # normalize to basename: 'Chart.yaml' and '.lok8s/addons/x/chart.yaml' -> 'chart.yaml'
        files[parts[i].strip().lower().rsplit("/", 1)[-1]] = content
    return files


def _y(s):
    try:
        return yaml.safe_load(s)
    except Exception:
        return None


def verify_addon(files: dict, sc: dict) -> tuple[bool, list]:
    reasons = []
    chart, kust = _y(files.get("chart.yaml", "")), _y(files.get("kustomization.yaml", ""))
    if not isinstance(chart, dict):
        reasons.append("chart.yaml missing/invalid")
    else:
        if chart.get("apiVersion") != "khelm.mgoltzsche.github.com/v2":
            reasons.append("chart.yaml apiVersion != khelm v2")
        if chart.get("kind") != "ChartRenderer":
            reasons.append("chart.yaml kind != ChartRenderer")
        if not chart.get("chart"):
            reasons.append("chart.yaml missing chart")
        if not chart.get("version"):
            reasons.append("chart.yaml missing pinned version")
        if not (chart.get("repository") or str(chart.get("chart", "")).startswith(".")):
            reasons.append("chart.yaml missing repository (or local chart path)")
    if not isinstance(kust, dict):
        reasons.append("kustomization.yaml missing/invalid")
    else:
        if "chart.yaml" not in (kust.get("generators") or []):
            reasons.append("kustomization generators missing chart.yaml")
        inc_ok, lab = False, {}
        for l in (kust.get("labels") or []):
            if l.get("includeSelectors") is False:
                inc_ok = True
            lab.update(l.get("pairs") or {})
        if not inc_ok:
            reasons.append("kustomization missing includeSelectors: false")
        for k in ("lok8s.dev/name", "lok8s.dev/type", "lok8s.dev/category"):
            if k not in lab:
                reasons.append(f"missing label {k}")
        if not kust.get("namespace"):
            reasons.append("kustomization missing namespace")
    if sc.get("needs_host") and "${LOK8S_SPEC_CLUSTER_DOMAIN}" not in files.get("values.yaml", ""):
        reasons.append("host not written as <name>.${LOK8S_SPEC_CLUSTER_DOMAIN}")
    return (not reasons, reasons)


def addon_bench(cfg: Config, with_schema: bool = False, model: str = None,
                tag: str = "", limit: int = 0) -> dict:
    spec = dict(cfg["llm"]["conductor"])
    if model:
        spec["model"] = model
    llm = LLM(spec)

    ds_path = cfg.resolve(cfg.get("eval.addon_dataset", "lo_ai/data/addons.json"))
    dataset_sha = hashlib.sha256(ds_path.read_bytes()).hexdigest()[:12]
    scenarios = json.loads(ds_path.read_text())
    if limit:
        scenarios = scenarios[:limit]

    schema = ""
    if with_schema:
        for f in cfg.get("eval.addon_schema_files", []) or []:
            p = cfg.resolve(f)
            if p.exists():
                schema += f"\n# {p.name}\n{p.read_text()}"
    system = SYS + (f"\n\nAUTHORITATIVE lok8s ADDON GUIDE:\n{schema}" if schema else "")

    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    label = f"addon-{stamp}-{tag or ('schema' if with_schema else 'noschema')}"
    out_dir = cfg.resolve(cfg["eval"]["out_dir"]) / label
    out_dir.mkdir(parents=True, exist_ok=True)

    counts, records = {"pass": 0, "fail": 0}, []
    for sc in scenarios:
        r = llm.chat([{"role": "system", "content": system},
                      {"role": "user", "content": sc["intent"]}])
        files = extract_files(r["content"])
        ok, reasons = verify_addon(files, sc)
        counts["pass" if ok else "fail"] += 1
        records.append({"id": sc["id"], "ok": ok, "reasons": reasons,
                        "files": list(files)})
        print(f"  {sc['id']:18} {'pass' if ok else 'fail':4} {'; '.join(reasons[:2])}",
              flush=True)

    summary = {"model": spec["model"], "with_schema": with_schema, "addon": True,
               "n": len(scenarios), "dataset_sha": dataset_sha, "think": spec.get("think"),
               **counts,
               "pass_rate": round(counts["pass"] / len(scenarios), 3) if scenarios else None}
    (out_dir / "summary.json").write_text(json.dumps(summary, indent=2))
    (out_dir / "records.jsonl").write_text("\n".join(json.dumps(r) for r in records))
    print(f"\naddon authoring [{spec['model']}, schema={with_schema}]: "
          f"{counts['pass']}/{len(scenarios)} conform  -> {out_dir}")
    from lo_ai.eval.ledger import build_ledger
    build_ledger(cfg)
    return summary

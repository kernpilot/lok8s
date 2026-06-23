"""Bucket-B hard verifier: does the model's YAML survive `lo lint` (+ `lo build`)?

Runs the configured verify commands against the generated YAML in a throwaway
clusters/ dir, using lok8s' PATH_* env so nothing in the real project is touched.
Returns ("pass" | "fail" | "error" | "skip", log).

Note: needs a real lok8s project (cfg.lo.cwd) with its toolchain installed
(`b install`). If `lo` isn't runnable it returns "skip" rather than failing the
whole benchmark.
"""
from __future__ import annotations

import os
import subprocess
import tempfile
from pathlib import Path


def _env_for(cfg) -> dict:
    project = cfg.resolve(cfg.get("lo.cwd", "."))
    env = os.environ.copy()
    extra = cfg.get("lo.extra_path") or []
    if extra:
        prefix = os.pathsep.join(str((project / p).resolve()) for p in extra)
        env["PATH"] = prefix + os.pathsep + env.get("PATH", "")
    env.setdefault("PATH_BASE", str(project))
    env.setdefault("PATH_BIN", str(project / ".bin"))
    env.setdefault("PATH_LOK8S", str(project / ".lok8s"))
    env.setdefault("PATH_SECRETS", str(project / ".secrets"))
    env.setdefault("KUSTOMIZE_PLUGIN_HOME", str(project / ".kustomize"))
    env.update({k: str(v) for k, v in (cfg.get("lo.env") or {}).items()})
    return env


def verify_yaml(cfg, yaml_text: str, vspec: dict) -> tuple[str, str]:
    if not yaml_text.strip():
        return "fail", "empty yaml"

    project = cfg.resolve(cfg.get("lo.cwd", "."))
    domain = vspec.get("domain", "eval-tmp")
    rel_file = vspec.get("file", "cluster.lok8s.yaml")
    commands = vspec.get("commands") or cfg.get("train.verify.commands") or [
        ["./.lok8s/lo", "lint"]
    ]
    env = _env_for(cfg)

    with tempfile.TemporaryDirectory(prefix="lo-eval-") as tmp:
        clusters = Path(tmp) / "clusters"
        ddir = clusters / domain
        ddir.mkdir(parents=True)
        # seed referenced clusters so a Deploy's clusterRef resolves (else lint
        # rejects a valid spec because the temp env lacks the target cluster)
        for ref in (vspec.get("ref_clusters") or []):
            rdir = clusters / ref
            rdir.mkdir(parents=True, exist_ok=True)
            (rdir / "cluster.lok8s.yaml").write_text(
                "apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\n"
                f"metadata:\n  name: {ref.split('.')[0]}\n"
                f"spec:\n  cluster:\n    domain: {ref}\n")
        # seed any prelude files (e.g. a base spec an addon edit attaches to)
        for name, content in (vspec.get("prelude") or {}).items():
            (ddir / name).write_text(content)
        (ddir / rel_file).write_text(yaml_text)
        env["PATH_CLUSTERS"] = str(clusters)

        log_parts = []
        for base_cmd in commands:
            cmd = [c.replace("{domain}", domain) for c in base_cmd]
            if "{domain}" not in " ".join(base_cmd):
                cmd = cmd + ["--domain", domain]   # lo takes the domain as a global flag
            try:
                p = subprocess.run(cmd, cwd=str(project), env=env,
                                   capture_output=True, text=True, timeout=120)
            except FileNotFoundError as e:
                return "skip", f"{cmd[0]} not runnable: {e}"
            except subprocess.TimeoutExpired:
                return "error", f"timeout: {' '.join(cmd)}"
            log_parts.append(f"$ {' '.join(cmd)}\n{p.stdout}\n{p.stderr}")
            if p.returncode != 0:
                return "fail", "\n".join(log_parts)
        return "pass", "\n".join(log_parts)

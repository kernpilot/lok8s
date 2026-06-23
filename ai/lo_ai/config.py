"""Config loading. Thin wrapper over a YAML dict with path resolution.

Relative paths in the config are resolved against the config file's directory so
the harness can be run from anywhere.
"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Any

try:
    import yaml
except ImportError as e:  # pragma: no cover
    raise SystemExit(
        "PyYAML is required. Install the bench deps:\n"
        "  pip install -r requirements-bench.txt"
    ) from e


class Config:
    """Attribute/dict access over the parsed YAML, plus path helpers."""

    def __init__(self, data: dict, base_dir: Path):
        self._d = data
        self.base_dir = base_dir

    def __getitem__(self, key: str) -> Any:
        return self._d[key]

    def get(self, path: str, default: Any = None) -> Any:
        """Dotted lookup, e.g. cfg.get('llm.conductor.model')."""
        cur: Any = self._d
        for part in path.split("."):
            if not isinstance(cur, dict) or part not in cur:
                return default
            cur = cur[part]
        return cur

    def resolve(self, p: str) -> Path:
        """Resolve a (possibly relative) config path against the config dir."""
        pp = Path(os.path.expanduser(p))
        return pp if pp.is_absolute() else (self.base_dir / pp)

    @property
    def raw(self) -> dict:
        return self._d


def load_config(path: str) -> Config:
    cfg_path = Path(path).expanduser().resolve()
    if not cfg_path.exists():
        raise SystemExit(
            f"config not found: {cfg_path}\n"
            "Copy config.example.yaml to config.yaml and edit it."
        )
    with open(cfg_path) as f:
        data = yaml.safe_load(f) or {}
    return Config(data, cfg_path.parent)

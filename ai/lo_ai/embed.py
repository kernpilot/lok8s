"""Embeddings client for the `semantic` injector. Optional.

Supports Ollama (/api/embeddings) and OpenAI-style (/v1/embeddings). Cosine uses
numpy if present, else a pure-python fallback.
"""
from __future__ import annotations

import json
import math
import urllib.request
from typing import Sequence

try:
    import numpy as _np
except ImportError:  # pragma: no cover
    _np = None


class Embedder:
    def __init__(self, spec: dict):
        self.base_url = spec["base_url"].rstrip("/")
        self.model = spec["model"]
        self.style = spec.get("api_style", "ollama")
        self.timeout = float(spec.get("request_timeout", 60))

    def embed(self, text: str) -> list[float]:
        if self.style == "openai":
            url = f"{self.base_url}/v1/embeddings"
            body = {"model": self.model, "input": text}
        else:  # ollama
            url = f"{self.base_url}/api/embeddings"
            body = {"model": self.model, "prompt": text}
        req = urllib.request.Request(
            url, data=json.dumps(body).encode(), method="POST")
        req.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(req, timeout=self.timeout) as resp:
            payload = json.loads(resp.read().decode())
        if self.style == "openai":
            return payload["data"][0]["embedding"]
        return payload["embedding"]


def cosine(a: Sequence[float], b: Sequence[float]) -> float:
    if _np is not None:
        va, vb = _np.asarray(a, dtype=float), _np.asarray(b, dtype=float)
        denom = (_np.linalg.norm(va) * _np.linalg.norm(vb)) or 1.0
        return float(va @ vb / denom)
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a)) or 1.0
    nb = math.sqrt(sum(y * y for y in b)) or 1.0
    return dot / (na * nb)

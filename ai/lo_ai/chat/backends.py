"""Conductor backends — pluggable + streaming.

- HTTPBackend: a local OpenAI/Ollama endpoint, token-streamed. Does structured
  tool routing (it's "agentic").
- CLIBackend: an external frontier CLI (`claude -p`, `gemini -p`, `codex exec`).
  Used as an escalation/handoff — it answers directly (it has its own tools), so
  it's not driven through our routing loop. Auto-detected via `which`.

Switchable mid-conversation (the app's /model command).
"""
from __future__ import annotations

import json
import shutil
import subprocess
import urllib.request
from typing import Iterator


class Backend:
    name: str = "?"
    label: str = "?"
    agentic: bool = True          # True => driven through our route/execute loop

    def available(self) -> bool:
        return True

    def stream(self, messages: list[dict]) -> Iterator[str]:
        raise NotImplementedError

    def complete(self, messages: list[dict]) -> str:
        return "".join(self.stream(messages))


class HTTPBackend(Backend):
    agentic = True

    def __init__(self, name: str, spec: dict):
        self.name = name
        self.model = spec["model"]
        self.label = f"{name} ({self.model})"
        self.base_url = spec["base_url"].rstrip("/")
        self.api = spec.get("api", "openai")
        self.api_key = spec.get("api_key", "")
        self.temperature = float(spec.get("temperature", 0.2))
        self.num_ctx = spec.get("num_ctx")
        self.think = spec.get("think")
        self.timeout = float(spec.get("request_timeout", 300))

    def _post(self, url: str, body: dict):
        req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
        req.add_header("Content-Type", "application/json")
        if self.api_key:
            req.add_header("Authorization", f"Bearer {self.api_key}")
        return urllib.request.urlopen(req, timeout=self.timeout)

    def stream(self, messages):
        if self.api == "ollama":
            root = self.base_url[:-3] if self.base_url.endswith("/v1") else self.base_url
            opts = {"temperature": self.temperature}
            if self.num_ctx:
                opts["num_ctx"] = int(self.num_ctx)
            body = {"model": self.model, "messages": messages, "stream": True, "options": opts}
            if self.think is not None:
                body["think"] = bool(self.think)
            with self._post(root + "/api/chat", body) as resp:
                for raw in resp:
                    raw = raw.decode().strip()
                    if not raw:
                        continue
                    try:
                        chunk = json.loads(raw)
                    except json.JSONDecodeError:
                        continue
                    tok = (chunk.get("message") or {}).get("content", "")
                    if tok:
                        yield tok
                    if chunk.get("done"):
                        break
        else:  # openai-compatible SSE
            body = {"model": self.model, "messages": messages,
                    "stream": True, "temperature": self.temperature}
            with self._post(f"{self.base_url}/chat/completions", body) as resp:
                for raw in resp:
                    raw = raw.decode().strip()
                    if not raw.startswith("data:"):
                        continue
                    data = raw[5:].strip()
                    if data == "[DONE]":
                        break
                    try:
                        delta = json.loads(data)["choices"][0]["delta"]
                    except (json.JSONDecodeError, KeyError, IndexError):
                        continue
                    tok = delta.get("content") or ""
                    if tok:
                        yield tok


class CLIBackend(Backend):
    agentic = False               # frontier CLI handles its own tools — handoff

    def __init__(self, name: str, spec: dict):
        self.name = name
        self.command = spec["command"]
        self.detect = spec.get("detect", self.command[0])
        self.label = f"{name} (cli: {' '.join(self.command)})"

    def available(self) -> bool:
        return shutil.which(self.detect) is not None

    @staticmethod
    def _flatten(messages: list[dict]) -> str:
        parts = []
        for m in messages:
            role = m["role"]
            if role == "system":
                parts.append(m["content"])
            else:
                parts.append(f"{role.capitalize()}: {m['content']}")
        return "\n\n".join(parts)

    def stream(self, messages):
        proc = subprocess.Popen(self.command, stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, text=True, bufsize=1)
        proc.stdin.write(self._flatten(messages))
        proc.stdin.close()
        for line in proc.stdout:
            yield line
        proc.wait()


def make_backend(name: str, spec: dict) -> Backend:
    return CLIBackend(name, spec) if spec.get("type") == "cli" else HTTPBackend(name, spec)


def load_backends(cfg) -> dict:
    """Build all configured backends. Local endpoints inherit the llm.conductor
    connection but can override the model."""
    base = dict(cfg["llm"]["conductor"])
    out = {}
    for name, spec in (cfg.get("chat.backends") or {}).items():
        spec = dict(spec)
        if spec.get("type") == "cli":
            out[name] = CLIBackend(name, spec)
        else:
            merged = {**base, **spec}          # spec.model overrides
            out[name] = HTTPBackend(name, merged)
    return out

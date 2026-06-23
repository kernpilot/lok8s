"""OpenAI-compatible chat client over stdlib urllib (no heavy deps).

Works with any /v1/chat/completions endpoint: Ollama, llama.cpp server, vLLM,
LM Studio. Supports both native tool-calling (`tools=`) and plain text.
"""
from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from typing import Any


class LLMError(RuntimeError):
    pass


class LLM:
    def __init__(self, spec: dict):
        self.base_url = spec["base_url"].rstrip("/")
        self.model = spec["model"]
        self.api_key = spec.get("api_key", "")
        self.temperature = float(spec.get("temperature", 0.0))
        self.max_tokens = int(spec.get("max_tokens", 1024))
        self.timeout = float(spec.get("request_timeout", 120))

    def chat(
        self,
        messages: list[dict],
        tools: list[dict] | None = None,
        tool_choice: Any = None,
    ) -> dict:
        """Return {content, tool_calls, latency_ms, raw}.

        tool_calls is a list of {name, arguments(dict)} or [] if none.
        """
        body: dict = {
            "model": self.model,
            "messages": messages,
            "temperature": self.temperature,
            "max_tokens": self.max_tokens,
        }
        if tools:
            body["tools"] = tools
            if tool_choice is not None:
                body["tool_choice"] = tool_choice

        url = f"{self.base_url}/chat/completions"
        data = json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method="POST")
        req.add_header("Content-Type", "application/json")
        if self.api_key:
            req.add_header("Authorization", f"Bearer {self.api_key}")

        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                payload = json.loads(resp.read().decode())
        except urllib.error.HTTPError as e:
            raise LLMError(f"{e.code} {e.reason}: {e.read().decode()[:500]}") from e
        except urllib.error.URLError as e:
            raise LLMError(f"cannot reach {url}: {e.reason}") from e
        latency_ms = int((time.time() - t0) * 1000)

        msg = (payload.get("choices") or [{}])[0].get("message", {})
        tool_calls = []
        for tc in msg.get("tool_calls") or []:
            fn = tc.get("function", {})
            args = fn.get("arguments")
            if isinstance(args, str):
                try:
                    args = json.loads(args)
                except json.JSONDecodeError:
                    args = {"_raw": args}
            tool_calls.append({"name": fn.get("name"), "arguments": args or {}})

        return {
            "content": msg.get("content") or "",
            "tool_calls": tool_calls,
            "latency_ms": latency_ms,
            "raw": payload,
        }

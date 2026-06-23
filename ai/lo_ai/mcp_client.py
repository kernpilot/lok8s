"""Minimal MCP stdio client — just enough to do `tools/list` against `lo mcp`.

MCP stdio transport is newline-delimited JSON-RPC 2.0 over the child's
stdin/stdout. We do the initialize handshake then list tools. This gives the
harness the EXACT tool surface the real assistant would route over.
"""
from __future__ import annotations

import json
import os
import queue
import subprocess
import threading
from pathlib import Path
from typing import Any

PROTOCOL_VERSION = "2025-06-18"


class MCPError(RuntimeError):
    pass


class MCPClient:
    def __init__(
        self,
        command: list[str],
        cwd: str = ".",
        env: dict | None = None,
        extra_path: list[str] | None = None,
        timeout: float = 30.0,
    ):
        self.command = command
        self.cwd = str(Path(cwd).expanduser().resolve())
        self.timeout = timeout
        self._proc: subprocess.Popen | None = None
        self._q: "queue.Queue[dict]" = queue.Queue()
        self._next_id = 0

        self._env = os.environ.copy()
        if extra_path:
            prefix = os.pathsep.join(
                str((Path(self.cwd) / p).resolve()) for p in extra_path
            )
            self._env["PATH"] = prefix + os.pathsep + self._env.get("PATH", "")
        if env:
            self._env.update({k: str(v) for k, v in env.items()})

    # -- lifecycle --------------------------------------------------------
    def __enter__(self) -> "MCPClient":
        self._proc = subprocess.Popen(
            self.command,
            cwd=self.cwd,
            env=self._env,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            bufsize=1,
        )
        threading.Thread(target=self._reader, daemon=True).start()
        self._initialize()
        return self

    def __exit__(self, *exc) -> None:
        if self._proc and self._proc.poll() is None:
            try:
                self._proc.terminate()
                self._proc.wait(timeout=5)
            except Exception:
                self._proc.kill()

    # -- io ---------------------------------------------------------------
    def _reader(self) -> None:
        assert self._proc and self._proc.stdout
        for line in self._proc.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                self._q.put(json.loads(line))
            except json.JSONDecodeError:
                # non-JSON log line on stdout; ignore
                continue

    def _send(self, msg: dict) -> None:
        assert self._proc and self._proc.stdin
        self._proc.stdin.write(json.dumps(msg) + "\n")
        self._proc.stdin.flush()

    def _request(self, method: str, params: dict | None = None) -> Any:
        self._next_id += 1
        rid = self._next_id
        self._send({"jsonrpc": "2.0", "id": rid, "method": method,
                    "params": params or {}})
        # wait for the response with this id (drain unrelated notifications)
        deadline_msgs = 0
        while True:
            try:
                msg = self._q.get(timeout=self.timeout)
            except queue.Empty:
                raise MCPError(f"timeout waiting for response to {method!r}")
            if msg.get("id") == rid:
                if "error" in msg:
                    raise MCPError(f"{method} -> {msg['error']}")
                return msg.get("result")
            deadline_msgs += 1
            if deadline_msgs > 1000:
                raise MCPError("too many unrelated messages; giving up")

    def _notify(self, method: str, params: dict | None = None) -> None:
        self._send({"jsonrpc": "2.0", "method": method, "params": params or {}})

    def _initialize(self) -> None:
        self._request("initialize", {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {},
            "clientInfo": {"name": "lo-ai-bench", "version": "0.1"},
        })
        self._notify("notifications/initialized")

    # -- api --------------------------------------------------------------
    def list_tools(self) -> list[dict]:
        """Return [{name, description, inputSchema}, ...], following pagination."""
        tools: list[dict] = []
        cursor: str | None = None
        while True:
            params = {"cursor": cursor} if cursor else {}
            result = self._request("tools/list", params) or {}
            tools.extend(result.get("tools", []))
            cursor = result.get("nextCursor")
            if not cursor:
                break
        return tools

    def call_tool(self, name: str, arguments: dict | None = None) -> str:
        """Invoke a tool; return its text output (digested)."""
        result = self._request("tools/call",
                               {"name": name, "arguments": arguments or {}})
        parts = [c.get("text", "") for c in (result or {}).get("content", [])
                 if c.get("type") == "text"]
        text = "\n".join(p for p in parts if p)
        if (result or {}).get("isError"):
            text = "[tool error] " + text
        return text or "(tool produced no text output)"


def fetch_tools(cfg) -> list[dict]:
    """Get the tool surface, from cache if configured, else live over MCP."""
    cache = cfg.get("lo.tools_cache") or ""
    if cache:
        p = cfg.resolve(cache)
        if p.exists():
            return json.loads(p.read_text())
    lo = cfg["lo"]
    with MCPClient(
        command=lo["mcp_command"],
        cwd=str(cfg.resolve(lo.get("cwd", "."))),
        env=lo.get("env"),
        extra_path=lo.get("extra_path"),
        timeout=lo.get("mcp_timeout", 30),
    ) as client:
        return client.list_tools()

"""The conductor — turns a user message into a transparent, streamed answer.

Local (agentic) backend: route over the flat diet tool menu -> posture-gate the
choice -> execute READ tools via `lo mcp` -> stream a final answer grounded in
the outputs (with schema-in-context when authoring). Frontier CLI backend: a
direct handoff (it brings its own tools). Every step is emitted as an event so
the UI can show exactly what's happening — nothing is a black box.
"""
from __future__ import annotations

import json
import re

from lo_ai.mcp_client import MCPClient, fetch_tools
from lo_ai.tools import ToolCatalog

ROUTE_SYS = """You are the lok8s assistant ({posture} mode). To answer the user you
may run lo tools to gather facts. Respond with ONE JSON object per step:
  {{"tool": "<name>", "args": {{...}}}}   to run a tool, or
  {{"tool": null}}                          when you have enough to answer.
{constraint} Available tools:
{menu}"""

ANSWER_SYS = """You are the lok8s assistant. Answer the user clearly and concisely
from the tool outputs. If they asked you to author lok8s config, output the
complete, valid YAML in a fenced block. Surface the exact `lo ...` command when
relevant. Don't invent tool output you didn't see.{schema}"""


def _json(text: str) -> dict:
    """First balanced {...} object (tracking string literals/escapes) — robust to
    prose around the JSON or a trailing second object, unlike a greedy regex."""
    start = text.find("{")
    if start < 0:
        return {}
    depth = 0
    in_str = esc = False
    for i in range(start, len(text)):
        ch = text[i]
        if in_str:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_str = False
            continue
        if ch == '"':
            in_str = True
        elif ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                try:
                    return json.loads(text[start:i + 1])
                except json.JSONDecodeError:
                    return {}
    return {}


class Conductor:
    def __init__(self, cfg, backend, posture: str = None):
        self.cfg = cfg
        self.backend = backend
        self.posture = posture or cfg.get("chat.posture", "read-only")
        self.catalog = ToolCatalog(fetch_tools(cfg), cfg["injection"])
        self.tools = self.catalog._dieted()
        self.max_steps = int(cfg.get("chat.max_tool_steps", 4))
        self.history: list[dict] = []
        lo = cfg["lo"]
        self._mcp = MCPClient(command=lo["mcp_command"],
                              cwd=str(cfg.resolve(lo.get("cwd", "."))),
                              env=lo.get("env"), extra_path=lo.get("extra_path"),
                              timeout=lo.get("mcp_timeout", 30))
        self._started = False

    # -- lifecycle --
    def start(self):
        if not self._started:
            self._mcp.__enter__()
            self._started = True

    def close(self):
        if self._started:
            self._mcp.__exit__(None, None, None)
            self._started = False

    def set_backend(self, backend):
        self.backend = backend

    # -- helpers --
    def _tag(self, n: str) -> str:
        return "read" if self.catalog.tier(n) in ("readonly", "idempotent") else "write"

    def _menu(self) -> str:
        return "\n".join(
            f"- {n} [{self._tag(n)}]: {(self.catalog.tools[n].description.splitlines() or [''])[0]}"
            for n in self.tools)

    def _exposed(self, n: str) -> bool:
        # drop/deny-filtered tools (plumbing + secret-readers) are off the menu and
        # must never run, even if the model names one and it's read-tagged.
        return n in self.tools

    def _allowed(self, n: str) -> bool:
        if not self._exposed(n):
            return False
        return True if self.posture == "open" else self._tag(n) == "read"

    def _schema(self, user_msg: str) -> str:
        if not re.search(r"author|write|create|cluster\.lok8s|deploy\.lok8s|addon|chart|secret|yaml",
                         user_msg, re.I):
            return ""
        parts = []
        for f in self.cfg.get("chat.schema_files", []) or []:
            p = self.cfg.resolve(f)
            if p.exists():
                parts.append(p.read_text())
        return ("\n\nAUTHORITATIVE lok8s SCHEMA (follow exactly):\n" + "\n".join(parts)) if parts else ""

    # -- main --
    def respond(self, user_msg: str):
        """Generator of events: route / gate / tool / answer_start / token / answer_done / error."""
        self.start()
        self.history.append({"role": "user", "content": user_msg})

        if not self.backend.agentic:                      # frontier CLI handoff
            yield {"type": "handoff", "backend": self.backend.label}
            sys = {"role": "system",
                   "content": "You are assisting in a lok8s `lo chat` session (cluster ops). "
                              "Answer the user; you may use your own tools."}
            answer = ""
            try:
                for tok in self.backend.stream([sys] + self.history):
                    answer += tok
                    yield {"type": "token", "text": tok}
            except Exception as e:
                yield {"type": "error", "error": str(e)}
            self.history.append({"role": "assistant", "content": answer})
            yield {"type": "answer_done"}
            return

        constraint = ("All tools may run, including [write] tools." if self.posture == "open"
                      else "Only [read] tools may run; [write] tools are blocked.")
        route_msgs = [{"role": "system",
                       "content": ROUTE_SYS.format(posture=self.posture, constraint=constraint,
                                                   menu=self._menu())}] + list(self.history)
        tool_ctx = []
        for _ in range(self.max_steps):
            try:
                j = _json(self.backend.complete(route_msgs))
            except Exception as e:
                yield {"type": "error", "error": f"routing: {e}"}
                return
            tool = j.get("tool")
            if not tool:
                break
            args = j.get("args") or {}
            yield {"type": "route", "tool": tool, "args": args}
            if not self.catalog.is_tool(tool):
                yield {"type": "gate", "tool": tool, "decision": "unknown"}
                route_msgs += [{"role": "assistant", "content": json.dumps(j)},
                               {"role": "user", "content": f"{tool} is not a valid tool. Pick from the menu or answer."}]
                continue
            if not self._allowed(tool):
                if not self._exposed(tool):
                    reason = f"'{tool}' is not available in lo chat"
                    reprompt = f"{tool} is not available here (off the menu). Pick a tool from the menu or answer directly."
                else:
                    reason = f"{self.posture}: '{tool}' is a write tool"
                    reprompt = (f"{tool} is a write tool — blocked in {self.posture} mode. "
                                "Use `--posture open`, or a read tool.")
                yield {"type": "gate", "tool": tool, "decision": "blocked", "reason": reason}
                route_msgs += [{"role": "assistant", "content": json.dumps(j)},
                               {"role": "user", "content": reprompt}]
                continue
            try:
                out = self._mcp.call_tool(tool, args)[:2000]
            except Exception as e:
                out = f"[error] {e}"
            tool_ctx.append((tool, out))
            yield {"type": "tool", "tool": tool, "args": args, "output": out}
            route_msgs += [{"role": "assistant", "content": json.dumps(j)},
                           {"role": "user", "content": f"Output of {tool}:\n{out}"}]

        ctx = "\n\n".join(f"$ lo {t}\n{o}" for t, o in tool_ctx) or "(no tools were run)"
        ans_msgs = ([{"role": "system", "content": ANSWER_SYS.format(schema=self._schema(user_msg))}]
                    + list(self.history)
                    + [{"role": "user", "content": f"Tool outputs:\n{ctx}\n\nNow answer my request above."}])
        yield {"type": "answer_start"}
        answer = ""
        try:
            for tok in self.backend.stream(ans_msgs):
                answer += tok
                yield {"type": "token", "text": tok}
        except Exception as e:
            yield {"type": "error", "error": str(e)}
        self.history.append({"role": "assistant", "content": answer})
        yield {"type": "answer_done"}

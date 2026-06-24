"""Tool catalog + injection strategies.

The benchmark's whole point is comparing what the model sees at a decision point:

  raw       — every tool (the deliberately-bad baseline)
  diet      — minus plumbing/CI + secret-readers
  hierarchy — verb -> (namespace) -> tool, never > max_tools on screen at once
  semantic  — top-k tools by embedding similarity to the user intent

Verbs default to the CLI's own tiers (@readonly -> investigate, @idempotent ->
build, lifecycle -> operate, everything else -> configure) but are fully
overridable via injection.verbs in the config.
"""
from __future__ import annotations

import fnmatch
from collections import Counter
from dataclasses import dataclass, field
from typing import Optional

# default verb taxonomy; selectors: tool name, glob, or @readonly/@idempotent/@mutating
DEFAULT_VERBS: dict[str, list[str]] = {
    "investigate": ["@readonly"],
    "build": ["@idempotent"],
    "operate": [
        "lo_up", "lo_down", "lo_provision", "lo_deploy", "lo_destroy",
        "lo_clean", "lo_use", "lo_trust", "lo_tilt_*", "lo_registry_*",
        "lo_image_clean",
    ],
    "configure": ["@mutating"],  # catch-all; keep last
}


@dataclass
class Tool:
    name: str
    description: str
    input_schema: dict = field(default_factory=dict)


@dataclass
class Option:
    """One thing presented to the model at a routing step."""
    name: str
    description: str
    kind: str           # "tool" | "group"
    parameters: dict = field(default_factory=dict)


class ToolCatalog:
    def __init__(self, tools: list[dict], injection_cfg: dict):
        self.tools: dict[str, Tool] = {
            t["name"]: Tool(t["name"], (t.get("description") or "").strip(),
                            t.get("inputSchema") or t.get("input_schema") or {})
            for t in tools
        }
        self.cfg = injection_cfg or {}
        self.deny = set(self.cfg.get("deny", []))
        self.drop = set(self.cfg.get("drop", []))
        self.max_tools = int(self.cfg.get("max_tools", 8))
        self.compress = bool(self.cfg.get("compress", True))
        tiers = self.cfg.get("tiers", {}) or {}
        self._readonly = set(tiers.get("readonly", []))
        self._idempotent = set(tiers.get("idempotent", []))
        self._verbs = self.cfg.get("verbs") or DEFAULT_VERBS

    # -- classification ---------------------------------------------------
    def tier(self, name: str) -> str:
        if name in self._readonly:
            return "readonly"
        if name in self._idempotent:
            return "idempotent"
        return "mutating"

    def group(self, name: str) -> Optional[str]:
        """Namespace from lo_<group>_* when <group> is shared by >=2 tools."""
        parts = name[3:].split("_") if name.startswith("lo_") else name.split("_")
        if len(parts) < 2:
            return None
        return parts[0] if parts[0] in self._dynamic_groups() else None

    def _dynamic_groups(self) -> set[str]:
        firsts = Counter()
        for n in self.tools:
            parts = n[3:].split("_") if n.startswith("lo_") else n.split("_")
            if len(parts) >= 2:
                firsts[parts[0]] += 1
        return {g for g, c in firsts.items() if c >= 2}

    # -- candidate sets ---------------------------------------------------
    def _all(self) -> list[str]:
        return [n for n in self.tools if n not in self.deny]

    def _dieted(self) -> list[str]:
        return [n for n in self._all() if n not in self.drop]

    def _match(self, selector: str, names: list[str]) -> list[str]:
        if selector == "@readonly":
            return [n for n in names if self.tier(n) == "readonly"]
        if selector == "@idempotent":
            return [n for n in names if self.tier(n) == "idempotent"]
        if selector == "@mutating":
            return [n for n in names if self.tier(n) == "mutating"]
        if any(c in selector for c in "*?["):
            return [n for n in names if fnmatch.fnmatch(n, selector)]
        return [n for n in names if n == selector]

    def _verb_members(self) -> dict[str, list[str]]:
        """Resolve verbs over the dieted set; first verb to claim a tool wins."""
        pool = self._dieted()
        claimed: set[str] = set()
        out: dict[str, list[str]] = {}
        for verb, selectors in self._verbs.items():
            members: list[str] = []
            for sel in selectors:
                for n in self._match(sel, pool):
                    if n not in claimed:
                        members.append(n)
                        claimed.add(n)
            out[verb] = members
        return out

    # -- schema shaping ---------------------------------------------------
    def _shape(self, name: str) -> dict:
        t = self.tools[name]
        schema = t.input_schema or {"type": "object", "properties": {}}
        if not self.compress:
            return schema
        props = {}
        for k, v in (schema.get("properties") or {}).items():
            v = v or {}
            desc = (v.get("description") or "").strip().splitlines()
            props[k] = {"type": v.get("type", "string")}
            if desc:
                props[k]["description"] = desc[0]
        return {"type": "object", "properties": props,
                "required": schema.get("required", [])}

    def _tool_option(self, name: str) -> Option:
        desc = self.tools[name].description.splitlines()
        return Option(name, desc[0] if desc else "", "tool", self._shape(name))

    def _group_option(self, label: str, members: list[str]) -> Option:
        sample = ", ".join(m.replace("lo_", "") for m in members[:6])
        more = "" if len(members) <= 6 else f", +{len(members) - 6} more"
        return Option(label, f"{label}: {sample}{more}", "group")

    # -- presentation -----------------------------------------------------
    def present(self, strategy: str, query: str = "", embedder=None,
                path: Optional[list[str]] = None) -> list[Option]:
        """Options to show at the current step. Group options mean 'descend'."""
        path = path or []
        if strategy == "raw":
            return [self._tool_option(n) for n in self._all()]
        if strategy == "diet":
            return [self._tool_option(n) for n in self._dieted()]
        if strategy == "semantic":
            return self._semantic(query, embedder)
        if strategy == "hierarchy":
            return self._hierarchy(path)
        raise ValueError(f"unknown injection strategy: {strategy!r}")

    def _semantic(self, query: str, embedder) -> list[Option]:
        if embedder is None:
            raise ValueError("semantic strategy needs embeddings.enabled: true")
        from lo_ai.embed import cosine
        qv = embedder.embed(query)
        scored = []
        for n in self._dieted():
            tv = embedder.embed(f"{n}: {self.tools[n].description}")
            scored.append((cosine(qv, tv), n))
        scored.sort(reverse=True)
        return [self._tool_option(n) for _, n in scored[: self.max_tools]]

    def _hierarchy(self, path: list[str]) -> list[Option]:
        members = self._verb_members()
        if not path:                                  # level 0: verbs
            return [self._group_option(v, m) for v, m in members.items() if m]
        verb = path[0]
        names = members.get(verb, [])
        if len(path) == 1:
            if len(names) <= self.max_tools:
                return [self._tool_option(n) for n in names]
            # too many: split by namespace
            opts: list[Option] = []
            grouped: dict[str, list[str]] = {}
            for n in names:
                g = self.group(n)
                if g:
                    grouped.setdefault(g, []).append(n)
                else:
                    opts.append(self._tool_option(n))
            for g, ms in grouped.items():
                opts.append(self._group_option(g, ms))
            # don't truncate — dropping a tool here would unfairly fail routing.
            # A verb that still exceeds max_tools after grouping is itself a finding.
            return opts
        # level 2: a namespace within a verb
        ns = path[1]
        return [self._tool_option(n) for n in names if self.group(n) == ns]

    # -- helpers for the runner ------------------------------------------
    def is_tool(self, name: str) -> bool:
        return name in self.tools

    def known_option_names(self, options: list[Option]) -> set[str]:
        return {o.name for o in options}

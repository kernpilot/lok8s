"""lo chat — a fully-local, transparent, streaming assistant over `lo mcp`.

Architecture (decided by the benchmark, see ../../LOCAL-AI-PLAN.md):
  conductor (gemma4:e2b default / 14b option / frontier-CLI escalation)
    + flat diet tools (lo mcp)  + schema-in-context authoring
    + doctor-tree-guided debug  + a DETERMINISTIC read-only posture gate.
"""

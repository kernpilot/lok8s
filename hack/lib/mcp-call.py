#!/usr/bin/env python3
"""mcp-call.py: one tools/call through `lo mcp start` for hack/parity-kubehz.sh.

It starts `<lo> mcp start --allow-destructive` in the working directory,
sends initialize and the call over stdio, and prints two lines: "ok" or
"error", then the text of the result (for an ok call, the stdout and stderr
of the tool).

Usage: mcp-call.py <lo> <tool> <arguments-json>
"""

import json
import subprocess
import sys

LO, TOOL, ARGUMENTS = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])

proc = subprocess.Popen([LO, "mcp", "start", "--allow-destructive", "--log-level", "error"],
                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)


def send(msg):
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()


def answer(want_id):
    for line in proc.stdout:
        msg = json.loads(line)
        if msg.get("id") == want_id:
            return msg
    raise SystemExit("the server closed stdout before it answered")


send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
    "protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "parity", "version": "0"}}})
answer(1)
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
send({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": TOOL, "arguments": ARGUMENTS}})
result = answer(2)["result"]
proc.stdin.close()
proc.wait(timeout=30)
text = "".join(c.get("text", "") for c in result.get("content", []))
try:
    out = json.loads(text)
    text = out.get("stdout", "") + out.get("stderr", "")
except (ValueError, AttributeError):
    pass
print("error" if result.get("isError") else "ok")
print(text)

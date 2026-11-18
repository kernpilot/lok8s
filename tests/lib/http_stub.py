#!/usr/bin/env python3
"""http_stub.py: a local HTTP or HTTPS server for the bats tests that run the real curl.

Usage: http_stub.py <port-file> <request-log> [<cert.pem> <key.pem>]

It binds 127.0.0.1 on a free port, writes the port to <port-file> and
serves until it is stopped, for 120 seconds at most. With a certificate and
a key it serves HTTPS. The answers:

  GET  /api/spaces/<id>             200 {"ok":true,"data":{"id":"<id>"}}
  POST /api/spaces/<id>/join-token  201, the ticket in STUB_TICKET
  GET  /snap.db                     200, the bytes of the file STUB_SNAPSHOT names
  any other request                 404

Each request appends one line to <request-log>: the method, the path and the
credential: "bearer" when the Authorization header is "Bearer " followed by
STUB_BEARER, "none" without the header, "other" for any other value. The log
never holds the value.
"""
import http.server
import json
import os
import ssl
import sys
import threading

PORT_FILE, LOG = sys.argv[1:3]
CERT_KEY = sys.argv[3:5]


def compact(obj):
    return json.dumps(obj, separators=(",", ":")).encode()


class Handler(http.server.BaseHTTPRequestHandler):
    def _answer(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        auth = self.headers.get("Authorization")
        if auth is None:
            cred = "none"
        elif auth == "Bearer " + os.environ.get("STUB_BEARER", ""):
            cred = "bearer"
        else:
            cred = "other"
        with open(LOG, "a", encoding="utf-8") as log:
            log.write(f"{self.command} {self.path} {cred}\n")

        parts = self.path.split("?")[0].split("/")
        code, body = 404, compact({"ok": False})
        if self.command == "GET" and len(parts) == 4 and parts[1:3] == ["api", "spaces"]:
            code, body = 200, compact({"ok": True, "data": {"id": parts[3]}})
        elif (self.command == "POST" and len(parts) == 5 and parts[1:3] == ["api", "spaces"]
              and parts[4] == "join-token"):
            code, body = 201, compact({"ok": True, "data": {
                "token": os.environ["STUB_TICKET"], "expiresAt": "2026-08-07T20:00:00Z"}})
        elif self.command == "GET" and self.path == "/snap.db":
            with open(os.environ["STUB_SNAPSHOT"], "rb") as snap:
                code, body = 200, snap.read()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = _answer
    do_POST = _answer

    def log_message(self, *args):
        pass


server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
if CERT_KEY:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(*CERT_KEY)
    server.socket = ctx.wrap_socket(server.socket, server_side=True)
# A test that fails before its teardown cannot leave the server behind for long.
threading.Timer(120, os._exit, (0,)).start()
part = PORT_FILE + ".part"
with open(part, "w", encoding="utf-8") as f:
    f.write(str(server.server_address[1]))
os.replace(part, PORT_FILE)
server.serve_forever()

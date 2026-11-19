#!/usr/bin/env python3
"""http_stub.py: a local HTTP or HTTPS server for the bats tests that run the real curl.

Usage: http_stub.py <port-file> <request-log> [<cert.pem> <key.pem>]

It binds 127.0.0.1 on a free port, writes the port to <port-file> and
serves until it is stopped, for 120 seconds at most. With a certificate and
a key it serves HTTPS. The answers:

  a request in the route table      the answer of the table (below)
  GET  /api/spaces/<id>             200 {"ok":true,"data":{"id":"<id>"}}
  POST /api/spaces/<id>/join-token  201, the ticket in STUB_TICKET
  GET  /snap.db                     200, the bytes of the file STUB_SNAPSHOT names
  CONNECT <host>:<port>             a tunnel, when STUB_TUNNEL_CERT and
                                    STUB_TUNNEL_KEY name a certificate and a key:
                                    the server answers 200, then speaks TLS with
                                    that certificate in the tunnel and serves the
                                    requests in it with these same answers
  any other request                 404

The route table: STUB_ROUTES names a JSON file with one entry for each
request, keyed "<METHOD> <path>" (the path as curl sends it, with the
query). An entry is {"code": <status>, "body": <a string, or JSON that the
server sends compact>, "headers": {<name>: <value>}}. With
"fail_if_body_has": <text>, a request body that holds <text> gets
"fail_code" (default 400) and no body. A key without the query matches too
when the full path has no entry.

Each request appends one line to <request-log>: the method, the path and the
credential: "bearer" when the Authorization header is "Bearer " followed by
a word of STUB_BEARER (one bearer, or a list with a space between two),
"basic" when it is "Basic " followed by the base64 of STUB_BASIC, "none"
without the header, "other" for any other value. The log never holds the
value. A CONNECT line has the target in place of the path.
"""
import base64
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


def load_routes():
    path = os.environ.get("STUB_ROUTES")
    if not path:
        return {}
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def log_line(text):
    with open(LOG, "a", encoding="utf-8") as log:
        log.write(text + "\n")


class Handler(http.server.BaseHTTPRequestHandler):
    def _credential(self):
        auth = self.headers.get("Authorization")
        if auth is None:
            return "none"
        if auth in ["Bearer " + t for t in os.environ.get("STUB_BEARER", "").split()]:
            return "bearer"
        basic = os.environ.get("STUB_BASIC")
        if basic and auth == "Basic " + base64.b64encode(basic.encode()).decode():
            return "basic"
        return "other"

    def _answer(self):
        length = int(self.headers.get("Content-Length") or 0)
        data = self.rfile.read(length) if length else b""
        log_line(f"{self.command} {self.path} {self._credential()}")

        parts = self.path.split("?")[0].split("/")
        code, body, headers = 404, compact({"ok": False}), {}
        routes = load_routes()
        route = routes.get(f"{self.command} {self.path}") or routes.get(
            f"{self.command} {self.path.split('?')[0]}")
        if route is not None:
            code = route.get("code", 200)
            raw = route.get("body", "")
            body = raw.encode() if isinstance(raw, str) else compact(raw)
            headers = route.get("headers", {})
            marker = route.get("fail_if_body_has")
            if marker and marker.encode() in data:
                code, body, headers = route.get("fail_code", 400), b"", {}
        elif self.command == "GET" and len(parts) == 4 and parts[1:3] == ["api", "spaces"]:
            code, body = 200, compact({"ok": True, "data": {"id": parts[3]}})
        elif (self.command == "POST" and len(parts) == 5 and parts[1:3] == ["api", "spaces"]
              and parts[4] == "join-token"):
            code, body = 201, compact({"ok": True, "data": {
                "token": os.environ["STUB_TICKET"], "expiresAt": "2026-08-07T20:00:00Z"}})
        elif self.command == "GET" and self.path == "/snap.db":
            with open(os.environ["STUB_SNAPSHOT"], "rb") as snap:
                code, body = 200, snap.read()
        if code in (204, 304):
            body = b""
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        for name, value in headers.items():
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = _answer
    do_POST = _answer
    do_DELETE = _answer

    def do_CONNECT(self):
        log_line(f"CONNECT {self.path} none")
        cert = os.environ.get("STUB_TUNNEL_CERT")
        key = os.environ.get("STUB_TUNNEL_KEY")
        if not cert or not key:
            self.send_error(405)
            return
        self.send_response(200, "Connection established")
        self.end_headers()
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(cert, key)
        try:
            tls = ctx.wrap_socket(self.connection, server_side=True)
        except (ssl.SSLError, OSError):
            self.close_connection = True
            return
        # One more handler on the TLS stream serves the request in the tunnel.
        Handler(tls, self.client_address, self.server)
        self.close_connection = True

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

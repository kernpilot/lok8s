#!/usr/bin/env python3
"""A kubehz api stub over HTTPS for hack/parity-kubehz.sh (the register cases).

Usage: kubehz-api-stub.py <request-log> <cert.pem> <key.pem>

It listens on 127.0.0.1 at a free port and prints "port <n>". Each request
adds one line to <request-log>: the method, the path without the query,
whether an Authorization header came (never its value), and the JSON body
with sorted keys. POST /api/clusters/register answers one response that
fits every register mode and hands out a new bind secret. The two Hetzner
calls of the claim-key mode get an empty key list and a created key.
"""
import http.server, json, ssl, sys

log, cert, key = sys.argv[1:4]
NEXT = "009c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"


class Handler(http.server.BaseHTTPRequestHandler):
    def record(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        try:
            body = json.dumps(json.loads(raw), sort_keys=True) if raw else "-"
        except ValueError:
            body = "unparsable"
        bearer = "yes" if self.headers.get("Authorization") else "no"
        with open(log, "a") as f:
            f.write(f"{self.command} {self.path.split('?')[0]} bearer={bearer} {body}\n")

    def reply(self, code, obj):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        self.record()
        self.reply(200, {"ssh_keys": []})

    def do_POST(self):
        self.record()
        if self.path == "/api/clusters/register":
            self.reply(200, {"id": "cl-parity", "registered": True, "claimed": True, "bindSecret": NEXT,
                             "claimKey": {"publicKey": "ssh-ed25519 AAAA k", "fingerprint": "aa:bb",
                                          "name": "kubehz-claim-bind-reg.dev"}})
        else:
            self.reply(201, {"ssh_key": {"id": 43}})

    def log_message(self, *args):
        pass


srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
print(f"port {srv.server_address[1]}", flush=True)
srv.serve_forever()
